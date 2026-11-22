package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// uploadEnterProbe is the transport both upload tests use. It lets the test
// inspect the request the instant it reaches the transport, then either sends
// it on or fails the call there.
//
// The transport is the only place the request's context can be acted on while
// the call is still in flight. Waiting for the server to receive the request is
// not an option: a cancel that fires on entry lands before the connection is
// dialled.
type uploadEnterProbe struct {
	onEnter func(req *http.Request) error
	inner   *deadlineProbe
}

func (p *uploadEnterProbe) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := p.onEnter(req); err != nil {
		return nil, err
	}
	return p.inner.RoundTrip(req)
}

var uploadStrategies = map[string][]ClientOption{
	"derived":          nil,
	"dedicated pinned": {WithUploadTimeout(7 * time.Second)},
	"general pinned":   {WithTimeout(7 * time.Second)},
	// A zero ceiling is what makes a lost cancel worst: with no ceiling of its
	// own the upload would run until the server answers, which it never does.
	"dedicated zero": {WithUploadTimeout(0)},
	"general zero":   {WithTimeout(0)},
}

// newHoldingServer answers nothing: the handler returns only when the request
// is cancelled.
func newHoldingServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.Body.Close()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	return server
}

func uploadOnce(t *testing.T, c *Client, ctx context.Context, method string) error {
	t.Helper()
	if method == "knowledge file" {
		_, err := c.CreateKnowledgeFromFile(ctx, "kb-1", writePayload(t, 1024), nil, nil, "", "", nil)
		return err
	}
	_, err := c.UploadSandboxSkill(ctx, "cfg-1", "s.zip", []byte("PK\x03\x04payload"))
	return err
}

// TestCallerCancellationReachesTheUpload asserts that a cancel on the caller's
// context aborts an in-flight upload. The handler never answers, so the only
// way the call can return is cancellation reaching it through the request's
// context -- which requires that context to be built on the caller's.
//
// Checking a Done channel after Do returns cannot test this: Do cancels the
// request's own context on its way out, so that channel is always closed. The
// verdict has to come from the call being interrupted.
func TestCallerCancellationReachesTheUpload(t *testing.T) {
	for name, opts := range uploadStrategies {
		for _, method := range []string{"knowledge file", "sandbox skill"} {
			t.Run(name+"/"+method, func(t *testing.T) {
				server := newHoldingServer(t)
				callerCtx, cancel := context.WithCancel(context.WithValue(t.Context(), callerContextProbe{}, "caller"))
				defer cancel()
				probe := &deadlineProbe{}
				tr := &uploadEnterProbe{
					onEnter: func(*http.Request) error { cancel(); return nil },
					inner:   probe,
				}
				c := NewClient(server.URL, append(opts, WithTransport(tr))...)

				done := make(chan error, 1)
				go func() { done <- uploadOnce(t, c, callerCtx, method) }()

				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("the upload ended with %v, want context.Canceled", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("the caller's cancel did not reach the upload within 5s")
				}
			})
		}
	}
}

// TestCallerDeadlineEndsTheUpload is the other half of the same contract: the
// deadline on the caller's context must be the one the upload ends at, and it
// must be the one the request carries.
//
// A deadline inside the test alone cannot tell the caller's deadline from a
// replacement: a detached context re-armed with the request's own deadline --
// which is earlier still, because the ceiling is added to it -- expires too.
// What separates them is that a request still chained to the caller sees the
// caller's deadline returned by context.Deadline, which is recorded here before
// the transport lets the call continue.
func TestCallerDeadlineEndsTheUpload(t *testing.T) {
	const callerLease = 500 * time.Millisecond

	for name, opts := range uploadStrategies {
		t.Run(name, func(t *testing.T) {
			server := newHoldingServer(t)
			callerDeadline := time.Now().Add(callerLease)
			callerCtx, cancel := context.WithDeadline(context.WithValue(t.Context(), callerContextProbe{}, "caller"), callerDeadline)
			defer cancel()
			probe := &deadlineProbe{}
			var seen time.Time
			var seenOK bool
			tr := &uploadEnterProbe{
				onEnter: func(req *http.Request) error {
					seen, seenOK = req.Context().Deadline()
					// The deadline the request carries is recorded above; now
					// act on the caller, which is the only thing that tells a
					// request chained to it from one rebuilt with the same
					// deadline and value. A chained request comes back
					// cancelled within the same call; a detached one runs on
					// until the caller's deadline, which has already been
					// arranged to be the earlier of the two.
					cancel()
					return nil
				},
				inner: probe,
			}
			c := NewClient(server.URL, append(opts, WithTransport(tr))...)

			err := uploadOnce(t, c, callerCtx, "sandbox skill")

			if !seenOK || !seen.Equal(callerDeadline) {
				t.Fatalf("the request carries deadline %v, want the caller's %v", seen, callerDeadline)
			}
			// Canceled, not DeadlineExceeded: a detached request re-armed with
			// the caller's own deadline still ends at that deadline, so
			// accepting either would accept it.
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("the upload ended with %v, want context.Canceled", err)
			}
		})
	}
}
