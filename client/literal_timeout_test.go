package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// uploadCeiling runs fn against a server that answers both upload routes, and
// returns the ceiling the transport received.
//
// It returns two budgets: the deadline measured from just before fn was called,
// and from the moment the transport was entered. Both must equal want, so the
// assertion is two-sided with no slack of its own -- a ceiling that is 500ms or
// 1% off lands outside the bracket no matter where in fn the drift happens.
func uploadCeiling(t *testing.T, opts []ClientOption, fn func(c *Client) error) (fromCall, fromEntry time.Duration) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Both upload routes answer with a shape the methods accept.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"id": "k-1", "skill_id": "s-1"},
		})
	}))
	defer server.Close()

	probe := &deadlineProbe{}
	c := NewClient(server.URL, append(opts, WithTransport(probe))...)
	call := time.Now()
	if err := fn(c); err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	got := probe.lastDeadline()
	if got.IsZero() {
		t.Fatalf("the upload ran without any ceiling")
	}
	entry := probe.lastEntry()
	if entry.IsZero() {
		t.Fatalf("the transport did not record when it was entered")
	}
	return got.Sub(call), got.Sub(entry)
}

// expectCeiling asserts the budget the transport received equals want.
//
// The decisive bracket is the transport entry: the deadline is stamped inside
// doUpload, a few instants before the transport is entered, so the deadline is
// `want` after that instant give or take a hair. No upload work happens in
// between, which is what keeps this tight -- a ceiling off by 1% (7s → 7.07s)
// or 500ms lands outside it.
//
// The bracket from before the call is only a lower bound. Everything the
// public method does before stamping the deadline -- framing the multipart,
// marshalling the metadata, opening the file -- happens inside it, and that
// work costs milliseconds normally and tens of milliseconds with -race, so an
// upper bound there would be a flaky assertion rather than a contract.
func expectCeiling(t *testing.T, want, fromCall, fromEntry time.Duration) {
	t.Helper()
	const slack = 20 * time.Millisecond
	if fromCall < want-slack {
		t.Fatalf("deadline measured from the call = %v, want at least %v", fromCall.Round(time.Microsecond), want)
	}
	if fromEntry < want-slack || fromEntry > want+slack {
		t.Fatalf("deadline measured from transport entry = %v, want %v", fromEntry.Round(time.Microsecond), want)
	}
}

func writePayload(t *testing.T, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	return path
}

// Both documented upload methods must derive their ceiling from the body they
// actually send. Two sizes are checked so a client that stops counting past a
// threshold -- or that still applies the 30-second JSON ceiling -- lands
// outside the bracket at one of them.
func TestPublicUploadMethodsDeriveTheirCeilingFromTheBody(t *testing.T) {
	//
	// 102399 bytes sits just under one second of allowance (102400 bytes), but
	// the multipart framing pushes the body that is actually sent over it --
	// 102642 bytes for a file, 102636 for a skill archive -- so the ceiling is
	// 61s for the framed body and 60s for the raw one. A ceiling fed the file
	// or archive length instead of the body lands a second short. 3 MiB repeats
	// that at a size past any internal clamp.
	for _, tc := range []struct {
		bytes int
		want  time.Duration
	}{
		{bytes: 102399, want: 60*time.Second + 1*time.Second},
		{bytes: 1 << 20, want: 60*time.Second + 10*time.Second},
		{bytes: 2 << 20, want: 60*time.Second + 20*time.Second},
		{bytes: 3 << 20, want: 60*time.Second + 30*time.Second},
		// Above the default file limit: raising MAX_FILE_SIZE_MB must not leave
		// the ceiling stuck at a clamp the default happened to hit.
		{bytes: 51 << 20, want: 60*time.Second + 522*time.Second},
	} {
		t.Run("knowledge file/"+tc.want.String(), func(t *testing.T) {
			path := writePayload(t, tc.bytes)
			fromCall, fromEntry := uploadCeiling(t, nil, func(c *Client) error {
				_, err := c.CreateKnowledgeFromFile(t.Context(), "kb-1", path, nil, nil, "", "", nil)
				return err
			})
			expectCeiling(t, tc.want, fromCall, fromEntry)
		})

		t.Run("sandbox skill/"+tc.want.String(), func(t *testing.T) {
			fromCall, fromEntry := uploadCeiling(t, nil, func(c *Client) error {
				_, err := c.UploadSandboxSkill(t.Context(), "cfg-1", "s.zip", make([]byte, tc.bytes))
				return err
			})
			expectCeiling(t, tc.want, fromCall, fromEntry)
		})
	}
}

// The option contract is asserted at the transport, in absolute terms: a value
// handed to WithUploadTimeout or WithTimeout must arrive unchanged, not shifted
// by an offset the upload path adds on its way.
func TestUploadCeilingArrivesUnchanged(t *testing.T) {
	for _, want := range []time.Duration{3 * time.Second, 5 * time.Second, 7 * time.Second} {
		for name, opts := range map[string][]ClientOption{
			"WithUploadTimeout": {WithUploadTimeout(want)},
			"WithTimeout":       {WithTimeout(want)},
		} {
			t.Run(name+"/"+want.String(), func(t *testing.T) {
				fromCall, fromEntry := uploadCeiling(t, opts, func(c *Client) error {
					_, err := c.UploadSandboxSkill(t.Context(), "cfg-1", "s.zip", []byte("PK\x03\x04payload"))
					return err
				})
				expectCeiling(t, want, fromCall, fromEntry)
			})
		}
	}
}

// The no-ceiling branch needs its own assertion. A zero ceiling leaves the
// transport with no deadline at all, so a shift applied only on that branch
// (Timeout += 500ms turning "unbounded" into a 500ms abort) is invisible to
// every duration comparison above: there is no want to drift from, only "no
// deadline" becoming "a deadline".
func TestNoCeilingStaysUnbounded(t *testing.T) {
	for name, opts := range map[string][]ClientOption{
		"WithUploadTimeout(0)": {WithUploadTimeout(0)},
		"WithTimeout(0)":       {WithTimeout(0)},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": true,
					"data":    map[string]any{"id": "k-1", "skill_id": "s-1"},
				})
			}))
			defer server.Close()

			probe := &deadlineProbe{}
			c := NewClient(server.URL, append(opts, WithTransport(probe))...)
			if _, err := c.UploadSandboxSkill(t.Context(), "cfg-1", "s.zip", []byte("PK\x03\x04payload")); err != nil {
				t.Fatalf("upload failed: %v", err)
			}
			if got := probe.lastDeadline(); !got.IsZero() {
				t.Fatalf("an unbounded upload arrived with a deadline %v away", time.Until(got).Round(time.Millisecond))
			}
		})
	}
}

// The documented constants themselves. The behaviour tests above would still
// pass a "60s" that had quietly become 30s if their expectations were derived
// from the same constant, so the numbers the docs promise are pinned here.
func TestDocumentedUploadConstants(t *testing.T) {
	if uploadBaseTimeout != 60*time.Second {
		t.Fatalf("base timeout = %v, the documented floor is 60s", uploadBaseTimeout)
	}
	if uploadMinThroughput != 100*1024 {
		t.Fatalf("assumed throughput = %d B/s, the documented rate is 100 KiB/s", uploadMinThroughput)
	}
	if uploadMaxDerived != 2*time.Hour {
		t.Fatalf("cap = %v, the documented cap is 2h", uploadMaxDerived)
	}
}

// The documented example: a 50 MiB file gets 572 seconds. It is the largest
// size the server accepts by default and the case the policy exists for, so it
// is asserted end to end rather than through the helper -- a clamp anywhere on
// the path from the file to doUpload shrinks it.
func TestFiftyMiBUploadGetsTheDocumentedBudget(t *testing.T) {
	const bodySize = 50 << 20
	const want = 60*time.Second + 512*time.Second

	t.Run("knowledge file", func(t *testing.T) {
		path := writePayload(t, bodySize)
		fromCall, fromEntry := uploadCeiling(t, nil, func(c *Client) error {
			_, err := c.CreateKnowledgeFromFile(t.Context(), "kb-1", path, nil, nil, "", "", nil)
			return err
		})
		expectCeiling(t, want, fromCall, fromEntry)
	})

	t.Run("sandbox skill", func(t *testing.T) {
		_, fromEntry := uploadCeiling(t, nil, func(c *Client) error {
			_, err := c.UploadSandboxSkill(t.Context(), "cfg-1", "s.zip", make([]byte, bodySize))
			return err
		})
		// Building a 50 MiB multipart in memory takes longer than the framing of
		// the smaller cases, so only the transport bracket is asserted here.
		if fromEntry < want-20*time.Millisecond || fromEntry > want+20*time.Millisecond {
			t.Fatalf("transport saw %v from its entry, want %v", fromEntry.Round(time.Microsecond), want)
		}
	})
}

// A caller's context is part of the request and must survive whatever ceiling
// the upload runs under: the ceiling bounds how long the client will wait, it
// never replaces the caller's deadline, cancellation or values. Each strategy
// that can be selected is exercised on both upload methods, because each one
// builds its request separately.
func TestUploadsPreserveTheCallerContext(t *testing.T) {
	strategies := map[string][]ClientOption{
		"derived":          nil,
		"dedicated pinned": {WithUploadTimeout(7 * time.Second)},
		"general pinned":   {WithTimeout(7 * time.Second)},
		// Two ways to reach "no ceiling", and they take different branches: a
		// zero dedicated option pins zero, while WithTimeout(0) turns the upload
		// ceiling off through the general option.
		"dedicated zero": {WithUploadTimeout(0)},
		"general zero":   {WithTimeout(0)},
	}

	for name, opts := range strategies {
		for _, method := range []string{"knowledge file", "sandbox skill"} {
			t.Run(name+"/"+method, func(t *testing.T) {
				// The caller's deadline is far out, so a substituted context
				// cannot accidentally match it, and a cancel is the only thing
				// that can end the request while it is in flight.
				callerDeadline := time.Now().Add(10 * time.Minute)

				// The transport is where the request context is observable: it is
				// the one the client actually sends, with the caller's deadline,
				// value and cancellation chain. The handler runs on the server
				// side and never sees it.
				probe := &deadlineProbe{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"success": true,
						"data":    map[string]any{"id": "k-1", "skill_id": "s-1"},
					})
				}))
				defer server.Close()

				c := NewClient(server.URL, append(opts, WithTransport(probe))...)
				callerCtx, cancel := context.WithDeadline(context.WithValue(t.Context(), callerContextProbe{}, "caller"), callerDeadline)
				defer cancel()

				var err error
				if method == "knowledge file" {
					_, err = c.CreateKnowledgeFromFile(callerCtx, "kb-1", writePayload(t, 1024), nil, nil, "", "", nil)
				} else {
					_, err = c.UploadSandboxSkill(callerCtx, "cfg-1", "s.zip", []byte("PK\x03\x04payload"))
				}
				if err != nil {
					t.Fatalf("upload failed: %v", err)
				}

				// The request runs under whichever deadline is earlier -- the
				// caller's or the ceiling -- but it may never run past the
				// caller's, and the caller's context itself must be in the chain
				// so cancellation reaches it.
				if got := probe.lastDeadline(); got.After(callerDeadline) {
					t.Fatalf("the request carries deadline %v, past the caller's %v",
						got.Round(time.Millisecond), callerDeadline.Round(time.Millisecond))
				}
				if got := probe.lastValue(); got != "caller" {
					t.Fatalf("the caller's context value was lost: %v", got)
				}
				// Cancellation itself is asserted in
				// TestCallerCancellationReachesTheUpload, which covers all five
				// strategies decisively: it cancels and requires the upload to
				// come back cancelled. It cannot be inferred here from the Done
				// channel: when the client carries a timeout, net/http derives
				// a context with the ceiling as its deadline, so the request's
				// channel is legitimately not the caller's even though the
				// caller's cancel still reaches it.
			})
		}
	}
}
