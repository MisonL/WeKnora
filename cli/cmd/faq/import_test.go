package faq

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/Tencent/WeKnora/client"
)

// fakeService is an in-memory FAQ backend: enough of the wire behaviour for the
// orchestrator's rules to be exercised without a server.
type fakeService struct {
	entries   []sdk.FAQEntry
	total     int64
	submitted []sdk.FAQBatchUpsertPayload
	// results supplies one progress record per submission, in order.
	results []sdk.FAQImportProgress
	// sequence records the order of submit/poll so serial execution is visible.
	sequence []string
}

func (f *fakeService) ListFAQEntries(_ context.Context, _ string, _, _ int, _ int64, _, _, _ string) (*sdk.FAQEntriesPage, error) {
	return &sdk.FAQEntriesPage{Total: f.total, Entries: f.entries}, nil
}

func (f *fakeService) UpsertFAQEntries(_ context.Context, _ string, payload *sdk.FAQBatchUpsertPayload) (string, error) {
	f.submitted = append(f.submitted, *payload)
	f.sequence = append(f.sequence, "submit")
	return "task-" + string(rune('a'+len(f.submitted)-1)), nil
}

func (f *fakeService) GetFAQImportProgress(_ context.Context, taskID string) (*sdk.FAQImportProgress, error) {
	f.sequence = append(f.sequence, "poll:"+taskID)
	idx := int(taskID[len(taskID)-1] - 'a')
	if idx < len(f.results) {
		p := f.results[idx]
		return &p, nil
	}
	return &sdk.FAQImportProgress{TaskID: taskID, Status: "completed"}, nil
}

// withBudgets swaps the package-level budgets for one test and restores them.
//
// They are package variables so a deployment can raise them, which makes them
// shared state: two tests changing them at once would have one test's byte
// budget blamed on another's entries. Every test that touches them goes
// through this, which serialises those tests.
func withBudgets(t *testing.T, entries int, bytes int64) {
	t.Helper()
	prevEntries, prevBytes := importMaxEntries, importMaxBodyBytes
	importMaxEntries, importMaxBodyBytes = entries, bytes
	t.Cleanup(func() {
		importMaxEntries, importMaxBodyBytes = prevEntries, prevBytes
	})
}

func writePayloadFile(t *testing.T, dir string, entries []sdk.FAQEntryPayload) string {
	t.Helper()
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	path := filepath.Join(dir, "faq.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func makeEntries(n int) []sdk.FAQEntryPayload {
	out := make([]sdk.FAQEntryPayload, n)
	for i := range out {
		out[i] = sdk.FAQEntryPayload{
			StandardQuestion: "question " + string(rune('a'+i%26)) + " #" + itoa(i),
			Answers:          []string{"answer"},
		}
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

// A run larger than the server's cap has to leave in several requests, and
// each must respect the cap.
func TestImportChunksUnderTheEntryBudget(t *testing.T) {
	dir := t.TempDir()
	path := writePayloadFile(t, dir, makeEntries(23))
	withBudgets(t, 10, 32<<20)

	svc := &fakeService{total: 0}
	run, err := runImport(context.Background(), svc, path, importOptions{
		KB: "kb", Mode: "append", Wait: true, FailureGate: 5, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	sizes := make([]int, len(svc.submitted))
	for i, p := range svc.submitted {
		sizes[i] = len(p.Entries)
		if p.Mode != "append" {
			t.Fatalf("batch %d carried mode %q", i, p.Mode)
		}
	}
	want := []int{10, 10, 3}
	if len(sizes) != len(want) {
		t.Fatalf("batches = %v, want %v", sizes, want)
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Fatalf("batches = %v, want %v", sizes, want)
		}
	}
	if len(run.Batches) != 3 {
		t.Fatalf("run recorded %d batches", len(run.Batches))
	}
}

// replace is all-or-nothing: more than one batch would have each request delete
// the previous one's rows, so the run must refuse before sending anything.
func TestImportRefusesSplitReplaceBeforeSendingAnything(t *testing.T) {
	dir := t.TempDir()
	path := writePayloadFile(t, dir, makeEntries(23))
	withBudgets(t, 10, 32<<20)

	svc := &fakeService{}
	_, err := runImport(context.Background(), svc, path, importOptions{
		KB: "kb", Mode: "replace", Snapshot: false, FailureGate: 5, Timeout: time.Minute,
	})
	if err == nil {
		t.Fatal("a split replace must be refused")
	}
	if len(svc.submitted) != 0 {
		t.Fatalf("%d batches were sent before the refusal", len(svc.submitted))
	}
}

// A single-batch replace whose file is not the whole list deletes the rest. The
// server's count is the only thing that can tell, so it has to be checked.
func TestImportRefusesReplaceThatWouldDeleteUnlistedEntries(t *testing.T) {
	dir := t.TempDir()
	path := writePayloadFile(t, dir, makeEntries(3))
	svc := &fakeService{total: 10}

	_, err := runImport(context.Background(), svc, path, importOptions{
		KB: "kb", Mode: "replace", Snapshot: false, FailureGate: 5, Timeout: time.Minute,
	})
	if err == nil {
		t.Fatal("replace of 3 of 10 entries must be refused")
	}
	if len(svc.submitted) != 0 {
		t.Fatal("nothing may be sent")
	}
}

// The gate stops a run whose batch failed too much: continuing would repeat the
// same failure against the remaining entries.
func TestImportStopsWhenTheFailureGateTrips(t *testing.T) {
	dir := t.TempDir()
	path := writePayloadFile(t, dir, makeEntries(20))
	withBudgets(t, 10, 32<<20)

	svc := &fakeService{
		results: []sdk.FAQImportProgress{
			{Status: "completed", Total: 10, SuccessCount: 4, FailedCount: 6},
		},
	}
	run, err := runImport(context.Background(), svc, path, importOptions{
		KB: "kb", Mode: "append", Snapshot: false, Wait: true, FailureGate: 5, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.Stopped != "gate" {
		t.Fatalf("stopped = %q, want gate", run.Stopped)
	}
	if len(svc.submitted) != 1 {
		t.Fatalf("%d batches sent; the second must not run after the gate trips", len(svc.submitted))
	}
	if run.exitCode() == 0 {
		t.Fatal("a gated run must not exit 0")
	}
}

// A small failure ratio continues: one bad row out of fifty is a data problem
// in that row, not a reason to abandon the import.
func TestImportContinuesBelowTheFailureGate(t *testing.T) {
	dir := t.TempDir()
	path := writePayloadFile(t, dir, makeEntries(40))
	withBudgets(t, 20, 32<<20)

	svc := &fakeService{
		results: []sdk.FAQImportProgress{
			{Status: "completed", Total: 20, SuccessCount: 20, FailedCount: 0},
			{Status: "completed", Total: 20, SuccessCount: 20, FailedCount: 0},
		},
	}
	run, err := runImport(context.Background(), svc, path, importOptions{
		KB: "kb", Mode: "append", Snapshot: false, Wait: true, FailureGate: 5, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(svc.submitted) != 2 {
		t.Fatalf("%d batches sent, want 2", len(svc.submitted))
	}
	if run.Stopped != "" {
		t.Fatalf("stopped = %q, want a clean finish", run.Stopped)
	}
	if run.exitCode() != 0 {
		t.Fatalf("exit = %d, want 0", run.exitCode())
	}
}

// A batch whose entries imported with questions stripped is not a clean batch.
// Those entries are in neither success_count nor failed_count, so a gate that
// reads failed_count alone lets a run continue that has already lost a large
// share of its rows.
func TestImportStopsWhenPartiallyFailedEntriesTripTheGate(t *testing.T) {
	dir := t.TempDir()
	path := writePayloadFile(t, dir, makeEntries(20))
	withBudgets(t, 10, 32<<20)

	svc := &fakeService{
		results: []sdk.FAQImportProgress{
			{Status: "completed", Total: 10, SuccessCount: 6, FailedCount: 0, PartialFailedCount: 4},
		},
	}
	run, err := runImport(context.Background(), svc, path, importOptions{
		KB: "kb", Mode: "append", Snapshot: false, Wait: true, FailureGate: 5, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.Stopped != "gate" {
		t.Fatalf("stopped = %q, want gate", run.Stopped)
	}
	if len(svc.submitted) != 1 {
		t.Fatalf("%d batches sent; partial failures must stop the run", len(svc.submitted))
	}
	if run.Batches[0].PartialFailed != 4 {
		t.Fatalf("partial failures recorded as %d, want 4", run.Batches[0].PartialFailed)
	}
	if run.Batches[0].Failed != 0 {
		t.Fatalf("partial failures were folded into the failed count (%d)", run.Batches[0].Failed)
	}
}

// A few partial failures stay below the gate but still mean the run did not do
// what the file asked, so the exit code has to say so.
func TestImportSucceedsButExitsNonZeroOnPartialFailures(t *testing.T) {
	dir := t.TempDir()
	path := writePayloadFile(t, dir, makeEntries(40))
	withBudgets(t, 20, 32<<20)

	svc := &fakeService{
		results: []sdk.FAQImportProgress{
			{Status: "completed", Total: 20, SuccessCount: 19, PartialFailedCount: 1},
			{Status: "completed", Total: 20, SuccessCount: 20},
		},
	}
	run, err := runImport(context.Background(), svc, path, importOptions{
		KB: "kb", Mode: "append", Snapshot: false, Wait: true, FailureGate: 5, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(svc.submitted) != 2 {
		t.Fatalf("%d batches sent, want 2", len(svc.submitted))
	}
	if run.Stopped != "" {
		t.Fatalf("stopped = %q, want a clean finish", run.Stopped)
	}
	if run.exitCode() == 0 {
		t.Fatal("an import whose entries lost questions must not exit 0")
	}
}

// A dry run's record is the plan it validated, and the CLI no longer derives
// counts from the entry lists: the server's own counters are the only ones that
// survive the failed entries being moved into a CSV, and the gate reads them
// the same way for a dry run and a real import.
func TestDryRunGateReadsTheServersOwnCounts(t *testing.T) {
	dir := t.TempDir()
	path := writePayloadFile(t, dir, makeEntries(20))
	withBudgets(t, 10, 32<<20)

	svc := &fakeService{
		results: []sdk.FAQImportProgress{
			{Status: "completed", DryRun: true, Total: 10, SuccessCount: 3, FailedCount: 7},
		},
	}
	run, err := runImport(context.Background(), svc, path, importOptions{
		KB: "kb", Mode: "append", DryRun: true, Snapshot: false, Wait: true, FailureGate: 5, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.Stopped != "gate" {
		t.Fatalf("stopped = %q, want gate", run.Stopped)
	}
	if run.Batches[0].Failed != 7 || run.Batches[0].Imported != 3 {
		t.Fatalf("batch = %+v, want failed 7 / imported 3 from the server's counts", run.Batches[0])
	}
}

// Batches must be strictly serial: the next submission may only happen after
// the previous batch's result was read. Submitting the second before reading
// the first is what lets a failed batch go unnoticed.
func TestImportSubmitsOneBatchAtATime(t *testing.T) {
	dir := t.TempDir()
	path := writePayloadFile(t, dir, makeEntries(30))
	withBudgets(t, 10, 32<<20)

	svc := &fakeService{}
	if _, err := runImport(context.Background(), svc, path, importOptions{
		KB: "kb", Mode: "append", Snapshot: false, Wait: true, FailureGate: 0, Timeout: time.Minute,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Expected: submit, poll..., submit, poll..., submit, poll...
	seenUnfinishedSubmit := false
	for i, ev := range svc.sequence {
		if ev != "submit" {
			continue
		}
		if i > 0 {
			prev := svc.sequence[i-1]
			if prev != "" && prev[0] == 'p' {
				seenUnfinishedSubmit = true
				break
			}
		}
	}
	if !seenUnfinishedSubmit {
		t.Fatalf("no batch waited for the previous one's result: %v", svc.sequence)
	}
}

// An entry that alone exceeds the byte budget can never be sent; refusing it
// locally beats a server 400 after earlier batches committed.
func TestImportRefusesAnEntryLargerThanTheBodyBudget(t *testing.T) {
	dir := t.TempDir()
	big := makeEntries(1)
	big[0].Answers = []string{string(make([]byte, 2048))}
	path := writePayloadFile(t, dir, big)
	withBudgets(t, 1000, 512)

	svc := &fakeService{}
	if _, err := runImport(context.Background(), svc, path, importOptions{
		KB: "kb", Mode: "append", Snapshot: false, Wait: true, FailureGate: 5, Timeout: time.Minute,
	}); err == nil {
		t.Fatal("an oversized entry must be refused")
	}
	if len(svc.submitted) != 0 {
		t.Fatal("nothing may be sent")
	}
}

// Only a wait that ran out is 124. The other stop reasons have their own
// meaning: a failed submission or a failed batch is 1, and --wait=false is a
// successful submission whose outcome belongs to the task, so 0.
func TestImportExitCodeFollowsTheStopReason(t *testing.T) {
	cases := []struct {
		name string
		run  importRun
		want int
		why  string
	}{
		{"clean finish", importRun{Batches: []importBatch{{Status: "completed"}}}, 0, ""},
		{"submit failed", importRun{Stopped: "submit_failed", Batches: []importBatch{{Status: "submit_failed"}}}, 1,
			"a refused submission is a failed run, not a timeout"},
		{"batch failed", importRun{Stopped: "batch_failed", Batches: []importBatch{{Status: "failed"}}}, 1,
			"a batch that ended failed is a failed run, not a timeout"},
		{"gate", importRun{Stopped: "gate", Batches: []importBatch{{Status: "completed"}}}, 1, ""},
		{"not waited", importRun{Stopped: "not_waited", Batches: []importBatch{{Status: "running", TaskID: "t"}}}, 0,
			"--wait=false submits and returns; the task keeps running"},
		{"timeout", importRun{Stopped: "timeout", Batches: []importBatch{{Status: "timeout"}}}, 124,
			"only an exhausted wait uses the timeout convention"},
	}
	for _, tc := range cases {
		if got := tc.run.exitCode(); got != tc.want {
			t.Fatalf("%s: exitCode = %d, want %d %s", tc.name, got, tc.want, tc.why)
		}
	}
}
