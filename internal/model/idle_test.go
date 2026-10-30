package model

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"magi/internal/core/protocol"
)

func TestHTTPIdleTimeoutClassifiesByActualStreamState(t *testing.T) {
	if streamIdleTimeout != 180*time.Second {
		t.Fatal("production idle default changed")
	}
	for _, tc := range []struct {
		name, body string
		headers    bool
	}{
		{"before_headers", "", false},
		{"empty_body", "", true},
		{"keepalive_only", ": ping\n\n", true},
		{"partial_generation", event(toolCallStart), true},
		{"known_finish_usage", event(toolCallStart) + event(toolCallFinish), true},
		{"known_finish_only", event(toolCallStart) + event(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observation := &attemptObservation{}
			exited := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(exited)
				_, _ = io.Copy(io.Discard, r.Body)
				if tc.headers {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, tc.body)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			observeAttempts(t, observation)
			client := &client{attempts: 1, key: "test-key", url: server.URL, http: server.Client(), idleTimeout: 80 * time.Millisecond}
			response, err := client.Invoke(context.Background(), requestFixture())
			if !errors.Is(err, protocol.ErrModelUnavailable) || !reflect.DeepEqual(response, protocol.Response{}) {
				t.Fatal("idle returned usable output or changed error identity", err)
			}
			if observation.StreamFailure != streamIdle {
				t.Fatal("idle failure event missing")
			}
			select {
			case <-exited:
			case <-time.After(time.Second):
				t.Fatal("HTTP request was not canceled")
			}
		})
	}
}

func TestHTTPIdleAllowsContinuousChunksPastIdleInterval(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for range 12 {
			_, _ = io.WriteString(w, ": keepalive\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
		_, _ = io.WriteString(w, event(toolCallStart)+event(toolCallFinish)+event("[DONE]"))
	}))
	defer server.Close()
	c := &client{attempts: 1, key: "test-key", url: server.URL, http: server.Client(), idleTimeout: 100 * time.Millisecond}
	started := time.Now()
	response, err := c.Invoke(context.Background(), requestFixture())
	if err != nil || time.Since(started) < 2*c.idleTimeout || len(response.Message.ToolCalls) != 1 {
		t.Fatal("active stream treated as total-duration timeout", err)
	}
}

func TestHTTPIdleReconnectsAndKeepsHealthyHTTP1Connections(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	type connection struct{ address, protocol string }
	connections := make(chan connection, 3)
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		connections <- connection{r.RemoteAddr, r.Proto}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, event(toolCallStart))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, event(toolCallStart)+event(toolCallFinish)+event("[DONE]"))
	}))
	server.EnableHTTP2 = true // The client must choose HTTP/1 even when HTTP/2 is available.
	server.StartTLS()
	defer server.Close()
	provider, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	client := provider.(*client)
	client.attempts = 1
	transport := client.http.Transport.(*http.Transport)
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	defer transport.CloseIdleConnections()
	client.url, client.idleTimeout = server.URL, 150*time.Millisecond
	_, err = client.Invoke(context.Background(), requestFixture())
	if !errors.Is(err, protocol.ErrModelUnavailable) {
		t.Fatal("partial stream did not produce an idle timeout", err)
	}
	interrupted := <-connections
	var healthy [2]connection
	for i := range healthy {
		response, err := client.Invoke(context.Background(), requestFixture())
		if err != nil || len(response.Message.ToolCalls) != 1 {
			t.Fatal("request after interrupted stream failed", err)
		}
		healthy[i] = <-connections
	}
	for _, c := range []connection{interrupted, healthy[0], healthy[1]} {
		if c.protocol != "HTTP/1.1" {
			t.Fatal("client negotiated another protocol", c.protocol)
		}
	}
	if interrupted.address == healthy[0].address {
		t.Fatal("retry reused the interrupted connection")
	}
	if healthy[0].address != healthy[1].address {
		t.Fatal("complete responses did not retain healthy connection reuse")
	}
}

func TestHTTPIdleParentCancellationAndDeadlineWin(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			exited := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(exited)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, event(toolCallStart))
				w.(http.Flusher).Flush()
				if !deadline {
					cancel()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			request := requestFixture()
			if deadline {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 60*time.Millisecond)
				defer deadlineCancel()
			}
			c := &client{attempts: 1, key: "test-key", url: server.URL, http: server.Client(), idleTimeout: time.Second}
			_, err := c.Invoke(ctx, request)
			want := context.Canceled
			if deadline {
				want = context.DeadlineExceeded
			}
			var failure modelFailure
			if !errors.Is(err, want) || errors.As(err, &failure) || errors.Is(err, errStreamIdleTimeout) {
				t.Fatal("parent cancellation became retryable idle", err)
			}
			select {
			case <-exited:
			case <-time.After(time.Second):
				t.Fatal("request retained after cancellation")
			}
		})
	}
}

func TestIdleWatchStopAndZeroByteRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		w := newIdleWatch(cancel, 40*time.Millisecond)
		w.stop()
		time.Sleep(80 * time.Millisecond)
		if ctx.Err() != nil {
			t.Fatal("stopped timer canceled completed request")
		}
		ctx2, cancel2 := context.WithCancelCause(context.Background())
		defer cancel2(nil)
		w2 := newIdleWatch(cancel2, 40*time.Millisecond)
		defer w2.stop()
		r := responseBody{ReadCloser: io.NopCloser(zeroIdleReader{}), watch: w2}
		for range 8 {
			_, _ = r.Read(make([]byte, 1))
			time.Sleep(10 * time.Millisecond)
		}
		if !errors.Is(context.Cause(ctx2), errStreamIdleTimeout) {
			t.Fatal("zero-byte reads extended idle deadline")
		}
	})
}

type zeroIdleReader struct{}

func (zeroIdleReader) Read([]byte) (int, error) { return 0, nil }

func TestHTTPIdleExcludesObservationPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		previous := protocol.Observer
		t.Cleanup(func() { protocol.Observer = previous })
		protocol.Observer = observationFuncs{started: func(ctx context.Context, _ string, _ []byte) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
		var calls atomic.Int32
		transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			body := event(toolCallStart) + event(toolCallFinish) + event("[DONE]")
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		c := &client{attempts: 1, key: "test-key", url: endpoint, http: &http.Client{Transport: transport}, idleTimeout: 80 * time.Millisecond}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		done := make(chan error, 1)
		exited := make(chan struct{})
		request := requestFixture()
		go func() {
			defer close(exited)
			_, err := c.Invoke(ctx, request)
			done <- err
		}()
		defer func() {
			cancel()
			<-exited
		}()
		<-entered
		select {
		case err := <-done:
			t.Fatal("pause counted as idle", err)
		case <-time.After(2 * c.idleTimeout):
		}
		if calls.Load() != 0 {
			t.Fatal("HTTP started before observation released")
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 1 {
			t.Fatal("released observation did not start exactly one HTTP request")
		}
	})
}

func TestHTTPIdleErrorBodyPreservesHTTPClassification(t *testing.T) {
	observation := &attemptObservation{}
	observeAttempts(t, observation)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	c := &client{attempts: 1, key: "test-key", url: server.URL, http: server.Client(), idleTimeout: 80 * time.Millisecond}
	_, err := c.Invoke(context.Background(), requestFixture())
	if !errors.Is(err, protocol.ErrModelUnavailable) || err.Error() != "model unavailable: DeepSeek HTTP 400" {
		t.Fatal("HTTP status policy changed", err)
	}
	if observation.StreamFailure != streamIdle {
		t.Fatal("error-body idle detail missing")
	}
}
