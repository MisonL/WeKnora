package faq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/cli/internal/cmdutil"
	sdk "github.com/Tencent/WeKnora/client"
)

// Offset paging cannot produce a consistent snapshot while the knowledge base
// is being modified, and the total guard alone misses the case where a delete
// in an already-read prefix and an insert in the unread tail cancel out: the
// total agrees, but the pages were slices of two different states. The export
// therefore collects the whole list at least twice and only accepts the
// snapshot when two consecutive full passes are identical (compared per
// entry on key-sorted JSON, so irrelevant key order cannot cause a false
// mismatch); it resamples up to 3 passes and fails with a retry-means message
// if the list never settles.

// scriptedPasses serves scripted snapshots of the whole list. A pass is one
// full collection (the export's page 1 marks its start); calls beyond the
// script reuse the last scripted pass, which is what "the knowledge base went
// idle" looks like from the CLI.
//
// Each scripted pass is a list of pages; a page shorter than
// exportPageSize ends the pass, so a pass of a single short page is one
// ListFAQEntries call.
type scriptedPasses struct {
	passes [][][]sdk.FAQEntry // [pass][page][]rows
	total  int64
	// pass counts the collection passes started so far, calls the ListFAQEntries
	// calls inside the current pass.
	pass  int
	calls int
	// listCalls counts every ListFAQEntries call, for cost assertions.
	listCalls int
	// totalsByPage overrides the total reported by one (1-based) call number,
	// which is how a pass that mixed two states looks from the CLI.
	totalsByPage map[int]int64
}

func (s *scriptedPasses) ListFAQEntries(_ context.Context, _ string, page, _ int, _ int64, _, _, _ string) (*sdk.FAQEntriesPage, error) {
	s.listCalls++
	if page == 1 {
		s.pass++
		s.calls = 0
	}
	idx := s.pass - 1
	if idx >= len(s.passes) {
		idx = len(s.passes) - 1
	}
	pages := s.passes[idx]
	total := s.total
	if override, ok := s.totalsByPage[s.listCalls]; ok {
		total = override
	}
	if s.calls >= len(pages) {
		s.calls++
		return &sdk.FAQEntriesPage{Total: total, Page: page}, nil
	}
	rows := pages[s.calls]
	s.calls++
	return &sdk.FAQEntriesPage{Total: total, Page: page, Entries: rows}, nil
}

func (s *scriptedPasses) UpsertFAQEntries(context.Context, string, *sdk.FAQBatchUpsertPayload) (string, error) {
	panic("not used")
}

func (s *scriptedPasses) GetFAQImportProgress(context.Context, string) (*sdk.FAQImportProgress, error) {
	panic("not used")
}

func rowsFrom(from, to int) []sdk.FAQEntry {
	rows := make([]sdk.FAQEntry, 0, to-from)
	for i := from; i < to; i++ {
		rows = append(rows, sdk.FAQEntry{ID: int64(i + 1), StandardQuestion: fmt.Sprintf("q%d", i+1)})
	}
	return rows
}

func questions(got []sdk.FAQEntryPayload) string {
	parts := make([]string, 0, len(got))
	for _, p := range got {
		parts = append(parts, p.StandardQuestion)
	}
	return strings.Join(parts, ",")
}

func contains(qs, want string) bool {
	for _, p := range strings.Split(qs, ",") {
		if p == want {
			return true
		}
	}
	return false
}

func notContains(qs, unwanted string) error {
	if contains(qs, unwanted) {
		return fmt.Errorf("snapshot contains %q: %s", unwanted, qs)
	}
	return nil
}

// Stable data: the export still succeeds, and its output equals what a single
// consistent read would have produced (regression guard against the stability
// comparison mangling or rejecting a settled snapshot).
func TestExportTwoPassStillReturnsTheStableSnapshot(t *testing.T) {
	rows := rowsFrom(0, 3)
	svc := &scriptedPasses{passes: [][][]sdk.FAQEntry{{rows}}, total: 3}
	got, err := exportEntries(context.Background(), svc, "kb-1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(got) != 3 || questions(got) != "q1,q2,q3" {
		t.Fatalf("snapshot = %s, want q1,q2,q3", questions(got))
	}
	if svc.listCalls != 2 {
		t.Fatalf("%d list calls, want the two confirmation passes of a settled export", svc.listCalls)
	}
}

// The attack the total guard cannot see: while pass 1 is mid-flight, one
// already-read row is deleted and one new row is inserted -- the total never
// moves, page 1 and page 2 of pass 1 are slices of two different states, and
// the read looks complete. The KB then settles: pass 2 and pass 3 agree, and
// only their content may be handed back.
func TestExportResamplesPastADriftThatKeepsTheTotal(t *testing.T) {
	const total = exportPageSize + 25
	// Pass 1: page 1 read the old state (r1..r100); page 2, after delete(r1)
	// + insert(r126) with the total still 125, served the new ordering's tail
	// -- so q1 (deleted) is present and q101 (shifted across the boundary) is
	// missing.
	drifted := [][]sdk.FAQEntry{
		rowsFrom(0, exportPageSize),
		rowsFrom(exportPageSize+1, total+1),
	}
	// Pass 2 onwards: the settled state r2..r126.
	settled := [][]sdk.FAQEntry{
		rowsFrom(1, exportPageSize+1),
		rowsFrom(exportPageSize+1, total+1),
	}
	svc := &scriptedPasses{passes: [][][]sdk.FAQEntry{drifted, settled}, total: total}

	got, err := exportEntries(context.Background(), svc, "kb-1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	qs := questions(got)
	if len(got) != total {
		t.Fatalf("snapshot has %d entries, want %d: %s", len(got), total, qs)
	}
	if err := notContains(qs, "q1"); err != nil {
		t.Fatal(err)
	}
	if !contains(qs, "q101") {
		t.Fatalf("snapshot missed q101 (the row that drifted across the page boundary): %s", qs)
	}
	if !contains(qs, "q126") {
		t.Fatalf("snapshot missed q126 (inserted during pass 1): %s", qs)
	}
}

// The list is still moving between pass 1 and pass 2 but goes idle before
// pass 3: passes 2 and 3 match and the export succeeds with that content.
func TestExportSucceedsWhenTheKnowledgeBaseSettlesWithinTheRetryCap(t *testing.T) {
	a := rowsFrom(0, 2) // q1,q2
	b := rowsFrom(1, 3) // q2,q3
	svc := &scriptedPasses{passes: [][][]sdk.FAQEntry{{a}, {b}}, total: 2}
	got, err := exportEntries(context.Background(), svc, "kb-1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if questions(got) != "q2,q3" {
		t.Fatalf("snapshot = %s, want the settled q2,q3", questions(got))
	}
	if svc.listCalls != 3 {
		t.Fatalf("%d list calls, want 3 passes (two unstable, third confirming)", svc.listCalls)
	}
}

// A knowledge base that never settles must produce a refusal, not a snapshot
// whose rows belong to different states: fed to `faq import --mode replace`
// such a snapshot deletes entries it missed. The refusal follows the existing
// exit-code classification (operation.failed -> exit 1), the same one the
// total guard already uses for "the knowledge base changed under the read".
func TestExportFailsAfterThreeUnstablePasses(t *testing.T) {
	svc := &scriptedPasses{
		passes: [][][]sdk.FAQEntry{
			{rowsFrom(0, 1)}, // q1
			{rowsFrom(1, 2)}, // q2
			{rowsFrom(2, 3)}, // q3
		},
		total: 1,
	}
	got, err := exportEntries(context.Background(), svc, "kb-1")
	if err == nil {
		t.Fatalf("an export that never settled returned %d entries and no error", len(got))
	}
	if got != nil {
		t.Fatalf("a refused export must not return partial data")
	}
	if !strings.Contains(err.Error(), "changed while it was being read") {
		t.Fatalf("error should say the knowledge base changed, got: %v", err)
	}
	if !strings.Contains(err.Error(), "retry while it is idle") {
		t.Fatalf("error should tell the caller to retry while idle, got: %v", err)
	}
	var appErr *cmdutil.Error
	if !errors.As(err, &appErr) || appErr.Code != cmdutil.CodeOperationFailed {
		t.Fatalf("error = %v, want code %s", err, cmdutil.CodeOperationFailed)
	}
	if code := cmdutil.ExitCode(err); code != 1 {
		t.Fatalf("exit code = %d, want 1 (the operation.failed bucket)", code)
	}
	if svc.listCalls != 3 {
		t.Fatalf("%d list calls, want the export to stop at the 3-pass cap", svc.listCalls)
	}
}

// A pass that mixes two states of its own (the total moved between its pages)
// must not be fatal on its own: the export retries it like any other unstable
// read, and a later read that matches the last stable one is the snapshot.
func TestExportRetriesAPassWhoseOwnPagesDisagreed(t *testing.T) {
	const total = exportPageSize + 5
	stable := pagesOfStable(total)
	svc := &scriptedPasses{passes: [][][]sdk.FAQEntry{stable}, total: total}
	// Pass 2's second page reports a different total.
	svc.totalsByPage = map[int]int64{2: total + 1}
	got, err := exportEntries(context.Background(), svc, "kb-1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(got) != total {
		t.Fatalf("snapshot has %d entries, want %d", len(got), total)
	}
	// Three full reads of two pages each: one stable, one mixed, one confirming.
	if svc.listCalls != 6 {
		t.Fatalf("%d list calls, want the mixed pass retried and the result re-confirmed", svc.listCalls)
	}
}

func pagesOfStable(n int) [][]sdk.FAQEntry {
	pages := make([][]sdk.FAQEntry, 0, n/exportPageSize+1)
	for from := 0; from < n; from += exportPageSize {
		to := from + exportPageSize
		if to > n {
			to = n
		}
		pages = append(pages, rowsFrom(from, to))
	}
	return pages
}

// The stability comparison must not report a spurious mismatch over details
// that carry no information: each entry is normalised to key-sorted JSON
// before comparing, so an irrelevant key order cannot make two identical
// passes look different, while real content differences -- and reordering,
// which both passes' fixed (updated_at, id) order makes significant -- still
// compare unequal.
func TestSnapshotComparisonIgnoresIrrelevantJSONDetails(t *testing.T) {
	base := []sdk.FAQEntryPayload{{StandardQuestion: "q", Answers: []string{"a"}}}
	if !snapshotsEqual(base, base) {
		t.Fatal("identical entries must compare equal")
	}
	// Nil vs empty slice: both serialise away under omitempty.
	nilList := []sdk.FAQEntryPayload{{StandardQuestion: "q", Answers: []string{"a"}, SimilarQuestions: nil}}
	emptyList := []sdk.FAQEntryPayload{{StandardQuestion: "q", Answers: []string{"a"}, SimilarQuestions: []string{}}}
	if !snapshotsEqual(nilList, emptyList) {
		t.Fatal("nil and empty (omitempty) fields must not be a spurious mismatch")
	}
	longer := append(append([]sdk.FAQEntryPayload{}, base...), sdk.FAQEntryPayload{StandardQuestion: "q2"})
	if snapshotsEqual(base, longer) {
		t.Fatal("different lengths must compare unequal")
	}
	diff := []sdk.FAQEntryPayload{{StandardQuestion: "q", Answers: []string{"changed"}}}
	if snapshotsEqual(base, diff) {
		t.Fatal("different content must compare unequal")
	}
	swapped := []sdk.FAQEntryPayload{
		{StandardQuestion: "b", Answers: []string{"1"}},
		{StandardQuestion: "a", Answers: []string{"2"}},
	}
	ordered := []sdk.FAQEntryPayload{swapped[1], swapped[0]}
	if snapshotsEqual(swapped, ordered) {
		t.Fatal("entries in a different order must compare unequal")
	}
}

// Key-order tolerance is a property of the normaliser, not of Go's struct
// marshalling (which is deterministic), so test the raw-JSON layer directly.
func TestCanonicalJSONIgnoresObjectKeyOrder(t *testing.T) {
	one := json.RawMessage(`{"standard_question":"q","answers":["a"],"is_enabled":true}`)
	other := json.RawMessage(`{"is_enabled":true,"answers":["a"],"standard_question":"q"}`)
	equal, err := canonicalJSONEqual(one, other)
	if err != nil {
		t.Fatalf("canonicalise: %v", err)
	}
	if !equal {
		t.Fatal("same content in different key order must compare equal")
	}
	changed := json.RawMessage(`{"answers":["b"],"is_enabled":true,"standard_question":"q"}`)
	equal, err = canonicalJSONEqual(one, changed)
	if err != nil {
		t.Fatalf("canonicalise: %v", err)
	}
	if equal {
		t.Fatal("changed values must not compare equal")
	}
	// Numbers survive the round trip exactly (no float re-serialisation).
	// 9007199254740992 and 9007199254740993 are the same float64; only a
	// json.Number path can tell them apart.
	precise := json.RawMessage(`{"id":9007199254740993}`)
	nearby := json.RawMessage(`{"id":9007199254740992}`)
	equal, err = canonicalJSONEqual(precise, nearby)
	if err != nil {
		t.Fatalf("canonicalise: %v", err)
	}
	if equal {
		t.Fatal("integers above 2^53 must not collapse onto the same float value")
	}
}
