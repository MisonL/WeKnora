package faq

import (
	"context"
	"errors"
	"fmt"
	"time"

	sdk "github.com/Tencent/WeKnora/client"
	"github.com/spf13/cobra"

	"github.com/Tencent/WeKnora/cli/internal/cmdutil"
	"github.com/Tencent/WeKnora/cli/internal/iostreams"
	"github.com/Tencent/WeKnora/cli/internal/output"
)

// progressFields enumerates what `faq progress --format json` surfaces.
var progressFields = []string{
	"task_id", "kb_id", "status", "progress", "total", "processed",
	"success_count", "failed_count", "partial_failed_count", "skipped_count",
	"merged_count", "added_count", "message", "dry_run",
}

// NewCmdProgress builds `weknora faq progress <task-id>`.
func NewCmdProgress(f *cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "progress <task-id>",
		Short: "Show one FAQ import task's progress",
		Long: `Print the progress record of an FAQ import task.

A completed record describes the batch that finished most recently for the
task, not every batch: the server overwrites the record when a batch completes.
A caller that submits several batches has to wait for each one and read its
result before sending the next -- 'faq import' does that itself.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			fopts, err := cmdutil.CheckFormatFlag(c)
			if err != nil {
				return err
			}
			fopts.ResolveDefault(iostreams.IO.IsStdoutTTY())
			cli, err := f.Client()
			if err != nil {
				return err
			}
			progress, err := cli.GetFAQImportProgress(c.Context(), args[0])
			if err != nil {
				return cmdutil.WrapHTTP(err, "get FAQ import progress")
			}
			return emitProgress(progress, fopts)
		},
	}
	cmdutil.AddFormatFlag(cmd, progressFields...)
	cmdutil.SetAgentHelp(cmd, cmdutil.AgentHelp{
		RequiredFlags: []string{"task-id (positional)"},
		UsedFor:       "Read one FAQ import task's status without waiting for it.",
		Examples:      []string{"weknora faq progress faq_import_abc"},
		Output:        "status is one of pending/running/completed/failed; failed_count counts entries that could not be imported, partial_failed_count counts entries that imported with conflicting questions removed; dry_run marks a validation run that wrote nothing.",
	})
	return cmd
}

func emitProgress(p *sdk.FAQImportProgress, fopts *cmdutil.FormatOptions) error {
	if fopts.WantsJSON() {
		meta := &output.Meta{Count: output.IntPtr(1)}
		return fopts.Emit(iostreams.IO.Out, p, meta)
	}
	w := iostreams.IO.Out
	fmt.Fprintf(w, "task      %s\n", p.TaskID)
	fmt.Fprintf(w, "knowledge base  %s\n", p.KBID)
	fmt.Fprintf(w, "status    %s (%d%%, %d/%d processed)\n", p.Status, p.Progress, p.Processed, p.Total)
	fmt.Fprintf(w, "imported  %d   partial  %d   failed  %d   skipped  %d\n",
		p.SuccessCount, p.PartialFailedCount, p.FailedCount, p.SkippedCount)
	if p.DryRun {
		fmt.Fprintln(w, "dry-run   yes (nothing was written)")
	}
	if p.Error != "" {
		fmt.Fprintf(w, "error     %s\n", p.Error)
	}
	if p.Message != "" {
		fmt.Fprintf(w, "message   %s\n", p.Message)
	}
	return nil
}

// waitOptions captures `faq wait` flag state.
type waitOptions struct {
	Timeout  time.Duration
	Interval time.Duration
}

// NewCmdWait builds `weknora faq wait <task-id>`.
//
// The exit codes match `doc wait`, so an agent that already handles document
// waits needs no second table.
func NewCmdWait(f *cmdutil.Factory) *cobra.Command {
	opts := &waitOptions{}
	cmd := &cobra.Command{
		Use:   "wait <task-id>",
		Short: "Wait for a FAQ import task to finish",
		Long: `Block until the import task reaches a terminal state (completed or failed),
the timeout expires, or the user interrupts (Ctrl-C).

Exit codes:
  0    completed with every entry imported as submitted
  1    failed, or completed with entries that could not be imported or lost
       conflicting similar/negative questions
  124  --timeout reached (matches GNU 'timeout')
  130  Ctrl-C / SIGINT`,
		Example: `  weknora faq wait faq_import_abc
  weknora faq wait faq_import_abc --timeout 20m`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			fopts, err := cmdutil.CheckFormatFlag(c)
			if err != nil {
				return err
			}
			fopts.ResolveDefault(iostreams.IO.IsStdoutTTY())
			cli, err := f.Client()
			if err != nil {
				return err
			}
			p, err := waitForTask(c.Context(), cli, args[0], *opts)
			if err != nil {
				return err
			}
			if err := emitProgress(p, fopts); err != nil {
				return err
			}
			switch exitCodeFor(p) {
			case 0:
				return nil
			default:
				return silentExit(1, fmt.Sprintf(
					"FAQ import %s ended with %d failed and %d partially failed entries",
					p.TaskID, p.FailedCount, p.PartialFailedCount))
			}
		},
	}
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", 30*time.Minute, "Give up after this long (0 = wait indefinitely)")
	cmd.Flags().DurationVar(&opts.Interval, "interval", 2*time.Second, "Poll interval")
	cmdutil.SetAgentHelp(cmd, cmdutil.AgentHelp{
		RequiredFlags: []string{"task-id (positional)"},
		UsedFor:       "Block until an FAQ import finishes; the exit code is the outcome.",
		Examples:      []string{"weknora faq wait faq_import_abc --timeout 20m"},
		Output:        "The final progress record, then exit 0/1/124/130 for completed/failed/timeout/interrupted.",
	})
	return cmd
}

// errWaitBudgetExhausted marks a poll the wait budget cut off. It is a timeout
// of the caller's own making, not a transport failure: the server may well be
// answering, just not before the deadline.
var errWaitBudgetExhausted = errors.New("wait budget exhausted")

// getFAQProgress asks once for a task's progress, clipping the request itself
// to the remaining budget. Clipping only the sleeps is not enough: one poll
// that blocks past the deadline outlives the timeout the caller asked for, and
// the command would only notice after that request finally returned.
func getFAQProgress(
	ctx context.Context, svc FAQService, taskID string, deadline time.Time,
) (*sdk.FAQImportProgress, error) {
	reqCtx := ctx
	cancel := func() {}
	if !deadline.IsZero() {
		reqCtx, cancel = context.WithDeadline(ctx, deadline)
	}
	p, err := svc.GetFAQImportProgress(reqCtx, taskID)
	cancel()
	if err != nil {
		if !deadline.IsZero() && errors.Is(err, context.DeadlineExceeded) {
			return nil, errWaitBudgetExhausted
		}
		return nil, err
	}
	return p, nil
}

// waitForTask polls until the task is terminal, the budget runs out, or the
// caller's context is cancelled.
func waitForTask(ctx context.Context, svc FAQService, taskID string, opts waitOptions) (*sdk.FAQImportProgress, error) {
	deadline := time.Time{}
	if opts.Timeout > 0 {
		deadline = time.Now().Add(opts.Timeout)
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		p, err := getFAQProgress(ctx, svc, taskID, deadline)
		if err != nil {
			if errors.Is(err, errWaitBudgetExhausted) {
				return nil, silentExit(124, fmt.Sprintf("wait timed out; the task may still be running (id: %s)", taskID))
			}
			return nil, cmdutil.WrapHTTP(err, "get FAQ import progress")
		}
		if p != nil && (p.Status == "completed" || p.Status == "failed") {
			return p, nil
		}
		sleep := interval
		// Both the sleep and the next request are clipped to the remaining
		// budget: a timeout that already passed must not be extended by one
		// more poll.
		if !deadline.IsZero() {
			left := time.Until(deadline)
			if left <= 0 {
				return nil, silentExit(124, fmt.Sprintf("wait timed out; the task may still be running (id: %s)", taskID))
			}
			if sleep > left {
				sleep = left
			}
		}
		select {
		case <-ctx.Done():
			return nil, silentExit(124, fmt.Sprintf("interrupted; the task may still be running (id: %s)", taskID))
		case <-time.After(sleep):
		}
	}
}
