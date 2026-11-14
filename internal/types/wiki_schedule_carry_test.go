package types

import "testing"

// A PUT replaces wiki_config wholesale, and clients that predate the schedule
// switch send it without schedule fields. The service treats an empty mode as
// "not carried" and must therefore be able to tell it apart from an explicit
// on_change: this pins the contract the carry-over relies on.
func TestWikiScheduleModeEmptyMeansAbsent(t *testing.T) {
	var zero WikiScheduleMode
	if zero != "" {
		t.Fatalf("zero value changed: %q", zero)
	}
	empty := WikiConfig{}
	if empty.Schedule() != WikiScheduleOnChange {
		t.Fatal("an absent mode must still resolve to on_change for legacy configs")
	}
	explicit := WikiConfig{ScheduleMode: WikiScheduleOnChange}
	if explicit.ScheduleMode == "" {
		t.Fatal("an explicit on_change must not look absent")
	}
}
