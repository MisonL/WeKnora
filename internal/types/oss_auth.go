package types

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// OSS authentication modes.
const (
	OSSAuthTypeAccessKey  = "access_key"
	OSSAuthTypeRAMRoleARN = "ram_role_arn"
)

var (
	ossRoleARNPattern         = regexp.MustCompile(`^acs:ram::\d{6,20}:role/[A-Za-z0-9._-]{1,64}$`)
	ossRoleSessionNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	ossSTSRegionHostPattern   = regexp.MustCompile(`^sts(-vpc)?\.[a-z0-9-]+\.aliyuncs\.com$`)
)

// NormalizeOSSAuthType returns the effective auth type, defaulting to
// access_key when unset (historical behavior).
func NormalizeOSSAuthType(authType string) string {
	v := strings.ToLower(strings.TrimSpace(authType))
	if v == "" {
		return OSSAuthTypeAccessKey
	}
	return v
}

// ValidateOSSRoleAuth validates the RAM role parameters used when
// OSS_AUTH_TYPE=ram_role_arn. It is shared by the tenant engine config and the
// storage backend config so both entry points enforce the same rules.
func ValidateOSSRoleAuth(roleARN, sessionName, stsEndpoint, policy string, expiration int) error {
	roleARN = strings.TrimSpace(roleARN)
	if roleARN == "" {
		return fmt.Errorf("role_arn is required when auth_type=%s", OSSAuthTypeRAMRoleARN)
	}
	if !ossRoleARNPattern.MatchString(roleARN) {
		return fmt.Errorf("role_arn must look like acs:ram::<account-id>:role/<name>")
	}
	if sessionName != "" && !ossRoleSessionNamePattern.MatchString(sessionName) {
		return fmt.Errorf("role_session_name must be 1-64 characters of [A-Za-z0-9._-]")
	}
	if expiration != 0 && (expiration < 900 || expiration > 43200) {
		return fmt.Errorf("role_session_expiration must be between 900 and 43200 seconds")
	}
	if stsEndpoint != "" {
		host := strings.ToLower(strings.TrimSpace(stsEndpoint))
		if strings.Contains(host, "://") {
			parsed, err := url.Parse(host)
			if err != nil || parsed.Hostname() == "" {
				return fmt.Errorf("sts_endpoint is not a valid URL")
			}
			host = parsed.Hostname()
		} else if i := strings.IndexByte(host, '/'); i >= 0 {
			host = host[:i]
		}
		if host != "sts.aliyuncs.com" && !ossSTSRegionHostPattern.MatchString(host) {
			// The STS endpoint receives the caller's signed request; pin it to
			// Aliyun hosts so a misconfiguration cannot exfiltrate the caller AK.
			return fmt.Errorf("sts_endpoint must be sts.aliyuncs.com or sts.<region>.aliyuncs.com")
		}
	}
	if policy != "" {
		if len(policy) > 2048 {
			return fmt.Errorf("policy must be at most 2048 characters")
		}
		if !json.Valid([]byte(policy)) {
			return fmt.Errorf("policy must be valid JSON")
		}
	}
	return nil
}

// envInt parses an optional integer environment value, returning 0 when unset
// or malformed (validation happens later against the effective value).
func envInt(name string) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return v
}

// firstNonEmpty returns the first non-empty trimmed value.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
