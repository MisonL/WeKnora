package utils

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The two entry points know different things: the scan knows a floor, the
// service knows the count. The message must say which, rather than reading the
// number and guessing -- an exact 5 reported as "at least 5" understates the
// caller's own request, and a floor of 4 reported as "4" invents one.
func TestFAQTooManyEntriesMessageStatesWhetherTheCountIsExact(t *testing.T) {
	require.Contains(t, FAQTooManyEntriesMessage(3, 4, FAQBatchModeAppend, false), "当前至少 4 条")
	require.Contains(t, FAQTooManyEntriesMessage(3, 5, FAQBatchModeAppend, true), "当前 5 条")
	require.NotContains(t, FAQTooManyEntriesMessage(3, 5, FAQBatchModeAppend, true), "至少")
	for _, exact := range []bool{false, true} {
		require.Contains(t, FAQTooManyEntriesMessage(3, 4, FAQBatchModeReplace, exact), "拆小请求会把批外条目标记为待删除")
		require.NotContains(t, FAQTooManyEntriesMessage(3, 4, FAQBatchModeReplace, exact), "请拆成几批提交")
	}
}

// The negative-question cap was defined but never checked, and negative
// questions were left out of the text budget, so one entry could carry an
// unbounded number of them.
func TestCheckFAQEntryShapeBoundsNegativeQuestions(t *testing.T) {
	ok := make([]string, FAQEntryMaxNegativeQuestions)
	for i := range ok {
		ok[i] = "n"
	}
	require.NoError(t, CheckFAQEntryShape("q", nil, ok, []string{"a"}))

	tooMany := append(append([]string{}, ok...), "one more")
	err := CheckFAQEntryShape("q", nil, tooMany, []string{"a"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "反例问")

	// Their text counts toward the entry's byte budget like everything else.
	big := strings.Repeat("n", FAQEntryMaxTextBytes)
	err = CheckFAQEntryShape("q", nil, []string{big}, []string{"a"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "文本合计")
}
