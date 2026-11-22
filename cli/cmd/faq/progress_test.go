package faq

import (
	"strings"
	"testing"

	sdk "github.com/Tencent/WeKnora/client"

	"github.com/Tencent/WeKnora/cli/internal/cmdutil"
	"github.com/Tencent/WeKnora/cli/internal/iostreams"
)

// `faq wait` reports the outcome through its exit code, and a partially
// imported entry did not land as the caller supplied it: the answer is there,
// the similar/negative questions are not. Exit 0 would tell an agent the import
// is exactly what the file said.
func TestWaitExitCodeCountsPartialFailures(t *testing.T) {
	tests := []struct {
		name string
		p    *sdk.FAQImportProgress
		want int
	}{
		{name: "clean import", p: &sdk.FAQImportProgress{Status: "completed", SuccessCount: 10}, want: 0},
		{name: "one failed entry", p: &sdk.FAQImportProgress{Status: "completed", SuccessCount: 9, FailedCount: 1}, want: 1},
		{name: "one partial entry", p: &sdk.FAQImportProgress{Status: "completed", SuccessCount: 9, PartialFailedCount: 1}, want: 1},
		{name: "task failed", p: &sdk.FAQImportProgress{Status: "failed"}, want: 1},
		{name: "no record", p: nil, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCodeFor(tt.p); got != tt.want {
				t.Fatalf("exitCodeFor = %d, want %d", got, tt.want)
			}
		})
	}
}

// The text record has to keep the two numbers apart: folding partial failures
// into "failed" would misreport entries that are searchable.
func TestProgressTextShowsPartialFailuresSeparately(t *testing.T) {
	out, _ := iostreams.SetForTest(t)
	p := &sdk.FAQImportProgress{
		TaskID: "task", KBID: "kb", Status: "completed",
		Progress: 100, Processed: 10, Total: 10,
		SuccessCount: 8, FailedCount: 1, PartialFailedCount: 1,
	}
	if err := emitProgress(p, &cmdutil.FormatOptions{Mode: cmdutil.FormatText}); err != nil {
		t.Fatalf("emitProgress: %v", err)
	}
	got := out.String()
	for _, want := range []string{"imported  8", "partial  1", "failed  1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q in:\n%s", want, got)
		}
	}
}
