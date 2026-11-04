package file

import (
	"errors"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func roleStorageEngineConfig() *types.StorageEngineConfig {
	return &types.StorageEngineConfig{
		DefaultProvider: "oss",
		OSS: &types.OSSEngineConfig{
			Endpoint:   "oss-cn-hangzhou.aliyuncs.com",
			Region:     "cn-hangzhou",
			BucketName: "noraoss01",
			AccessKey:  "caller-ak",
			SecretKey:  "caller-sk",
			AuthType:   types.OSSAuthTypeRAMRoleARN,
			RoleARN:    "acs:ram::1645446608222577:role/nora-prd-br",
		},
	}
}

func TestFactoryRejectsRoleConfigFromTenantSource(t *testing.T) {
	_, _, err := NewFileServiceFromStorageConfig("oss", roleStorageEngineConfig(), t.TempDir(), "tenant")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrStorageRoleNotAllowed),
		"rejection must be distinguishable so callers cannot fall back to the global identity")
}

func TestFactoryRejectsRoleConfigWithoutSource(t *testing.T) {
	_, _, err := NewFileServiceFromStorageConfig("oss", roleStorageEngineConfig(), t.TempDir(), "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrStorageRoleNotAllowed))
}
