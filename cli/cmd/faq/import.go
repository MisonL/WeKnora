package faq

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	sdk "github.com/Tencent/WeKnora/client"
	"github.com/spf13/cobra"

	"github.com/Tencent/WeKnora/cli/internal/cmdutil"
	"github.com/Tencent/WeKnora/cli/internal/iostreams"
	"github.com/Tencent/WeKnora/cli/internal/output"
)

// Import limits mirror the server's default budgets. The server refuses an
// over-limit request before parsing it, so a batch is sized to stay under both.
// They are variables so a deployment that raised its caps can raise these too.
var (
	// importMaxEntries is the server's WEKNORA_FAQ_IMPORT_MAX_ENTRIES default.
	importMaxEntries = 1000
	// importMaxBodyBytes is the server's WEKNORA_FAQ_IMPORT_MAX_JSON_BYTES
	// default. The envelope ({"mode":...,"entries":[...]}) counts toward it.
	importMaxBodyBytes = int64(32 << 20)
)

// importFailureGate is the fraction of a batch that may fail before the run
// stops. A handful of bad rows is worth continuing past; a large fraction means
// the file or the tag mapping is wrong, and every later batch would repeat it.
const defaultFailureGatePercent = 5.0

type importOptions struct {
	KB          string
	Mode        string
	DryRun      bool
	BatchSize   int
	FailureGate float64
	Snapshot    bool
	Report      string
	Wait        bool
	Timeout     time.Duration
}

// NewCmdImport builds `weknora faq import <payload.json>`.
func NewCmdImport(f *cmdutil.Factory) *cobra.Command {
	opts := &importOptions{}
	cmd := &cobra.Command{
		Use:   "import <payload.json>",
		Short: "Import FAQ entries from a JSON array or payload file",
		Long: `Import FAQ entries, applying the server's rules so a caller does not have to.

The file is either a bare JSON array of entries or a complete payload
({"mode": ..., "entries": [...]}); --mode wins over a mode inside the file.

What this command guarantees:

  * Batches stay under the CLI's built-in per-request entry (1000) and byte
    (32 MiB) budgets, counting the JSON envelope; the server applies its own
    caps to the same figures. A single entry too large for one request is
    refused rather than sent to fail server-side.
  * Batches are strictly serial: the next one is submitted only after the
    previous reports completed. The server's per-knowledge-base admission is
    atomic now, but a second import would still be refused, and a refused
    batch would lose the run.
  * A batch whose failure ratio exceeds --failure-gate stops the run.
  * replace is never split. More than one batch under replace is refused
    before anything is sent, because each batch would delete everything the
    previous one created. A single-batch replace checks the expected count
    against the server's list first.
  * A snapshot of the current entries is taken before writing, unless
    --snapshot=false. The first snapshot is never overwritten: a later run
    with different content saves alongside it with a hash suffix.

Dry-run sends the same batches the real import would, and reports what the
server would do. Its record carries the counts the validation itself produced.

The failure gate counts a partial failure -- an entry that imported but lost
similar or negative questions to a conflict -- together with outright
failures: neither landed as written, and a large share of either usually means
the file or the tag mapping is wrong rather than a few bad rows. The report
keeps the two apart, and so does the summary line.

Exit codes: 0 nothing failed or partially failed -- or the single batch was
submitted with --wait=false and its outcome belongs to the returned task id
(a --wait=false run that needs more than one batch is refused with exit 5,
because the server admits one import per knowledge base at a time), 1 a batch
failed or the gate stopped the run (including partially imported entries), 124
timeout while waiting, 130 interrupted.`,
		Example: `  weknora faq import faq.json --kb kb_abc
  weknora faq import faq.json --kb kb_abc --mode replace --dry-run
  weknora faq import faq.json --kb kb_abc --batch-size 500 --report run.json`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			fopts, err := cmdutil.CheckFormatFlag(c)
			if err != nil {
				return err
			}
			fopts.ResolveDefault(iostreams.IO.IsStdoutTTY())
			if err := validateImportOptions(opts); err != nil {
				return err
			}
			cli, err := f.Client()
			if err != nil {
				return err
			}
			run, err := runImport(c.Context(), cli, args[0], *opts)
			if err != nil {
				return err
			}
			if err := emitImportRun(run, fopts); err != nil {
				return err
			}
			if code := run.exitCode(); code != 0 {
				return silentExit(code, run.summary())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.KB, "kb", "", "Knowledge base id (required)")
	cmd.Flags().StringVar(&opts.Mode, "mode", "", "append or replace (default: the file's mode, else append)")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "Validate without writing")
	cmd.Flags().IntVar(&opts.BatchSize, "batch-size", 1000, "Entries per request (built-in cap 1000; does not query the server cap)")
	cmd.Flags().Float64Var(&opts.FailureGate, "failure-gate", defaultFailureGatePercent, "Stop the run once a batch's failure ratio (failed + partial) exceeds this percentage (0 disables)")
	cmd.Flags().BoolVar(&opts.Snapshot, "snapshot", true, "Export the current entries before writing, so the run can be undone")
	cmd.Flags().StringVar(&opts.Report, "report", "", "Write a run report (task ids, hashes, outcome) to this file")
	cmd.Flags().BoolVar(&opts.Wait, "wait", true, "Wait for each batch to finish before submitting the next")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", 30*time.Minute, "Give up waiting after this long (0 = no limit)")
	cmdutil.SetAgentHelp(cmd, cmdutil.AgentHelp{
		RequiredFlags: []string{"payload (positional)"},
		UsedFor:       "Import FAQ entries with batching, ordering and safety checks applied.",
		Examples:      []string{"weknora faq import faq.json --kb kb_abc --dry-run"},
		Output:        "A run summary: batches sent, entries imported/failed, the snapshot path, and exit 0/1/5/124/130.",
	})
	return cmd
}

func validateImportOptions(opts *importOptions) error {
	if opts.KB == "" {
		return &cmdutil.Error{Code: cmdutil.CodeInputInvalidArgument, Message: "--kb is required"}
	}
	switch opts.Mode {
	case "", "append", "replace":
	default:
		return &cmdutil.Error{Code: cmdutil.CodeInputInvalidArgument, Message: "--mode must be append or replace"}
	}
	if opts.BatchSize < 1 {
		return &cmdutil.Error{Code: cmdutil.CodeInputInvalidArgument, Message: "--batch-size must be at least 1"}
	}
	if opts.FailureGate < 0 || opts.FailureGate > 100 {
		return &cmdutil.Error{Code: cmdutil.CodeInputInvalidArgument, Message: "--failure-gate must be between 0 and 100"}
	}
	return nil
}

// importRun is the record of one import invocation. It is both the command's
// output and the --report file, so what an operator reads is what an audit log
// keeps.
type importRun struct {
	KBID        string        `json:"kb_id"`
	Mode        string        `json:"mode"`
	DryRun      bool          `json:"dry_run"`
	PayloadPath string        `json:"payload_path"`
	PayloadSHA  string        `json:"payload_sha256"`
	Total       int           `json:"total_entries"`
	Snapshot    string        `json:"snapshot,omitempty"`
	BaseURL     string        `json:"base_url,omitempty"`
	Profile     string        `json:"profile,omitempty"`
	OperationID string        `json:"operation_id"`
	StartedAt   string        `json:"started_at"`
	Batches     []importBatch `json:"batches"`
	Stopped     string        `json:"stopped,omitempty"`
}

type importBatch struct {
	Index    int    `json:"index"`
	Entries  int    `json:"entries"`
	TaskID   string `json:"task_id,omitempty"`
	Status   string `json:"status"`
	Imported int    `json:"imported"`
	Failed   int    `json:"failed"`
	// PartialFailed counts entries that imported with conflicting similar or
	// negative questions removed. They are not in Imported or Failed, so a
	// batch's own numbers only add up once this is included.
	PartialFailed int    `json:"partial_failed,omitempty"`
	Skipped       int    `json:"skipped"`
	Error         string `json:"error,omitempty"`
}

func (r *importRun) totals() (imported, failed, partial, skipped int) {
	for _, b := range r.Batches {
		imported += b.Imported
		failed += b.Failed
		partial += b.PartialFailed
		skipped += b.Skipped
	}
	return
}

func (r *importRun) exitCode() int {
	switch r.Stopped {
	case "timeout":
		// Conventional "the wait ran out, the work may still be running".
		return 124
	case "not_waited":
		// The caller chose --wait=false: the submission succeeded and the run
		// reports the task id. Later outcomes belong to the task, so this is
		// not a failure of the command.
		return 0
	}
	// submit_failed / batch_failed: the batch record carries the failure, and
	// its status is not "completed", so the loop below reports it as exit 1.
	for _, b := range r.Batches {
		if b.Status != "completed" {
			return 1
		}
	}
	if r.Stopped == "gate" {
		return 1
	}
	_, failed, partial, _ := r.totals()
	if failed+partial > 0 {
		return 1
	}
	return 0
}

func (r *importRun) summary() string {
	imported, failed, partial, skipped := r.totals()
	where := fmt.Sprintf("%d imported, %d failed, %d partial, %d skipped across %d batch(es)",
		imported, failed, partial, skipped, len(r.Batches))
	if r.Stopped != "" {
		return fmt.Sprintf("import stopped (%s): %s", r.Stopped, where)
	}
	return "import finished: " + where
}

// readImportPayload parses the file into entries plus the mode the file states.
func readImportPayload(path string) ([]sdk.FAQEntryPayload, string, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", "", &cmdutil.Error{Code: cmdutil.CodeInputInvalidArgument, Message: fmt.Sprintf("read %s: %v", path, err)}
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])

	var entries []sdk.FAQEntryPayload
	var mode string
	if err := json.Unmarshal(raw, &entries); err != nil {
		var payload sdk.FAQBatchUpsertPayload
		if err2 := json.Unmarshal(raw, &payload); err2 != nil {
			return nil, "", "", &cmdutil.Error{
				Code:    cmdutil.CodeInputInvalidArgument,
				Message: fmt.Sprintf("%s is neither a JSON array of entries nor a payload object: %v", path, err2),
			}
		}
		entries = payload.Entries
		mode = payload.Mode
	}
	if len(entries) == 0 {
		return nil, "", "", &cmdutil.Error{Code: cmdutil.CodeInputInvalidArgument, Message: "the file contains no entries"}
	}
	return entries, mode, digest, nil
}

// chunkEntries splits entries into batches under both budgets.
//
// The byte check measures the serialized envelope, not the entries alone: the
// server's budget counts what it receives, and a chunk that fits entry-wise but
// not byte-wise would be refused after the previous batches had already
// committed.
func chunkEntries(entries []sdk.FAQEntryPayload, mode string, maxEntries int, maxBytes int64) ([][]sdk.FAQEntryPayload, error) {
	var out [][]sdk.FAQEntryPayload
	var current []sdk.FAQEntryPayload
	flush := func() {
		if len(current) > 0 {
			out = append(out, current)
			current = nil
		}
	}
	for i, e := range entries {
		current = append(current, e)
		size, err := envelopeSize(current, mode)
		if err != nil {
			return nil, err
		}
		if int64(size) <= maxBytes && len(current) <= maxEntries {
			continue
		}
		if len(current) == 1 {
			// One entry alone over the budget can never be sent, and splitting
			// it further is not possible.
			return nil, &cmdutil.Error{
				Code: cmdutil.CodeInputInvalidArgument,
				Message: fmt.Sprintf("entry %d alone serializes to %d bytes, over the %d byte request budget",
					i+1, size, maxBytes),
			}
		}
		// The entry that overflowed starts the next batch. Its size is
		// measured again for that batch: the check above tested it together
		// with the entries that are now being flushed.
		current = current[:len(current)-1]
		flush()
		current = append(current, e)
		alone, err := envelopeSize(current, mode)
		if err != nil {
			return nil, err
		}
		if int64(alone) > maxBytes {
			return nil, &cmdutil.Error{
				Code:    cmdutil.CodeInputInvalidArgument,
				Message: fmt.Sprintf("entry %d alone serializes to %d bytes, over the %d byte request budget", i+1, alone, maxBytes),
			}
		}
	}
	flush()
	return out, nil
}

func envelopeSize(entries []sdk.FAQEntryPayload, mode string) (int, error) {
	data, err := json.Marshal(sdk.FAQBatchUpsertPayload{Mode: mode, Entries: entries})
	if err != nil {
		return 0, &cmdutil.Error{Code: cmdutil.CodeInputInvalidArgument, Message: fmt.Sprintf("encode batch: %v", err)}
	}
	return len(data), nil
}

func emitImportRun(run *importRun, fopts *cmdutil.FormatOptions) error {
	if fopts.WantsJSON() {
		meta := &output.Meta{Count: output.IntPtr(len(run.Batches))}
		return fopts.Emit(iostreams.IO.Out, run, meta)
	}
	w := iostreams.IO.Out
	fmt.Fprintf(w, "knowledge base  %s   mode %s%s\n", run.KBID, run.Mode, dryRunLabel(run.DryRun))
	if run.Snapshot != "" {
		fmt.Fprintf(w, "snapshot        %s\n", run.Snapshot)
	}
	fmt.Fprintf(w, "payload         %s (%d entries, sha256 %s)\n", run.PayloadPath, run.Total, shortHash(run.PayloadSHA))
	for _, b := range run.Batches {
		status := b.Status
		if b.TaskID != "" {
			status += " " + b.TaskID
		}
		fmt.Fprintf(w, "  batch %d/%d  %4d entries  %-10s imported %d  partial %d  failed %d  skipped %d\n",
			b.Index+1, len(run.Batches), b.Entries, status, b.Imported, b.PartialFailed, b.Failed, b.Skipped)
	}
	fmt.Fprintf(w, "%s\n", run.summary())
	if run.Stopped != "" {
		fmt.Fprintf(w, "stopped: %s\n", run.Stopped)
	}
	return nil
}

func dryRunLabel(dry bool) string {
	if dry {
		return " (dry-run, nothing written)"
	}
	return ""
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// snapshotDirFor is where run snapshots go: beside the payload, so a backup
// travels with the input it was taken for.
func snapshotDirFor(payloadPath string) string {
	return filepath.Dir(payloadPath)
}
