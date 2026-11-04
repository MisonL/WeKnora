package file

import (
	"context"
	"errors"
	"fmt"
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
)

// stsCredentialSource is the seam used by ossStsProvider. The production
// implementation is credentials-go's RAMRoleARNCredentialsProvider; tests
// inject their own.
type stsCredentialSource interface {
	GetCredentials() (*credproviders.Credentials, error)
}

// ossStsProvider adapts credentials-go's non-thread-safe, error-prone RAM role
// provider into the OSS SDK's CredentialsProvider contract:
//
//   - serializes refreshes (single flight) so concurrent requests share one STS call;
//   - remembers the last successful credentials together with their expiry and
//     keeps serving them while they are still valid, so a transient STS failure
//     does not turn into a site-wide 403;
//   - reports the expiry to the SDK, which gives us the soft-refresh/stale
//     fallback behavior of its fetcher provider for free.
type ossStsProvider struct {
	src stsCredentialSource
	ttl time.Duration

	mu         sync.Mutex
	cached     credentials.Credentials
	expiresAt  time.Time
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
	if p.validLocked(2 * time.Minute) {
		creds := p.cached
		p.mu.Unlock()
		return creds, nil
	}
	if p.refreshing {
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
		return credentials.Credentials{}, errors.New("sts credentials unavailable")
	}
	p.refreshing = true
	p.done = make(chan struct{})
	done := p.done
	p.mu.Unlock()

	cc, err := p.fetch()

	exp := time.Now().Add(p.ttl)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshing = false
	close(done)
	if err != nil {
		// Keep serving the previous credentials while they are still valid.
		if p.validLocked(0) {
			logger.Warnf(ctx, "STS refresh failed, serving previously fetched credentials until %s", p.expiresAt.Format(time.RFC3339))
			return p.cached, nil
		}
		logger.Errorf(ctx, "STS credentials refresh failed: %v", err)
		return credentials.Credentials{}, errors.New("sts credentials temporarily unavailable")
	}
	p.cached = credentials.Credentials{
		AccessKeyID:     cc.AccessKeyId,
		AccessKeySecret: cc.AccessKeySecret,
		SecurityToken:   cc.SecurityToken,
		Expires:         &exp,
	}
	p.expiresAt = exp
	return p.cached, nil
}

func (p *ossStsProvider) fetch() (*credproviders.Credentials, error) {
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

// ossClientKey identifies a cached provider/client. Credentials are not part of
// the key beyond the caller access key id, so rotating the secret rebuilds.
type ossClientKey struct {
	endpoint, region, bucket, tempBucket, tempRegion                string
	authType, roleARN, sessionName, stsEndpoint, policy, externalID string
	expiration, presignTTL                                          int
	accessKeyID                                                     string
}

var (
	ossProviderMu    sync.Mutex
	ossProviderCache = map[ossClientKey]*ossStsProvider{}
)

// getOrCreateStsProvider returns the process-wide provider for cfg so every
// resolved FileService shares one credential cache and one AssumeRole stream.
func getOrCreateStsProvider(cfg OSSAuthConfig) *ossStsProvider {
	key := ossClientKey{
		endpoint:    cfg.Endpoint,
		region:      cfg.Region,
		bucket:      cfg.BucketName,
		tempBucket:  cfg.TempBucket,
		tempRegion:  cfg.TempRegion,
		authType:    cfg.AuthType,
		roleARN:     cfg.RoleARN,
		sessionName: cfg.SessionName,
		stsEndpoint: cfg.STSEndpoint,
		policy:      cfg.Policy,
		externalID:  cfg.ExternalID,
		expiration:  cfg.Expiration,
		presignTTL:  cfg.PresignTTL,
		accessKeyID: cfg.AccessKey,
	}
	ossProviderMu.Lock()
	defer ossProviderMu.Unlock()
	if p, ok := ossProviderCache[key]; ok {
		return p
	}
	p, err := newOSSStsProvider(cfg)
	if err != nil {
		// Callers that need an error go through newOSSClientWithConfig; the
		// cache only holds successfully built providers.
		return nil
	}
	ossProviderCache[key] = p
	return p
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
		sts := getOrCreateStsProvider(cfg)
		if sts == nil {
			return nil, errors.New("failed to initialize STS credentials provider")
		}
		// The SDK fetcher adds soft refresh (0.8 * TTL) plus stale fallback.
		provider = credentials.NewCredentialsFetcherProvider(credentials.CredentialsFetcherFunc(func(ctx context.Context) (credentials.Credentials, error) {
			return sts.GetCredentials(ctx)
		}))
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
