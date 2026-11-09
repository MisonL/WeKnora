package file

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
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
	// TolerateProbeFailure turns a failed startup bucket probe into a warning
	// instead of an error. The process bootstrap sets it because
	// initFileService is registered with must(): an STS outage at boot would
	// otherwise crash-loop the container and take chat and retrieval down with
	// it. Per-knowledge-base backends leave it false so a broken configuration
	// still fails the request that needs it.
	TolerateProbeFailure bool
}

const (
	ossAuthTypeAccessKey  = "access_key"
	ossAuthTypeRAMRoleARN = "ram_role_arn"

	defaultRoleSessionExpiration = 3600
	defaultRolePresignTTL        = 3000
	staticPresignTTL             = 24 * time.Hour

	// stsRefreshMargin is how long before expiry a refresh is attempted.
	stsRefreshMargin = 2 * time.Minute
	// stsExpirySafety shortens the locally computed expiry. credentials-go
	// discards the STS Expiration, so the local deadline must not overrun a
	// server-granted shorter TTL.
	stsExpirySafety = 60 * time.Second
	// Failure backoff so a broken STS endpoint is not retried on every request.
	stsMinRetryBackoff = 5 * time.Second
	stsMaxRetryBackoff = 60 * time.Second
	// ossUnchangedRetryInterval spaces attempts after credentials-go answered
	// with the token it already holds, so one renewal window does not turn into
	// a refresh attempt (and a log line) on every storage call.
	ossUnchangedRetryInterval = 15 * time.Second
	// ossProviderCacheLimit bounds the process-wide provider cache. Config
	// rotation should reuse one entry; the limit only guards against unbounded
	// growth from repeatedly changing credentials.
	ossProviderCacheLimit = 16
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

	createdAt time.Time

	mu         sync.Mutex
	cached     credentials.Credentials
	expiresAt  time.Time
	lastErr    error
	failures   int
	retryAfter time.Time
	refreshing bool
	done       chan struct{}
}

// normalizeSTSEndpoint reduces an STS endpoint to the bare host that
// credentials-go expects; it appends the region suffix to whatever it is given,
// so a value carrying a scheme or a path would otherwise produce a broken host.
func normalizeSTSEndpoint(endpoint string) string {
	host := strings.ToLower(strings.TrimSpace(endpoint))
	if strings.Contains(host, "://") {
		if parsed, err := url.Parse(host); err == nil && parsed.Hostname() != "" {
			return parsed.Hostname()
		}
	}
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	return host
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
		builder = builder.WithStsEndpoint(normalizeSTSEndpoint(cfg.STSEndpoint))
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
	return &ossStsProvider{src: src, ttl: time.Duration(duration) * time.Second, createdAt: time.Now()}, nil
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
	p.mu.Unlock()

	// Fetch in a goroutine so a caller can stop waiting on ctx, while the
	// in-flight call still finishes and publishes its result. That keeps the
	// single-flight slot occupied only for the real duration of the STS call
	// and lets other callers benefit from the outcome.
	resultCh := make(chan stsFetchOutcome, 1)
	go func() {
		creds, err := p.safeFetch()
		resultCh <- stsFetchOutcome{creds: creds, err: err}
	}()

	start := time.Now()
	select {
	case res := <-resultCh:
		return p.completeRefresh(ctx, start, res)
	case <-ctx.Done():
		go func() {
			p.completeRefresh(ctx, start, <-resultCh)
		}()
		return credentials.Credentials{}, ctx.Err()
	}
}

// stsFetchOutcome is the result of one credentials fetch.
type stsFetchOutcome struct {
	creds *credproviders.Credentials
	err   error
}

// completeRefresh publishes the result of a fetch and releases waiters. It is
// always called exactly once per refresh, including when the initiating caller
// stopped waiting.
func (p *ossStsProvider) completeRefresh(
	ctx context.Context, start time.Time, res stsFetchOutcome,
) (credentials.Credentials, error) {
	cc, err := res.creds, res.err
	if err == nil && (cc == nil || strings.TrimSpace(cc.AccessKeyId) == "" ||
		strings.TrimSpace(cc.AccessKeySecret) == "" || strings.TrimSpace(cc.SecurityToken) == "") {
		// A provider returning nil or partial credentials must be treated as a
		// failure; caching them would surface as opaque 403s on every request.
		err = errors.New("sts provider returned incomplete credentials")
	}
	latency := time.Since(start)
	// Anchor expiry to the fetch start and keep a small margin: credentials-go
	// discards the STS Expiration field, so this is a conservative local bound.
	exp := start.Add(p.ttl - stsExpirySafety)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshing = false
	close(p.done)
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
	// credentials-go only renews once less than 180 seconds remain on its own
	// copy, which is wider than this layer's two-minute refresh margin. When it
	// hands back the token it already holds, re-anchoring the deadline to this
	// attempt would push the same token's local lifetime a whole TTL past its
	// real expiry — the process then signs OSS requests with an expired token
	// and gets intermittent 403s. A new AccessKeyId is the only proof of an
	// actual renewal (each AssumeRole mints a fresh one).
	renewed := p.cached.AccessKeyID == "" || p.cached.AccessKeyID != cc.AccessKeyId
	if !renewed {
		if !p.expiresAt.IsZero() {
			exp = p.expiresAt
		}
		// Until credentials-go's own 180-second window opens it keeps answering
		// from its cache, so asking again on the very next request would only
		// repeat this outcome. Hold off briefly instead of logging on every
		// storage call; the credentials stay usable in the meantime.
		p.retryAfter = time.Now().Add(ossUnchangedRetryInterval)
		logger.Infof(ctx,
			"metric=sts_refresh_total result=unchanged latency_ms=%d access_key_id=%s expires_at=%s",
			latency.Milliseconds(), cc.AccessKeyId, exp.Format(time.RFC3339))
	}
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
	if !renewed {
		p.retryAfter = time.Now().Add(ossUnchangedRetryInterval)
	}
	if renewed {
		logger.Infof(ctx, "metric=sts_refresh_total result=success latency_ms=%d expires_in_s=%d",
			latency.Milliseconds(), int64(p.ttl.Seconds()))
	}
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
	return stsCredentialParamPattern.ReplaceAllString(msg, "$1<redacted>")
}

// stsCredentialParamPattern scrubs credential-bearing query or form parameters
// from STS errors that are not JSON (transport failures, XML responses). These
// errors are the only remaining place a caller key or signature could surface
// in a log line.
var stsCredentialParamPattern = regexp.MustCompile(
	`(?i)\b(AccessKeyId|AccessKeySecret|SecurityToken|Signature|SignatureNonce|X-Amz-Security-Token)=([^&\s"]+)`,
)

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
		stsEndpoint:    normalizeSTSEndpoint(cfg.STSEndpoint),
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
	if len(ossProviderCache) >= ossProviderCacheLimit {
		// Evict the least recently created entry. Rotation should keep this
		// cache at one or two entries; the limit only prevents unbounded
		// growth when credentials keep changing.
		var oldestKey ossClientKey
		var oldest *ossStsProvider
		for k, v := range ossProviderCache {
			if oldest == nil || v.createdAt.Before(oldest.createdAt) {
				oldestKey, oldest = k, v
			}
		}
		if oldest != nil {
			delete(ossProviderCache, oldestKey)
		}
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

// ossProbeRetryInterval spaces the startup probe retries; a variable so tests
// do not wait.
var ossProbeRetryInterval = 2 * time.Second

// ossProbeAttempts bounds the tolerant startup probe. STS is an external
// dependency; three attempts ride out a short blip without holding boot open.
const ossProbeAttempts = 3

// probeBucketReadOnly runs the read-only bucket probe up to attempts times.
// Only the tolerant startup path retries: a transient STS blip should not even
// produce the warning.
func probeBucketReadOnly(client *oss.Client, bucketName string, attempts int) error {
	err := ossEnsureBucketReadOnly(client, bucketName)
	for attempt := 1; attempt < attempts && err != nil; attempt++ {
		time.Sleep(ossProbeRetryInterval)
		err = ossEnsureBucketReadOnly(client, bucketName)
	}
	return err
}

// probeOSSAtStartup verifies the bucket is reachable while the service is being
// constructed.
//
// Role credentials are scoped to the bucket, so the bucket cannot be created and
// the probe is read-only. A probe failure is normally fatal — a wrong bucket or
// role should surface at construction, not on the first upload. The process
// bootstrap opts out (TolerateProbeFailure) because initFileService is built
// with must(): an STS outage at boot would crash-loop the container and take
// chat and retrieval down with it. In that mode the probe retries briefly and
// then only warns; the first storage request reports the failure.
func probeOSSAtStartup(client *oss.Client, cfg OSSAuthConfig, bucketName string) error {
	if cfg.AuthType != ossAuthTypeRAMRoleARN && cfg.EnsureBucket {
		return ossEnsureBucket(client, bucketName)
	}
	attempts := 1
	if cfg.TolerateProbeFailure {
		attempts = ossProbeAttempts
	}
	err := probeBucketReadOnly(client, bucketName, attempts)
	if err != nil && cfg.TolerateProbeFailure {
		logger.Warnf(context.Background(),
			"OSS bucket probe failed; starting anyway and retrying on first use (bucket=%s): %s",
			bucketName, summarizeSTSError(err))
		return nil
	}
	return err
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
