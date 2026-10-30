package characters

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"magi/internal/core/protocol"
	"magi/internal/storage/mock"
)

func TestHistoryReadSharesWorkspaceFragments(t *testing.T) {
	isolateBindings(t)
	setupHistoryStorage(t)
	ctx := context.Background()
	for _, lane := range []protocol.Role{protocol.RolePlanner, protocol.RoleWorker, protocol.RoleReviewer} {
		ref := numberedTestSource(t, lane, "orphan original 汉🙂")
		page, err := readHistory(ctx, historyReadArgs{Ref: ref})
		noError(t, err)
		if page != "orphan original 汉🙂" {
			t.Fatal("shared original changed", page)
		}
		if _, cached := cachedHistory(ref); cached {
			t.Fatal("old history retained in current-round cache")
		}
	}
	// Identical replies remain distinct sources, accessible through the same
	// history tool both before and after the round's cache is discarded.
	original := "  用户原文\r\n{\"choice\": \"no\"}\n"
	for _, expected := range []string{"U1", "U2"} {
		ref, err := CacheUserReply(ctx, original)
		noError(t, err)
		if ref != expected {
			t.Fatal("user replies were deduplicated or used role numbering", ref)
		}
	}
	for _, flush := range []bool{false, true} {
		if flush {
			noError(t, FlushHistory(ctx, protocol.Storage))
			ResetHistory()
		}
		body, err := readHistory(ctx, historyReadArgs{Ref: "U1"})
		noError(t, err)
		if body != original {
			t.Fatal("user history lost exact input or added a wrapper", body)
		}
	}
	for _, ref := range []string{"P999", "P01", "U999", "U01", "U0", "U", "M1", "../P1", "logical-archive"} {
		if _, err := readHistory(ctx, historyReadArgs{Ref: ref}); err == nil {
			t.Fatal("invalid or missing source accepted", ref)
		}
	}
}

func TestHistoryReadPreservesCompleteUTF8Fragments(t *testing.T) {
	isolateBindings(t)
	for _, text := range []string{strings.Repeat("汉🙂\r\n\"", 5000), "", string([]byte{0xff})} {
		ctx := context.Background()
		setupHistoryStorage(t)
		ref, err := CacheHistoryOutput(ctx, protocol.RoleReviewer, text)
		noError(t, err)
		if _, cached := cachedHistory(ref); !cached {
			t.Fatal("registration did not retain the original")
		}
		result, err := readHistory(ctx, historyReadArgs{Ref: ref})
		if text == string([]byte{0xff}) {
			if !protocol.IsCode(err, protocol.CodeIntegrityFailure) {
				t.Fatal("invalid UTF-8 was replaced or admitted", err)
			}
			continue
		}
		noError(t, err)
		if result != text {
			t.Fatal("complete original was truncated")
		}
	}
}

func TestHistoryRegistrationAllocatesDistinctReferences(t *testing.T) {
	isolateBindings(t)
	setupHistoryStorage(t)
	ctx := context.Background()
	body := "original"
	ref, err := CacheHistoryOutput(ctx, protocol.RoleWorker, body)
	noError(t, err)
	next, err := CacheHistoryOutput(ctx, protocol.RoleWorker, "original")
	noError(t, err)
	if ref != "W1" || next != "W2" {
		t.Fatal("separate registrations did not receive distinct numbers", ref, next)
	}
	for _, ref := range []string{ref, next} {
		source, found := cachedHistory(ref)
		if !found || source != "original" {
			t.Fatal("registration changed an original", source)
		}
	}
}

func readHistory(ctx context.Context, args historyReadArgs) (string, error) {
	body, _ := json.Marshal(args)
	outcome, err := ReadHistory(ctx, body)
	return string(outcome), err
}

func numberedTestSource(t *testing.T, role protocol.Role, text string) string {
	t.Helper()
	ref, err := protocol.Storage.Allocate(string(role))
	noError(t, err)
	noError(t, protocol.Storage.WriteFragment(context.Background(), ref, []byte(text)))
	return ref
}

type historyStorage struct {
	protocol.StorageBackend
	err error
}

func (s *historyStorage) ReadFragment(context.Context, string) ([]byte, error) {
	return []byte("original"), s.err
}

func TestHistoryReadOnlyRepairsReferenceErrors(t *testing.T) {
	isolateBindings(t)
	for _, failure := range []error{
		protocol.InvalidCommand("numbered history reference does not exist"),
		protocol.IntegrityFailure("history path changed"),
		errors.New("disk read failed"),
		context.Canceled,
		context.DeadlineExceeded,
	} {
		t.Run(failure.Error(), func(t *testing.T) {
			ResetHistory()
			protocol.Storage = &historyStorage{err: failure}
			_, err := readHistory(context.Background(), historyReadArgs{Ref: "P1"})
			var refusal protocol.ToolCallError
			if protocol.IsCode(failure, protocol.CodeInvalidCommand) {
				if !errors.As(err, &refusal) || refusal.Detail != "SOURCE_UNAVAILABLE" {
					t.Fatal("missing reference is not repairable", err)
				}
			} else if errors.As(err, &refusal) || !errors.Is(err, failure) {
				t.Fatal("runtime failure was hidden or changed", err)
			}
		})
	}
}

type parallelHistoryStorage struct {
	protocol.StorageBackend
	entered chan string
	release chan struct{}
}

func (s parallelHistoryStorage) ReadFragment(_ context.Context, ref string) ([]byte, error) {
	s.entered <- ref
	<-s.release
	return []byte(ref), nil
}

func TestHistoricalReadsOverlap(t *testing.T) {
	isolateBindings(t)
	disk := parallelHistoryStorage{entered: make(chan string, 2), release: make(chan struct{})}
	protocol.Storage = disk
	var group sync.WaitGroup
	for _, ref := range []string{"P1", "W1"} {
		group.Go(func() {
			body, err := resolveHistory(context.Background(), ref)
			if err != nil || string(body) != ref {
				t.Errorf("history changed: %q, %v", body, err)
			}
		})
	}
	// Always unblock and join readers, including when a serialization regression
	// prevents the second read from reaching storage.
	defer group.Wait()
	defer close(disk.release)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for range 2 {
		select {
		case <-disk.entered:
		case <-timer.C:
			t.Fatal("independent history reads were serialized")
		}
	}
}

func noError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func setupHistoryStorage(t *testing.T) {
	t.Helper()
	protocol.Storage = &mock.Storage{}
	ResetHistory()
}
