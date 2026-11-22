package service

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

// The failed-entries download is opened in Excel, so its cells are a formula
// surface just like the export endpoint's. A failing question that starts with
// '=' must come back neutralised rather than live.
func TestFailedEntriesCSVNeutralisesFormulas(t *testing.T) {
	files := &fakeFileService{}
	svc := &knowledgeService{fileSvc: files}
	ctx := context.Background()

	path, err := svc.generateFailedEntriesCSV(ctx, 7, "task-1", []types.FAQFailedEntry{{
		Reason:            "=1+1",
		TagName:           "分类",
		StandardQuestion:  "=SUM(A1:A2)",
		SimilarQuestions:  []string{"+1", "plain"},
		NegativeQuestions: []string{"-1"},
		Answers:           []string{"@cmd"},
	}})
	require.NoError(t, err)

	raw, ok := files.saved[path]
	require.True(t, ok, "the CSV should be stored under the returned path")
	body := string(raw)
	require.True(t, strings.HasPrefix(body, "\xEF\xBB\xBF"), "BOM stays for Excel's UTF-8 detection")

	for _, cell := range []string{"'=1+1", "'=SUM(A1:A2)", "'+1##plain", "'-1", "'@cmd"} {
		require.Contains(t, body, cell, "formula-like cell %q must be neutralised", cell)
	}
}
