package types

import (
	"fmt"
	"testing"
)

func TestValidateWikiScheduleAcceptsValid(t *testing.T) {
	cases := []*WikiConfig{
		nil,
		{},
		{ScheduleMode: WikiScheduleOnChange},
		{ScheduleMode: WikiScheduleDaily, ScheduleTime: "02:00"},
		{ScheduleMode: WikiScheduleDaily},
		{ScheduleMode: WikiScheduleDaily, ScheduleTime: "12:00,18:00"},
		{ScheduleMode: WikiScheduleWeekly, ScheduleWeekdays: []int{1, 3}},
		{ScheduleMode: WikiScheduleWeekly, ScheduleTime: "09:00,21:00", ScheduleWeekdays: []int{5}},
		{ScheduleMode: WikiScheduleManual},
	}
	for i, cfg := range cases {
		if err := ValidateWikiSchedule(cfg); err != nil {
			t.Errorf("case %d (%+v) rejected: %v", i, cfg, err)
		}
	}
}

func TestValidateWikiScheduleRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		cfg  *WikiConfig
	}{
		{"unknown mode", &WikiConfig{ScheduleMode: "hourly"}},
		{"bad time", &WikiConfig{ScheduleMode: WikiScheduleDaily, ScheduleTime: "9am"}},
		{"empty list entry", &WikiConfig{ScheduleMode: WikiScheduleDaily, ScheduleTime: "12:00,,18:00"}},
		{"one bad time in the list", &WikiConfig{ScheduleMode: WikiScheduleDaily, ScheduleTime: "12:00,25:00"}},
		{"too many runs", &WikiConfig{ScheduleMode: WikiScheduleDaily, ScheduleTime: "00:00,01:00,02:00,03:00,04:00,05:00,06:00,07:00,08:00,09:00,10:00,11:00,12:00"}},
		{"hour out of range", &WikiConfig{ScheduleMode: WikiScheduleDaily, ScheduleTime: "24:00"}},
		{"minute out of range", &WikiConfig{ScheduleMode: WikiScheduleDaily, ScheduleTime: "10:75"}},
		{"weekday out of range", &WikiConfig{ScheduleMode: WikiScheduleWeekly, ScheduleWeekdays: []int{7}}},
		{"weekly without weekdays", &WikiConfig{ScheduleMode: WikiScheduleWeekly}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateWikiSchedule(tc.cfg); err == nil {
				t.Errorf("%+v was accepted", tc.cfg)
			}
		})
	}
}

func TestWikiConfigTimeOfDayList(t *testing.T) {
	cases := []struct {
		raw  string
		want [][2]int
		ok   bool
	}{
		{"", [][2]int{{2, 0}}, true},
		{"12:00,18:00", [][2]int{{12, 0}, {18, 0}}, true},
		{"18:00, 12:00", [][2]int{{18, 0}, {12, 0}}, true},
		{"12:00,12:00", [][2]int{{12, 0}}, true},
		{"12:00,", nil, false},
		{"a:b", nil, false},
	}
	for _, tc := range cases {
		got, ok := (&WikiConfig{ScheduleTime: tc.raw}).TimeOfDayList()
		if ok != tc.ok || (ok && fmt.Sprint(got) != fmt.Sprint(tc.want)) {
			t.Errorf("TimeOfDayList(%q) = %v ok=%v, want %v ok=%v", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

func TestWikiConfigTimeOfDay(t *testing.T) {
	cases := []struct {
		raw  string
		h, m int
		ok   bool
	}{
		{"", 2, 0, true},
		{"00:00", 0, 0, true},
		{"23:59", 23, 59, true},
		{" 9:05 ", 9, 5, true},
		{"12", 0, 0, false},
		{"-1:00", 0, 0, false},
		{"a:b", 0, 0, false},
	}
	for _, tc := range cases {
		cfg := &WikiConfig{ScheduleTime: tc.raw}
		h, m, ok := cfg.TimeOfDay()
		if ok != tc.ok || (ok && (h != tc.h || m != tc.m)) {
			t.Errorf("TimeOfDay(%q) = %d:%d ok=%v, want %d:%d ok=%v",
				tc.raw, h, m, ok, tc.h, tc.m, tc.ok)
		}
	}
}
