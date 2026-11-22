package errors

import (
	"fmt"
	"net/http"
)

// ErrorCode defines the error code type
type ErrorCode int

// System error codes
const (
	// Common error codes (1000-1999)
	ErrBadRequest         ErrorCode = 1000
	ErrUnauthorized       ErrorCode = 1001
	ErrForbidden          ErrorCode = 1002
	ErrNotFound           ErrorCode = 1003
	ErrMethodNotAllowed   ErrorCode = 1004
	ErrConflict           ErrorCode = 1005
	ErrTooManyRequests    ErrorCode = 1006
	ErrInternalServer     ErrorCode = 1007
	ErrServiceUnavailable ErrorCode = 1008
	ErrTimeout            ErrorCode = 1009
	ErrValidation         ErrorCode = 1010
	// ErrPayloadTooLarge distinguishes "your body exceeded the endpoint's
	// limit" from a malformed request: the caller can shrink and retry, and a
	// retry of the identical body will always fail the same way.
	ErrPayloadTooLarge ErrorCode = 1011

	// Tenant related error codes (2000-2099)
	ErrTenantNotFound         ErrorCode = 2000
	ErrTenantAlreadyExists    ErrorCode = 2001
	ErrTenantInactive         ErrorCode = 2002
	ErrTenantNameRequired     ErrorCode = 2003
	ErrTenantInvalidStatus    ErrorCode = 2004
	ErrTenantCreationDisabled ErrorCode = 2005

	// Agent related error codes (2100-2199)
	ErrAgentMissingThinkingModel ErrorCode = 2100
	ErrAgentMissingAllowedTools  ErrorCode = 2101
	ErrAgentInvalidMaxIterations ErrorCode = 2102
	ErrAgentInvalidTemperature   ErrorCode = 2103

	// VectorStore binding related error codes (2200-2299).
	// Both map to HTTP 400 with a generic message; the typed code lets
	// clients distinguish "wrong UUID / cross-tenant" from "store exists
	// but is currently unavailable" without parsing the message text.
	ErrVectorStoreBindingInvalid ErrorCode = 2200
	ErrVectorStoreUnavailable    ErrorCode = 2201

	// Model lifecycle errors (2300-2399).
	// The dedicated code lets clients render structured usage details without
	// parsing the backwards-compatible English error message.
	ErrModelInUse ErrorCode = 2300

	// Import prerequisite errors (2400-2499).
	// The dedicated code lets the datasource sync path distinguish permanent
	// configuration-mismatch rejections (VLM/ASR not configured for an
	// image/audio import) from retryable ingest failures without parsing
	// message text. HTTP semantics are identical to a bad request.
	ErrImportPrerequisiteMissing ErrorCode = 2400

	// Enqueue failures (2500-2599).
	// The uploaded row exists and is stored, but no parse job could be
	// queued, so nothing will ever process it. HTTP 503 rather than 200: a
	// caller that treats this as success stops watching a document that is
	// already known to be unparsed. The typed code lets clients tell this
	// apart from a generic 5xx -- the fix is to reparse the returned row,
	// not to re-upload the file.
	ErrKnowledgeEnqueueFailed ErrorCode = 2500

	// Add more error codes here
)

// AppError defines the application error structure
type AppError struct {
	Code     ErrorCode `json:"code"`
	Message  string    `json:"message"`
	Details  any       `json:"details,omitempty"`
	HTTPCode int       `json:"-"`
	// RetryAfterSeconds, when positive, is sent as the Retry-After header on a
	// 429 so a client can wait the right amount instead of guessing. Zero
	// leaves the header off: an admission refusal with no estimate is still a
	// valid 429.
	RetryAfterSeconds int `json:"-"`
}

// Error implements the error interface
func (e *AppError) Error() string {
	return fmt.Sprintf("error code: %d, error message: %s", e.Code, e.Message)
}

// WithDetails adds error details
func (e *AppError) WithDetails(details any) *AppError {
	e.Details = details
	return e
}

// WithRetryAfter sets the Retry-After hint a 429 response carries.
func (e *AppError) WithRetryAfter(seconds int) *AppError {
	e.RetryAfterSeconds = seconds
	return e
}

// NewBadRequestError creates a bad request error
func NewBadRequestError(message string) *AppError {
	return &AppError{
		Code:     ErrBadRequest,
		Message:  message,
		HTTPCode: http.StatusBadRequest,
	}
}

// NewPayloadTooLargeError creates the HTTP 413 returned when a request body
// exceeds the endpoint's byte budget.
func NewPayloadTooLargeError(message string) *AppError {
	return &AppError{
		Code:     ErrPayloadTooLarge,
		Message:  message,
		HTTPCode: http.StatusRequestEntityTooLarge,
	}
}

// NewImportPrerequisiteError creates the typed HTTP 400 returned when an
// import requires a model (VLM for images, ASR for audio) that the target
// knowledge base has not configured. The message stays user-facing; the code
// lets the datasource sync path treat the rejection as permanent instead of
// retrying a task that cannot succeed until the operator configures a model.
func NewImportPrerequisiteError(message string) *AppError {
	return &AppError{
		Code:     ErrImportPrerequisiteMissing,
		Message:  message,
		HTTPCode: http.StatusBadRequest,
	}
}

// NewModelInUseError creates the typed HTTP 400 returned when active tenant
// resources still reference a model.
func NewModelInUseError(message string, details any) *AppError {
	return &AppError{
		Code:     ErrModelInUse,
		Message:  message,
		Details:  details,
		HTTPCode: http.StatusBadRequest,
	}
}

// NewUnauthorizedError creates an unauthorized error
func NewUnauthorizedError(message string) *AppError {
	return &AppError{
		Code:     ErrUnauthorized,
		Message:  message,
		HTTPCode: http.StatusUnauthorized,
	}
}

// NewForbiddenError creates a forbidden error
func NewForbiddenError(message string) *AppError {
	return &AppError{
		Code:     ErrForbidden,
		Message:  message,
		HTTPCode: http.StatusForbidden,
	}
}

// NewNotFoundError creates a not found error
func NewNotFoundError(message string) *AppError {
	return &AppError{
		Code:     ErrNotFound,
		Message:  message,
		HTTPCode: http.StatusNotFound,
	}
}

// NewConflictError creates a conflict error
func NewConflictError(message string) *AppError {
	return &AppError{
		Code:     ErrConflict,
		Message:  message,
		HTTPCode: http.StatusConflict,
	}
}

// NewTooManyRequestsError creates a 429 error, used by quota-style
// guards (e.g. per-user self-service tenant creation cap).
func NewTooManyRequestsError(message string) *AppError {
	if message == "" {
		message = "too many requests"
	}
	return &AppError{
		Code:     ErrTooManyRequests,
		Message:  message,
		HTTPCode: http.StatusTooManyRequests,
	}
}

// NewInternalServerError creates an internal server error
func NewInternalServerError(message string) *AppError {
	if message == "" {
		message = "服务器内部错误"
	}
	return &AppError{
		Code:     ErrInternalServer,
		Message:  message,
		HTTPCode: http.StatusInternalServerError,
	}
}

// NewServiceUnavailableError creates a service unavailable (503)
// error. Used for transient failures where the caller can retry.
func NewServiceUnavailableError(message string) *AppError {
	if message == "" {
		message = "服务暂时不可用"
	}
	return &AppError{
		Code:     ErrServiceUnavailable,
		Message:  message,
		HTTPCode: http.StatusServiceUnavailable,
	}
}

// NewKnowledgeEnqueueFailedError creates the 503 returned when an uploaded
// document was stored but its parse job could not be queued. The message says
// both halves, because the caller's next step depends on it: the file is safe,
// and the row needs a reparse rather than a re-upload.
func NewKnowledgeEnqueueFailedError(message string) *AppError {
	if message == "" {
		message = "文件已保存，但解析任务未能入队，请稍后重新解析"
	}
	return &AppError{
		Code:     ErrKnowledgeEnqueueFailed,
		Message:  message,
		HTTPCode: http.StatusServiceUnavailable,
	}
}

// NewValidationError creates a validation error
func NewValidationError(message string) *AppError {
	return &AppError{
		Code:     ErrValidation,
		Message:  message,
		HTTPCode: http.StatusBadRequest,
	}
}

// Tenant related errors
func NewTenantNotFoundError() *AppError {
	return &AppError{
		Code:     ErrTenantNotFound,
		Message:  "空间不存在",
		HTTPCode: http.StatusNotFound,
	}
}

// NewTenantAlreadyExistsError creates a tenant already exists error
func NewTenantAlreadyExistsError() *AppError {
	return &AppError{
		Code:     ErrTenantAlreadyExists,
		Message:  "空间已存在",
		HTTPCode: http.StatusConflict,
	}
}

// NewTenantInactiveError creates a tenant inactive error
func NewTenantInactiveError() *AppError {
	return &AppError{
		Code:     ErrTenantInactive,
		Message:  "空间已停用",
		HTTPCode: http.StatusForbidden,
	}
}

// NewTenantCreationDisabledError reports a deployment-policy denial for
// ordinary self-service tenant creation.
func NewTenantCreationDisabledError() *AppError {
	return &AppError{
		Code:     ErrTenantCreationDisabled,
		Message:  "self-service workspace creation is disabled; join a workspace by invitation",
		HTTPCode: http.StatusForbidden,
	}
}

// Agent related errors
func NewAgentMissingThinkingModelError() *AppError {
	return &AppError{
		Code:     ErrAgentMissingThinkingModel,
		Message:  "启用Agent模式前，请先选择思考模型",
		HTTPCode: http.StatusBadRequest,
	}
}

func NewAgentMissingAllowedToolsError() *AppError {
	return &AppError{
		Code:     ErrAgentMissingAllowedTools,
		Message:  "至少需要选择一个允许的工具",
		HTTPCode: http.StatusBadRequest,
	}
}

func NewAgentInvalidMaxIterationsError() *AppError {
	return &AppError{
		Code:     ErrAgentInvalidMaxIterations,
		Message:  "最大迭代次数须为正整数，或 -1 表示不限制",
		HTTPCode: http.StatusBadRequest,
	}
}

func NewAgentInvalidTemperatureError() *AppError {
	return &AppError{
		Code:     ErrAgentInvalidTemperature,
		Message:  "温度参数必须在0-2之间",
		HTTPCode: http.StatusBadRequest,
	}
}

// NewVectorStoreBindingInvalidError signals that a knowledge base create
// request referenced a vector store that does not exist under the caller's
// tenant (or carried a malformed UUID). The user-facing message is
// intentionally generic to avoid enumeration oracles — see the structured
// log at the call site for the tenant/store pair.
func NewVectorStoreBindingInvalidError(message string) *AppError {
	if message == "" {
		message = "vector store not found"
	}
	return &AppError{
		Code:     ErrVectorStoreBindingInvalid,
		Message:  message,
		HTTPCode: http.StatusBadRequest,
	}
}

// NewVectorStoreUnavailableError signals that the requested vector store
// row exists in the database but is not currently wired into the in-memory
// engine registry (factory failure on CreateStore, dynamic config error,
// or stale state after a connection-config rotation).
func NewVectorStoreUnavailableError(message string) *AppError {
	if message == "" {
		message = "vector store is currently unavailable"
	}
	return &AppError{
		Code:     ErrVectorStoreUnavailable,
		Message:  message,
		HTTPCode: http.StatusBadRequest,
	}
}

// IsAppError checks if the error is an AppError type
func IsAppError(err error) (*AppError, bool) {
	appErr, ok := err.(*AppError)
	return appErr, ok
}
