package file

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	credproviders "github.com/aliyun/credentials-go/credentials/providers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSTSSource struct {
	calls atomic.Int32
	err   error
	n     int32
}

func (f *fakeSTSSource) GetCredentials() (*credproviders.Credentials, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return &credproviders.Credentials{
		AccessKeyId:     "STS.test",
		AccessKeySecret: "secret",
		SecurityToken:   "token",
	}, nil
}

func TestOssStsProviderCachesUntilExpiry(t *testing.T) {
	src := &fakeSTSSource{}
	p := &ossStsProvider{src: src, ttl: time.Hour}

	first, err := p.GetCredentials(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "STS.test", first.AccessKeyID)
	assert.Equal(t, "token", first.SecurityToken)
	require.NotNil(t, first.Expires)

	second, err := p.GetCredentials(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "STS.test", second.AccessKeyID)
	assert.EqualValues(t, 1, src.calls.Load(), "credentials must be cached")
}

func TestOssStsProviderServesStaleCredentialsOnRefreshFailure(t *testing.T) {
	src := &fakeSTSSource{}
	p := &ossStsProvider{src: src, ttl: time.Hour}

	_, err := p.GetCredentials(context.Background())
	require.NoError(t, err)

	// Force a refresh by moving the expiry into the refresh margin.
	p.mu.Lock()
	p.expiresAt = time.Now().Add(30 * time.Second)
	p.mu.Unlock()

	src.err = errors.New("sts down")
	creds, err := p.GetCredentials(context.Background())
	require.NoError(t, err, "still-valid credentials must be served")
	assert.Equal(t, "STS.test", creds.AccessKeyID)
}

func TestOssStsProviderFailsAfterExpiry(t *testing.T) {
	src := &fakeSTSSource{err: errors.New("sts down")}
	p := &ossStsProvider{src: src, ttl: time.Hour}

	_, err := p.GetCredentials(context.Background())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "sts down", "errors must be sanitized")
}

func TestOssStsProviderConcurrentRefreshIsSingleFlight(t *testing.T) {
	src := &fakeSTSSource{}
	p := &ossStsProvider{src: src, ttl: time.Hour}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.GetCredentials(context.Background())
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 1, src.calls.Load(), "concurrent callers must share one STS call")
}

type panicSTSSource struct{}

func (panicSTSSource) GetCredentials() (*credproviders.Credentials, error) {
	panic("boom")
}

func TestOssStsProviderRecoversFromSourcePanic(t *testing.T) {
	p := &ossStsProvider{src: panicSTSSource{}, ttl: time.Hour}

	_, err := p.GetCredentials(context.Background())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "boom", "internal panic details must not leak")

	// A panic must not leave the provider stuck in the refreshing state.
	p.mu.Lock()
	refreshing := p.refreshing
	p.mu.Unlock()
	assert.False(t, refreshing)
}

type blockingSTSSource struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingSTSSource) GetCredentials() (*credproviders.Credentials, error) {
	b.calls.Add(1)
	close(b.entered)
	<-b.release
	return &credproviders.Credentials{AccessKeyId: "STS.new", AccessKeySecret: "secret", SecurityToken: "token"}, nil
}

func TestOssStsProviderDoesNotBlockWaitersWithValidCredentials(t *testing.T) {
	src := &blockingSTSSource{entered: make(chan struct{}), release: make(chan struct{})}
	p := &ossStsProvider{src: src, ttl: time.Hour}
	p.cached = credentials.Credentials{AccessKeyID: "STS.old", AccessKeySecret: "secret", SecurityToken: "old"}
	p.expiresAt = time.Now().Add(30 * time.Second) // inside the refresh margin, still valid

	// Start the refresh in the background; it blocks inside GetCredentials.
	go func() { _, _ = p.GetCredentials(context.Background()) }()
	<-src.entered

	// A concurrent request must return the still-valid credential immediately
	// instead of waiting for the slow STS call.
	done := make(chan struct{})
	var creds credentials.Credentials
	go func() {
		creds, _ = p.GetCredentials(context.Background())
		close(done)
	}()
	select {
	case <-done:
		assert.Equal(t, "STS.old", creds.AccessKeyID)
	case <-time.After(2 * time.Second):
		t.Fatal("waiter blocked although valid credentials existed")
	}
	close(src.release)
}

func TestOssStsProviderBacksOffAfterFailure(t *testing.T) {
	src := &fakeSTSSource{err: errors.New("sts down")}
	p := &ossStsProvider{src: src, ttl: time.Hour}

	_, err := p.GetCredentials(context.Background())
	require.Error(t, err)
	require.EqualValues(t, 1, src.calls.Load())

	// The next call inside the backoff window must fail fast, without another
	// STS request.
	_, err = p.GetCredentials(context.Background())
	require.Error(t, err)
	assert.EqualValues(t, 1, src.calls.Load(), "backoff must suppress repeated STS calls")
}

func TestOssStsProviderBackoffStillServesValidCredentials(t *testing.T) {
	src := &fakeSTSSource{}
	p := &ossStsProvider{src: src, ttl: time.Hour}

	_, err := p.GetCredentials(context.Background())
	require.NoError(t, err)

	// Move into the refresh margin so the next call attempts a refresh, then
	// make the source fail: the still-valid cached credential must be served.
	p.mu.Lock()
	p.expiresAt = time.Now().Add(30 * time.Second)
	p.mu.Unlock()
	src.err = errors.New("sts down")

	creds, err := p.GetCredentials(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "STS.test", creds.AccessKeyID)

	// The failed refresh armed the backoff. A later call must still return the
	// valid cached credential rather than failing fast.
	creds, err = p.GetCredentials(context.Background())
	require.NoError(t, err, "backoff must not hide still-valid credentials")
	assert.Equal(t, "STS.test", creds.AccessKeyID)
}

func TestOssStsProviderTreatsNilCredentialsAsFailure(t *testing.T) {
	src := &nilSTSSource{}
	p := &ossStsProvider{src: src, ttl: time.Hour}

	_, err := p.GetCredentials(context.Background())
	require.Error(t, err, "nil credentials with nil error must not be cached")
	assert.Empty(t, p.cached.AccessKeyID)
}

type nilSTSSource struct{}

func (nilSTSSource) GetCredentials() (*credproviders.Credentials, error) { return nil, nil }

func TestOssStsProviderCancelledCallerDoesNotLeakRefreshSlot(t *testing.T) {
	src := &blockingSTSSource{entered: make(chan struct{}), release: make(chan struct{})}
	p := &ossStsProvider{src: src, ttl: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = p.GetCredentials(ctx)
		close(done)
	}()
	<-src.entered
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled caller kept blocking on the STS fetch")
	}

	// The in-flight fetch still completes in the background and publishes its
	// result, so the next call is served from the cache with no extra STS call.
	close(src.release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		creds, err := p.GetCredentials(context.Background())
		if err == nil && creds.AccessKeyID == "STS.new" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background refresh did not publish credentials: err=%v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.EqualValues(t, 1, src.calls.Load(), "the background completion must reuse the single fetch")
}

func TestNormalizeSTSEndpoint(t *testing.T) {
	cases := map[string]string{
		"sts.cn-hangzhou.aliyuncs.com":          "sts.cn-hangzhou.aliyuncs.com",
		"https://sts.cn-hangzhou.aliyuncs.com":  "sts.cn-hangzhou.aliyuncs.com",
		"https://sts.cn-hangzhou.aliyuncs.com/": "sts.cn-hangzhou.aliyuncs.com",
		"  STS.aliyuncs.com  ":                  "sts.aliyuncs.com",
		"sts.aliyuncs.com/path":                 "sts.aliyuncs.com",
		"":                                      "",
	}
	for in, want := range cases {
		if got := normalizeSTSEndpoint(in); got != want {
			t.Errorf("normalizeSTSEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSummarizeSTSErrorRedactsNonJSONCredentials(t *testing.T) {
	err := errors.New(`sts request failed: 403 AccessKeyId=LTAI5tEXAMPLE Signature=abc123&SignatureNonce=xyz SecurityToken=secrettoken`)
	got := summarizeSTSError(err)
	for _, leaked := range []string{"LTAI5tEXAMPLE", "abc123", "secrettoken"} {
		if strings.Contains(got, leaked) {
			t.Errorf("summarizeSTSError leaked %q in %q", leaked, got)
		}
	}
}

func TestSummarizeSTSErrorRedactsJSONBody(t *testing.T) {
	err := errors.New(`{"Code":"NoPermission","Message":"x","RequestId":"rid-1","AccessKeyId":"LTAI5tEXAMPLE"}`)
	got := summarizeSTSError(err)
	if !strings.Contains(got, "NoPermission") || !strings.Contains(got, "rid-1") {
		t.Errorf("summarizeSTSError lost the diagnostic fields: %q", got)
	}
	if strings.Contains(got, "LTAI5tEXAMPLE") {
		t.Errorf("summarizeSTSError leaked the access key id: %q", got)
	}
}

// stableSTSSource returns the same AccessKeyId on every call, the way
// credentials-go behaves while its own 180-second renewal threshold has not
// passed: it answers from its cache instead of calling STS.
type stableSTSSource struct{ calls atomic.Int32 }

func (s *stableSTSSource) GetCredentials() (*credproviders.Credentials, error) {
	s.calls.Add(1)
	return &credproviders.Credentials{
		AccessKeyId:     "STS.stable",
		AccessKeySecret: "secret",
		SecurityToken:   "token",
	}, nil
}

// rotatingSTSSource mints a new AccessKeyId per call, like a real AssumeRole.
type rotatingSTSSource struct{ calls atomic.Int32 }

func (s *rotatingSTSSource) GetCredentials() (*credproviders.Credentials, error) {
	n := s.calls.Add(1)
	return &credproviders.Credentials{
		AccessKeyId:     "STS." + string(rune('0'+n)),
		AccessKeySecret: "secret",
		SecurityToken:   "token",
	}, nil
}

// credentials-go only renews inside its own 180-second window, which is wider
// than our two-minute margin, so a refresh attempt can come back with the token
// we already hold. Re-anchoring the deadline to that attempt would stretch the
// same token's local lifetime by up to a full TTL past its real expiry, which
// surfaces as intermittent OSS 403s.
func TestOssStsProviderDoesNotExtendUnchangedToken(t *testing.T) {
	src := &stableSTSSource{}
	p := &ossStsProvider{src: src, ttl: time.Hour}
	ctx := context.Background()

	_, err := p.GetCredentials(ctx)
	require.NoError(t, err)

	forced := time.Now().Add(30 * time.Second)
	p.mu.Lock()
	p.expiresAt = forced
	p.mu.Unlock()

	creds, err := p.GetCredentials(ctx)
	require.NoError(t, err)
	assert.Equal(t, "STS.stable", creds.AccessKeyID)
	assert.EqualValues(t, 2, src.calls.Load(), "the provider still asks; it just must not trust the answer")
	require.NotNil(t, creds.Expires)
	assert.WithinDuration(t, forced, *creds.Expires, time.Second,
		"an unchanged token keeps its original deadline")
	assert.WithinDuration(t, forced, p.expiresAt, time.Second)

	// Repeat attempts inside the unchanged window must not call STS again.
	for i := 0; i < 3; i++ {
		_, err := p.GetCredentials(ctx)
		require.NoError(t, err)
	}
	assert.EqualValues(t, 2, src.calls.Load(),
		"an unchanged answer must not re-ask on every request")
}

// A genuinely renewed token does move the deadline forward.
func TestOssStsProviderExtendsDeadlineForRenewedToken(t *testing.T) {
	src := &rotatingSTSSource{}
	p := &ossStsProvider{src: src, ttl: time.Hour}
	ctx := context.Background()

	_, err := p.GetCredentials(ctx)
	require.NoError(t, err)

	forced := time.Now().Add(30 * time.Second)
	p.mu.Lock()
	p.expiresAt = forced
	p.mu.Unlock()

	before := time.Now()
	creds, err := p.GetCredentials(ctx)
	require.NoError(t, err)
	assert.Equal(t, "STS.2", creds.AccessKeyID)
	assert.True(t, p.expiresAt.After(before.Add(50*time.Minute)),
		"a new AccessKeyId must re-anchor the deadline to this fetch, got %s", p.expiresAt)
}

// A credentials provider that always fails lets the startup probe fail at the
// signing step, without any network traffic.
type failingCredProvider struct{ calls atomic.Int32 }

func (f *failingCredProvider) GetCredentials(context.Context) (credentials.Credentials, error) {
	f.calls.Add(1)
	return credentials.Credentials{}, errors.New("sts unavailable")
}

func newFailingProbeClient() (*oss.Client, *failingCredProvider) {
	provider := &failingCredProvider{}
	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(provider).
		WithRegion("cn-hangzhou").
		WithEndpoint("oss-cn-hangzhou.aliyuncs.com")
	return oss.NewClient(cfg), provider
}

// A storage probe that fails while the process is starting must not be fatal:
// initFileService is registered with must(), so an STS outage at boot would
// crash-loop the container and take chat and retrieval down with it. Per-KB
// backends keep failing so a request reports a broken configuration at once.
func TestProbeOSSAtStartupOnlySwallowsWhenTolerated(t *testing.T) {
	previous := ossProbeRetryInterval
	ossProbeRetryInterval = 0
	t.Cleanup(func() { ossProbeRetryInterval = previous })

	client, _ := newFailingProbeClient()

	err := probeOSSAtStartup(client, OSSAuthConfig{}, "probe-bucket")
	require.Error(t, err, "a per-KB backend must still report the failure")

	err = probeOSSAtStartup(client, OSSAuthConfig{TolerateProbeFailure: true}, "probe-bucket")
	require.NoError(t, err, "the process bootstrap must survive a failed probe")
}

// The tolerant path retries before it gives up, but not indefinitely.
func TestProbeOSSAtStartupRetriesThenGivesUp(t *testing.T) {
	previous := ossProbeRetryInterval
	ossProbeRetryInterval = 0
	t.Cleanup(func() { ossProbeRetryInterval = previous })

	client, provider := newFailingProbeClient()
	cfg := OSSAuthConfig{TolerateProbeFailure: true}

	require.NoError(t, probeOSSAtStartup(client, cfg, "probe-bucket"))
	assert.EqualValues(t, ossProbeAttempts, provider.calls.Load(),
		"the tolerant probe must retry the configured number of attempts")
}
