package utils

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// FAQ import admission budgets.
//
// A FAQ batch upsert (`POST /knowledge-bases/:id/faq/entries`) carries an
// `entries` array whose length the request type does not bound, so one request
// can drive work proportional to however many entries it sends: the import
// plan, the validation index, the per-entry index writes and the progress
// object all scale with the count, and none of them shrink at the 50-row write
// batch. Two budgets bound that, and they protect different layers — bytes
// bound what the HTTP layer allocates before it parses anything, entries bound
// what the asynchronous import then does.
//
// These live here rather than in the handler because the bound has to hold on
// every path into the import, not just the HTTP one.

const (
	// EnvFAQImportMaxEntries and EnvFAQImportMaxJSONBytes are exported so the
	// handler and service tests name the same knobs the code reads.
	EnvFAQImportMaxEntries   = "WEKNORA_FAQ_IMPORT_MAX_ENTRIES"
	EnvFAQImportMaxJSONBytes = "WEKNORA_FAQ_IMPORT_MAX_JSON_BYTES"

	defaultFAQImportMaxEntries   = 1000
	defaultFAQImportMaxJSONBytes = 32 << 20
)

// GetFAQImportMaxEntries bounds how many entries one import may carry.
//
// This is the bound a caller cannot raise on its own, so it is applied where
// the payload enters the service as well as in the HTTP handler. The default
// sits above the batches the web UI and CLI send, so it does not trip on
// normal use.
func GetFAQImportMaxEntries() int {
	if v := strings.TrimSpace(os.Getenv(EnvFAQImportMaxEntries)); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			return n
		}
		// Falling back silently would leave an operator who mistyped the knob
		// with a limit they believe they changed and cannot see.
		log.Printf("[config] %s=%q is not a positive integer; using the default of %d",
			EnvFAQImportMaxEntries, v, defaultFAQImportMaxEntries)
	}
	return defaultFAQImportMaxEntries
}

// GetFAQImportMaxJSONBytes bounds the byte size of a FAQ batch upsert body.
//
// The cap never exceeds the configured file-upload limit: a body this endpoint
// accepts should not be larger than one the file endpoint would take. An
// explicit WEKNORA_FAQ_IMPORT_MAX_JSON_BYTES may only lower the budget, not
// raise it past that limit — an operator widening the batch budget is far more
// likely to have misread the knob than to intend a body larger than the
// deployment's own upload ceiling.
func GetFAQImportMaxJSONBytes() int64 {
	budget := int64(defaultFAQImportMaxJSONBytes)
	if v := strings.TrimSpace(os.Getenv(EnvFAQImportMaxJSONBytes)); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err == nil && n > 0 {
			budget = n
		} else {
			log.Printf("[config] %s=%q is not a positive integer; using the default of %d",
				EnvFAQImportMaxJSONBytes, v, defaultFAQImportMaxJSONBytes)
		}
	}
	if limit := GetMaxFileSize(); limit > 0 && limit < budget {
		return limit
	}
	return budget
}

// FAQBatchModeAppend and FAQBatchModeReplace mirror the payload modes so this
// package can word its advice without importing internal/types.
const (
	FAQBatchModeAppend  = "append"
	FAQBatchModeReplace = "replace"
)

// FAQSplitAdvice is the sentence that tells a caller what to do about an
// over-budget request.
//
// Only `append` can be split safely. Under `replace` a smaller request is a
// smaller set, and everything outside it is a deletion. When the mode is not
// established the advice has to say so rather than assume append: a caller who
// read "split it up" as licence to re-send a replace in pieces would delete
// their FAQ list.
func FAQSplitAdvice(mode string) string {
	switch mode {
	case FAQBatchModeReplace:
		return "replace 模式下拆小请求会把批外条目标记为待删除"
	case FAQBatchModeAppend:
		return "请拆成几批提交"
	default:
		return "只有 append 模式可以拆批提交；如果这是 replace 请求，请调高上限后一次提交完整集合"
	}
}

// FAQTooManyEntriesMessage words the refusal for a batch over the entry cap.
//
// It lives next to FAQSplitAdvice because both the HTTP layer and the service
// layer refuse the same request and must say the same thing: a caller who reads
// one wording from one entry point and a different one from another has no way
// to know which is true.
//
// exact says whether got is the true count. The HTTP layer stops counting at
// cap+1 so an oversized body is never walked to its end, and it passes that
// floor along; the service layer counts a decoded slice and knows the real
// number. Quoting a floor as the exact count would be a false statement about
// the caller's own request, and quoting an exact count as a floor would
// understate it -- so the caller states which one it has instead of the message
// guessing from the value.
//
// The advice differs by mode, and getting it wrong is destructive: `replace`
// treats entries missing from the batch as deletions, so a caller who split a
// replace request in batches would delete every entry outside each batch. It
// also cannot be swapped for append, which never deletes, so the old entries
// would survive next to the new ones.
func FAQTooManyEntriesMessage(maxEntries, got int, mode string, exact bool) string {
	count := fmt.Sprintf("当前 %d 条", got)
	if !exact {
		count = fmt.Sprintf("当前至少 %d 条", got)
	}
	if mode == FAQBatchModeReplace {
		return fmt.Sprintf(
			"单次最多导入 %d 条 FAQ，%s。%s。"+
				"请调高 %s 后一次提交完整集合（append 不能顶替 replace：它只追加不删除，旧条目会与新条目并存）；"+
				"如果确实要先清空再分批 append，请先导出备份，因为清空会立即删除全部旧条目。",
			maxEntries, count, FAQSplitAdvice(mode), EnvFAQImportMaxEntries)
	}
	return fmt.Sprintf("单次最多导入 %d 条 FAQ，%s，%s", maxEntries, count, FAQSplitAdvice(mode))
}

// FAQTooManyExclusionsMessage words the refusal for an over-limit exclude_ids
// list sent with by_tag.
func FAQTooManyExclusionsMessage(maxExclusions, got int) string {
	return fmt.Sprintf(
		"单次最多排除 %d 条 FAQ，当前 %d 条，且排除列表不能拆批、不能截短。"+
			"请改用 by_id 直接更新需要的条目：每批最多 %d 条，且同时不带 by_tag 和 exclude_ids",
		maxExclusions, got, maxExclusions)
}

// Per-entry shape limits.
//
// The batch caps bound how many entries a request carries; they say nothing
// about what one entry carries. That gap is reachable: under
// question_index_mode=separate with index_mode=question_answer the indexer
// copies every answer into an index item for each similar question, so one
// entry with many similar questions and a large answer expands by that factor
// before any storage check runs. A request well inside the byte budget can
// therefore ask for hundreds of megabytes of index text. These limits bound
// the factor itself, at the one point every entry passes through.
const (
	// FAQEntryMaxSimilarQuestions bounds the expansion factor for the common
	// case; hundreds of similar questions for one standard question is a
	// data-entry mistake, not a use case.
	FAQEntryMaxSimilarQuestions = 128
	// FAQEntryMaxNegativeQuestions bounds the counter-examples the same way.
	FAQEntryMaxNegativeQuestions = 128
	// FAQEntryMaxTextBytes bounds the text one entry may carry in total.
	FAQEntryMaxTextBytes = 64 * 1024
)

// FAQEntryTextBytes is the serialized size of an entry's own text, negative
// questions included: they are stored and served per entry like the rest.
func FAQEntryTextBytes(standard string, similar, negative, answers []string) int {
	n := len(standard)
	for _, q := range similar {
		n += len(q)
	}
	for _, q := range negative {
		n += len(q)
	}
	for _, a := range answers {
		n += len(a)
	}
	return n
}

// CheckFAQEntryShape refuses an entry that carries too much in its own fields,
// reporting why. It takes plain values because internal/types imports this
// package, so this one cannot import the type back.
//
// It is the one check every entry must pass, whether it arrives in a request or
// is produced by merging an import entry into an existing one: a merge unions
// the question lists, so the merged entry can exceed what either side carried.
func CheckFAQEntryShape(standard string, similar, negative, answers []string) error {
	if len(similar) > FAQEntryMaxSimilarQuestions {
		return fmt.Errorf("单条 FAQ 最多 %d 个相似问，当前 %d 个", FAQEntryMaxSimilarQuestions, len(similar))
	}
	if len(negative) > FAQEntryMaxNegativeQuestions {
		return fmt.Errorf("单条 FAQ 最多 %d 个反例问，当前 %d 个", FAQEntryMaxNegativeQuestions, len(negative))
	}
	if total := FAQEntryTextBytes(standard, similar, negative, answers); total > FAQEntryMaxTextBytes {
		return fmt.Errorf("单条 FAQ 的文本合计最多 %d 字节，当前 %d 字节", FAQEntryMaxTextBytes, total)
	}
	return nil
}

// FAQEntryShapeAdvice explains the per-entry limits in one sentence, for call
// sites that report rather than refuse.
func FAQEntryShapeAdvice() string {
	return fmt.Sprintf("单条 FAQ 最多 %d 个相似问、%d 个反例问、文本合计最多 %d 字节",
		FAQEntryMaxSimilarQuestions, FAQEntryMaxNegativeQuestions, FAQEntryMaxTextBytes)
}
