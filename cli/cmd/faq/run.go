package faq

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/Tencent/WeKnora/client"

	"github.com/Tencent/WeKnora/cli/internal/cmdutil"
	"github.com/Tencent/WeKnora/cli/internal/iostreams"
)

// runImport is the orchestrator: snapshot, chunk, then submit one batch at a
// time and read each result before the next.
func runImport(ctx context.Context, svc FAQService, payloadPath string, opts importOptions) (*importRun, error) {
	entries, fileMode, digest, err := readImportPayload(payloadPath)
	if err != nil {
		return nil, err
	}
	mode := opts.Mode
	if mode == "" {
		mode = fileMode
	}
	if mode == "" {
		mode = "append"
	}

	// --batch-size can only lower the batch below the server's cap, never
	// raise it; 0 means the flag was not given (direct callers), so the cap
	// stands.
	maxEntries := importMaxEntries
	if opts.BatchSize > 0 && opts.BatchSize < maxEntries {
		maxEntries = opts.BatchSize
	}
	batches, err := chunkEntries(entries, mode, maxEntries, importMaxBodyBytes)
	if err != nil {
		return nil, err
	}

	// replace is all-or-nothing by definition. Two batches under replace would
	// have the second delete everything the first created, so the run is
	// refused before anything is sent rather than failing halfway.
	if mode == "replace" && len(batches) > 1 {
		return nil, &cmdutil.Error{
			Code: cmdutil.CodeInputInvalidArgument,
			Message: fmt.Sprintf(
				"%d entries need %d requests, but replace cannot be split: every request would delete what the previous one created.\n"+
					"Raise the server's WEKNORA_FAQ_IMPORT_MAX_ENTRIES, or clear the knowledge base and import with --mode append in batches.",
				len(entries), len(batches)),
		}
	}

	// Without waiting, only one batch can be submitted: the next submission
	// would race the per-KB admission the server runs, and with batches that
	// don't fit in one request the rest of the file would silently never be
	// sent. Refuse rather than report a partial import as a success.
	if !opts.Wait && len(batches) > 1 {
		return nil, &cmdutil.Error{
			Code: cmdutil.CodeInputInvalidArgument,
			Message: fmt.Sprintf(
				"%d entries need %d requests, but --wait=false submits one batch: the server runs one import per knowledge base at a time, so the remaining batches would be refused.\n"+
					"Raise --batch-size (and the server's WEKNORA_FAQ_IMPORT_MAX_ENTRIES if needed), or drop --wait=false.",
				len(entries), len(batches)),
		}
	}

	run := &importRun{
		KBID:        opts.KB,
		Mode:        mode,
		DryRun:      opts.DryRun,
		PayloadPath: payloadPath,
		PayloadSHA:  digest,
		Total:       len(entries),
		OperationID: newOperationID(),
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	// Snapshot before writing, so the run has a way back. A dry run writes
	// nothing, so it needs none.
	if opts.Snapshot && !opts.DryRun {
		path, err := snapshotBeforeImport(ctx, svc, opts.KB, payloadPath)
		if err != nil {
			return nil, err
		}
		run.Snapshot = path
	}

	// replace with a single batch still has one way to lose data: the file is
	// not the whole list, so the entries it omits are deleted. The server
	// knows the real count; this refuses when the two disagree.
	if mode == "replace" && !opts.DryRun {
		if err := checkReplaceExpectedCount(ctx, svc, opts.KB, len(entries)); err != nil {
			return nil, err
		}
	}

	deadline := time.Time{}
	if opts.Timeout > 0 {
		deadline = time.Now().Add(opts.Timeout)
	}

	for i, batch := range batches {
		b := importBatch{Index: i, Entries: len(batch), Status: "pending"}
		taskID, err := svc.UpsertFAQEntries(ctx, opts.KB, &sdk.FAQBatchUpsertPayload{
			Mode:    mode,
			DryRun:  opts.DryRun,
			Entries: batch,
		})
		if err != nil {
			b.Status = "submit_failed"
			b.Error = err.Error()
			run.Batches = append(run.Batches, b)
			run.Stopped = "submit_failed"
			return run, writeReportIfRequested(run, opts.Report)
		}
		b.TaskID = taskID
		b.Status = "running"
		run.Batches = append(run.Batches, b)

		if !opts.Wait {
			// Without waiting, the next batch would race the admission the
			// server enforces per knowledge base, and the result could not be
			// attributed to a batch. Stop after the first submission.
			run.Stopped = "not_waited"
			break
		}

		progress, err := waitForBatch(ctx, svc, taskID, deadline)
		if err != nil {
			var timedOut *waitTimeoutError
			if errors.As(err, &timedOut) {
				run.Batches[len(run.Batches)-1].Status = "timeout"
				run.Stopped = "timeout"
				_ = writeReportIfRequested(run, opts.Report)
				return run, nil
			}
			return nil, err
		}
		applyBatchProgress(&run.Batches[len(run.Batches)-1], progress)

		if failed, total := batchFailureCounts(run.Batches[len(run.Batches)-1], progress); gateTripped(opts.FailureGate, failed, total) {
			run.Stopped = "gate"
			break
		}
		if run.Batches[len(run.Batches)-1].Status != "completed" {
			run.Stopped = "batch_failed"
			break
		}
	}

	if err := writeReportIfRequested(run, opts.Report); err != nil {
		return nil, err
	}
	return run, nil
}

// waitTimeoutError reports that the batch did not finish inside the budget. It
// is distinct from a failed batch: the work may still be running server-side,
// so the run reports the task id rather than pretending the batch failed.
type waitTimeoutError struct {
	taskID string
}

func (e *waitTimeoutError) Error() string {
	return fmt.Sprintf("timed out waiting for FAQ import task %s; it may still be running", e.taskID)
}

// waitForBatch polls one batch. Every request and every sleep is clipped to the
// remaining budget, so a timeout is never extended by one more poll.
func waitForBatch(ctx context.Context, svc FAQService, taskID string, deadline time.Time) (*sdk.FAQImportProgress, error) {
	const interval = 2 * time.Second
	for {
		progress, err := getFAQProgress(ctx, svc, taskID, deadline)
		if err != nil {
			if errors.Is(err, errWaitBudgetExhausted) {
				return nil, &waitTimeoutError{taskID: taskID}
			}
			return nil, cmdutil.WrapHTTP(err, "get FAQ import progress")
		}
		if progress != nil && (progress.Status == "completed" || progress.Status == "failed") {
			return progress, nil
		}
		sleep := interval
		if !deadline.IsZero() {
			left := time.Until(deadline)
			if left <= 0 {
				return nil, &waitTimeoutError{taskID: taskID}
			}
			if sleep > left {
				sleep = left
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sleep):
		}
	}
}

// applyBatchProgress records one batch's outcome.
//
// The counts come from the task's own record for both a real import and a dry
// run. failed_count alone is not enough: the server counts an entry whose
// conflicting similar/negative questions were stripped as a partial failure,
// and leaves it out of both failed_count and success_count, so a batch can
// look clean while a large share of it lost questions. PartialFailedCount is
// kept as its own number because those entries did import -- they are not the
// same thing as a row that could not be written.
func applyBatchProgress(b *importBatch, p *sdk.FAQImportProgress) {
	if p == nil {
		b.Status = "unknown"
		return
	}
	b.Status = p.Status
	b.Skipped = p.SkippedCount
	switch {
	case p.Status == "failed" && p.Error != "":
		b.Error = p.Error
	}
	b.Imported = p.SuccessCount
	b.Failed = p.FailedCount
	b.PartialFailed = p.PartialFailedCount
}

// batchFailureCounts returns what the gate measures: entries that did not land
// intact, against entries the batch set out to write.
//
// A partial failure counts. It usually means the file conflicts with the
// knowledge base rather than that one row is bad, and that conflict repeats for
// every later batch, which is exactly what the gate is there to stop.
func batchFailureCounts(b importBatch, p *sdk.FAQImportProgress) (failed, total int) {
	total = b.Entries
	failed = b.Failed + b.PartialFailed
	if total == 0 && p != nil && p.Total > 0 {
		total = p.Total
	}
	return failed, total
}

// gateTripped reports whether the failure ratio exceeds the configured gate.
// A gate of 0 disables the check; a batch that failed outright always trips it.
func gateTripped(gatePercent float64, failed, total int) bool {
	if failed == 0 {
		return false
	}
	if gatePercent <= 0 {
		return false
	}
	if total <= 0 {
		return true
	}
	return float64(failed)/float64(total)*100 > gatePercent
}

// checkReplaceExpectedCount refuses a replace whose entry count does not match
// what the knowledge base holds. replace makes the submitted set the whole
// list, so a mismatch means entries the caller did not intend to touch would be
// deleted.
func checkReplaceExpectedCount(ctx context.Context, svc FAQService, kbID string, expected int) error {
	actual, err := countEntries(ctx, svc, kbID)
	if err != nil {
		return err
	}
	if actual != expected {
		return &cmdutil.Error{
			Code: cmdutil.CodeInputInvalidArgument,
			Message: fmt.Sprintf(
				"--mode replace would delete %d entries this file does not contain (the knowledge base has %d, the file %d).\n"+
					"Re-export the knowledge base and edit that file, or use --mode append.",
				maxInt(actual-expected, 0), actual, expected),
		}
	}
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func countEntries(ctx context.Context, svc FAQService, kbID string) (int, error) {
	res, err := svc.ListFAQEntries(ctx, kbID, 1, 1, 0, "", "", "")
	if err != nil {
		return 0, cmdutil.WrapHTTP(err, "count FAQ entries")
	}
	return int(res.Total), nil
}

// snapshotBeforeImport exports the knowledge base beside the payload, and never
// overwrites the first snapshot: a run that starts from a different state keeps
// both, so a later import cannot destroy the only copy of an earlier one.
func snapshotBeforeImport(ctx context.Context, svc FAQService, kbID, payloadPath string) (string, error) {
	entries, err := exportEntries(ctx, svc, kbID)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return "", cmdutil.NewError(cmdutil.CodeOperationFailed, fmt.Sprintf("encode snapshot: %v", err))
	}
	data = append(data, '\n')
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	dir := snapshotDirFor(payloadPath)
	stem := strings.TrimSuffix(filepath.Base(payloadPath), filepath.Ext(payloadPath))
	base := filepath.Join(dir, stem+".snapshot.json")
	if existing, err := os.ReadFile(base); err == nil {
		existingSum := sha256.Sum256(existing)
		if hex.EncodeToString(existingSum[:]) == digest {
			// Same content: the earlier snapshot is this exact baseline, so
			// writing it again would only touch a file that already records it.
			fmt.Fprintf(iostreams.IO.Err, "snapshot unchanged: %s\n", base)
			return base, nil
		}
		// Different content: keep the original untouched and save alongside it.
		base = filepath.Join(dir, fmt.Sprintf("%s.%s.snapshot.json", stem, digest[:8]))
		if _, err := os.Stat(base); err == nil {
			fmt.Fprintf(iostreams.IO.Err, "snapshot already recorded: %s\n", base)
			return base, nil
		}
	}
	if err := writeFileAtomic(base, data); err != nil {
		return "", cmdutil.NewError(cmdutil.CodeOperationFailed, fmt.Sprintf("write snapshot %s: %v", base, err))
	}
	fmt.Fprintf(iostreams.IO.Err, "snapshot of %d existing entries: %s\n", len(entries), base)
	return base, nil
}

func writeReportIfRequested(run *importRun, path string) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return cmdutil.NewError(cmdutil.CodeOperationFailed, fmt.Sprintf("encode report: %v", err))
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data); err != nil {
		return cmdutil.NewError(cmdutil.CodeOperationFailed, fmt.Sprintf("write report %s: %v", path, err))
	}
	return nil
}

// newOperationID labels one run in an audit trail. Random rather than
// time-based so two runs in the same second are still distinguishable.
func newOperationID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("op-%d", time.Now().UnixNano())
	}
	return "op-" + hex.EncodeToString(buf)
}
