package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestUploadTimeoutForGrowsWithPayload pins the shape of the derived ceiling:
// a floor large enough for connection setup and the JSON response, plus a
// transfer allowance that scales with the body.
func TestUploadTimeoutForGrowsWithPayload(t *testing.T) {
	if got := uploadTimeoutFor(0); got != 60*time.Second {
		t.Fatalf("empty payload ceiling = %v, want %v", got, 60*time.Second)
	}
	if got := uploadTimeoutFor(-1); got != 60*time.Second {
		t.Fatalf("negative payload ceiling = %v, want %v", got, 60*time.Second)
	}

	// A ceiling below the base timeout would abort uploads the base timeout
	// already allows. Payloads smaller than the per-second floor contribute no
	// whole second of transfer allowance, so they sit exactly at the base.
	for _, size := range []int64{1, 1024, 50 << 20, 51 << 20} {
		if got := uploadTimeoutFor(size); got < 60*time.Second {
			t.Fatalf("ceiling for %d bytes = %v, want >= %v", size, got, 60*time.Second)
		}
	}

	// The limit that matters: the largest file the server accepts must get
	// more than the ordinary request timeout, with room to spare.
	const maxFile = 50 << 20
	if got := uploadTimeoutFor(maxFile); got <= 30*time.Second {
		t.Fatalf("ceiling for a %d-byte file = %v, which does not clear the 30s request timeout", maxFile, got)
	}

	small, large := uploadTimeoutFor(1<<20), uploadTimeoutFor(50<<20)
	if large <= small {
		t.Fatalf("ceiling is not monotonic: 1MiB=%v, 50MiB=%v", small, large)
	}
}

// TestUploadOutlivesOrdinaryRequestTimeout is the regression this policy
// exists for: a server that takes longer than the 30-second JSON ceiling to
// finish an upload must not abort it. The ordinary request timeout is scaled
// down so the test stays fast; the ratio is what matters.
func TestUploadOutlivesOrdinaryRequestTimeout(t *testing.T) {
	const delay = 150 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every route is slow, so the only difference between the two clients
		// below is which ceiling applies.
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":"doc-1","knowledge_base_id":"kb-1","type":"file","title":"a.txt","parse_status":"pending"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"data":{}}`))
	}))
	defer server.Close()

	c := NewClient(server.URL)
	// Scale the ordinary ceiling down so a delay that would kill it is cheap
	// to express. The upload ceiling is untouched and stays at its real default.
	c.httpClient.Timeout = 50 * time.Millisecond

	file := writeTempFile(t, "a.txt", strings.Repeat("x", 1024))
	if _, err := c.CreateKnowledgeFromFile(t.Context(), "kb-1", file, nil, nil, "", "", nil); err != nil {
		t.Fatalf("upload aborted by the ordinary request timeout: %v", err)
	}

	// An ordinary JSON request against the same slow server still honours the
	// ordinary ceiling, so the fix does not disable timeouts wholesale.
	if _, err := c.doRequest(t.Context(), http.MethodGet, "/api/v1/auth/me", nil, nil); err == nil {
		t.Fatal("ordinary request did not time out; the ordinary ceiling stopped applying")
	}
}

// TestWithUploadTimeoutOverridesDerivedCeiling covers the escape hatch for
// links slower than the pessimistic floor.
func TestWithUploadTimeoutOverridesDerivedCeiling(t *testing.T) {
	const delay = 200 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":"s-1"}}`))
	}))
	defer server.Close()

	file := writeTempFile(t, "a.txt", strings.Repeat("x", 1024))

	// An explicit ceiling shorter than the server's handling time must abort.
	tight := NewClient(server.URL, WithUploadTimeout(50*time.Millisecond))
	if _, err := tight.CreateKnowledgeFromFile(t.Context(), "kb-1", file, nil, nil, "", "", nil); err == nil {
		t.Fatal("WithUploadTimeout did not override the derived ceiling")
	}

	// The same upload under the default derived ceiling succeeds.
	relaxed := NewClient(server.URL)
	relaxed.httpClient.Timeout = 50 * time.Millisecond
	if _, err := relaxed.CreateKnowledgeFromFile(t.Context(), "kb-1", file, nil, nil, "", "", nil); err != nil {
		t.Fatalf("default derived ceiling aborted the upload: %v", err)
	}
}

// TestWithTimeoutStillBoundsUploads guards the compatibility half of the
// policy: before uploads had their own ceiling, WithTimeout constrained them,
// and a caller who pins a deadline must not find it silently exceeded. The
// derived ceiling is deliberately generous, so an explicit bound has to win.
func TestWithTimeoutStillBoundsUploads(t *testing.T) {
	const delay = 200 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":"doc-1"}}`))
	}))
	defer server.Close()

	file := writeTempFile(t, "a.txt", strings.Repeat("x", 1024))

	c := NewClient(server.URL, WithTimeout(50*time.Millisecond))
	_, err := c.CreateKnowledgeFromFile(t.Context(), "kb-1", file, nil, nil, "", "", nil)
	if err == nil {
		t.Fatal("WithTimeout no longer bounds uploads")
	}
	if !isTimeoutError(err) {
		t.Fatalf("upload failed for a reason other than the timeout: %v", err)
	}

	// WithUploadTimeout overrides WithTimeout for uploads only.
	c = NewClient(server.URL, WithTimeout(50*time.Millisecond), WithUploadTimeout(5*time.Second))
	if _, err := c.CreateKnowledgeFromFile(t.Context(), "kb-1", file, nil, nil, "", "", nil); err != nil {
		t.Fatalf("WithUploadTimeout should override WithTimeout for uploads: %v", err)
	}
}

// TestUploadTimeoutOptionWinsInEitherOrder pins the documented precedence: the
// dedicated upload option decides upload bounds no matter which order the
// caller lists the two options in.
func TestUploadTimeoutOptionWinsInEitherOrder(t *testing.T) {
	const delay = 200 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":"doc-1"}}`))
	}))
	defer server.Close()

	file := writeTempFile(t, "a.txt", strings.Repeat("x", 1024))
	orders := map[string][]ClientOption{
		"upload option second": {WithTimeout(50 * time.Millisecond), WithUploadTimeout(5 * time.Second)},
		"upload option first":  {WithUploadTimeout(5 * time.Second), WithTimeout(50 * time.Millisecond)},
	}
	for name, opts := range orders {
		t.Run(name, func(t *testing.T) {
			c := NewClient(server.URL, opts...)
			if _, err := c.CreateKnowledgeFromFile(t.Context(), "kb-1", file, nil, nil, "", "", nil); err != nil {
				t.Fatalf("WithUploadTimeout lost to WithTimeout: %v", err)
			}
			// The ordinary requests still honour WithTimeout.
			if _, err := c.doRequest(t.Context(), http.MethodGet, "/api/v1/auth/me", nil, nil); !isTimeoutError(err) {
				t.Fatalf("ordinary request should still time out, got %v", err)
			}
		})
	}
}

// TestUploadCeilingContractMatrix pins the whole option contract in one place:
// for every combination of WithTimeout (T) and WithUploadTimeout (U) that
// matters, what ceiling does a real upload carry? The assertion is the deadline
// the transport actually receives, so a refactor of doUpload cannot keep the
// matrix green while changing what callers observe.
//
// The rules it encodes:
//   - no options: the derived ceiling, never the 30-second default;
//   - U wins over T for uploads, whatever order they are listed in;
//   - U(0) means no upload timeout;
//   - U(negative) forgets U, leaving T in charge; T(0) then means no bound;
//   - with neither U nor a T, uploads derive.
func TestUploadCeilingContractMatrix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"skill_id":"s-1"}}`))
	}))
	defer server.Close()

	const short = 7 * time.Second
	const upload = 5 * time.Second
	// The two outcomes that are not a plain duration.
	const (
		noBound = time.Duration(-1) // explicitly no upload timeout
		derived = time.Duration(-2) // computed from the payload size
	)
	derivedForTest := uploadTimeoutFor(int64(len("PK\x03\x04payload")) + 512)

	for _, tt := range []struct {
		name string
		opts func(probe *deadlineProbe) []ClientOption
		want time.Duration // zero means "no bound"
	}{
		{name: "default", want: derived, opts: func(p *deadlineProbe) []ClientOption { return []ClientOption{WithTransport(p)} }},
		{name: "T(7s)", want: short, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithTimeout(short), WithTransport(p)}
		}},
		{name: "T(0)", want: noBound, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithTimeout(0), WithTransport(p)}
		}},
		{name: "U(5s)", want: upload, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithUploadTimeout(upload), WithTransport(p)}
		}},
		{name: "U(0)", want: noBound, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithUploadTimeout(0), WithTransport(p)}
		}},
		{name: "U(5s) then T(7s)", want: upload, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithUploadTimeout(upload), WithTimeout(short), WithTransport(p)}
		}},
		{name: "T(7s) then U(5s)", want: upload, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithTimeout(short), WithUploadTimeout(upload), WithTransport(p)}
		}},
		{name: "U(-1) alone keeps the default", want: derived, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithUploadTimeout(-1), WithTransport(p)}
		}},
		{name: "U(5s) then U(-1) then T(7s)", want: short, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithUploadTimeout(upload), WithUploadTimeout(-1), WithTimeout(short), WithTransport(p)}
		}},
		{name: "U(-1) then T(7s) then U(5s)", want: upload, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithUploadTimeout(-1), WithTimeout(short), WithUploadTimeout(upload), WithTransport(p)}
		}},
		// A negative U forgets U; what is left is T, and T(0) means no bound.
		// This has to hold in both orders.
		{name: "T(0) then U(-1)", want: noBound, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithTimeout(0), WithUploadTimeout(-1), WithTransport(p)}
		}},
		{name: "U(-1) then T(0)", want: noBound, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithUploadTimeout(-1), WithTimeout(0), WithTransport(p)}
		}},
		{name: "U(0) then T(7s)", want: noBound, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithUploadTimeout(0), WithTimeout(short), WithTransport(p)}
		}},
		{name: "T(7s) then U(0)", want: noBound, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithTimeout(short), WithUploadTimeout(0), WithTransport(p)}
		}},
		// A second WithTimeout replaces the first. The value it left on uploads
		// is indistinguishable from a dedicated one unless the policy remembers
		// which option set it, so these cases would otherwise keep the stale
		// bound while ordinary requests move on.
		{name: "T(7s) then T(3s)", want: 3 * time.Second, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithTimeout(short), WithTimeout(3 * time.Second), WithTransport(p)}
		}},
		{name: "T(7s) then T(0)", want: noBound, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithTimeout(short), WithTimeout(0), WithTransport(p)}
		}},
		{name: "T(0) then T(3s)", want: 3 * time.Second, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithTimeout(0), WithTimeout(3 * time.Second), WithTransport(p)}
		}},
		{name: "T(7s) then U(-1) then T(3s)", want: 3 * time.Second, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithTimeout(short), WithUploadTimeout(-1), WithTimeout(3 * time.Second), WithTransport(p)}
		}},
		{name: "U(5s) then T(7s) then U(-1) then T(3s)", want: 3 * time.Second, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithUploadTimeout(upload), WithTimeout(short), WithUploadTimeout(-1), WithTimeout(3 * time.Second), WithTransport(p)}
		}},
		// A negative U as the LAST option must drop the value the earlier U left
		// behind, not just its ownership: leaving the policy as "dedicated" with
		// the old duration would keep a bound the caller has just withdrawn.
		{name: "U(5s) then T(7s) then U(-1)", want: short, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithUploadTimeout(upload), WithTimeout(short), WithUploadTimeout(-1), WithTransport(p)}
		}},
		{name: "T(7s) then U(5s) then U(-1)", want: short, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithTimeout(short), WithUploadTimeout(upload), WithUploadTimeout(-1), WithTransport(p)}
		}},
		{name: "U(5s) then T(0) then U(-1)", want: noBound, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithUploadTimeout(upload), WithTimeout(0), WithUploadTimeout(-1), WithTransport(p)}
		}},
		{name: "U(5s) then U(-1) alone keeps the default", want: derived, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithUploadTimeout(upload), WithUploadTimeout(-1), WithTransport(p)}
		}},
		// The dedicated option still wins over a later WithTimeout.
		{name: "U(5s) then T(7s) then T(3s)", want: upload, opts: func(p *deadlineProbe) []ClientOption {
			return []ClientOption{WithUploadTimeout(upload), WithTimeout(short), WithTimeout(3 * time.Second), WithTransport(p)}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			probe := &deadlineProbe{}
			c := NewClient(server.URL, tt.opts(probe)...)
			start := time.Now()
			if _, err := c.UploadSandboxSkill(t.Context(), "cfg-1", "s.zip", []byte("PK\x03\x04payload")); err != nil {
				t.Fatalf("upload failed: %v", err)
			}
			got := probe.lastDeadline()
			want := tt.want
			if want == derived {
				want = derivedForTest
			}
			if want == noBound {
				// http.Client only stamps a deadline when its own Timeout is
				// set, so "no bound" is a zero deadline at the transport.
				if !got.IsZero() {
					t.Fatalf("expected no upload bound, transport saw one in %v", time.Until(got).Round(time.Millisecond))
				}
				return
			}
			if got.IsZero() {
				t.Fatalf("expected a %v bound, transport saw none", want)
			}
			// The budget is measured from the moment the transport was entered,
			// which is a few instants before the deadline was stamped: the
			// difference is the cost of one context.WithTimeout, not upload work.
			// Asserting against that instant keeps the window in the same
			// magnitude as the drift a wrong ceiling would introduce, so an
			// offset of 500ms or a fraction of a percent cannot hide behind a
			// generous bound.
			entry := probe.lastEntry()
			if entry.IsZero() {
				t.Fatalf("the transport did not record when it was entered")
			}
			budget := got.Sub(entry)
			const slack = 20 * time.Millisecond
			if budget < want-slack || budget > want+slack {
				t.Fatalf("transport saw %v from its entry, want %v", budget.Round(time.Microsecond), want)
			}
			// From the call the deadline is only ever further away, never closer.
			if fromCall := got.Sub(start); fromCall < want-slack {
				t.Fatalf("transport saw %v from the call, want at least %v", fromCall.Round(time.Microsecond), want)
			}
		})
	}
}

// TestUploadTimeoutIsClampedToTheMaximum checks the ceiling is applied to the
// whole derived value, not just its transfer allowance: a payload whose
// transfer allowance alone would land just under the cap once the base is
// added must come back at exactly the cap, not above it.
func TestUploadTimeoutIsClampedToTheMaximum(t *testing.T) {
	// The clamp subtracts the base timeout before comparing, so a fixture only
	// tests it if base + allowance lands one second PAST the cap. With
	// base = 60s and cap = 2h, the allowance that does that is exactly
	// cap/base - base + 1 seconds of payload. A "+1 then -1" pair that adds back
	// to the cap would leave an unbounded implementation green.
	perSecond := int64(100 * 1024)
	overAllowance := int64(2*time.Hour/time.Second) - int64(60*time.Second/time.Second) + 1
	overCap := overAllowance * perSecond
	if unclamped := 60*time.Second + time.Duration(overAllowance)*time.Second; unclamped <= 2*time.Hour {
		t.Fatalf("fixture does not exceed the cap: unclamped %v <= cap %v", unclamped, 2*time.Hour)
	}
	if got := uploadTimeoutFor(overCap); got != 2*time.Hour {
		t.Fatalf("ceiling for %d bytes = %v, want the cap %v", overCap, got, 2*time.Hour)
	}
	// And the largest payload that stays under the cap must not be clamped, or
	// the fix would be a blanket 2h. Skipping a full second (-2) keeps this
	// fixture inside the allowance instead of on the cap itself; the exact
	// boundary is pinned separately below.
	underAllowance := overAllowance - 2
	if unclamped := 60*time.Second + time.Duration(underAllowance)*time.Second; unclamped >= 2*time.Hour {
		t.Fatalf("fixture is not under the cap: unclamped %v >= cap %v", unclamped, 2*time.Hour)
	}
	if got := uploadTimeoutFor(underAllowance * perSecond); got != 60*time.Second+time.Duration(underAllowance)*time.Second {
		t.Fatalf("ceiling for %d seconds of payload = %v, want the unclamped %v", underAllowance, got, 60*time.Second+time.Duration(underAllowance)*time.Second)
	}
	// The value that lands exactly on the cap must come back as the cap. This
	// is the largest payload a correct implementation leaves alone, and a
	// clamp that fires early would shrink it.
	atCap := int64(2*time.Hour/time.Second) - int64(60*time.Second/time.Second)
	if got := uploadTimeoutFor(atCap * perSecond); got != 2*time.Hour {
		t.Fatalf("ceiling for %d bytes (exactly the cap) = %v, want %v", atCap*perSecond, got, 2*time.Hour)
	}

	// Exact values at the sizes the documentation quotes. A unit mix-up
	// (seconds added as nanoseconds) still passes every ">= base" and "<= cap"
	// assertion, so the numbers themselves have to be pinned.
	for _, tt := range []struct {
		size int64
		want time.Duration
	}{
		{size: 0, want: 60 * time.Second},
		{size: 1 << 20, want: 60*time.Second + 10*time.Second},
		{size: 300 * 1024, want: 60*time.Second + 3*time.Second},
		{size: 50 << 20, want: 60*time.Second + 512*time.Second},
	} {
		if got := uploadTimeoutFor(tt.size); got != tt.want {
			t.Fatalf("ceiling for %d bytes = %v, want exactly %v", tt.size, got, tt.want)
		}
	}
	// 100 KiB/s: a payload just under one second of transfer gains nothing.
	if got := uploadTimeoutFor(perSecond - 1); got != 60*time.Second {
		t.Fatalf("sub-second payload gained transfer time: %v", got)
	}
	huge := int64(2*time.Hour/time.Second+30) * perSecond
	if got := uploadTimeoutFor(huge); got != 2*time.Hour {
		t.Fatalf("ceiling for a saturating payload = %v, want %v", got, 2*time.Hour)
	}
	for _, size := range []int64{0, 1 << 20, 50 << 20, 512 << 20, 1 << 40} {
		if got := uploadTimeoutFor(size); got > 2*time.Hour {
			t.Fatalf("ceiling for %d bytes = %v exceeds the maximum %v", size, got, 2*time.Hour)
		}
	}
}

// callerContextProbe is the key a test stores a value under to check that the
// caller's context reached the transport.
type callerContextProbe struct{}

// deadlineProbe records the deadline of the context each request arrives with,
// and when the transport was entered, so a test can bracket the budget between
// two instants it controls instead of allowing a window of its own.
type deadlineProbe struct {
	mu     sync.Mutex
	got    []time.Time
	enters []time.Time
	values []any
	dones  []<-chan struct{}
	roots  []context.Context
}

func (p *deadlineProbe) record(req *http.Request) {
	// The entry instant is taken first: it is meant to be the moment the
	// transport was entered, not the moment a few instructions later.
	entered := time.Now()
	d, _ := req.Context().Deadline()
	p.mu.Lock()
	p.got = append(p.got, d)
	p.enters = append(p.enters, entered)
	p.values = append(p.values, req.Context().Value(callerContextProbe{}))
	p.dones = append(p.dones, req.Context().Done())
	p.roots = append(p.roots, req.Context())
	p.mu.Unlock()
}

func (p *deadlineProbe) lastValue() any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.values) == 0 {
		return nil
	}
	return p.values[len(p.values)-1]
}

func (p *deadlineProbe) lastEntry() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.enters) == 0 {
		return time.Time{}
	}
	return p.enters[len(p.enters)-1]
}

func (p *deadlineProbe) RoundTrip(req *http.Request) (*http.Response, error) {
	p.record(req)
	return (&noTimeoutTransport{}).RoundTrip(req)
}

func (p *deadlineProbe) lastDeadline() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.got) == 0 {
		return time.Time{}
	}
	return p.got[len(p.got)-1]
}

// noTimeoutTransport is an http.Transport with its own response-header
// timeout raised far beyond the test, standing in for a transport layer that
// does not add a bound of its own. Sharing one keeps the pooled connections
// alive instead of leaking an idle connection per attempt.
type noTimeoutTransport struct {
	once sync.Once
	rt   *http.Transport
}

func (t *noTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.once.Do(func() { t.rt = &http.Transport{ResponseHeaderTimeout: time.Minute} })
	return t.rt.RoundTrip(req)
}

// TestSkillUploadUsesTheUploadCeiling covers the second multipart call site: a
// skill bundle is an archive and hits the same transfer-rate problem.
func TestSkillUploadUsesTheUploadCeiling(t *testing.T) {
	const delay = 150 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"skill_id":"s-1"}}`))
	}))
	defer server.Close()

	c := NewClient(server.URL)
	c.httpClient.Timeout = 50 * time.Millisecond // ordinary ceiling only

	if _, err := c.UploadSandboxSkill(t.Context(), "cfg-1", "s.zip", []byte("PK\x03\x04payload")); err != nil {
		t.Fatalf("skill upload aborted by the ordinary request timeout: %v", err)
	}
}

// isTimeoutError reports whether err is a deadline, in either the context form
// or the http.Client form. Asserting the kind matters: any error would also
// make a negative test pass.
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}
