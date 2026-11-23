package service

// 本轮复核缺陷修复的回归测试（FAQ 租约 fail-open、终态收尾、staleness 同 task、
// CSV 空策略、lite 进度快照、entries 对象清理、claim 拒绝消息）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

// failCmdHook makes every command whose name matches target fail without
// touching the store, so tests can inject "the store is unreachable" at one
// call site (renew EVAL vs reclaim SetNX) while the others (progress SET,
// marker GET) keep working.
type failCmdHook struct{ target string }

func (h *failCmdHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *failCmdHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if h.target != "" && cmd.Name() == h.target {
			return errors.New("redis: injected failure for " + h.target)
		}
		return next(ctx, cmd)
	}
}
func (h *failCmdHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// The reclaim path lapsed between renewals: the EVAL says "missing" and the
// SetNX that takes the lease back fails. Reporting success there let the
// strict gate write irreversibly without a verified lease.
func TestReclaimFailureIsReportedAsUnverifiable(t *testing.T) {
	svc, mr := newLeaseService(t)
	ctx := context.Background()
	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("mine")))

	// The marker lapses (renew answers "missing"), and the reclaim SetNX fails
	// while renew's EVAL still works.
	mr.FastForward(faqImportLeaseTTL + time.Second)
	// go-redis implements SetNX as SET..NX, so the command to fail is "set".
	svc.redisClient.AddHook(&failCmdHook{target: "set"})

	require.ErrorIs(t,
		svc.renewFAQImportLeaseAllow(ctx, "kb-1", "mine", "inst-mine", 0, true),
		errFAQImportLeaseUnverifiable,
		"a reclaim that could not run proves nothing about ownership")

	require.ErrorIs(t,
		svc.ensureFAQImportLeaseForWriteStrict(ctx, "kb-1", "mine", "inst-mine", 0),
		errFAQImportLeaseUnverifiable)
}

// A reclaim that succeeded must publish the full marker identity, including
// the enqueue time: without it the timestamp leg of later ownership checks
// silently degrades.
func TestReclaimKeepsTheEnqueuedAtIdentity(t *testing.T) {
	svc, mr := newLeaseService(t)
	ctx := context.Background()
	const enqueued int64 = 1_700_000_000
	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", &runningFAQImportInfo{
		TaskID: "mine", InstanceID: "inst-mine", EnqueuedAt: enqueued,
	}))
	mr.FastForward(faqImportLeaseTTL + time.Second)

	require.NoError(t, svc.renewFAQImportLeaseAllow(ctx, "kb-1", "mine", "inst-mine", enqueued, true))

	info, err := svc.getRunningFAQImportInfo(ctx, "kb-1")
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, enqueued, info.EnqueuedAt,
		"a reclaimed marker has to carry the enqueue time it was claimed with")
}

// The strict gate refusing an unverifiable lease must also close out the run:
// archiving it with SkipRetry while the record still says "processing" leaves
// the initiator polling a dead task until the progress TTL.
func TestStrictGateUnverifiableClosesTheFinalizationRecord(t *testing.T) {
	f := newFAQAdmissionFixture(t)
	payload := &types.FAQImportPayload{
		TaskID: "task-strict", KBID: f.kb.ID, KnowledgeID: "faq",
		TenantID: f.kb.TenantID, Mode: types.FAQBatchModeAppend, InstanceID: "inst-1",
	}
	progress := &types.FAQImportProgress{
		TaskID: "task-strict", KBID: f.kb.ID, KnowledgeID: "faq",
		TenantID: f.kb.TenantID, InstanceID: "inst-1",
		Status: types.FAQImportStatusProcessing, Total: 1,
	}
	// The lease is ours and progress writes (plain SET) still work; only the
	// renew EVAL fails, so the strict gate sees "unverifiable".
	require.NoError(t, f.svc.claimFAQImportLease(f.ctx, f.kb.ID, &runningFAQImportInfo{
		TaskID: payload.TaskID, InstanceID: payload.InstanceID, EnqueuedAt: time.Now().Unix(),
	}))
	require.NoError(t, f.svc.saveFAQImportProgress(f.ctx, progress))
	f.svc.redisClient.AddHook(&failCmdHook{target: "eval"})

	require.ErrorIs(t, f.svc.finalizeFAQValidation(f.ctx, payload, progress, 1), asynq.SkipRetry)

	got, err := f.svc.GetFAQImportProgress(f.ctx, payload.TaskID)
	require.NoError(t, err)
	require.Equal(t, types.FAQImportStatusFailed, got.Status,
		"an archived-without-retry terminal outcome must land in the record")
	require.Contains(t, got.Error, "租约")
}

func TestFAQImportRetryStalenessSameTaskRules(t *testing.T) {
	const enqueued int64 = 1_700_000_000
	payload := func(taskID string) *types.FAQImportPayload {
		return &types.FAQImportPayload{
			Mode: types.FAQBatchModeReplace, TaskID: taskID,
			InstanceID: "inst-" + taskID, EnqueuedAt: enqueued, EnqueuedAtMillis: enqueued * 1000,
		}
	}
	result := func(taskID, instance string, offset int64) *types.FAQImportResult {
		return &types.FAQImportResult{
			TaskID: taskID, InstanceID: instance,
			ImportedAt: time.Unix(enqueued+offset, 0), ImportedAtMillis: (enqueued + offset) * 1000,
		}
	}
	for _, tc := range []struct {
		name    string
		payload *types.FAQImportPayload
		result  *types.FAQImportResult
		stale   bool
	}{
		// A replace retry of a task whose own result is the stored one (crash
		// after the result write, before the ACK) resumes idempotently: it is
		// never fenced by its own completion.
		{name: "own_result_resumes", payload: payload("task-a"),
			result: result("task-a", "inst-task-a", 60), stale: false},
		// A different task completing at/after enqueue is the fence's purpose.
		{name: "other_task_after", payload: payload("task-a"),
			result: result("task-b", "inst-task-b", 60), stale: true},
		{name: "other_task_before", payload: payload("task-a"),
			result: result("task-b", "inst-task-b", -60), stale: false},
		// A reused task id whose newer instance completed: the queued older
		// instance is fenced even though the task id matches.
		{name: "reused_task_newer_instance", payload: payload("task-a"),
			result: result("task-a", "inst-other", 60), stale: true},
		// Legacy results without an instance id keep the timestamp comparison
		// -- the safe (over-refusal) direction for a replace.
		{name: "legacy_result_no_instance", payload: payload("task-a"),
			result: result("task-b", "", 60), stale: true},
		{name: "own_result_legacy_no_instance", payload: payload("task-a"),
			result: result("task-a", "", 60), stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.stale, faqImportRetryIsStale(tc.payload, tc.result))
		})
	}
}

// The CSV export is a round-trip channel: a legacy row whose metadata never
// stored a strategy means "all" for every reader, so the export must not
// encode it as a refusal to answer everything.
// The heartbeat may tolerate an unreachable store, but not for longer than
// the lease itself: past one full TTL of unverifiable renewals a new import
// could already hold the key, and continuing to write beside it is exactly
// what the lease exists to prevent.
func TestHeartbeatStopsAfterAFullTTLofUnverifiableRenewals(t *testing.T) {
	svc, mr := newLeaseService(t)
	ctx := context.Background()
	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("running")))

	old := faqImportLeaseHeartbeatInterval
	faqImportLeaseHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { faqImportLeaseHeartbeatInterval = old })

	// An outage lasting past the lease's own TTL must stop the run.
	savedTTL := faqImportLeaseTTL
	faqImportLeaseTTL = 20 * time.Millisecond
	t.Cleanup(func() { faqImportLeaseTTL = savedTTL })

	lost := make(chan struct{}, 1)
	stop := svc.startFAQImportLeaseHeartbeat(ctx, "kb-1", "running", "inst-running", 0, func() {
		select {
		case lost <- struct{}{}:
		default:
		}
	})
	mr.SetError("ERR lease store unavailable")

	select {
	case <-lost:
	case <-time.After(2 * time.Second):
		t.Fatal("a lease that cannot be verified past its TTL must stop the run")
	}
	stop()
}

func TestFAQCSVExportTreatsEmptyStrategyAsAll(t *testing.T) {
	svc := &knowledgeService{}
	legacy := &types.Chunk{SeqID: 1, IsEnabled: true, ChunkType: types.ChunkTypeFAQ}
	require.NoError(t, legacy.SetFAQMetadata(&types.FAQChunkMetadata{
		StandardQuestion: "q legacy", Answers: []string{"a1"}, AnswerStrategy: "",
	}))
	chunks := []*types.Chunk{legacy}
	body := string(svc.buildFAQCSV(chunks, map[string]string{}))
	require.Contains(t, body, "q legacy")
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "q legacy") {
			require.Contains(t, line, ",TRUE,",
				"an empty answer_strategy has to export 是否全部回复 as TRUE, got %q", line)
		}
	}
	// Counter-case stays: a genuine random still exports FALSE.
	random := &types.Chunk{SeqID: 2, IsEnabled: true, ChunkType: types.ChunkTypeFAQ}
	require.NoError(t, random.SetFAQMetadata(&types.FAQChunkMetadata{
		StandardQuestion: "q random", Answers: []string{"a2"},
		AnswerStrategy: types.AnswerStrategyRandom,
	}))
	body2 := string(svc.buildFAQCSV([]*types.Chunk{random}, map[string]string{}))
	for _, line := range strings.Split(body2, "\n") {
		if strings.Contains(line, "q random") {
			require.Contains(t, line, ",FALSE,",
				"a random strategy still exports 是否全部回复 as FALSE, got %q", line)
		}
	}
}

// The in-memory progress map handed callers the worker's own pointer: every
// reader (HTTP poll, sweep) raced the writing goroutine. Records must be
// copied in and out.
func TestLiteProgressIsStoredAsACopy(t *testing.T) {
	svc := &knowledgeService{}
	progress := &types.FAQImportProgress{
		TaskID: "lite-task", KBID: "kb", Status: types.FAQImportStatusProcessing, Progress: 10,
		SuccessEntries: []types.FAQSuccessEntry{{Index: 0, SeqID: 1}},
	}
	require.NoError(t, svc.writeFAQImportProgress(context.Background(), progress))

	// The worker moves on: mutating its own struct must not change the record.
	progress.Progress = 90
	progress.Status = types.FAQImportStatusCompleted
	progress.SuccessEntries[0].SeqID = 99

	got, err := svc.GetFAQImportProgress(context.Background(), "lite-task")
	require.NoError(t, err)
	require.Equal(t, 10, got.Progress, "the record is a snapshot, not a live pointer")
	require.Equal(t, types.FAQImportStatusProcessing, got.Status)
	require.Equal(t, int64(1), got.SuccessEntries[0].SeqID)

	// The returned copy is decoupled the other way too: a poller editing what
	// it received must not corrupt the stored record.
	got.Progress = 55
	again, err := svc.GetFAQImportProgress(context.Background(), "lite-task")
	require.NoError(t, err)
	require.Equal(t, 10, again.Progress)
}
