package file

import (
	"os"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

// stsMetrics is the process-level registry for STS credential refreshes.
// Providers write to it; the admin diagnostics endpoint reads from it. It is
// package-level (not per-provider) because file services are rebuilt on every
// resolution and a per-instance counter would be invisible to readers.
var stsMetrics = &stsMetricsRegistry{}

type stsMetricsRegistry struct {
	mu              sync.Mutex
	successTotal    uint64
	failureTotal    uint64
	lastSuccessUnix int64
	lastFailureUnix int64
	lastErrorCode   string
	lastLatencyMs   int64
}

// STSRefreshSnapshot is a point-in-time copy of the registry.
type STSRefreshSnapshot struct {
	SuccessTotal    uint64 `json:"refresh_success_total"`
	FailureTotal    uint64 `json:"refresh_failure_total"`
	LastSuccessUnix int64  `json:"last_success_unix"`
	LastFailureUnix int64  `json:"last_failure_unix"`
	LastErrorCode   string `json:"last_error_code,omitempty"`
	LastLatencyMs   int64  `json:"last_latency_ms"`
}

func recordSTSSuccess(latency time.Duration) {
	now := time.Now().Unix()
	stsMetrics.mu.Lock()
	stsMetrics.successTotal++
	stsMetrics.lastSuccessUnix = now
	stsMetrics.lastLatencyMs = latency.Milliseconds()
	stsMetrics.mu.Unlock()
}

func recordSTSFailure(errCode string, latency time.Duration) {
	now := time.Now().Unix()
	stsMetrics.mu.Lock()
	stsMetrics.failureTotal++
	stsMetrics.lastFailureUnix = now
	stsMetrics.lastErrorCode = errCode
	stsMetrics.lastLatencyMs = latency.Milliseconds()
	stsMetrics.mu.Unlock()
}

// STSDiagnostics returns the process-wide STS refresh state, including whether
// the current process is configured for RAM role authentication. It only
// reports non-sensitive scalars: no role ARN, session name, keys, token,
// endpoint, bucket or policy.
func STSDiagnostics() STSStatus {
	authType := types.NormalizeOSSAuthType(os.Getenv("OSS_AUTH_TYPE"))
	configured := authType == types.OSSAuthTypeRAMRoleARN &&
		os.Getenv("OSS_ROLE_ARN") != "" &&
		os.Getenv("OSS_ACCESS_KEY") != "" &&
		os.Getenv("OSS_SECRET_KEY") != ""

	var snapshot STSRefreshSnapshot
	stsMetrics.mu.Lock()
	snapshot = STSRefreshSnapshot{
		SuccessTotal:    stsMetrics.successTotal,
		FailureTotal:    stsMetrics.failureTotal,
		LastSuccessUnix: stsMetrics.lastSuccessUnix,
		LastFailureUnix: stsMetrics.lastFailureUnix,
		LastErrorCode:   stsMetrics.lastErrorCode,
		LastLatencyMs:   stsMetrics.lastLatencyMs,
	}
	stsMetrics.mu.Unlock()

	return STSStatus{
		AuthType:             authType,
		Configured:           configured,
		Metrics:              snapshot,
		CredentialExpiresInS: minCredentialRemainingSeconds(),
	}
}

// STSStatus is the payload of the admin diagnostics endpoint.
type STSStatus struct {
	AuthType             string             `json:"auth_type"`
	Configured           bool               `json:"configured"`
	Metrics              STSRefreshSnapshot `json:"metrics"`
	CredentialExpiresInS int64              `json:"credential_expires_in_s"`
}

// minCredentialRemainingSeconds reports the smallest remaining lifetime across
// all cached role providers, so an operator sees the credential closest to
// expiry. Zero means no provider currently holds credentials.
func minCredentialRemainingSeconds() int64 {
	now := time.Now()
	var min int64
	ossProviderMu.Lock()
	defer ossProviderMu.Unlock()
	for _, p := range ossProviderCache {
		p.mu.Lock()
		if p.validLocked(0) {
			remaining := int64(p.expiresAt.Sub(now).Seconds())
			if remaining < 0 {
				remaining = 0
			}
			if min == 0 || remaining < min {
				min = remaining
			}
		}
		p.mu.Unlock()
	}
	return min
}

var processStartedAt = time.Now()

// ProcessStartedAt is used by the diagnostics endpoint to help interpret
// timestamps relative to this process's lifetime.
func ProcessStartedAt() time.Time { return processStartedAt }
