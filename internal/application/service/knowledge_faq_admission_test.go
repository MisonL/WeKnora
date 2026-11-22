package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type faqAdmissionTenant struct{ interfaces.TenantRepository }

func (faqAdmissionTenant) GetTenantByID(_ context.Context, id uint64) (*types.Tenant, error) {
	return &types.Tenant{ID: id}, nil
}

type faqAdmissionQueue struct{ tasks []*asynq.Task }

func (q *faqAdmissionQueue) Enqueue(task *asynq.Task, _ ...asynq.Option) (*asynq.TaskInfo, error) {
	var payload types.FAQImportPayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return nil, err
	}
	q.tasks = append(q.tasks, task)
	return &asynq.TaskInfo{ID: payload.TaskID + ":" + payload.InstanceID, Queue: types.QueueMaintenance}, nil
}

type faqAdmissionFixture struct {
	*faqWriteFixture
	mini  *miniredis.Miniredis
	queue *faqAdmissionQueue
}

func newFAQAdmissionFixture(t *testing.T) *faqAdmissionFixture {
	t.Helper()
	f := newFAQWriteFixture(t)
	sqlDB, err := f.db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	mini := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	f.svc.redisClient = rdb
	f.svc.tenantRepo = faqAdmissionTenant{}
	queue := &faqAdmissionQueue{}
	f.svc.task = queue
	return &faqAdmissionFixture{faqWriteFixture: f, mini: mini, queue: queue}
}

func (f *faqAdmissionFixture) enqueue(t *testing.T, taskID string, count int) *asynq.Task {
	t.Helper()
	entries := make([]types.FAQEntryPayload, count)
	for i := range entries {
		entries[i] = types.FAQEntryPayload{
			StandardQuestion: fmt.Sprintf("How do I complete step %d?", i+1),
			Answers:          []string{fmt.Sprintf("Follow step %d.", i+1)},
		}
	}
	got, err := f.svc.UpsertFAQEntries(f.ctx, f.kb.ID, &types.FAQBatchUpsertPayload{
		TaskID: taskID, Mode: types.FAQBatchModeAppend, DryRun: true, Entries: entries,
	})
	require.NoError(t, err)
	require.Equal(t, taskID, got)
	require.NotEmpty(t, f.queue.tasks)
	return f.queue.tasks[len(f.queue.tasks)-1]
}

func (f *faqAdmissionFixture) prepareReplacement(t *testing.T, reuseTaskID bool) (*asynq.Task, string) {
	t.Helper()
	taskID := utils.GenerateTaskID("faq_import", f.kb.TenantID, f.kb.ID)
	old := f.enqueue(t, taskID, 1)
	// Keep the original queued payload while its three-hour marker/progress
	// expires. A later Upsert creates a distinct import instance for this KB.
	f.mini.FastForward(faqImportProgressTTL + time.Second)
	if !reuseTaskID {
		taskID = utils.GenerateTaskID("faq_import", f.kb.TenantID, f.kb.ID)
	}
	replacement := f.enqueue(t, taskID, 2)
	var a, b types.FAQImportPayload
	require.NoError(t, json.Unmarshal(old.Payload(), &a))
	require.NoError(t, json.Unmarshal(replacement.Payload(), &b))
	require.NotEmpty(t, a.InstanceID)
	require.NotEmpty(t, b.InstanceID)
	require.NotEqual(t, a.InstanceID, b.InstanceID)
	if reuseTaskID {
		require.Equal(t, a.TaskID, b.TaskID)
	} else {
		require.NotEqual(t, a.TaskID, b.TaskID)
	}
	return old, taskID
}

func TestFAQImportRejectsSupersededInstance(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reuse bool
	}{
		{name: "same_task_id", reuse: true},
		{name: "different_task_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFAQAdmissionFixture(t)
			old, taskID := f.prepareReplacement(t, tc.reuse)
			before, err := f.svc.GetFAQImportProgress(f.ctx, taskID)
			require.NoError(t, err)
			require.Equal(t, types.FAQImportStatusPending, before.Status)
			require.Equal(t, 2, before.Total)
			markerBefore, err := f.svc.getRunningFAQImportInfo(f.ctx, f.kb.ID)
			require.NoError(t, err)
			require.NotNil(t, markerBefore)

			processErr := f.svc.ProcessFAQImport(f.ctx, old)

			after, err := f.svc.GetFAQImportProgress(f.ctx, taskID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			markerAfter, err := f.svc.getRunningFAQImportInfo(f.ctx, f.kb.ID)
			require.NoError(t, err)
			require.Equal(t, markerBefore, markerAfter)
			require.ErrorIs(t, processErr, asynq.SkipRetry)
			require.Zero(t, f.chunks.writes)
		})
	}
}

func TestFAQImportAdmissionAllowsRecoveryAndLegacy(t *testing.T) {
	for _, scenario := range []string{"matching", "missing", "legacy_raw_task_id", "legacy_same_timestamp"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFAQAdmissionFixture(t)
			taskID := utils.GenerateTaskID("faq_import", f.kb.TenantID, f.kb.ID)
			task := f.enqueue(t, taskID, 1)
			var payload types.FAQImportPayload
			require.NoError(t, json.Unmarshal(task.Payload(), &payload))
			switch scenario {
			case "missing":
				f.mini.FastForward(faqImportProgressTTL + time.Second)
			case "legacy_raw_task_id":
				payload.InstanceID = ""
				payload.EnqueuedAt = 0
				require.NoError(t, f.mini.Set(getFAQImportRunningKey(f.kb.ID), taskID))
			case "legacy_same_timestamp":
				payload.InstanceID = ""
				require.NoError(t, f.svc.setRunningFAQImportInfo(f.ctx, f.kb.ID, &runningFAQImportInfo{
					TaskID: taskID, EnqueuedAt: payload.EnqueuedAt,
				}))
			}
			task = asynq.NewTask(types.TypeFAQImport, mustWriteJSON(t, payload))
			require.NoError(t, f.svc.ProcessFAQImport(f.ctx, task))
			progress, err := f.svc.GetFAQImportProgress(f.ctx, taskID)
			require.NoError(t, err)
			require.Equal(t, types.FAQImportStatusCompleted, progress.Status)
			require.Equal(t, 1, progress.Total)
			require.Equal(t, 1, progress.SuccessCount)
			marker, err := f.svc.getRunningFAQImportInfo(f.ctx, f.kb.ID)
			require.NoError(t, err)
			require.Nil(t, marker)
		})
	}
}

func TestFAQImportAdmissionRejectsUnreadableMarker(t *testing.T) {
	for _, scenario := range []string{"redis_read_error", "malformed", "empty_identity"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFAQAdmissionFixture(t)
			taskID := utils.GenerateTaskID("faq_import", f.kb.TenantID, f.kb.ID)
			task := f.enqueue(t, taskID, 1)
			markerKey := getFAQImportRunningKey(f.kb.ID)
			switch scenario {
			case "malformed":
				require.NoError(t, f.mini.Set(markerKey, "{invalid-json"))
			case "empty_identity":
				require.NoError(t, f.mini.Set(markerKey, "{}"))
			}
			before, err := f.mini.Get(getFAQImportProgressKey(taskID))
			require.NoError(t, err)
			markerBefore, err := f.mini.Get(markerKey)
			require.NoError(t, err)
			if scenario == "redis_read_error" {
				f.mini.SetError("ERR marker store temporarily unavailable")
				t.Cleanup(func() { f.mini.SetError("") })
			}

			processErr := f.svc.ProcessFAQImport(f.ctx, task)

			require.Error(t, processErr)
			require.NotErrorIs(t, processErr, asynq.SkipRetry)
			f.mini.SetError("")
			after, err := f.mini.Get(getFAQImportProgressKey(taskID))
			require.NoError(t, err)
			require.Equal(t, before, after)
			markerAfter, err := f.mini.Get(markerKey)
			require.NoError(t, err)
			require.Equal(t, markerBefore, markerAfter)
			require.Zero(t, f.chunks.writes)
		})
	}
}

// The submission endpoint caps the entry count, but the cap is read at request
// time: a file written while the cap was higher -- or a payload carrying only
// EntriesURL -- reaches the worker oversized. Validation, merging and indexing
// all scale with the array, so the worker has to refuse it and give the lease
// back instead of blocking the knowledge base for a task it will never run.
func TestFAQImportWorkerRefusesAnOversizedPayloadAndReleasesTheLease(t *testing.T) {
	f := newFAQAdmissionFixture(t)
	taskID := utils.GenerateTaskID("faq_import", f.kb.TenantID, f.kb.ID)
	task := f.enqueue(t, taskID, 2)

	// Lower the cap after the submission: the queued payload now exceeds it.
	t.Setenv(utils.EnvFAQImportMaxEntries, "1")

	require.ErrorIs(t, f.svc.ProcessFAQImport(f.ctx, task), asynq.SkipRetry)

	progress, err := f.svc.GetFAQImportProgress(f.ctx, taskID)
	require.NoError(t, err)
	require.Equal(t, types.FAQImportStatusFailed, progress.Status)
	require.Contains(t, progress.Error, "单次最多导入 1 条")

	marker, err := f.svc.getRunningFAQImportInfo(f.ctx, f.kb.ID)
	require.NoError(t, err)
	require.Nil(t, marker, "a refused task has to release the lease it holds")

	require.Zero(t, f.chunks.writes)
}

// The running marker is a ten-minute lease while the progress record lives for
// three hours, so a queued task can outlive its marker. If the task id was
// meanwhile reused by a newer submission, the record belongs to that newer
// instance: the stale run must not resume or overwrite it -- and it must hand
// back the lease it re-asserted on the way in, or the knowledge base stays
// blocked for a task that is stopping.
func TestFAQImportStaleInstanceLeavesTheNewerRecordAlone(t *testing.T) {
	f := newFAQAdmissionFixture(t)
	taskID := utils.GenerateTaskID("faq_import", f.kb.TenantID, f.kb.ID)
	task := f.enqueue(t, taskID, 1)

	// The marker lapses; the progress record survives and is then replaced by
	// the newer submission that reused the task id.
	f.mini.FastForward(faqImportLeaseTTL + time.Second)
	newer := &types.FAQImportProgress{
		TaskID: taskID, KBID: f.kb.ID, KnowledgeID: "faq", TenantID: f.kb.TenantID,
		InstanceID: "instance-newer", Status: types.FAQImportStatusPending, Total: 5,
	}
	raw, err := json.Marshal(newer)
	require.NoError(t, err)
	require.NoError(t, f.mini.Set(getFAQImportProgressKey(taskID), string(raw)))

	require.ErrorIs(t, f.svc.ProcessFAQImport(f.ctx, task), asynq.SkipRetry)

	after, err := f.svc.GetFAQImportProgress(f.ctx, taskID)
	require.NoError(t, err)
	require.Equal(t, "instance-newer", after.InstanceID)
	require.Equal(t, types.FAQImportStatusPending, after.Status, "the newer instance's record must be untouched")

	marker, err := f.svc.getRunningFAQImportInfo(f.ctx, f.kb.ID)
	require.NoError(t, err)
	require.Nil(t, marker, "the stale instance has to release the lease it re-asserted")
	require.Zero(t, f.chunks.writes)
}

// A retry can come back after its lease lapsed and a newer import was admitted,
// ran and finished. Its marker is gone, so the running-instance check sees
// nothing; the persisted result is the only evidence that this retry is stale.
// Without refusing it, a replace retry would delete everything the newer
// import added.
func TestFAQImportRefusesARetryOlderThanTheLastCompletedImport(t *testing.T) {
	f := newFAQAdmissionFixture(t)
	taskID := utils.GenerateTaskID("faq_import", f.kb.TenantID, f.kb.ID)
	task := f.enqueue(t, taskID, 1)

	var payload types.FAQImportPayload
	require.NoError(t, json.Unmarshal(task.Payload(), &payload))
	require.NotZero(t, payload.EnqueuedAt)
	// The check applies to real replace imports; the fixture submits a dry-run
	// append import.
	payload.DryRun = false
	payload.Mode = types.FAQBatchModeReplace
	task = asynq.NewTask(types.TypeFAQImport, mustWriteJSON(t, payload))

	// The newer import completed after this task was accepted -- in the same
	// second, which is already stale -- and its marker is gone by the time this
	// retry runs.
	f.mini.Del(getFAQImportRunningKey(f.kb.ID))
	var faqKnowledge types.Knowledge
	require.NoError(t, f.db.First(&faqKnowledge, "id = ?", "faq").Error)
	require.NoError(t, faqKnowledge.SetLastFAQImportResult(&types.FAQImportResult{
		TaskID:           "faq_import_other",
		ImportedAt:       time.Unix(payload.EnqueuedAt, 0),
		ImportedAtMillis: payload.EnqueuedAtMillis,
	}))
	require.NoError(t, f.db.Save(&faqKnowledge).Error)

	processErr := f.svc.ProcessFAQImport(f.ctx, task)
	require.ErrorIs(t, processErr, asynq.SkipRetry)

	progress, err := f.svc.GetFAQImportProgress(f.ctx, taskID)
	require.NoError(t, err)
	require.Equal(t, types.FAQImportStatusFailed, progress.Status)
	require.Contains(t, progress.Error, "已过期")
	require.Zero(t, f.chunks.writes, "a stale retry must not write anything")
}

// The stale fence guards replace, whose deletion set is computed from its own
// old payload. An append retry only adds entries, so a completed import since
// enqueue does not make it unsafe. The comparison is inclusive: a completion in
// the same second as the enqueue already replaced the knowledge base.
func TestFAQImportRetryStalenessRules(t *testing.T) {
	const enqueuedAt int64 = 1_700_000_000
	completedAt := func(sec int64) *types.FAQImportResult {
		return &types.FAQImportResult{ImportedAt: time.Unix(sec, 0)}
	}
	payload := func(mode string) *types.FAQImportPayload {
		return &types.FAQImportPayload{Mode: mode, EnqueuedAt: enqueuedAt}
	}
	for _, tc := range []struct {
		name    string
		payload *types.FAQImportPayload
		result  *types.FAQImportResult
		stale   bool
	}{
		{name: "replace_after_completion", payload: payload(types.FAQBatchModeReplace),
			result: completedAt(enqueuedAt + 60), stale: true},
		{name: "replace_same_second", payload: payload(types.FAQBatchModeReplace),
			result: completedAt(enqueuedAt), stale: true},
		{name: "replace_before_completion", payload: payload(types.FAQBatchModeReplace),
			result: completedAt(enqueuedAt - 1)},
		{name: "append_after_completion", payload: payload(types.FAQBatchModeAppend),
			result: completedAt(enqueuedAt + 60)},
		{name: "replace_without_result", payload: payload(types.FAQBatchModeReplace)},
		{name: "replace_zero_imported_at", payload: payload(types.FAQBatchModeReplace),
			result: &types.FAQImportResult{}},
		{name: "replace_dry_run", payload: func() *types.FAQImportPayload {
			p := payload(types.FAQBatchModeReplace)
			p.DryRun = true
			return p
		}(), result: completedAt(enqueuedAt + 60)},
		{name: "replace_without_enqueue_time", payload: func() *types.FAQImportPayload {
			p := payload(types.FAQBatchModeReplace)
			p.EnqueuedAt = 0
			return p
		}(), result: completedAt(enqueuedAt)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.stale, faqImportRetryIsStale(tc.payload, tc.result))
		})
	}
}

// A task id is reused across submissions. When another instance of the same
// task owns the progress record, a stopping run must not mark that record
// failed: the initiator would see the import that is actually running as dead.
func TestStoppingAnImportLeavesANewerInstanceRecordAlone(t *testing.T) {
	f := newFAQAdmissionFixture(t)
	const taskID = "faq_import_shared"
	require.NoError(t, f.svc.writeFAQImportProgress(f.ctx, &types.FAQImportProgress{
		TaskID: taskID, InstanceID: "instance-newer", Status: types.FAQImportStatusPending, Total: 2,
	}))

	f.svc.markFAQImportStopped(f.ctx, taskID, "instance-old", "stopped")

	progress, err := f.svc.GetFAQImportProgress(f.ctx, taskID)
	require.NoError(t, err)
	require.Equal(t, types.FAQImportStatusPending, progress.Status)
	require.Equal(t, "instance-newer", progress.InstanceID)
	require.Empty(t, progress.Error)
}

// The entries object is named per import instance: a reused task id in the same
// second must not share the file one refusal could delete while the other is
// about to read it.
func TestFAQEntriesObjectNamesArePerInstance(t *testing.T) {
	first, err := faqImportEntriesFileName("task", 1_700_000_000, "inst-a")
	require.NoError(t, err)
	second, err := faqImportEntriesFileName("task", 1_700_000_000, "inst-b")
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	require.Contains(t, first, "inst-a")
	require.Contains(t, second, "inst-b")
}

// Inside one second, seconds cannot tell a completion from a submission that
// came after it. The millisecond fields can, and they only decide when present;
// older records fall back to seconds.
func TestFAQImportStalenessUsesMilliseconds(t *testing.T) {
	const base int64 = 1_700_000_000_000
	payload := func(ms int64) *types.FAQImportPayload {
		p := &types.FAQImportPayload{Mode: types.FAQBatchModeReplace, EnqueuedAt: ms / 1000}
		if ms > 0 {
			p.EnqueuedAtMillis = ms
		}
		return p
	}
	result := func(ms int64) *types.FAQImportResult {
		return &types.FAQImportResult{ImportedAt: time.UnixMilli(ms), ImportedAtMillis: ms}
	}

	require.False(t, faqImportRetryIsStale(payload(base+500), result(base+400)),
		"an import accepted after the earlier completion is not stale")
	require.True(t, faqImportRetryIsStale(payload(base+500), result(base+500)))
	require.False(t, faqImportRetryIsStale(payload(base), result(base-1)))
	// A payload from an older build has only seconds; the completion's
	// milliseconds still order it correctly against that second.
	legacy := &types.FAQImportPayload{Mode: types.FAQBatchModeReplace, EnqueuedAt: base / 1000}
	require.True(t, faqImportRetryIsStale(legacy, result(base+1)),
		"a completion after the enqueue second is stale")
}

// A stale replace retry will never run, so the entries file it wrote is
// unreachable. It is released with the refusal instead of waiting for the
// bucket's lifecycle rule.
func TestAStaleRetryReleasesItsEntriesFile(t *testing.T) {
	f := newFAQAdmissionFixture(t)
	files := &deleteRecorder{}
	f.svc.fileSvc = files
	payload := &types.FAQImportPayload{
		TaskID: "faq_import_stale", KBID: f.kb.ID, Mode: types.FAQBatchModeReplace,
		EnqueuedAt: time.Now().Unix(), InstanceID: "inst-stale",
		EntriesURL: "local://7/faq_import_entries_x_1_inst-stale.json",
	}

	err := f.svc.refuseStaleFAQImport(f.ctx, payload, &types.FAQImportResult{
		TaskID: "newer", ImportedAt: time.Now(),
	})

	require.ErrorIs(t, err, asynq.SkipRetry)
	require.Equal(t, []string{payload.EntriesURL}, files.deleted)
}
