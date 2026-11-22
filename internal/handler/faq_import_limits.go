package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// errNoEntriesField reports a top-level object without any entries field. It is
// a well-formed payload that simply carries nothing to import.
var errNoEntriesField = errors.New("payload has no entries field")

// countJSONArrayEntries counts the elements of the top-level array stored under
// key, refusing as soon as any occurrence passes limit.
//
// It exists so an over-limit batch can be refused before the payload is
// decoded. Decoding allocates one struct per entry, and the byte cap alone
// cannot bound that: a body of `{"entries":[{},{},{}...]}` spends almost every
// byte on an empty object, so a 32 MiB body reaches millions of entries and
// gigabytes of heap. Counting first costs one pass over bytes already in
// memory.
//
// Two details keep the count faithful to what encoding/json will actually do,
// because a count that disagrees with the decoder is a bypass:
//
//   - Field matching is case-insensitive, exactly like the decoder's, so
//     "ENTRIES" cannot slip past this scan and get decoded later.
//   - Duplicate keys are legal JSON and the decoder lets the last one win. A
//     scan that stopped at the first match would let `{"entries":[small],
//     "entries":[huge]}` pass the check and then allocate the huge array. So
//     every occurrence is counted and any occurrence over the limit refuses the
//     body outright — a payload carrying an over-limit array is over-limit
//     regardless of which duplicate shadows it.
//
// Element values are discarded rather than decoded, so the live footprint is
// one element copy at a time plus the decoder's internal buffer. That buffer
// keeps the capacity it grew to for the largest field seen, so the footprint is
// O(largest single value), not a fixed number of bytes.
//
// An error here is a real parse problem and the caller must not fall back to an
// unbounded decode: the whole point is to bound the work before it happens.
func countJSONArrayEntries(raw []byte, key string, limit int) (int, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))

	// Expect the top-level object.
	if tok, err := dec.Token(); err != nil {
		return 0, err
	} else if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return 0, fmt.Errorf("payload is not a JSON object")
	}

	last, seen := 0, false
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return 0, err
		}
		name, _ := tok.(string)
		if !strings.EqualFold(name, key) {
			// Skip the value of an unrelated field.
			var discard json.RawMessage
			if err := dec.Decode(&discard); err != nil {
				return 0, err
			}
			continue
		}

		value, err := dec.Token()
		if err != nil {
			return 0, err
		}
		switch v := value.(type) {
		case json.Delim:
			if v != '[' {
				return 0, fmt.Errorf("field %q is not an array", key)
			}
		case nil:
			// JSON null decodes into a nil slice; nothing to count.
			last, seen = 0, true
			continue
		default:
			return 0, fmt.Errorf("field %q is not an array", key)
		}

		count := 0
		for dec.More() {
			var discard json.RawMessage
			if err := dec.Decode(&discard); err != nil {
				return 0, err
			}
			count++
			if count > limit {
				// Stop at the first over-limit occurrence: no later duplicate
				// makes the body legal to decode cheaply.
				return count, nil
			}
		}
		// Consume the closing ']'.
		if _, err := dec.Token(); err != nil {
			return 0, err
		}
		last, seen = count, true
	}

	// The top-level object has to close.
	if _, err := dec.Token(); err != nil {
		return 0, err
	}
	if !seen {
		return 0, errNoEntriesField
	}
	return last, nil
}

// faqUpsertMode returns the mode the decoder would use, or "" when the body
// does not establish it.
//
// Used only to word an over-budget message. An oversized body was never read in
// full, so this reports a mode ONLY when the scan reached the end of the
// document: a prefix that happens to say "append" tells us nothing about a
// later `"mode":"replace"` that the size cap cut off, and advising a caller to
// split on the strength of a prefix would delete their FAQ list. A repeated key
// keeps the LAST value, exactly as the decoder does -- when the scan stops
// early the answer is unknown, not the last value seen.
//
// A JSON null for mode counts as "not established". That is deliberately more
// cautious than the decoder, which ignores a null written into a plain string
// field and keeps whatever earlier value it had -- imitating that here would
// mean reporting a mode for a field the body has since overwritten.
func faqUpsertMode(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil {
		return ""
	} else if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return ""
	}
	var values []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		name, _ := tok.(string)
		if !strings.EqualFold(name, "mode") {
			var discard json.RawMessage
			if err := dec.Decode(&discard); err != nil {
				return ""
			}
			continue
		}
		value, err := dec.Token()
		if err != nil {
			return ""
		}
		switch v := value.(type) {
		case nil:
			values = append(values, "")
		case string:
			values = append(values, v)
		default:
			// A non-string mode fails decoding later; nothing to word.
			return ""
		}
	}
	// The object must close: anything short of that is a truncation.
	if _, err := dec.Token(); err != nil {
		return ""
	}
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

// faqImportTooLargeBodyMessage explains a body over the byte budget.
func faqImportTooLargeBodyMessage(maxBytes int64, mode string) string {
	if mode == types.FAQBatchModeReplace {
		return fmt.Sprintf("请求体超过 %d 字节上限。%s，请调高 %s 后一次提交完整集合"+
			"（append 不能顶替 replace：它只追加不删除，旧条目会与新条目并存）；"+
			"如果确实要先清空再分批 append，请先导出备份，因为清空会立即删除全部旧条目。",
			maxBytes, secutils.FAQSplitAdvice(mode), "WEKNORA_FAQ_IMPORT_MAX_JSON_BYTES")
	}
	return fmt.Sprintf("请求体超过 %d 字节上限，%s", maxBytes, secutils.FAQSplitAdvice(mode))
}

// faqImportTooManyEntriesMessage explains an over-limit batch. The wording is
// shared with the service layer, which refuses the same request.
func faqImportTooManyEntriesMessage(maxEntries, got int, mode string) string {
	// The pre-decode scan stops one past the cap, so its count is a floor.
	return secutils.FAQTooManyEntriesMessage(maxEntries, got, mode, false)
}
