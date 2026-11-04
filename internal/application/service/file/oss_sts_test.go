package file

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
