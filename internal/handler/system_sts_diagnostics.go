package handler

import (
	"net/http"
	"time"

	"github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/gin-gonic/gin"
)

// GetSTSDiagnostics reports the process-wide OSS RAM role credential state.
// It is mounted under /system/admin (RequireSystemAdmin) and returns only
// non-sensitive scalars: no role ARN, session name, keys, token, endpoint,
// bucket, policy or raw provider error body.
//
// @Summary      查看 OSS STS 凭证刷新诊断信息
// @Description  仅限系统管理员；返回 STS 刷新计数、最近成功/失败时间、凭证明细剩余寿命与错误码
// @Tags         系统管理
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Router       /system/admin/diagnostics/sts [get]
func (h *SystemHandler) GetSTSDiagnostics(c *gin.Context) {
	status := file.STSDiagnostics()
	now := time.Now().Unix()
	c.JSON(http.StatusOK, gin.H{"code": 0, "data": gin.H{
		"now_unix":                now,
		"process_started_unix":    file.ProcessStartedAt().Unix(),
		"auth_type":               status.AuthType,
		"configured":              status.Configured,
		"last_success_unix":       status.Metrics.LastSuccessUnix,
		"last_success_age_s":      ageSeconds(now, status.Metrics.LastSuccessUnix),
		"last_failure_unix":       status.Metrics.LastFailureUnix,
		"last_failure_age_s":      ageSeconds(now, status.Metrics.LastFailureUnix),
		"refresh_success_total":   status.Metrics.SuccessTotal,
		"refresh_failure_total":   status.Metrics.FailureTotal,
		"credential_expires_in_s": status.CredentialExpiresInS,
		"last_error_code":         status.Metrics.LastErrorCode,
		"last_latency_ms":         status.Metrics.LastLatencyMs,
	}})
}

func ageSeconds(now, then int64) int64 {
	if then <= 0 {
		return -1
	}
	age := now - then
	if age < 0 {
		return 0
	}
	return age
}
