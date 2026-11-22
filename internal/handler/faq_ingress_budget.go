package handler

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Tencent/WeKnora/internal/errors"
)

// The per-request caps bound what one import may carry; they say nothing about
// how many arrive at once. The host runs a single import worker with a shared
// embedding slot, so a handful of concurrent 32 MiB submissions can hold more
// memory than the process has while the queue behind them grows. This budget is
// the admission control in front of that: a process-wide limit on imports in
// flight and on the bytes they hold.
//
// It is deliberately a refusal rather than a queue. The import is asynchronous
// already, and a caller that is told "busy" can retry; a caller parked in a
// handler ties up its connection and the body it is holding.
//
// Environment:
//
//	WEKNORA_FAQ_IMPORT_MAX_INFLIGHT         concurrent import requests (default 4)
//	WEKNORA_FAQ_IMPORT_MAX_INFLIGHT_BYTES   total body bytes held (default 96 MiB)
const (
	EnvFAQImportMaxInflight      = "WEKNORA_FAQ_IMPORT_MAX_INFLIGHT"
	EnvFAQImportMaxInflightBytes = "WEKNORA_FAQ_IMPORT_MAX_INFLIGHT_BYTES"

	defaultFAQImportMaxInflight      = 4
	defaultFAQImportMaxInflightBytes = 96 << 20
)

var (
	faqImportInflight      atomic.Int64
	faqImportInflightBytes atomic.Int64

	// faqImportBudgetMu serialises the check-and-take so the two totals cannot
	// both pass a limit that their sum exceeds.
	faqImportBudgetMu sync.Mutex
)

func faqImportMaxInflight() int {
	v := strings.TrimSpace(os.Getenv(EnvFAQImportMaxInflight))
	if v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultFAQImportMaxInflight
}

func faqImportMaxInflightBytes() int64 {
	v := strings.TrimSpace(os.Getenv(EnvFAQImportMaxInflightBytes))
	if v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultFAQImportMaxInflightBytes
}

// faqImportAdmission is a taken slot. release is idempotent so a handler can
// defer it and still release early on a rejection path.
type faqImportAdmission struct {
	bytes   int64
	release func()
}

// admitFAQImport takes a slot for one request before its body is read. The
// byte figure is the request's own cap rather than its length: the body has not
// been read yet, and reserving the cap is what guarantees the total held can
// never exceed the budget.
func admitFAQImport(maxBytes int64) (*faqImportAdmission, error) {
	maxInflight := int64(faqImportMaxInflight())
	maxBytesTotal := faqImportMaxInflightBytes()

	faqImportBudgetMu.Lock()
	inflight := faqImportInflight.Load()
	held := faqImportInflightBytes.Load()
	if inflight >= maxInflight || held+maxBytes > maxBytesTotal {
		faqImportBudgetMu.Unlock()
		log.Printf("[faq-import] refused by the in-flight budget: inflight=%d/%d bytes=%d+%d/%d",
			inflight, maxInflight, held, maxBytes, maxBytesTotal)
		return nil, errors.NewTooManyRequestsError(
			"当前导入请求过多，请稍后重试").WithRetryAfter(3)
	}
	faqImportInflight.Add(1)
	faqImportInflightBytes.Add(maxBytes)
	faqImportBudgetMu.Unlock()

	var once sync.Once
	return &faqImportAdmission{
		bytes: maxBytes,
		release: func() {
			once.Do(func() {
				faqImportBudgetMu.Lock()
				faqImportInflight.Add(-1)
				faqImportInflightBytes.Add(-maxBytes)
				faqImportBudgetMu.Unlock()
			})
		},
	}, nil
}
