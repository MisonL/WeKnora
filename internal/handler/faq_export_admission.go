package handler

import (
	"sync/atomic"

	"github.com/Tencent/WeKnora/internal/errors"
)

// FAQ exports materialise a whole knowledge base and its serialized form in
// memory. That ceiling is the KB's own size, which the product defines, but the
// number of copies is not: several exports of several large KBs running at once
// stack them. One at a time per process keeps the peak at one KB, and a caller
// told to retry later is better served than one whose download dies with the
// process.
var faqExportInflight atomic.Int64

const faqExportMaxInflight = 1

// admitFAQExport takes the export slot, or refuses with a retryable error.
// The slot is per process and shared by every knowledge base, so the message
// does not name the caller's KB as the one exporting.
func admitFAQExport() (*errors.AppError, func()) {
	if faqExportInflight.Add(1) > faqExportMaxInflight {
		faqExportInflight.Add(-1)
		return errors.NewTooManyRequestsError("服务器正在导出其他内容，请稍后重试").WithRetryAfter(5), nil
	}
	return nil, func() { faqExportInflight.Add(-1) }
}
