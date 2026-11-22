package handler

import (
	stderrors "errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/utils"
)

// faqEntriesBody builds an upsert payload carrying n minimal entries.
func faqEntriesBody(n int) string {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = fmt.Sprintf(`{"standard_question":"q%d"}`, i)
	}
	return `{"mode":"append","entries":[` + strings.Join(entries, ",") + `]}`
}

// The entry count is the bound a caller cannot raise on its own: without it one
// request submits an arbitrarily long array whose import plan, validation maps
// and progress object all scale with the count.
func TestUpsertFAQsRejectsMoreEntriesThanTheLimit(t *testing.T) {
	t.Setenv(utils.EnvFAQImportMaxEntries, "3")
	svc := &fakeFAQUpsertService{}

	c, _ := upsertFAQProbe(t, faqEntriesBody(4), svc)

	require.NotEmpty(t, c.Errors, "4 entries should be refused under a limit of 3")
	appErr, ok := c.Errors[0].Err.(*apperrors.AppError)
	require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
	require.Contains(t, appErr.Error(), "3")
	require.Zero(t, svc.calls, "the service must not be reached for a rejected batch")
}

// A batch exactly at the limit is legal; the cap must not off-by-one away a
// legitimate submission.
func TestUpsertFAQsAcceptsEntriesAtTheLimit(t *testing.T) {
	t.Setenv(utils.EnvFAQImportMaxEntries, "3")
	svc := &fakeFAQUpsertService{}

	c, _ := upsertFAQProbe(t, faqEntriesBody(3), svc)

	require.Empty(t, c.Errors)
	require.Equal(t, 1, svc.calls)
}

// Reading the body before decoding means the `binding` tags on the payload no
// longer run on their own: the decoder ignores them. Without an explicit
// validation pass, a bogus mode or an empty entries array would reach the
// service and be reported as a business error rather than a malformed request.
func TestUpsertFAQsStillValidatesThePayloadItDecoded(t *testing.T) {
	t.Setenv(utils.EnvFAQImportMaxEntries, "10")
	for name, body := range map[string]string{
		"bogus mode":   `{"mode":"bogus","entries":[{"standard_question":"q"}]}`,
		"empty array":  `{"mode":"append","entries":[]}`,
		"missing mode": `{"entries":[{"standard_question":"q"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			svc := &fakeFAQUpsertService{}
			c, _ := upsertFAQProbe(t, body, svc)

			require.NotEmpty(t, c.Errors, "the payload should be refused")
			appErr, ok := c.Errors[0].Err.(*apperrors.AppError)
			require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
			// Validator failures name the failed field or tag; the empty-array
			// check is our own. Both are refusals about the request itself.
			details := fmt.Sprint(appErr.Details)
			require.True(t,
				strings.Contains(details, "validation") || strings.Contains(details, "must not be empty"),
				"the refusal must be about the request, got %q", details)
			require.Zero(t, svc.calls, "a malformed payload must not reach the service")
		})
	}
}

// The env var is an operator override, so a malformed or non-positive value
// must fall back to the default rather than disabling the cap.
func TestFAQImportMaxEntriesFallsBackOnBadValues(t *testing.T) {
	cases := map[string]int{
		"":         1000,
		"   ":      1000,
		"0":        1000,
		"-5":       1000,
		"notanint": 1000,
		"250":      250,
		" 250 ":    250,
	}
	for value, want := range cases {
		t.Run(fmt.Sprintf("value=%q", value), func(t *testing.T) {
			t.Setenv(utils.EnvFAQImportMaxEntries, value)
			require.Equal(t, want, faqImportMaxEntries())
		})
	}
}

// The byte cap backs the entry cap: a body far under the file-size limit can
// still be rejected on its own budget, and an explicit override must win.
func TestFAQJSONMaxBytesHonoursItsOwnBudget(t *testing.T) {
	t.Setenv(utils.EnvFAQImportMaxJSONBytes, "")
	// The default budget is min(32 MiB, MAX_FILE_SIZE_MB), so a machine that
	// sets that variable would otherwise see its own value here.
	t.Setenv("MAX_FILE_SIZE_MB", "")
	require.Equal(t, int64(32<<20), faqJSONMaxBytes(), "default budget")

	t.Setenv(utils.EnvFAQImportMaxJSONBytes, "4194304")
	require.Equal(t, int64(4<<20), faqJSONMaxBytes(), "explicit override")

	t.Setenv(utils.EnvFAQImportMaxJSONBytes, "-1")
	require.Equal(t, int64(32<<20), faqJSONMaxBytes(), "a non-positive override keeps the default")
}

// Raising the file limit must not raise the FAQ budget past its default.
func TestFAQJSONMaxBytesFollowsALoweredFileSizeLimit(t *testing.T) {
	t.Setenv(utils.EnvFAQImportMaxJSONBytes, "")
	t.Setenv("MAX_FILE_SIZE_MB", "8")
	require.Equal(t, utils.GetMaxFileSize(), faqJSONMaxBytes())

	t.Setenv("MAX_FILE_SIZE_MB", "200")
	require.Equal(t, int64(32<<20), faqJSONMaxBytes())
}

// An explicit override may lower the budget but must not lift it above the
// deployment's own upload limit. Widening this knob past MAX_FILE_SIZE_MB is
// far more likely to be a mistake than an intent to accept a body larger than
// the upload ceiling.
func TestFAQJSONMaxBytesClampsAnExplicitOverrideToTheFileLimit(t *testing.T) {
	t.Setenv("MAX_FILE_SIZE_MB", "8")
	t.Setenv(utils.EnvFAQImportMaxJSONBytes, "16777216") // 16 MiB, above the 8 MiB file limit
	require.Equal(t, utils.GetMaxFileSize(), faqJSONMaxBytes())

	// Below the file limit the override still wins.
	t.Setenv(utils.EnvFAQImportMaxJSONBytes, "4194304") // 4 MiB
	require.Equal(t, int64(4<<20), faqJSONMaxBytes())
}

// The count check must fire before the entries array is materialized.
//
// A body of empty objects spends almost every byte on an entry that carries no
// content, so the byte cap alone leaves the decoded heap unbounded in practice:
// 32 MiB of `{}` is millions of structs. This body is far under the byte cap
// and still over the entry limit, which is exactly the shape that would slip
// past a byte-only defence.
func TestUpsertFAQsRejectsTooManyEntriesBeforeDecoding(t *testing.T) {
	t.Setenv(utils.EnvFAQImportMaxEntries, "50")
	svc := &fakeFAQUpsertService{}

	body := `{"mode":"append","entries":[` + strings.Repeat(`{},`, 60) + `{}]}`
	require.Less(t, len(body), 1024, "the fixture must stay well under any byte cap")

	c, _ := upsertFAQProbe(t, body, svc)

	require.NotEmpty(t, c.Errors)
	appErr, ok := c.Errors[0].Err.(*apperrors.AppError)
	require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
	require.Equal(t, apperrors.ErrBadRequest, appErr.Code)
	// The count is known from the pre-scan, so the message can name it.
	require.Contains(t, appErr.Message, "当前至少 51 条")
	require.Zero(t, svc.calls)
}

// A replace batch must not be told to "split into batches": replace treats
// entries missing from the batch as deletions, so obeying that advice would
// delete everything outside the current batch.
func TestUpsertFAQsReplaceOverLimitDoesNotAdviseBatching(t *testing.T) {
	t.Setenv(utils.EnvFAQImportMaxEntries, "3")
	svc := &fakeFAQUpsertService{}

	body := `{"mode":"replace","entries":[` + strings.Repeat(`{},`, 4) + `{}]}`
	c, _ := upsertFAQProbe(t, body, svc)

	require.NotEmpty(t, c.Errors)
	appErr, ok := c.Errors[0].Err.(*apperrors.AppError)
	require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
	require.Contains(t, appErr.Message, "replace")
	require.Contains(t, appErr.Message, "拆小请求会把批外条目标记为待删除")
	require.Zero(t, svc.calls)
}

// Counting must not reject payloads it cannot parse: a malformed body has to
// reach the real decoder so the caller sees the parse error.
func TestCountJSONArrayEntriesDefersMalformedPayloads(t *testing.T) {
	cases := []string{
		`{"entries":`,
		`{"entries":{"a":1}}`,
		`[]`,
		`{"entries":[{}]`, // top-level object never closes
	}
	for _, body := range cases {
		if _, err := countJSONArrayEntries([]byte(body), "entries", 10); err == nil {
			t.Fatalf("body %q should not be counted", body)
		}
	}

	// A well-formed payload with no entries field reports that specifically, so
	// the caller can let the payload's own validation answer instead.
	if _, err := countJSONArrayEntries([]byte(`{"other":1}`), "entries", 10); !stderrors.Is(err, errNoEntriesField) {
		t.Fatalf("missing entries fields should report errNoEntriesField, got %v", err)
	}

	// A well-formed payload counts, and stops early rather than walking the
	// whole array.
	count, err := countJSONArrayEntries([]byte(`{"a":"b","entries":[{},{},{}]}`), "entries", 10)
	require.NoError(t, err)
	require.Equal(t, 3, count)

	count, err = countJSONArrayEntries([]byte(`{"entries":[`+strings.Repeat(`{},`, 99)+`{}]}`), "entries", 10)
	require.NoError(t, err)
	require.Equal(t, 11, count, "the scan should stop one past the limit")

	// JSON null decodes to a nil slice; there is nothing to count.
	count, err = countJSONArrayEntries([]byte(`{"entries":null}`), "entries", 10)
	require.NoError(t, err)
	require.Zero(t, count)
}

// The scan has to agree with encoding/json, or it is a bypass rather than a
// bound.
func TestCountJSONArrayEntriesMatchesTheDecoder(t *testing.T) {
	// Field matching is case-insensitive in encoding/json, so an uppercase key
	// must be counted here too.
	count, err := countJSONArrayEntries([]byte(`{"ENTRIES":[{},{},{}]}`), "entries", 10)
	require.NoError(t, err)
	require.Equal(t, 3, count, "uppercase field name must be counted like the decoder counts it")

	// Duplicate keys are legal and the decoder keeps the last one. A small
	// array followed by a large one must still be refused.
	count, err = countJSONArrayEntries([]byte(`{"entries":[{}],"entries":[`+strings.Repeat(`{},`, 20)+`{}]}`), "entries", 10)
	require.NoError(t, err)
	require.Equal(t, 11, count, "an over-limit duplicate must not be shadowed by a small first occurrence")

	// And the reverse shape — a large array shadowed by a small one — is
	// refused too: a body that carries an over-limit array is over-limit.
	count, err = countJSONArrayEntries([]byte(`{"entries":[`+strings.Repeat(`{},`, 20)+`{}],"entries":[{}]}`), "entries", 10)
	require.NoError(t, err)
	require.Equal(t, 11, count)
}

// A body the scan refuses must never reach the decoder, which is where the
// unbounded allocation would happen.
func TestUpsertFAQsRefusesDuplicateKeyBypass(t *testing.T) {
	t.Setenv(utils.EnvFAQImportMaxEntries, "10")
	svc := &fakeFAQUpsertService{}

	// 25 entries hidden behind a one-entry duplicate. Without duplicate-aware
	// counting this passes the limit check and decodes the large array.
	body := `{"mode":"append","entries":[{}],"entries":[` + strings.Repeat(`{},`, 24) + `{}]}`

	c, _ := upsertFAQProbe(t, body, svc)

	require.NotEmpty(t, c.Errors)
	require.Zero(t, svc.calls)

	// "There is an error" is not enough on its own: the post-decode length check
	// rejects the same body, so this assertion would stay green even if the
	// pre-scan ran after the decode. The pre-scan is the only path that knows
	// the count without materializing the array, so the message has to come
	// from it — it names the shadowed occurrence's size, which the decoded
	// length (1) never could.
	appErr, ok := c.Errors[0].Err.(*apperrors.AppError)
	require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
	require.Contains(t, appErr.Message, "当前至少 11 条",
		"the refusal must come from the pre-scan, not from the post-decode length check")
}

// The pre-scan must also run before the decode in the ordinary case: a body
// well over the limit is refused without the array ever being materialized.
// The count in the message proves the scan produced it.
func TestUpsertFAQsCountsBeforeDecoding(t *testing.T) {
	t.Setenv(utils.EnvFAQImportMaxEntries, "5")
	svc := &fakeFAQUpsertService{}

	body := `{"entries":[` + strings.Repeat(`{},`, 29) + `{}]}`
	c, _ := upsertFAQProbe(t, body, svc)

	require.NotEmpty(t, c.Errors)
	require.Zero(t, svc.calls)
	appErr, ok := c.Errors[0].Err.(*apperrors.AppError)
	require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
	require.Contains(t, appErr.Message, "当前至少 6 条",
		"the scan stops one past the limit; a decoded count would read 30")
}

// The scan has to run before the decode, and this body makes the two orders
// distinguishable: one entry carries a non-string standard_question, so the
// decoder fails on it. If the scan runs first it refuses on the count — the
// message names the count, and the decoder never sees the bad field. If the
// decode runs first the caller gets a type error instead, which is also a 400,
// so only the message tells the two apart.
func TestUpsertFAQsCountsBeforeDecodingBadField(t *testing.T) {
	t.Setenv(utils.EnvFAQImportMaxEntries, "2")
	svc := &fakeFAQUpsertService{}
	c, _ := upsertFAQProbe(t, `{"entries":[{"standard_question":123},{},{}]}`, svc)

	require.NotEmpty(t, c.Errors)
	require.Zero(t, svc.calls)
	appErr, ok := c.Errors[0].Err.(*apperrors.AppError)
	require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
	require.Contains(t, appErr.Message, "当前至少 3 条",
		"a decode-first order would report the invalid field instead of the count")
}

// A scan error must refuse the request rather than fall through to the
// decoder: the scan is what bounds the allocation, and the benign
// no-entries-field case is the only shape allowed past it. `entries` as an
// object (not an array) is such an error, and its message names the field, so
// it can only have come from the scan.
func TestUpsertFAQsRefusesWhenEntriesCannotBeCounted(t *testing.T) {
	svc := &fakeFAQUpsertService{}
	c, _ := upsertFAQProbe(t, `{"entries":{"standard_question":"q"}}`, svc)

	require.NotEmpty(t, c.Errors)
	require.Zero(t, svc.calls)
	appErr, ok := c.Errors[0].Err.(*apperrors.AppError)
	require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
	details, _ := appErr.Details.(string)
	// Both paths mention the field, so "entries" alone is green either way:
	// the decoder says `cannot unmarshal object into Go struct field
	// FAQBatchUpsertPayload.entries`. Only the scan words it this way.
	require.Contains(t, details, `field "entries" is not an array`,
		"the refusal must come from the scan; the decoder reports a struct type error instead")
}

// faqUpsertMode only answers for a document it read to the end. A prefix that
// happens to contain a mode is not an answer: the writer may repeat the key
// later, and the decoder would take that later value. An unclosed object is
// exactly that hazard, so it reports "unknown" rather than the last value it
// saw.
func TestFAQImportModeRequiresACompleteDocument(t *testing.T) {
	for name, body := range map[string]string{
		"unclosed after the mode": `{"mode":"append"`,
		"unclosed mid-value":      `{"mode":"append","entries":[{"standard_question":"q"`,
		"not an object":           `["append"]`,
		"empty":                   ``,
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, "", faqUpsertMode([]byte(body)))
		})
	}
	for name, tc := range map[string]struct{ body, want string }{
		"single mode":          {`{"mode":"replace","entries":[]}`, "replace"},
		"mode after the array": {`{"entries":[],"mode":"replace"}`, "replace"},
		"duplicate keeps last": {`{"mode":"append","x":1,"mode":"replace"}`, "replace"},
		"case-insensitive key": {`{"MODE":"append"}`, "append"},
		"null mode":            {`{"mode":null,"x":1}`, ""},
		// The decoder would keep the earlier "append" here; this reports
		// "unknown" on purpose, because a mode field the body overwrites with
		// null is not a statement this can rely on when one more duplicate may
		// sit past a truncation.
		"append then null": {`{"mode":"append","mode":null}`, ""},
		"non-string mode":  {`{"mode":1}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, faqUpsertMode([]byte(tc.body)))
		})
	}
}

// The refusal wording is shared with the service layer, so the handler must not
// drift from it: a caller who hits the cap over HTTP and one who reaches the
// service through another path have to read the same instruction. Pinning the
// exact sentence here catches a handler-specific edit that quietly drops the
// replace warning.
func TestUpsertFAQsOverLimitUsesTheSharedWording(t *testing.T) {
	t.Setenv(utils.EnvFAQImportMaxEntries, "3")
	for _, mode := range []string{"append", "replace"} {
		t.Run(mode, func(t *testing.T) {
			svc := &fakeFAQUpsertService{}
			items := make([]string, 5) // more than the cap of 3
			for i := range items {
				items[i] = `{}`
			}
			body := `{"mode":"` + mode + `","entries":[` + strings.Join(items, ",") + `]}`
			c, _ := upsertFAQProbe(t, body, svc)

			require.NotEmpty(t, c.Errors)
			appErr, ok := c.Errors[0].Err.(*apperrors.AppError)
			require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
			// The scan stops one past the cap, so the count it passes on is a
			// floor and the message has to say so rather than claim 5.
			require.Equal(t, utils.FAQTooManyEntriesMessage(3, 4, mode, false), appErr.Message)
			require.Contains(t, appErr.Message, "当前至少 4 条")
			require.Zero(t, svc.calls)
		})
	}
}

// The byte-budget refusal has to give the replace advice whenever the mode is
// established, and must never offer "split it up" for a replace request:
// splitting deletes every entry outside each batch.
//
// This is asserted on the wording itself rather than through a request: the
// mode is only read from a document that closes, and a body over the byte
// budget is by definition not read past the budget, so the handler reaches this
// message with "unknown" mode and the conservative advice -- which is safe, and
// which the next test pins.
func TestReplaceOverBytesGetsTheReplaceAdvice(t *testing.T) {
	msg := faqImportTooLargeBodyMessage(200, "replace")
	require.Contains(t, msg, "拆小请求会把批外条目标记为待删除")
	require.NotContains(t, msg, "请拆成几批提交", "a replace body must never be told to split")
	require.Contains(t, msg, utils.EnvFAQImportMaxJSONBytes)
}

// A body over the byte budget is refused before its mode can be read from a
// closed document, so the handler words that refusal for an unknown mode. That
// advice must be safe for replace too: it may not say to split, and it must
// point at the possibility that this is a replace.
func TestUpsertFAQsOverBytesUsesTheUnknownModeAdvice(t *testing.T) {
	t.Setenv(utils.EnvFAQImportMaxJSONBytes, "200")
	t.Setenv(utils.EnvFAQImportMaxEntries, "1000")
	svc := &fakeFAQUpsertService{}

	body := `{"mode":"replace","entries":[` + strings.Repeat(`{"standard_question":"question"},`, 20) + `{}]}`
	c, _ := upsertFAQProbe(t, body, svc)

	require.NotEmpty(t, c.Errors)
	appErr, ok := c.Errors[0].Err.(*apperrors.AppError)
	require.True(t, ok, "expected an AppError, got %T", c.Errors[0].Err)
	require.Equal(t, faqImportTooLargeBodyMessage(200, ""), appErr.Message)
	require.NotContains(t, appErr.Message, "请拆成几批提交", "no caller may be told to split without knowing the mode")
	require.Contains(t, appErr.Message, "如果这是 replace 请求")
	require.Zero(t, svc.calls)
}
