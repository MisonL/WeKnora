// Package wikischedule runs background Wiki generation on a wall-clock
// window instead of after every document.
//
// A knowledge base with schedule_mode=daily or weekly keeps its wiki ingest
// ops queued (see WikiConfig.IsScheduled and the auto-generate gate in
// wikiIngestService) and this scheduler drains them once per window by
// enqueuing the same KB-scoped trigger a freshly uploaded document would.
//
// Dedup follows the data source scheduler: robfig/cron fires at absolute
// wall-clock times, so every app instance triggers at the same minute. The
// asynq TaskID is deterministic per (KB, window), and Redis accepts only one
// task per ID, so the loser gets ErrTaskIDConflict and exits.
package wikischedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hibiken/asynq"
	"github.com/robfig/cron/v3"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/runtime"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// Schedule-specific limits. A windowed KB waits at most a week, so the cron
// entry never needs to fire more often than once a day per KB.
const (
	// triggerTimeout is the per-trigger enqueue budget.
	triggerTimeout = 30 * time.Second
	// triggerMaxRetry mirrors the on-change trigger so a failed enqueue is
	// retried by asynq rather than silently skipping a day.
	triggerMaxRetry = 3
	// triggerTaskTimeout is how long the worker may spend on one KB drain.
	triggerTaskTimeout = 60 * time.Minute
	// reconcileEvery is how often the registered entries are compared with
	// the database. A settings change is therefore picked up within this
	// window without the update path having to know about the scheduler.
	reconcileEvery = 2 * time.Minute
	// stopWait bounds how long shutdown waits for a trigger in flight.
	stopWait = 5 * time.Second
)

// KnowledgeBaseStore is the slice of the knowledge base repository this
// scheduler uses. Narrow on purpose: the full repository interface carries
// dozens of unrelated methods, and a scheduler that needs two of them should
// not make every test double implement the rest.
type KnowledgeBaseStore interface {
	ListKnowledgeBases(ctx context.Context) ([]*types.KnowledgeBase, error)
	GetKnowledgeBaseByID(ctx context.Context, id string) (*types.KnowledgeBase, error)
}

// NewSchedulerFromRepository adapts the container's repository to the narrow
// store and builds the scheduler. The container resolves a constructor from
// its parameter types, so registering one that takes the narrow interface would
// leave dig looking for a provider nobody registers; this wrapper keeps the
// narrow interface for tests while the constructor names a type the container
// actually provides.
func NewSchedulerFromRepository(repo interfaces.KnowledgeBaseRepository, task interfaces.TaskEnqueuer) *Scheduler {
	var store KnowledgeBaseStore
	if repo != nil {
		store = repo
	}
	return NewScheduler(store, task)
}

// Scheduler registers one cron entry per knowledge base that selected a daily
// or weekly window.
type Scheduler struct {
	cron     *cron.Cron
	kbRepo   KnowledgeBaseStore
	task     interfaces.TaskEnqueuer
	location *time.Location

	mu sync.Mutex
	// entries holds one cron entry per (KB, run time): a KB scheduled twice a
	// day owns two entries. Slots record the cron expressions they were
	// registered with, so a reconcile pass can tell an unchanged KB from one
	// whose times moved.
	entries map[string][]cron.EntryID
	slots   map[string][]string
	started bool
	stop    chan struct{}
	once    sync.Once
}

// NewScheduler builds the scheduler. A nil task enqueuer or kb repo yields a
// scheduler that starts and stops cleanly but registers nothing, which keeps
// lite-mode and test wiring from needing a branch.
func NewScheduler(kbRepo KnowledgeBaseStore, task interfaces.TaskEnqueuer) *Scheduler {
	return &Scheduler{
		cron: cron.New(cron.WithParser(cron.NewParser(
			cron.Minute|cron.Hour|cron.Dom|cron.Month|cron.Dow,
		)), cron.WithChain(cron.Recover(cron.DefaultLogger))),
		kbRepo:   kbRepo,
		task:     task,
		location: time.Local,
		entries:  make(map[string][]cron.EntryID),
		slots:    make(map[string][]string),
		stop:     make(chan struct{}),
	}
}

// Start loads every knowledge base and registers the ones with a window.
func (s *Scheduler) Start(ctx context.Context) error {
	if s.kbRepo == nil || s.task == nil {
		logger.Infof(ctx, "[WikiSchedule] scheduler disabled (no repository or enqueuer)")
		return nil
	}
	kbs, err := s.kbRepo.ListKnowledgeBases(ctx)
	if err != nil {
		return fmt.Errorf("load knowledge bases: %w", err)
	}

	registered := 0
	for _, kb := range kbs {
		if !isWindowed(kb) {
			continue
		}
		if err := s.Register(kb); err != nil {
			logger.Warnf(ctx, "[WikiSchedule] failed to register KB %s: %v", kb.ID, err)
			continue
		}
		registered++
	}
	s.mu.Lock()
	s.cron.Start()
	s.started = true
	s.mu.Unlock()
	logger.Infof(ctx, "[WikiSchedule] started with %d windowed knowledge base(s)", registered)
	go s.reconcileLoop()
	return nil
}

// reconcileLoop keeps the cron entries in step with the stored settings. It is
// a poll rather than an event because the update path lives behind several
// callers (API, CLI, provisioning) and none of them should have to remember to
// notify the scheduler.
func (s *Scheduler) reconcileLoop() {
	ticker := time.NewTicker(reconcileEvery)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.Reconcile(context.Background())
		}
	}
}

// Reconcile re-reads every knowledge base and registers, updates or removes
// entries to match. An entry whose expression is unchanged is left alone, so a
// reconcile never disturbs a tick timing that is already correct.
func (s *Scheduler) Reconcile(ctx context.Context) {
	if s.kbRepo == nil {
		return
	}
	kbs, err := s.kbRepo.ListKnowledgeBases(ctx)
	if err != nil {
		logger.Warnf(ctx, "[WikiSchedule] reconcile skipped: %v", err)
		return
	}
	seen := make(map[string]struct{}, len(kbs))
	for _, kb := range kbs {
		if !isWindowed(kb) {
			continue
		}
		specs, err := CronSpecs(kb.WikiConfig)
		if err != nil {
			logger.Warnf(ctx, "[WikiSchedule] KB %s has an invalid schedule: %v", kb.ID, err)
			continue
		}
		seen[kb.ID] = struct{}{}
		s.mu.Lock()
		current, ok := s.slots[kb.ID]
		s.mu.Unlock()
		if ok && slices.Equal(current, specs) {
			continue
		}
		if err := s.Register(kb); err != nil {
			logger.Warnf(ctx, "[WikiSchedule] KB %s re-register failed: %v", kb.ID, err)
			continue
		}
		logger.Infof(ctx, "[WikiSchedule] KB %s schedule set to %q", kb.ID, strings.Join(specs, "; "))
	}
	for _, kbID := range s.registeredIDs() {
		if _, ok := seen[kbID]; ok {
			continue
		}
		s.Unregister(kbID)
		logger.Infof(ctx, "[WikiSchedule] KB %s no longer windowed; entry removed", kbID)
	}
}

func (s *Scheduler) registeredIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.entries))
	for id := range s.entries {
		out = append(out, id)
	}
	return out
}

// Stop stops the reconcile loop and the cron runner, waiting for an in-flight
// tick. Safe to call more than once. A tick that has already handed work to
// the queue is idempotent per window, so an expired wait leaves nothing to
// clean up; blocking shutdown forever would.
func (s *Scheduler) Stop() {
	s.once.Do(func() {
		close(s.stop)
		if !runtime.WaitFor(s.cron.Stop().Done(), stopWait) {
			logger.Warnf(context.Background(),
				"[WikiSchedule] in-flight trigger still running after %s; continuing shutdown", stopWait)
		}
	})
}

// Register (re)adds the KB's entries, replacing every previous one so a
// settings change takes effect without a restart. A KB scheduled at several
// times of day owns one entry per time.
func (s *Scheduler) Register(kb *types.KnowledgeBase) error {
	if !isWindowed(kb) {
		s.Unregister(kb.ID)
		return nil
	}
	specs, err := CronSpecs(kb.WikiConfig)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if ids, ok := s.entries[kb.ID]; ok {
		for _, id := range ids {
			s.cron.Remove(id)
		}
		delete(s.entries, kb.ID)
		delete(s.slots, kb.ID)
	}
	kbID, tenantID := kb.ID, kb.TenantID
	ids := make([]cron.EntryID, 0, len(specs))
	for _, spec := range specs {
		entryID, err := s.cron.AddFunc(spec, func() { s.trigger(kbID, tenantID) })
		if err != nil {
			for _, added := range ids {
				s.cron.Remove(added)
			}
			return fmt.Errorf("invalid schedule for KB %s: %w", kbID, err)
		}
		ids = append(ids, entryID)
	}
	if !s.started {
		s.cron.Start()
		s.started = true
	}
	s.entries[kbID] = ids
	s.slots[kbID] = specs
	return nil
}

// Unregister removes every entry of the KB. Safe for a KB that never had one.
func (s *Scheduler) Unregister(kbID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ids, ok := s.entries[kbID]; ok {
		for _, id := range ids {
			s.cron.Remove(id)
		}
		delete(s.entries, kbID)
		delete(s.slots, kbID)
	}
}

// CronSpecs renders a WikiConfig as one five-field cron expression per
// configured time of day, in the scheduler's location. Daily mode ignores the
// weekday list.
func CronSpecs(cfg *types.WikiConfig) ([]string, error) {
	switch cfg.Schedule() {
	case types.WikiScheduleDaily, types.WikiScheduleWeekly:
	default:
		return nil, fmt.Errorf("schedule mode %q is not windowed", cfg.Schedule())
	}
	times, ok := cfg.TimeOfDayList()
	if !ok {
		return nil, fmt.Errorf("invalid schedule_time %q, want HH:MM or a comma-separated list", cfg.ScheduleTime)
	}
	dow := "*"
	if cfg.Schedule() == types.WikiScheduleWeekly {
		days := cfg.WeekdaysOrDefault()
		parts := make([]string, 0, len(days))
		for _, d := range days {
			parts = append(parts, fmt.Sprint(d))
		}
		dow = strings.Join(parts, ",")
	}
	out := make([]string, 0, len(times))
	for _, t := range times {
		out = append(out, fmt.Sprintf("%d %d * * %s", t[1], t[0], dow))
	}
	return out, nil
}

// windowKey identifies the current window so a restarted process or a second
// instance firing in the same minute collapses onto one task. It hashes the KB
// id into the TaskID because asynq task ids have a length limit and a raw UUID
// plus the window prefix leaves little room.
func windowKey(kbID string, now time.Time, cfg *types.WikiConfig) string {
	window := now.Format("2006-01-02")
	if cfg.Schedule() == types.WikiScheduleWeekly {
		// Go's layout has no ISO week token, so the pair comes from ISOWeek
		// rather than a string layout. A calendar week spans year boundaries,
		// which is why both parts are needed.
		year, week := now.ISOWeek()
		window = fmt.Sprintf("%d-W%02d", year, week)
	}
	// The run time is part of the window: a KB scheduled at noon and six
	// drains twice a day, and the second drain must not collapse onto the
	// first through a shared task id. The run is the latest time of day at or
	// before now, so a tick a minute late still lands on its own run.
	if runHour, runMinute, ok := currentRun(now, cfg); ok {
		window += fmt.Sprintf("@%02d%02d", runHour, runMinute)
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(kbID))
	return fmt.Sprintf("wikischedule-%08x-%s", h.Sum32(), window)
}

// currentRun returns the latest configured time of day at or before now. ok is
// false when the schedule is unreadable, in which case the window key falls
// back to the day alone.
func currentRun(now time.Time, cfg *types.WikiConfig) (hour, minute int, ok bool) {
	times, valid := cfg.TimeOfDayList()
	if !valid {
		return 0, 0, false
	}
	hour, minute = -1, -1
	for _, t := range times {
		if t[0] < now.Hour() || (t[0] == now.Hour() && t[1] <= now.Minute()) {
			if t[0] > hour || (t[0] == hour && t[1] > minute) {
				hour, minute = t[0], t[1]
			}
		}
	}
	if hour < 0 {
		return 0, 0, false
	}
	return hour, minute, true
}

// trigger drains the KB's queued ops through the normal wiki ingest path. Ops
// are deduped per document inside the queue, so calling this once per tick
// never duplicates work.
func (s *Scheduler) trigger(kbID string, tenantID uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), triggerTimeout)
	defer cancel()

	kb, err := s.kbRepo.GetKnowledgeBaseByID(ctx, kbID)
	if err != nil || kb == nil {
		logger.Warnf(ctx, "[WikiSchedule] skipping KB %s (lookup failed: %v)", kbID, err)
		return
	}
	// Re-check the live config: a window removed since registration must not
	// run once more, and a disabled wiki must not spend model calls.
	if !isWindowed(kb) || !kb.IsWikiAutoGenerateEnabled() {
		s.Unregister(kbID)
		logger.Infof(ctx, "[WikiSchedule] KB %s no longer windowed; entry removed", kbID)
		return
	}

	payload, err := json.Marshal(map[string]any{
		"tenant_id":         tenantID,
		"knowledge_base_id": kbID,
		"language":          types.LanguageFromContextOrDefault(ctx),
		"scheduled":         true,
	})
	if err != nil {
		logger.Warnf(ctx, "[WikiSchedule] marshal trigger for KB %s: %v", kbID, err)
		return
	}
	task := asynq.NewTask(types.TypeWikiIngest, payload,
		asynq.Queue(types.QueueWiki),
		asynq.MaxRetry(triggerMaxRetry),
		asynq.Timeout(triggerTaskTimeout),
		asynq.TaskID(windowKey(kbID, time.Now(), kb.WikiConfig)),
	)
	if _, err := s.task.Enqueue(task); err != nil {
		if errors.Is(err, asynq.ErrTaskIDConflict) || errors.Is(err, asynq.ErrDuplicateTask) {
			return // another instance won this window
		}
		logger.Warnf(ctx, "[WikiSchedule] enqueue for KB %s failed: %v", kbID, err)
		return
	}
	logger.Infof(ctx, "[WikiSchedule] drained KB %s for the %s window", kbID, kb.WikiConfig.Schedule())
}

// isWindowed reports whether the KB has a window that needs a cron entry. The
// auto-generate switch wins: a KB with automatic generation off has nothing
// queued, and the batch would drop its ops anyway.
func isWindowed(kb *types.KnowledgeBase) bool {
	return kb != nil && kb.IsWikiEnabled() && kb.WikiConfig.IsScheduled() &&
		kb.WikiConfig.IsAutoGenerateEnabled()
}
