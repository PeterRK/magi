package model

import (
	"context"
	"errors"
	"math"
	rand "math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"magi/internal/core/protocol"
)

const transportRetryMaxDelay = 32 * time.Second

// The model client owns provider retry classification and waiting. It
// owns the attempt allowance; core owns logical invocation identity and settlement.

func retryDelay(err error, retry, limit uint64) (time.Duration, bool) {
	var failure modelFailure
	if !errors.As(err, &failure) || errors.Is(err, protocol.ErrContextOverflow) || !failure.Retryable || retry >= limit || failure.RetryAfter > transportRetryMaxDelay {
		return 0, false
	}
	if failure.RetryAfter > 0 {
		return failure.RetryAfter, true
	}
	return transportRetryDelay(retry, rand.Float64()), true
}
func waitRetry(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Wait 2, 4, 8, 16, 32 seconds with downward-only jitter in [-10%, 0%].
func transportRetryDelay(retry uint64, random float64) time.Duration {
	base := min(2*time.Second<<min(retry, 4), transportRetryMaxDelay)
	return min(time.Duration(float64(base)*(0.9+0.1*random)), transportRetryMaxDelay)
}

func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 && !math.IsInf(seconds, 0) {
		// Keep accepted delays exact; saturate rejected delays before conversion
		// so a large provider value cannot overflow into a short retry.
		if seconds > transportRetryMaxDelay.Seconds() {
			return transportRetryMaxDelay + time.Second
		}
		return time.Duration(seconds * float64(time.Second))
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, date.Sub(now))
	}
	return 0
}
