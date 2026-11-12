package types

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateOSSRoleAuth(t *testing.T) {
	validARN := "acs:ram::1645446608222577:role/nora-prd-br"
	cases := []struct {
		name       string
		roleARN    string
		session    string
		stsHost    string
		policy     string
		expiration int
		wantErr    bool
	}{
		{"valid defaults", validARN, "", "", "", 0, false},
		{"valid full", validARN, "weknora-nora", "sts.cn-hangzhou.aliyuncs.com", "", 3600, false},
		{"sts global host", validARN, "", "sts.aliyuncs.com", "", 0, false},
		{"sts host with scheme", validARN, "", "https://sts.cn-hangzhou.aliyuncs.com", "", 0, false},
		{"empty arn", "", "", "", "", 0, true},
		{"bad arn", "acs:ram::123:role/x", "", "", "", 0, true},
		{"session with slash", validARN, "bad/name", "", "", 0, true},
		{"expiration too small", validARN, "", "", "", 899, true},
		{"expiration too large", validARN, "", "", "", 43201, true},
		{"sts host outside aliyun", validARN, "", "sts.evil.example.com", "", 0, true},
		{"policy not json", validARN, "", "", "not-json", 0, true},
		{"policy valid json", validARN, "", "", `{"Version":"1"}`, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateOSSRoleAuth(tc.roleARN, tc.session, tc.stsHost, tc.policy, tc.expiration)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestOSSEngineConfigValidateForTenantWrite(t *testing.T) {
	require.NoError(t, (*OSSEngineConfig)(nil).ValidateForTenantWrite())
	require.NoError(t, (&OSSEngineConfig{AuthType: "access_key", AccessKey: "ak"}).ValidateForTenantWrite())
	require.Error(t, (&OSSEngineConfig{AuthType: "ram_role_arn"}).ValidateForTenantWrite())
	require.Error(t, (&OSSEngineConfig{RoleARN: "acs:ram::1:role/x"}).ValidateForTenantWrite())
	require.Error(t, (&OSSEngineConfig{STSEndpoint: "sts.cn-hangzhou.aliyuncs.com"}).ValidateForTenantWrite())
}

func TestStorageBackendConfigValidateForProviderOSS(t *testing.T) {
	base := StorageBackendConfig{Endpoint: "oss-cn-hangzhou.aliyuncs.com", Region: "cn-hangzhou", BucketName: "b", AccessKeyID: "ak", SecretAccessKey: "sk"}
	require.NoError(t, base.ValidateForProvider("oss"))

	role := base
	role.AuthType = OSSAuthTypeRAMRoleARN
	role.RoleARN = "acs:ram::1645446608222577:role/nora-prd-br"
	require.NoError(t, role.ValidateForProvider("oss"))

	missingRole := base
	missingRole.AuthType = OSSAuthTypeRAMRoleARN
	require.Error(t, missingRole.ValidateForProvider("oss"))

	roleNoCaller := role
	roleNoCaller.AccessKeyID = ""
	require.Error(t, roleNoCaller.ValidateForProvider("oss"))

	unknown := base
	unknown.AuthType = "something-else"
	require.Error(t, unknown.ValidateForProvider("oss"))

	noBucket := base
	noBucket.BucketName = ""
	require.Error(t, noBucket.ValidateForProvider("oss"))
}

func TestNormalizeOSSAuthType(t *testing.T) {
	assert.Equal(t, OSSAuthTypeAccessKey, NormalizeOSSAuthType(""))
	assert.Equal(t, OSSAuthTypeRAMRoleARN, NormalizeOSSAuthType(" RAM_ROLE_ARN "))
	assert.Equal(t, strings.ToLower("Custom"), NormalizeOSSAuthType("Custom"))
}
