package model

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFromEnvKeepsHTTP1WhenDefaultTransportAdvertisesHTTP2(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "fixture-key")
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Proto != "HTTP/1.1" {
			t.Errorf("model transport negotiated %s", r.Proto)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, event(toolCallStart)+event(toolCallFinish)+event("[DONE]"))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	// Clone can inherit an already configured default transport's h2 ALPN.
	// The factory must align negotiation with its HTTP/1-only connection policy.
	previous := http.DefaultTransport
	seed := previous.(*http.Transport).Clone()
	seed.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	seed.TLSClientConfig.NextProtos = []string{"h2", "http/1.1"}
	http.DefaultTransport = seed
	t.Cleanup(func() { http.DefaultTransport = previous; seed.CloseIdleConnections() })
	provider, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	client := provider.(*client)
	client.url, client.attempts = server.URL, 1
	defer client.http.CloseIdleConnections()
	response, err := client.Invoke(context.Background(), requestFixture())
	if err != nil || len(response.Message.ToolCalls) != 1 {
		t.Fatalf("HTTP/1 model stream failed after inheriting h2 ALPN: %v", err)
	}
}
