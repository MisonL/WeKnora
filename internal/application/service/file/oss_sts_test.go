package file

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
