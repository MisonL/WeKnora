package file

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	credproviders "github.com/aliyun/credentials-go/credentials/providers"
)

// OSSAuthConfig is the full set of connection parameters for an OSS-backed
// FileService. AuthType selects between long-lived static credentials
// (access_key, the historical behavior) and temporary STS credentials obtained
// by assuming a RAM role (ram_role_arn).
type OSSAuthConfig struct {
	Endpoint     string
	Region       string
	AccessKey    string
	SecretKey    string
	BucketName   string
	PathPrefix   string
	TempBucket   string
	TempRegion   string
	AuthType     string
	RoleARN      string
	SessionName  string
	STSEndpoint  string
	Policy       string
	ExternalID   string
	Expiration   int // seconds; STS session duration
	PresignTTL   int // seconds; presigned URL lifetime
	EnsureBucket bool
}

const (
	ossAuthTypeAccessKey  = "access_key"
	ossAuthTypeRAMRoleARN = "ram_role_arn"

	defaultRoleSessionExpiration = 3600
	defaultRolePresignTTL        = 3000
	staticPresignTTL             = 24 * time.Hour

	// stsRefreshMargin is how long before expiry a refresh is attempted.
	stsRefreshMargin = 2 * time.Minute
	// Failure backoff so a broken STS endpoint is not retried on every request.
	stsMinRetryBackoff = 5 * time.Second
	stsMaxRetryBackoff = 60 * time.Second
)

// stsCredentialSource is the seam used by ossStsProvider. The production
// implementation is credentials-go's RAMRoleARNCredentialsProvider; tests
// inject their own.
type stsCredentialSource interface {
	GetCredentials() (*credproviders.Credentials, error)
}

// ossStsProvider adapts credentials-go's RAM role provider into the OSS SDK's
// CredentialsProvider contract. It is used directly as the SDK provider — the
// SDK's own CredentialsFetcherProvider is deliberately not layered on top,
// because that wrapper fetches from a goroutine (a panic there would take down
// the process) and its soft-refresh window interacts badly with our own
// early-refresh margin.
//
// Behavior:
//   - serializes refreshes (single flight) so concurrent requests share one STS call;
//   - keeps serving the last successful credentials while they are still valid,
//     so a transient STS failure does not turn into a site-wide 403;
//   - never blocks a request while usable credentials exist;
//   - backs off after a failed refresh instead of hammering STS.
type ossStsProvider struct {
	src stsCredentialSource
	ttl time.Duration

	mu         sync.Mutex
	cached     credentials.Credentials
	expiresAt  time.Time
	lastErr    error
	failures   int
	retryAfter time.Time
	refreshing bool
	done       chan struct{}
}

// newOSSStsProvider builds a provider that assumes roleARN with the caller
// access key. The returned value is safe for concurrent use.
func newOSSStsProvider(cfg OSSAuthConfig) (*ossStsProvider, error) {
	duration := cfg.Expiration
	if duration <= 0 {
		duration = defaultRoleSessionExpiration
	}
	if duration < 900 {
		return nil, fmt.Errorf("OSS_ROLE_SESSION_EXPIRATION must be at least 900 seconds")
	}
	if strings.TrimSpace(cfg.RoleARN) == "" {
		return nil, errors.New("OSS_ROLE_ARN is required when OSS_AUTH_TYPE=ram_role_arn")
	}
	if strings.TrimSpace(cfg.AccessKey) == "" || strings.TrimSpace(cfg.SecretKey) == "" {
		return nil, errors.New("caller OSS_ACCESS_KEY/OSS_SECRET_KEY are required to assume the role")
	}

	builder := credproviders.NewRAMRoleARNCredentialsProviderBuilder().
		WithAccessKeyId(cfg.AccessKey).
		WithAccessKeySecret(cfg.SecretKey).
		WithRoleArn(cfg.RoleARN).
		WithDurationSeconds(duration)
	if cfg.SessionName != "" {
		builder = builder.WithRoleSessionName(cfg.SessionName)
	}
	if cfg.STSEndpoint != "" {
		builder = builder.WithStsEndpoint(cfg.STSEndpoint)
	}
	if cfg.Policy != "" {
		builder = builder.WithPolicy(cfg.Policy)
	}
	if cfg.ExternalID != "" {
		builder = builder.WithExternalId(cfg.ExternalID)
	}

	src, err := builder.Build()
	if err != nil {
		return nil, fmt.Errorf("configure RAM role provider: %w", err)
	}
	return &ossStsProvider{src: src, ttl: time.Duration(duration) * time.Second}, nil
}

// GetCredentials implements credentials.CredentialsProvider.
func (p *ossStsProvider) GetCredentials(ctx context.Context) (credentials.Credentials, error) {
	p.mu.Lock()
	if p.validLocked(stsRefreshMargin) {
		creds := p.cached
		p.mu.Unlock()
		return creds, nil
	}
	if p.refreshing {
		// Never block a request while a still-usable credential exists.
		if p.validLocked(0) {
			creds := p.cached
			p.mu.Unlock()
			return creds, nil
		}
		done := p.done
		p.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return credentials.Credentials{}, ctx.Err()
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.validLocked(0) {
			return p.cached, nil
		}
		return credentials.Credentials{}, p.unavailableLocked()
	}
	if !p.retryAfter.IsZero() && time.Now().Before(p.retryAfter) {
		// A recent refresh failed. Keep serving still-valid credentials;
		// otherwise fail fast instead of hammering STS.
		if p.validLocked(0) {
			creds := p.cached
			p.mu.Unlock()
			return creds, nil
		}
		err := p.unavailableLocked()
		p.mu.Unlock()
		return credentials.Credentials{}, err
	}
	p.refreshing = true
	p.done = make(chan struct{})
	done := p.done
	p.mu.Unlock()

	// Anchor the expiry to the moment the request started so the computed
	// deadline is never later than the token's real one.
	start := time.Now()
	cc, err := p.safeFetch()
	latency := time.Since(start)
	exp := start.Add(p.ttl)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshing = false
	close(done)
	if err != nil {
		code := stsErrorCode(err)
		recordSTSFailure(code, latency)
		p.failures++
		p.retryAfter = time.Now().Add(p.backoffLocked())
		p.lastErr = err
		// Structured metric so log collectors can aggregate without a
		// Prometheus exporter.
		logger.Warnf(ctx, "metric=sts_refresh_total result=failure code=%s latency_ms=%d", code, latency.Milliseconds())
		if p.validLocked(0) {
			// Keep serving the current credentials until they actually expire.
			logger.Warnf(ctx, "STS refresh failed, serving current credentials until %s: %s",
				p.expiresAt.Format(time.RFC3339), summarizeSTSError(err))
			return p.cached, nil
		}
		logger.Errorf(ctx, "STS credentials refresh failed: %s", summarizeSTSError(err))
		return credentials.Credentials{}, p.unavailableLocked()
	}
	recordSTSSuccess(latency)
	p.cached = credentials.Credentials{
		AccessKeyID:     cc.AccessKeyId,
		AccessKeySecret: cc.AccessKeySecret,
		SecurityToken:   cc.SecurityToken,
		Expires:         &exp,
	}
	p.expiresAt = exp
	p.failures = 0
	p.retryAfter = time.Time{}
	p.lastErr = nil
	logger.Infof(ctx, "metric=sts_refresh_total result=success latency_ms=%d expires_in_s=%d",
		latency.Milliseconds(), int64(p.ttl.Seconds()))
	return p.cached, nil
}

// safeFetch turns a panic inside the credentials provider into an error.
// credentials-go does not recover panics itself, and an unrecovered panic on a
// request goroutine would abort the whole process.
func (p *ossStsProvider) safeFetch() (creds *credproviders.Credentials, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic while fetching STS credentials: %v", r)
		}
	}()
	// credentials-go does not expose a context-aware variant; the call is
	// bounded by the STS client's own timeouts.
	return p.src.GetCredentials()
}

func (p *ossStsProvider) validLocked(margin time.Duration) bool {
	if p.cached.AccessKeyID == "" || p.cached.AccessKeySecret == "" || p.expiresAt.IsZero() {
		return false
	}
	return time.Now().Add(margin).Before(p.expiresAt)
}

func (p *ossStsProvider) backoffLocked() time.Duration {
	backoff := stsMinRetryBackoff
	for i := 1; i < p.failures && backoff < stsMaxRetryBackoff; i++ {
		backoff *= 2
	}
	if backoff > stsMaxRetryBackoff {
		backoff = stsMaxRetryBackoff
	}
	return backoff
}

// unavailableLocked returns a sanitized error; the underlying STS error may
// contain the response body, which is only logged in summarized form.
func (p *ossStsProvider) unavailableLocked() error {
	return errors.New("sts credentials temporarily unavailable")
}

var (
	stsCodePattern      = regexp.MustCompile(`"Code"\s*:\s*"([^"]{1,64})"`)
	stsRequestIDPattern = regexp.MustCompile(`"RequestId"\s*:\s*"([^"]{1,128})"`)
)

// stsErrorCode returns only the STS error code (for example AccessDenied) so
// metrics and diagnostics never carry the raw response body.
func stsErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if m := stsCodePattern.FindStringSubmatch(err.Error()); m != nil {
		return m[1]
	}
	return "unknown"
}

// summarizeSTSError reduces an STS error to its code and request id. The raw
// error embeds the STS response body (credentials-go concatenates it), which
// should not be written to logs verbatim.
func summarizeSTSError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if strings.Contains(msg, "{") {
		code := stsCodePattern.FindStringSubmatch(msg)
		requestID := stsRequestIDPattern.FindStringSubmatch(msg)
		switch {
		case code != nil && requestID != nil:
			return fmt.Sprintf("code=%s request_id=%s", code[1], requestID[1])
		case code != nil:
			return "code=" + code[1]
		default:
			return "STS request failed (details suppressed)"
		}
	}
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	return msg
}

// ossClientKey identifies a cached credential provider. Only fields that
// affect the STS call are included, so rotating an unrelated setting (bucket,
// presign TTL, region) reuses the existing provider.
type ossClientKey struct {
	accessKeyID    string
	secretKeyHash  string
	roleARN        string
	sessionName    string
	stsEndpoint    string
	policy         string
	externalID     string
	expirationSecs int
}

var (
	ossProviderMu    sync.Mutex
	ossProviderCache = map[ossClientKey]*ossStsProvider{}
)

// getOrCreateStsProvider returns the process-wide provider for cfg so every
// resolved FileService shares one credential cache and one AssumeRole stream.
func getOrCreateStsProvider(cfg OSSAuthConfig) (*ossStsProvider, error) {
	secretHash := sha256.Sum256([]byte(cfg.SecretKey))
	key := ossClientKey{
		accessKeyID:    cfg.AccessKey,
		secretKeyHash:  hex.EncodeToString(secretHash[:]),
		roleARN:        cfg.RoleARN,
		sessionName:    cfg.SessionName,
		stsEndpoint:    cfg.STSEndpoint,
		policy:         cfg.Policy,
		externalID:     cfg.ExternalID,
		expirationSecs: cfg.Expiration,
	}
	ossProviderMu.Lock()
	defer ossProviderMu.Unlock()
	if p, ok := ossProviderCache[key]; ok {
		return p, nil
	}
	p, err := newOSSStsProvider(cfg)
	if err != nil {
		return nil, err
	}
	ossProviderCache[key] = p
	return p, nil
}

// newOSSClientWithConfig builds an OSS client for either auth mode.
func newOSSClientWithConfig(cfg OSSAuthConfig) (*oss.Client, error) {
	if err := validateOSSAuthConfig(cfg); err != nil {
		return nil, err
	}
	endpoint := cfg.Endpoint
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	if err := utils.ValidateURLForSSRF(cfg.Endpoint); err != nil {
		return nil, fmt.Errorf("unsafe OSS endpoint: %w", err)
	}

	var provider credentials.CredentialsProvider
	if cfg.AuthType == ossAuthTypeRAMRoleARN {
		sts, err := getOrCreateStsProvider(cfg)
		if err != nil {
			return nil, err
		}
		provider = sts
	} else {
		provider = credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")
	}

	ossCfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(provider).
		WithRegion(cfg.Region).
		WithEndpoint(strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")).
		WithHttpClient(objectStorageHTTPClient())
	return oss.NewClient(ossCfg), nil
}

func validateOSSAuthConfig(cfg OSSAuthConfig) error {
	if strings.TrimSpace(cfg.Endpoint) == "" || strings.TrimSpace(cfg.Region) == "" {
		return errors.New("OSS endpoint and region are required")
	}
	authType := strings.TrimSpace(cfg.AuthType)
	if authType == "" {
		authType = ossAuthTypeAccessKey
	}
	switch authType {
	case ossAuthTypeAccessKey:
		if strings.TrimSpace(cfg.AccessKey) == "" || strings.TrimSpace(cfg.SecretKey) == "" {
			return errors.New("OSS access key and secret key are required")
		}
	case ossAuthTypeRAMRoleARN:
		if strings.TrimSpace(cfg.AccessKey) == "" || strings.TrimSpace(cfg.SecretKey) == "" {
			return errors.New("caller OSS access key and secret key are required to assume the role")
		}
		return types.ValidateOSSRoleAuth(cfg.RoleARN, cfg.SessionName, cfg.STSEndpoint, cfg.Policy, cfg.Expiration)
	default:
		return fmt.Errorf("unsupported OSS auth type %q", cfg.AuthType)
	}
	return nil
}

// ossEnsureBucketReadOnly verifies the bucket is reachable without relying on
// GetBucketAcl (which the platform does not grant) and without attempting to
// create it. A missing or inaccessible bucket is an error: role credentials are
// scoped to one bucket, so silently continuing would only move the failure to
// the first object operation.
func ossEnsureBucketReadOnly(client *oss.Client, bucketName string) error {
	ctx, cancel := objectStorageSetupContext()
	defer cancel()
	_, err := client.ListObjectsV2(ctx, &oss.ListObjectsV2Request{
		Bucket:  oss.Ptr(bucketName),
		MaxKeys: 1,
	})
	if err != nil {
		return fmt.Errorf("failed to access OSS bucket %q: %w", bucketName, err)
	}
	return nil
}

// presignTTL returns the configured lifetime for presigned URLs. In role mode
// the effective lifetime is additionally capped by the remaining token life.
func presignTTL(cfg OSSAuthConfig) time.Duration {
	if cfg.PresignTTL > 0 {
		return time.Duration(cfg.PresignTTL) * time.Second
	}
	if cfg.AuthType == ossAuthTypeRAMRoleARN {
		return defaultRolePresignTTL * time.Second
	}
	return staticPresignTTL
}
