package wikischedule

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestWindowKeySeparatesRunsOfTheSameDay(t *testing.T) {
	cfg := &types.WikiConfig{ScheduleMode: types.WikiScheduleDaily, ScheduleTime: "12:00,18:00"}
	noon := time.Date(2026, 11, 13, 12, 0, 0, 0, time.Local)
	evening := time.Date(2026, 11, 13, 18, 0, 0, 0, time.Local)
	if windowKey("kb", noon, cfg) == windowKey("kb", evening, cfg) {
		t.Error("the noon and evening runs need separate task ids")
	}
	// A tick that fires a minute late still belongs to its own run.
	late := time.Date(2026, 11, 13, 12, 1, 0, 0, time.Local)
	if windowKey("kb", noon, cfg) != windowKey("kb", late, cfg) {
		t.Error("a slightly late tick must collapse onto its run")
	}
}

func TestCronSpec(t *testing.T) {
	cases := []struct {
		name string
		cfg  *types.WikiConfig
		want string
	}{
		{"daily default time", &types.WikiConfig{ScheduleMode: types.WikiScheduleDaily}, "0 2 * * *"},
		{"daily custom time", &types.WikiConfig{
			ScheduleMode: types.WikiScheduleDaily, ScheduleTime: "09:30",
		}, "30 9 * * *"},
		{"weekly weekdays", &types.WikiConfig{
			ScheduleMode:     types.WikiScheduleWeekly,
			ScheduleTime:     "18:05",
			ScheduleWeekdays: []int{1, 3, 5},
		}, "5 18 * * 1,3,5"},
		{"two times a day", &types.WikiConfig{
			ScheduleMode: types.WikiScheduleDaily, ScheduleTime: "12:00,18:00",
		}, "0 12 * * *;0 18 * * *"},
		{"weekly two times", &types.WikiConfig{
			ScheduleMode: types.WikiScheduleWeekly, ScheduleTime: "12:00, 18:30", ScheduleWeekdays: []int{1, 4},
		}, "0 12 * * 1,4;30 18 * * 1,4"},
		{"duplicate times collapse", &types.WikiConfig{
			ScheduleMode: types.WikiScheduleDaily, ScheduleTime: "12:00,12:00",
		}, "0 12 * * *"},
		{"weekly without weekdays means every day", &types.WikiConfig{
			ScheduleMode: types.WikiScheduleWeekly,
		}, "0 2 * * 0,1,2,3,4,5,6"},
		{"weekday values out of range are dropped", &types.WikiConfig{
			ScheduleMode: types.WikiScheduleWeekly, ScheduleWeekdays: []int{-1, 9, 2},
		}, "0 2 * * 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CronSpecs(tc.cfg)
			if err != nil {
				t.Fatalf("CronSpecs: %v", err)
			}
			if want := strings.Split(tc.want, ";"); strings.Join(got, ";") != strings.Join(want, ";") {
				t.Errorf("CronSpecs = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCronSpecRejectsBadTime(t *testing.T) {
	for _, bad := range []string{"25:00", "9", "aa:bb", "12:61"} {
		cfg := &types.WikiConfig{ScheduleMode: types.WikiScheduleDaily, ScheduleTime: bad}
		if _, err := CronSpecs(cfg); err == nil {
			t.Errorf("CronSpecs(%q) accepted an invalid time", bad)
		}
	}
}

func TestCronSpecRejectsNonWindowedModes(t *testing.T) {
	for _, mode := range []types.WikiScheduleMode{"", types.WikiScheduleOnChange, types.WikiScheduleManual} {
		cfg := &types.WikiConfig{ScheduleMode: mode}
		if _, err := CronSpecs(cfg); err == nil {
			t.Errorf("CronSpecs(%q) accepted a non-windowed mode", mode)
		}
	}
}

func TestScheduleDefaultsForLegacyConfig(t *testing.T) {
	// A config saved before the switch has no mode and must keep the
	// historical on-change behaviour instead of silently stopping.
	legacy := &types.WikiConfig{}
	if got := legacy.Schedule(); got != types.WikiScheduleOnChange {
		t.Errorf("Schedule() = %q, want on_change", got)
	}
	if !legacy.RunsOnChange() {
		t.Error("legacy config should still run on change")
	}
	if legacy.IsScheduled() {
		t.Error("legacy config is not windowed")
	}
	disabled := &types.WikiConfig{AutoGenerate: func() *bool { b := false; return &b }()}
	if disabled.RunsOnChange() {
		t.Error("auto_generate=false must not run on change")
	}
	daily := &types.WikiConfig{ScheduleMode: types.WikiScheduleDaily}
	if daily.RunsOnChange() || !daily.IsScheduled() {
		t.Error("daily must wait for its window")
	}
	manual := &types.WikiConfig{ScheduleMode: types.WikiScheduleManual}
	if manual.RunsOnChange() || manual.IsScheduled() {
		t.Error("manual must neither run on change nor be scheduled")
	}
}

func TestWindowKeyStableWithinWindow(t *testing.T) {
	cfg := &types.WikiConfig{ScheduleMode: types.WikiScheduleDaily}
	a := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	b := time.Date(2026, 11, 13, 23, 59, 0, 0, time.UTC)
	c := time.Date(2026, 11, 14, 2, 0, 0, 0, time.UTC)

	if windowKey("kb-1", a, cfg) != windowKey("kb-1", b, cfg) {
		t.Error("two triggers on the same day must share a task id")
	}
	if windowKey("kb-1", a, cfg) == windowKey("kb-1", c, cfg) {
		t.Error("the next day must get its own task id")
	}
	if windowKey("kb-1", a, cfg) == windowKey("kb-2", a, cfg) {
		t.Error("different knowledge bases must not collide")
	}
	if got := windowKey("kb-1", a, cfg); len(got) > 64 || strings.ContainsAny(got, " \t\n") {
		t.Errorf("task id %q is not a safe short token", got)
	}
}

func weeklyCfg() *types.WikiConfig {
	return &types.WikiConfig{ScheduleMode: types.WikiScheduleWeekly, ScheduleTime: "03:00", ScheduleWeekdays: []int{1}}
}

func TestWindowKeyWeeklyUsesISOWeek(t *testing.T) {
	cfg := weeklyCfg()
	mon := time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC)
	sun := time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
	next := time.Date(2026, 11, 23, 3, 0, 0, 0, time.UTC)
	if windowKey("kb", mon, cfg) != windowKey("kb", sun, cfg) {
		t.Error("one ISO week should collapse onto one trigger")
	}
	if windowKey("kb", mon, cfg) == windowKey("kb", next, cfg) {
		t.Error("a new ISO week needs a new trigger")
	}
}

// fakeEnqueuer records what reached the queue so a test can assert the task
// type, queue and dedup id.
type fakeEnqueuer struct {
	tasks []*asynq.Task
	opts  [][]asynq.Option
	err   error
}

func (f *fakeEnqueuer) Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	f.tasks = append(f.tasks, task)
	f.opts = append(f.opts, opts)
	if f.err != nil {
		return nil, f.err
	}
	return &asynq.TaskInfo{}, nil
}

func windowedKB(id string) *types.KnowledgeBase {
	auto := true
	return &types.KnowledgeBase{
		ID:               id,
		TenantID:         42,
		IndexingStrategy: types.IndexingStrategy{WikiEnabled: true},
		WikiConfig:       &types.WikiConfig{ScheduleMode: types.WikiScheduleDaily, AutoGenerate: &auto},
	}
}

func TestIsWindowed(t *testing.T) {
	kb := windowedKB("kb")
	if !isWindowed(kb) {
		t.Fatal("daily wiki KB should be windowed")
	}
	off := windowedKB("kb")
	off.IndexingStrategy.WikiEnabled = false
	if isWindowed(off) {
		t.Error("wiki disabled must not be windowed")
	}
	if isWindowed(&types.KnowledgeBase{ID: "kb"}) {
		t.Error("KB without wiki config must not be windowed")
	}
	onChange := windowedKB("kb")
	onChange.WikiConfig.ScheduleMode = types.WikiScheduleOnChange
	if isWindowed(onChange) {
		t.Error("on_change must not need a cron entry")
	}
	paused := windowedKB("kb")
	no := false
	paused.WikiConfig.AutoGenerate = &no
	if isWindowed(paused) {
		t.Error("auto_generate=false must not keep a cron entry")
	}
}

func TestTriggerEnqueuesWikiIngest(t *testing.T) {
	enq := &fakeEnqueuer{}
	repo := &fakeKBRepo{kbs: map[string]*types.KnowledgeBase{"kb": windowedKB("kb")}}
	s := NewScheduler(repo, enq)
	s.trigger("kb", 42)

	if len(enq.tasks) != 1 {
		t.Fatalf("enqueued %d tasks, want 1", len(enq.tasks))
	}
	if enq.tasks[0].Type() != types.TypeWikiIngest {
		t.Errorf("task type = %q, want %q", enq.tasks[0].Type(), types.TypeWikiIngest)
	}
	payload := string(enq.tasks[0].Payload())
	for _, want := range []string{`"knowledge_base_id":"kb"`, `"tenant_id":42`} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload %s missing %s", payload, want)
		}
	}
}

func TestTriggerSkipsKBThatLeftTheSchedule(t *testing.T) {
	kb := windowedKB("kb")
	kb.WikiConfig.ScheduleMode = types.WikiScheduleManual
	enq := &fakeEnqueuer{}
	s := NewScheduler(&fakeKBRepo{kbs: map[string]*types.KnowledgeBase{"kb": kb}}, enq)
	s.trigger("kb", 42)
	if len(enq.tasks) != 0 {
		t.Errorf("a manual-mode KB must not be triggered, enqueued %d", len(enq.tasks))
	}
}

//nolint:revive // t is unused on purpose: the assertion is "does not panic".
func TestTriggerSurvivesDuplicateTaskID(t *testing.T) {
	enq := &fakeEnqueuer{err: asynq.ErrTaskIDConflict}
	s := NewScheduler(&fakeKBRepo{kbs: map[string]*types.KnowledgeBase{"kb": windowedKB("kb")}}, enq)
	// The loser of a multi-instance race must return quietly: the winner is
	// already doing the work.
	s.trigger("kb", 42)
}

var errNotFound = errors.New("knowledge base not found")

type fakeKBRepo struct {
	kbs map[string]*types.KnowledgeBase
	err error
}

func (f *fakeKBRepo) ListKnowledgeBases(context.Context) ([]*types.KnowledgeBase, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]*types.KnowledgeBase, 0, len(f.kbs))
	for _, kb := range f.kbs {
		out = append(out, kb)
	}
	return out, nil
}

func (f *fakeKBRepo) GetKnowledgeBaseByID(_ context.Context, id string) (*types.KnowledgeBase, error) {
	kb, ok := f.kbs[id]
	if !ok {
		return nil, errNotFound
	}
	return kb, nil
}
