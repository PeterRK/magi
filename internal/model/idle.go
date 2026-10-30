package model

import (
	"context"
	"sync"
	"time"
)

const streamIdleTimeout = 180 * time.Second

type idleTimeoutError struct{}

func (idleTimeoutError) Error() string   { return "DeepSeek stream idle timeout" }
func (idleTimeoutError) Timeout() bool   { return true }
func (idleTimeoutError) Temporary() bool { return true }

var errStreamIdleTimeout = idleTimeoutError{}

// Cancellation releases the standard HTTP transport's blocked read. There is
// no read goroutine to strand, and stop joins any callback holding the mutex.
type idleWatch struct {
	mu       sync.Mutex
	timer    *time.Timer
	cancel   context.CancelCauseFunc
	interval time.Duration
	last     time.Time
	closed   bool
	expired  bool
}

func newIdleWatch(cancel context.CancelCauseFunc, interval time.Duration) *idleWatch {
	w := &idleWatch{cancel: cancel, interval: interval, last: time.Now()}
	w.timer = time.AfterFunc(interval, w.check)
	return w
}

func (w *idleWatch) check() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.expired {
		return
	}
	if remaining := w.interval - time.Since(w.last); remaining > 0 {
		w.timer.Reset(remaining)
		return
	}
	w.expired = true
	w.cancel(errStreamIdleTimeout)
}

func (w *idleWatch) received() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed && !w.expired {
		w.last = time.Now()
		w.timer.Reset(w.interval)
	}
}

func (w *idleWatch) timedOut() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.expired
}

func (w *idleWatch) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	w.timer.Stop()
}
