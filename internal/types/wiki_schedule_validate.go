package types

import (
	"fmt"
	"strings"
)

// MaxWikiRunsPerDay bounds how many times of day one knowledge base may
// schedule. Each run owns a cron entry; a runaway list would turn a nightly
// synthesis into a constant drain.
const MaxWikiRunsPerDay = 12

// ValidateWikiSchedule rejects a schedule the scheduler cannot run, so an
// invalid value fails the update instead of being stored and quietly skipped
// on every reconcile pass.
func ValidateWikiSchedule(cfg *WikiConfig) error {
	if cfg == nil {
		return nil
	}
	switch cfg.ScheduleMode {
	case "", WikiScheduleOnChange, WikiScheduleDaily, WikiScheduleWeekly, WikiScheduleManual:
	default:
		return fmt.Errorf("schedule_mode must be one of on_change, daily, weekly, manual (got %q)", cfg.ScheduleMode)
	}
	if strings.TrimSpace(cfg.ScheduleTime) != "" {
		if _, ok := cfg.TimeOfDayList(); !ok {
			return fmt.Errorf("schedule_time must be HH:MM or a comma-separated list of HH:MM in 24-hour form (got %q)",
				cfg.ScheduleTime)
		}
		if runs := strings.Count(cfg.ScheduleTime, ",") + 1; runs > MaxWikiRunsPerDay {
			return fmt.Errorf("schedule_time lists %d runs a day; at most %d are supported",
				runs, MaxWikiRunsPerDay)
		}
	}
	for _, d := range cfg.ScheduleWeekdays {
		if d < 0 || d > 6 {
			return fmt.Errorf("schedule_weekdays entries must be 0-6 with Sunday=0 (got %d)", d)
		}
	}
	// Weekly with no weekday selected runs every day, which is daily mode with
	// an extra concept. Say so rather than silently treating the two the same.
	if cfg.ScheduleMode == WikiScheduleWeekly && len(cfg.ScheduleWeekdays) == 0 {
		return fmt.Errorf("schedule_weekdays is required for weekly mode; use daily when every day should run")
	}
	return nil
}
