package faq

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/cli/internal/cmdutil"
	sdk "github.com/Tencent/WeKnora/client"
)

// pagedService serves fixed pages and reports a total that may not match them,
// which is how a knowledge base that changes mid-export looks from the CLI.
type pagedService struct {
	pages [][]sdk.FAQEntry
	total int64
	// totals, when set, gives the total each page reports in turn (a knowledge
	// base changing under the read). It overrides total.
	totals []int64
}

func (p *pagedService) ListFAQEntries(_ context.Context, _ string, page, _ int, _ int64, _, _, _ string) (*sdk.FAQEntriesPage, error) {
	total := p.total
	if len(p.totals) > 0 {
		idx := page - 1
		if idx >= len(p.totals) {
			idx = len(p.totals) - 1
		}
		total = p.totals[idx]
	}
	if page > len(p.pages) {
		return &sdk.FAQEntriesPage{Total: total}, nil
	}
	return &sdk.FAQEntriesPage{Total: total, Page: page, Entries: p.pages[page-1]}, nil
}

func (p *pagedService) UpsertFAQEntries(context.Context, string, *sdk.FAQBatchUpsertPayload) (string, error) {
	panic("not used")
}

func (p *pagedService) GetFAQImportProgress(context.Context, string) (*sdk.FAQImportProgress, error) {
	panic("not used")
}

// entryPage serves one page of the synthetic listing: rows [from, to).
func entryPage(from, to int) []sdk.FAQEntry {
	rows := make([]sdk.FAQEntry, 0, to-from)
	for i := from; i < to; i++ {
		rows = append(rows, sdk.FAQEntry{ID: int64(i + 1), StandardQuestion: fmt.Sprintf("q%d", i+1)})
	}
	return rows
}

// pagesOf splits n synthetic rows the way the endpoint paginates them: full
// pages of exportPageSize plus one short tail. Only a full page makes the
// export ask for the next one, so fixtures with a short first page cannot
// exercise page-to-page drift.
func pagesOf(n int) [][]sdk.FAQEntry {
	pages := make([][]sdk.FAQEntry, 0, n/exportPageSize+1)
	for from := 0; from < n; from += exportPageSize {
		to := from + exportPageSize
		if to > n {
			to = n
		}
		pages = append(pages, entryPage(from, to))
	}
	return pages
}

// A complete read is a normal export.
func TestExportAcceptsACompleteRead(t *testing.T) {
	const total = exportPageSize + 5
	svc := &pagedService{
		pages: pagesOf(total),
		total: total,
	}
	got, err := exportEntries(context.Background(), svc, "kb-1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(got) != total {
		t.Fatalf("read %d entries, want %d", len(got), total)
	}
}

// A knowledge base that changes under an offset-paginated read can lose a row
// without the final count ever disagreeing: deleting a row in an already-read
// prefix shifts an unread row into the previous page, and the total drops by
// the same one. A total that changes between pages is the detectable signal.
func TestExportRefusesAReadTheKnowledgeBaseChangedUnder(t *testing.T) {
	svc := &pagedService{
		pages:  pagesOf(exportPageSize + 5),
		totals: []int64{exportPageSize + 5, exportPageSize + 4},
	}
	got, err := exportEntries(context.Background(), svc, "kb-1")
	if err == nil {
		t.Fatalf("a total that moved under the read returned %d entries and no error", len(got))
	}
	if got != nil {
		t.Fatalf("a refused export must not return partial data")
	}
	if !strings.Contains(err.Error(), "changed while it was being read") {
		t.Fatalf("error should say the knowledge base changed, got: %v", err)
	}
}

// The server reported more rows than paging ended at (entries were deleted
// between page reads, or the listing and the count disagree). Returning the
// short read quietly would make a backup that --mode replace later deletes the
// rest with.
func TestExportRefusesAShortRead(t *testing.T) {
	svc := &pagedService{
		pages: pagesOf(exportPageSize + 5),
		total: exportPageSize + 10,
	}
	got, err := exportEntries(context.Background(), svc, "kb-1")
	if err == nil {
		t.Fatalf("short read returned %d entries and no error; want a refusal", len(got))
	}
	if got != nil {
		t.Fatalf("a refused export must not return partial data")
	}
	appErr, ok := err.(*cmdutil.Error)
	if !ok {
		t.Fatalf("error is %T, want *cmdutil.Error", err)
	}
	if appErr.Code != cmdutil.CodeOperationFailed {
		t.Fatalf("code = %s, want %s", appErr.Code, cmdutil.CodeOperationFailed)
	}
}

// The other direction of the same guard: rows were added while the pass ran,
// so paging turned up more than page 1's total. Those extra rows are a slice
// of a state the total does not describe, and the snapshot is refused too.
func TestExportRefusesAReadThatOutranItsTotal(t *testing.T) {
	svc := &pagedService{
		pages: pagesOf(exportPageSize + 5),
		total: exportPageSize,
	}
	got, err := exportEntries(context.Background(), svc, "kb-1")
	if err == nil {
		t.Fatalf("an over-long read returned %d entries and no error; want a refusal", len(got))
	}
	if got != nil {
		t.Fatalf("a refused export must not return partial data")
	}
	if !strings.Contains(err.Error(), "changed while it was being read") {
		t.Fatalf("error should say the knowledge base changed, got: %v", err)
	}
}
