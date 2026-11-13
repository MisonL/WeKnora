package kb

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Tencent/WeKnora/cli/internal/cmdutil"
	"github.com/Tencent/WeKnora/cli/internal/iostreams"
	sdk "github.com/Tencent/WeKnora/client"
)

// kbScheduleFields is what `--format json` surfaces: the values that were sent,
// echoed back so a script can read the resulting schedule without a second call.
var kbScheduleFields = []string{"knowledge_base_id", "schedule_mode", "schedule_time", "schedule_weekdays"}

// ScheduleOptions carries the flags. Strings (not pointers) with a Changed check
// so an omitted flag keeps the stored value.
type ScheduleOptions struct {
	Mode        string
	Time        string
	Weekdays    string
	Yes         bool
	DryRun      bool
	modeSet     bool
	timeSet     bool
	weekdaysSet bool
}

// ScheduleService is the narrow SDK surface this command depends on.
type ScheduleService interface {
	PatchKnowledgeBase(ctx context.Context, kbID string, req *sdk.PatchKnowledgeBaseRequest) (*sdk.KnowledgeBase, error)
}

// NewCmdSchedule builds `weknora kb schedule <kb-id>`, the write command for a
// knowledge base's Wiki generation window.
func NewCmdSchedule(f *cmdutil.Factory) *cobra.Command {
	opts := &ScheduleOptions{}
	cmd := &cobra.Command{
		Use:   "schedule <kb-id>",
		Short: "Set when Wiki pages are generated (on change, daily, weekly or manual)",
		Long: `Set the Wiki generation schedule for a knowledge base.

  --mode on_change   regenerate shortly after each document (the historical behaviour)
  --mode daily       collect changes and run once a day at --time (default 02:00)
  --mode weekly      run on the chosen --weekdays at --time
  --mode manual      stop background generation; pages only via the manual APIs

--time is HH:MM in the server's time zone; --weekdays is a comma-separated list
with Sunday=0 (for example 1,3,5 for Mon/Wed/Fri). Omitted flags keep their
stored value. Changes take effect within a couple of minutes; no restart.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			fopts, err := cmdutil.CheckFormatFlag(c)
			if err != nil {
				return err
			}
			fopts.ResolveDefault(iostreams.IO.IsStdoutTTY())
			opts.Yes, _ = c.Flags().GetBool("yes")
			opts.modeSet = c.Flag("mode").Changed
			opts.timeSet = c.Flag("time").Changed
			opts.weekdaysSet = c.Flag("weekdays").Changed
			kbID := args[0]

			patch, err := buildSchedulePatch(opts)
			if err != nil {
				return err
			}
			if handled, err := cmdutil.HandleDryRun(c, opts.DryRun, cmdutil.DryRunPlan{
				Action: "kb.schedule",
				Args:   schedulePlanArgs(kbID, opts),
			}); handled {
				return err
			}
			cli, err := f.Client()
			if err != nil {
				return err
			}
			if err := cmdutil.ConfirmWrite(f.Prompter(), opts.Yes, fopts.WantsJSON(),
				"schedule", "knowledge base", kbID, "kb.schedule",
				cmdutil.BuildRetryArgv(c, []string{"weknora", "kb", "schedule", kbID},
					"mode", "time", "weekdays", "format")); err != nil {
				return err
			}
			return runSchedule(c.Context(), opts, fopts, cli, kbID, patch)
		},
	}
	cmd.Flags().StringVar(&opts.Mode, "mode", "", "on_change | daily | weekly | manual")
	cmd.Flags().StringVar(&opts.Time, "time", "",
		"HH:MM local time for daily/weekly (default 02:00); comma-separate several runs, e.g. 12:00,18:00")
	cmd.Flags().StringVar(&opts.Weekdays, "weekdays", "", "comma-separated weekdays for weekly, Sunday=0 (e.g. 1,3,5)")
	cmdutil.AddFormatFlag(cmd, kbScheduleFields...)
	cmdutil.AddDryRunFlag(cmd, &opts.DryRun)
	cmdutil.SetWriteRisk(cmd, "kb.schedule")
	cmdutil.SetAgentHelp(cmd, cmdutil.AgentHelp{
		UsedFor:       "set a knowledge base's Wiki generation schedule (on_change / daily / weekly / manual)",
		RequiredFlags: []string{"<kb-id> (positional)", "at least one of --mode / --time / --weekdays"},
		Output:        "envelope.data echoes {knowledge_base_id, schedule_mode, schedule_time, schedule_weekdays}",
		Examples: []string{
			"weknora kb schedule kb_abc --mode daily --time 02:00 -y",
			"weknora kb schedule kb_abc --mode daily --time 12:00,18:00 -y",
			"weknora kb schedule kb_abc --mode weekly --weekdays 1,3,5 --time 22:30 -y",
			"weknora kb schedule kb_abc --mode on_change -y",
		},
		Warnings: []string{
			"Requires explicit user approval (exit 10 / input.confirmation_required); never auto-add -y.",
			"weekly without --weekdays is rejected by the server; use --mode daily instead.",
			"daily/weekly hold uploads until the window opens: documents finish processing but their Wiki pages are not written until then.",
		},
	})
	return cmd
}

func buildSchedulePatch(opts *ScheduleOptions) (*sdk.PatchKnowledgeBaseRequest, error) {
	if !opts.modeSet && !opts.timeSet && !opts.weekdaysSet {
		return nil, &cmdutil.Error{
			Code:    cmdutil.CodeInputMissingFlag,
			Message: "kb schedule requires at least one of --mode / --time / --weekdays",
			Hint:    "pass --mode daily --time 02:00, for example",
		}
	}
	wiki := &sdk.PatchWikiConfig{}
	if opts.modeSet {
		mode := strings.TrimSpace(opts.Mode)
		switch mode {
		case "on_change", "daily", "weekly", "manual":
			wiki.ScheduleMode = &mode
		default:
			return nil, &cmdutil.Error{
				Code:    cmdutil.CodeInputInvalidArgument,
				Message: fmt.Sprintf("--mode must be one of on_change, daily, weekly, manual (got %q)", opts.Mode),
			}
		}
	}
	if opts.timeSet {
		t := strings.TrimSpace(opts.Time)
		if !validClockList(t) {
			return nil, &cmdutil.Error{
				Code:    cmdutil.CodeInputInvalidArgument,
				Message: fmt.Sprintf("--time must be HH:MM in 24-hour form (got %q)", opts.Time),
			}
		}
		wiki.ScheduleTime = &t
	}
	if opts.weekdaysSet {
		days, err := parseWeekdays(opts.Weekdays)
		if err != nil {
			return nil, err
		}
		wiki.ScheduleWeekdays = &days
	}
	return &sdk.PatchKnowledgeBaseRequest{WikiConfig: wiki}, nil
}

// validClockList accepts "HH:MM" or a comma-separated list of them, matching
// the server's schedule_time contract.
func validClockList(raw string) bool {
	if raw == "" {
		return false
	}
	for _, part := range strings.Split(raw, ",") {
		if _, _, ok := splitClock(strings.TrimSpace(part)); !ok {
			return false
		}
	}
	return true
}

func splitClock(raw string) (hour, minute int, ok bool) {
	h, m, found := strings.Cut(raw, ":")
	if !found {
		return 0, 0, false
	}
	var errH, errM error
	hour, errH = strconv.Atoi(strings.TrimSpace(h))
	minute, errM = strconv.Atoi(strings.TrimSpace(m))
	if errH != nil || errM != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, false
	}
	return hour, minute, true
}

func parseWeekdays(raw string) ([]int, error) {
	out := make([]int, 0, 7)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		day, err := strconv.Atoi(part)
		if err != nil || day < 0 || day > 6 {
			return nil, &cmdutil.Error{
				Code:    cmdutil.CodeInputInvalidArgument,
				Message: fmt.Sprintf("--weekdays entries must be 0-6 with Sunday=0 (got %q)", part),
			}
		}
		out = append(out, day)
	}
	if len(out) == 0 {
		return nil, &cmdutil.Error{
			Code:    cmdutil.CodeInputInvalidArgument,
			Message: "--weekdays needs at least one day",
		}
	}
	return out, nil
}

func schedulePlanArgs(kbID string, opts *ScheduleOptions) map[string]any {
	args := map[string]any{"kb": kbID}
	if opts.modeSet {
		args["schedule_mode"] = opts.Mode
	}
	if opts.timeSet {
		args["schedule_time"] = opts.Time
	}
	if opts.weekdaysSet {
		args["schedule_weekdays"] = opts.Weekdays
	}
	return args
}

func runSchedule(
	ctx context.Context, opts *ScheduleOptions, fopts *cmdutil.FormatOptions,
	svc ScheduleService, kbID string, patch *sdk.PatchKnowledgeBaseRequest,
) error {
	if _, err := svc.PatchKnowledgeBase(ctx, kbID, patch); err != nil {
		return cmdutil.WrapHTTP(err, "set schedule for knowledge base %s", kbID)
	}
	out := map[string]any{"knowledge_base_id": kbID}
	if patch.WikiConfig != nil {
		if patch.WikiConfig.ScheduleMode != nil {
			out["schedule_mode"] = *patch.WikiConfig.ScheduleMode
		}
		if patch.WikiConfig.ScheduleTime != nil {
			out["schedule_time"] = *patch.WikiConfig.ScheduleTime
		}
		if patch.WikiConfig.ScheduleWeekdays != nil {
			out["schedule_weekdays"] = *patch.WikiConfig.ScheduleWeekdays
		}
	}
	if fopts.WantsJSON() {
		return fopts.Emit(iostreams.IO.Out, out, nil)
	}
	fmt.Fprintf(iostreams.IO.Out, "✓ Updated Wiki schedule for knowledge base %s\n", kbID)
	return nil
}
