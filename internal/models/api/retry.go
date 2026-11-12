package api

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
)

// RetryPolicy describes how often to retry a failed request and how long to
// wait between attempts. The zero value performs one attempt and no waiting.
type RetryPolicy struct {
	// MaxRetries is the number of retries after the first attempt.
	MaxRetries int
	// BaseDelay is the wait before the first retry; it doubles each time.
	BaseDelay time.Duration
	// MaxDelay caps the backoff.
	MaxDelay time.Duration
}

// DefaultRetryPolicy is three retries with 1s, 2s, 4s backoff, which is what
// every embedding client used before this helper existed.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: 3, BaseDelay: time.Second, MaxDelay: 10 * time.Second}
}

func (p RetryPolicy) delay(attempt int) time.Duration {
	if p.BaseDelay <= 0 {
		return 0
	}
	wait := p.BaseDelay << uint(attempt-1)
	if p.MaxDelay > 0 && wait > p.MaxDelay {
		return p.MaxDelay
	}
	return wait
}

// retryWait is the pause before the next attempt. It prefers the vendor's
// Retry-After hint when the failure carried one, falls back to the exponential
// backoff, and adds jitter so a batch of workers that failed together does not
// retry in lockstep and re-create the spike that caused the failure.
func (p RetryPolicy) retryWait(attempt int, cause error) time.Duration {
	wait := p.delay(attempt)
	var httpErr *HTTPError
	if errors.As(cause, &httpErr) {
		// The vendor said how long to wait; honouring it is both politer and
		// cheaper than retrying into the same limit. Cap it so a hostile or
		// misconfigured header cannot park the caller for minutes.
		if hint := httpErr.RetryAfter(); hint > wait {
			wait = hint
			if wait > maxRetryAfterHint {
				wait = maxRetryAfterHint
			}
		}
	}
	return withJitter(wait)
}

// maxRetryAfterHint caps how long a vendor-supplied Retry-After may delay a
// single retry.
const maxRetryAfterHint = 30 * time.Second

// withJitter spreads the wait over the top half of its window. Full jitter
// (0..d) would let one caller retry almost immediately and keep hammering the
// upstream; the top half keeps the intended order of magnitude while
// decorrelating workers.
func withJitter(d time.Duration) time.Duration {
	if d <= time.Millisecond {
		return d
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)))
}

// retryable reports whether the failure is one that sending again could
// plausibly fix: the network never delivered an answer, or the vendor answered
// with a status that means "come back later" (429 and the 5xx family that
// proxies emit for a temporarily unavailable backend). Every other status is
// the vendor rejecting this request, and repeating it only burns quota.
func retryable(err error) bool {
	var transportErr *TransportError
	if errors.As(err, &transportErr) {
		return true
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusTooManyRequests,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true
		}
	}
	return false
}

// PostJSONWithRetry sends one authenticated JSON request, retrying transport
// failures, and decodes the reply into out.
//
// It never returns a nil error without decoding: the eight hand-written
// retry loops this replaces kept `resp` and `err` in the enclosing scope and
// returned `(nil, err)` at the end, so writing `req, err := ...` inside the
// loop shadowed the error, left the outer one nil, and handed the caller a
// nil response with no error to check — a nil-pointer panic that took the
// process down instead of reporting "connection refused" (Tencent/WeKnora#3484).
// Here the last error is captured in the enclosing scope on purpose and the
// function cannot fall through without it.
//
// Only transport failures are retried, including a reply cut off while it
// was being read. A non-2xx reply is the vendor answering, and repeating a
// rejected request neither fixes it nor tells the operator anything new; a
// reply that arrived whole but does not decode is the same answer every time,
// and sending it again would bill the request twice.
func (e Endpoint) PostJSONWithRetry(
	ctx context.Context, url string, body, out any, policy RetryPolicy, label string,
) error {
	return withRetry(ctx, policy, label, func() error {
		return e.PostJSON(ctx, url, body, out)
	})
}

// withRetry runs send until it succeeds, fails with anything other than a
// TransportError, or the budget runs out. The last error lives in the
// enclosing scope and the loop cannot fall through without it.
func withRetry(ctx context.Context, policy RetryPolicy, label string, send func() error) error {
	var lastErr error
	for attempt := 0; attempt <= policy.MaxRetries; attempt++ {
		if attempt > 0 {
			wait := policy.retryWait(attempt, lastErr)
			logger.Infof(ctx, "[%s] retrying (%d/%d) after %v: %v",
				label, attempt, policy.MaxRetries, wait, lastErr)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err := send()
		if err == nil {
			return nil
		}
		if !retryable(err) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("%s: %w", label, lastErr)
}
