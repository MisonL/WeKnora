package handler

import (
	"log"
	"sync"
	"sync/atomic"

	"github.com/gin-gonic/gin"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// Upload admission.
//
// Before this the only bound on a document upload was MAX_FILE_SIZE_MB per
// request. Fifty concurrent 50 MB uploads therefore meant 2.5 GB of multipart
// spill files on the system disk plus fifty parse jobs behind them, which is
// what took the host down on 2026-11-13.
//
// This file admits the HTTP side: a slot and a byte reservation held only while
// the body is being read and the knowledge row created, released when the
// request returns. The parse job that the upload leaves behind is budgeted
// separately, in the service package (ReserveUploadWork), because it outlives
// the request and the worker may run in another process.
//
// Only "big" uploads are limited to one at a time: a large body is what costs
// the disk, while two small uploads are harmless.
const (
	// UploadMaxConcurrencyEnv is the number of document uploads admitted at once.
	UploadMaxConcurrencyEnv = "UPLOAD_MAX_CONCURRENCY"
	// UploadBigBytesEnv is the size at or above which an upload counts as big.
	UploadBigBytesEnv = "UPLOAD_BIG_BYTES"
	// UploadMaxInFlightBytesEnv caps the declared bytes of in-flight uploads.
	UploadMaxInFlightBytesEnv = "UPLOAD_MAX_INFLIGHT_BYTES"
)

// Defaults match the documented .env.example values.
const (
	defaultUploadMaxConcurrency   = 2
	defaultUploadBigBytes         = int64(32 << 20)
	defaultUploadMaxInFlightBytes = int64(128 << 20)
)

func uploadMaxConcurrency() int { return secutils.GetUploadMaxConcurrency() }
func uploadBigBytes() int64     { return secutils.GetUploadBigBytes() }
func uploadMaxInFlightBytes() int64 {
	return secutils.GetUploadMaxInFlightBytes()
}

var (
	uploadInflight      atomic.Int64
	uploadInflightBytes atomic.Int64
	uploadBigInflight   atomic.Int64

	uploadBudgetMu sync.Mutex
)

// errUploadBusy is the admission refusal. The hint is a couple of seconds: an
// upload slot is held for one body transfer, so the wait for the next free
// slot is short relative to the retry's own backoff.
var errUploadBusy = apperrors.NewTooManyRequestsError(
	"服务器正在处理其他上传，请稍后重试").
	WithDetails("upload admission is full; retry after a short delay").
	WithRetryAfter(3)

// uploadSlot is one admitted upload. release must be called exactly once per
// admitted upload. It is idempotent, so a handler can defer it and still release
// on an early return.
type uploadSlot struct {
	bytes int64
	big   bool
	once  sync.Once
}

func (s *uploadSlot) release() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		uploadBudgetMu.Lock()
		uploadInflight.Add(-1)
		uploadInflightBytes.Add(-s.bytes)
		if s.big {
			uploadBigInflight.Add(-1)
		}
		uploadBudgetMu.Unlock()
	})
}

// admitUpload takes an upload slot for a body of the declared size.
//
// The check and the accounting happen under one lock: two admissions that each
// fit on their own can still exceed the byte budget together, and reading the
// totals outside the lock is how that slips through.
func admitUpload(declaredBytes int64) (*uploadSlot, error) {
	if declaredBytes < 0 {
		declaredBytes = 0
	}
	maxInflight := int64(uploadMaxConcurrency())
	big := declaredBytes >= uploadBigBytes()

	uploadBudgetMu.Lock()
	defer uploadBudgetMu.Unlock()
	inflight, bytes, bigInflight := uploadInflight.Load(), uploadInflightBytes.Load(), uploadBigInflight.Load()
	if inflight >= maxInflight ||
		bytes+declaredBytes > uploadMaxInFlightBytes() ||
		(big && bigInflight >= 1) {
		log.Printf("[upload] refused by admission: inflight=%d/%d bytes=%d+%d big=%d",
			inflight, maxInflight, bytes, declaredBytes, bigInflight)
		return nil, errUploadBusy
	}
	uploadInflight.Add(1)
	uploadInflightBytes.Add(declaredBytes)
	if big {
		uploadBigInflight.Add(1)
	}
	return &uploadSlot{bytes: declaredBytes, big: big}, nil
}

// resetUploadAdmission clears every counter. Tests use it; production never
// calls it.
func resetUploadAdmission() {
	uploadBudgetMu.Lock()
	defer uploadBudgetMu.Unlock()
	uploadInflight.Store(0)
	uploadInflightBytes.Store(0)
	uploadBigInflight.Store(0)
}

// uploadDeclaredBytes estimates the request size before anything is read.
//
// Content-Length is the only figure available before the multipart parse, and
// the decision has to be made on it: waiting for the parsed size would mean
// holding the body before deciding whether to hold it. A chunked request has no
// length and is admitted as if it were at the cap -- the conservative side,
// since it costs disk like a big upload would.
func uploadDeclaredBytes(c *gin.Context, maxFileBytes int64) int64 {
	n := c.Request.ContentLength
	if n <= 0 {
		return maxFileBytes
	}
	// The body is the file plus its multipart framing; the ceiling
	// limitUploadBody applies is the file cap plus exactly that framing. Work
	// from the file's own size so a legal maximum-size upload is counted and
	// judged as what it is, not as slightly larger than the biggest allowed.
	fileBytes := n - uploadEnvelopeSlack
	if fileBytes > maxFileBytes {
		fileBytes = maxFileBytes
	}
	if fileBytes < 0 {
		return 0
	}
	return fileBytes
}
