package kb

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sdk "github.com/Tencent/WeKnora/client"
)

func TestBuildSchedulePatch(t *testing.T) {
	daily := &ScheduleOptions{Mode: "daily", Time: "02:30", modeSet: true, timeSet: true}
	patch, err := buildSchedulePatch(daily)
	require.NoError(t, err)
	require.NotNil(t, patch.WikiConfig)
	assert.Equal(t, "daily", *patch.WikiConfig.ScheduleMode)
	assert.Equal(t, "02:30", *patch.WikiConfig.ScheduleTime)
	assert.Nil(t, patch.WikiConfig.ScheduleWeekdays, "weekdays stay untouched when not passed")

	weekly := &ScheduleOptions{Mode: "weekly", Weekdays: "1, 3,5", modeSet: true, weekdaysSet: true}
	patch, err = buildSchedulePatch(weekly)
	require.NoError(t, err)
	assert.Equal(t, []int{1, 3, 5}, *patch.WikiConfig.ScheduleWeekdays)
	assert.Nil(t, patch.WikiConfig.ScheduleTime, "time stays untouched when not passed")

	twice := &ScheduleOptions{Mode: "daily", Time: "12:00,18:00", modeSet: true, timeSet: true}
	patch, err = buildSchedulePatch(twice)
	require.NoError(t, err)
	assert.Equal(t, "12:00,18:00", *patch.WikiConfig.ScheduleTime, "a run list reaches the server verbatim")

	// A false-valued patch would have cleared fields; every unset pointer must
	// stay nil so PATCH leaves the stored wiki config alone.
	assert.Nil(t, patch.Name)
	assert.Nil(t, patch.ChunkingConfig)
}

func TestBuildSchedulePatchRejectsBadInput(t *testing.T) {
	cases := []*ScheduleOptions{
		{modeSet: true, Mode: "hourly"},
		{timeSet: true, Time: "9am"},
		{timeSet: true, Time: "24:00"},
		{timeSet: true, Time: "12:00,"},
		{timeSet: true, Time: "12:00,9am"},
		{weekdaysSet: true, Weekdays: "7"},
		{weekdaysSet: true, Weekdays: ""},
		{}, // no flags at all
	}
	for _, opts := range cases {
		if _, err := buildSchedulePatch(opts); err == nil {
			t.Errorf("%+v was accepted", opts)
		}
	}
}

func TestScheduleCmdDryRunNeedsNoClient(t *testing.T) {
	f := kbDryRunFactory(t)
	root := withRootHarness(NewCmdSchedule(f), "kb_abc", "--mode", "daily", "--dry-run", "--format", "json")
	require.NoError(t, root.Execute(), "a valid plan must preview without network or prompt")
}

// The SDK request type is the only way these fields reach the server; a typo in
// the JSON tag would silently drop the setting. Guard the wire names.
func TestPatchWikiConfigJSONTags(t *testing.T) {
	mode, tm := "daily", "02:00"
	days := []int{1, 3}
	body := sdk.PatchWikiConfig{ScheduleMode: &mode, ScheduleTime: &tm, ScheduleWeekdays: &days}
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	raw := string(encoded)
	for _, want := range []string{`"schedule_mode":"daily"`, `"schedule_time":"02:00"`, `"schedule_weekdays":[1,3]`} {
		assert.Contains(t, raw, want)
	}
}
