package service

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// One entry can carry far more than the batch caps allow: under
// question_index_mode=separate with index_mode=question_answer the indexer
// copies every answer once per similar question, so a single entry inside the
// byte budget can ask for hundreds of megabytes of index text. The shape check
// bounds that factor where every path sanitizes.
func TestSanitizeRefusesAnEntryWhoseIndexWouldExplode(t *testing.T) {
	bigAnswer := strings.Repeat("a", secutils.FAQEntryMaxTextBytes)
	entry := &types.FAQEntryPayload{
		StandardQuestion: "q",
		SimilarQuestions: makeSimilarQuestions(secutils.FAQEntryMaxSimilarQuestions + 1),
		Answers:          []string{bigAnswer},
	}
	_, err := sanitizeFAQEntryPayload(entry)
	require.Error(t, err, "an entry past the similar-question bound must be refused")
	require.Contains(t, err.Error(), "相似问")
}

func TestSanitizeRefusesAnEntryPastTheTextBudget(t *testing.T) {
	entry := &types.FAQEntryPayload{
		StandardQuestion: "q",
		Answers:          []string{strings.Repeat("a", secutils.FAQEntryMaxTextBytes+1)},
	}
	_, err := sanitizeFAQEntryPayload(entry)
	require.Error(t, err)
	require.Contains(t, err.Error(), "字节")
}

// The limits must not touch ordinary entries: the largest FAQ in the shipped
// corpus has ten similar questions and under half a kilobyte of text.
func TestSanitizeAcceptsAnOrdinaryEntry(t *testing.T) {
	entry := &types.FAQEntryPayload{
		StandardQuestion: "打印机脱机怎么处理",
		SimilarQuestions: makeSimilarQuestions(10),
		Answers:          []string{strings.Repeat("a", 400)},
	}
	meta, err := sanitizeFAQEntryPayload(entry)
	require.NoError(t, err)
	require.NotNil(t, meta)
}

// The bounds have to be reachable through the entry point that actually runs an
// import, not just through the sanitiser. The validation stage is where a
// rejected entry becomes a failure record; an entry rejected later, while the
// plan is built, was dropped as "skipped" after the success count had been
// fixed and so was reported as imported.
func TestOversizedEntryIsARecordedFailureNotASilentSkip(t *testing.T) {
	svc := &knowledgeService{}
	entries := []types.FAQEntryPayload{
		{
			StandardQuestion: "oversized",
			SimilarQuestions: makeSimilarQuestions(secutils.FAQEntryMaxSimilarQuestions + 1),
			Answers:          []string{"a"},
		},
		{StandardQuestion: "fine", Answers: []string{"a"}},
	}
	progress := &types.FAQImportProgress{}

	valid := svc.validateEntriesForReplaceModeWithProgress(context.Background(), entries, progress)

	require.Equal(t, []int{1}, valid, "only the well-formed entry may be planned")
	require.Equal(t, 1, progress.FailedCount, "the oversized entry must be counted as failed")
	require.Len(t, progress.FailedEntries, 1)
	require.Equal(t, 0, progress.FailedEntries[0].Index)
	require.Contains(t, progress.FailedEntries[0].Reason, "相似问")
}

func makeSimilarQuestions(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "similar question " + strings.Repeat("x", i%5) + " #" + string(rune('a'+i%26)) + "-" + itoa(i)
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
