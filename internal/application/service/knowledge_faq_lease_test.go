package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

func newLeaseService(t *testing.T) (*knowledgeService, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &knowledgeService{redisClient: rdb}, mr
}

func leaseInfo(taskID string) *runningFAQImportInfo {
	return &runningFAQImportInfo{TaskID: taskID, EnqueuedAt: time.Now().Unix(), InstanceID: "inst-" + taskID}
}

// The previous guard was GET then SET: two submissions could both read "free"
// and both write. The lease has to admit exactly one of two concurrent claims.
func TestImportLeaseAdmitsOnlyOneOfTwoConcurrentClaims(t *testing.T) {
	svc, _ := newLeaseService(t)
	ctx := context.Background()

	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("first")))

	err := svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("second"))
	require.Error(t, err)
	var held *faqImportLeaseHeldError
	require.ErrorAs(t, err, &held)
	require.Equal(t, "first", held.TaskID, "the refusal has to name the task that holds the lease")

	// A different knowledge base is unaffected.
	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-2", leaseInfo("other")))
}

// A worker that dies without clearing its lease must not block the knowledge
// base forever, and a running import must refresh the lease so it is not
// mistaken for a dead one.
func TestImportLeaseExpiresWithoutRenewalAndIsKeptByIt(t *testing.T) {
	svc, mr := newLeaseService(t)
	ctx := context.Background()

	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("first")))
	require.NoError(t, svc.renewFAQImportLease(ctx, "kb-1", "first", "inst-first", 0))

	// Past the original lease but inside the renewal: still held.
	mr.FastForward(faqImportLeaseTTL - time.Second)
	err := svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("second"))
	require.Error(t, err, "the renewed lease must still be held")

	mr.FastForward(2 * time.Second)
	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("third")),
		"an abandoned lease has to expire so the knowledge base is not blocked forever")
}

// A renewal must not refresh someone else's lease. The old Expire did: a task
// that had been superseded kept the other import's lease alive, and a task
// whose own lease had lapsed renewed nothing and kept writing beside the new
// holder.
func TestRenewalIsOwnershipCheckedAndReclaimsALapsedLease(t *testing.T) {
	svc, mr := newLeaseService(t)
	ctx := context.Background()

	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("first")))

	// A different task's renewal is reported as a takeover and leaves the
	// holder's lease alone: the key still names the first task.
	err := svc.renewFAQImportLease(ctx, "kb-1", "second", "inst-second", 0)
	require.ErrorIs(t, err, errFAQImportLeaseTaken)
	info, err := svc.getRunningFAQImportInfo(ctx, "kb-1")
	require.NoError(t, err)
	require.Equal(t, "first", info.TaskID, "a foreign renewal must not touch the holder's lease")

	// Let the lease lapse; the owning task reclaims it rather than renewing
	// nothing, and its next admission still sees it held.
	mr.FastForward(faqImportLeaseTTL + time.Second)
	require.NoError(t, svc.renewFAQImportLease(ctx, "kb-1", "first", "inst-first", 0),
		"the owning task has to take back its own lapsed lease")
	var held *faqImportLeaseHeldError
	require.ErrorAs(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("third")), &held)
	require.Equal(t, "first", held.TaskID, "the reclaimed lease belongs to the owning task")

	// A takeover by someone else is still reported after the lapse.
	mr.FastForward(faqImportLeaseTTL + time.Second)
	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("third")))
	require.ErrorIs(t, svc.renewFAQImportLease(ctx, "kb-1", "stale", "inst-stale", 0), errFAQImportLeaseTaken)
}

// The same ownership rule has to hold on the in-memory path.
func TestRenewalIsOwnershipCheckedWithoutRedis(t *testing.T) {
	svc := &knowledgeService{}
	ctx := context.Background()

	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("first")))
	require.NoError(t, svc.renewFAQImportLease(ctx, "kb-1", "first", "inst-first", 0))
	require.ErrorIs(t, svc.renewFAQImportLease(ctx, "kb-1", "second", "inst-second", 0), errFAQImportLeaseTaken)
}

// Failing open would admit the concurrency the lease exists to stop, at the
// moment the guard cannot be trusted. An unreachable Redis has to refuse.
func TestImportLeaseFailsClosedWhenRedisIsGone(t *testing.T) {
	svc, mr := newLeaseService(t)
	ctx := context.Background()

	mr.Close()

	err := svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("first"))
	require.Error(t, err)
	var held *faqImportLeaseHeldError
	require.False(t, errors.As(err, &held), "an unreachable Redis is a failure, not a held lease")
}

// The in-memory path (no Redis) has to offer the same atomicity.
func TestImportLeaseIsAtomicWithoutRedis(t *testing.T) {
	svc := &knowledgeService{}
	ctx := context.Background()

	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("first")))
	err := svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("second"))
	require.Error(t, err)
}

// A progress save is the point where a takeover is noticed. Once another task
// holds the lease, the run has to stop rather than keep writing beside it; the
// initial/validation/batch saves go through the strict wrapper for that reason.
func TestProgressSaveStopsWhenAnotherTaskTookTheLease(t *testing.T) {
	svc, _ := newLeaseService(t)
	ctx := context.Background()

	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("holder")))
	err := svc.saveFAQImportProgressOrStop(ctx, &types.FAQImportProgress{TaskID: "mine", KBID: "kb-1"})
	require.ErrorIs(t, err, errFAQImportLeaseTaken)

	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-2", leaseInfo("mine")))
	require.NoError(t, svc.saveFAQImportProgressOrStop(ctx, &types.FAQImportProgress{TaskID: "mine", KBID: "kb-2", InstanceID: "inst-mine"}),
		"the lease holder's own saves go through")
}

// A task that loses the lease is archived without retries, so its initiator
// would poll "processing" until the record expires. The stopped task's record
// has to end in a terminal state, while the new holder's marker stays.
func TestProgressSaveMarksTheSupersededTaskStopped(t *testing.T) {
	svc, _ := newLeaseService(t)
	ctx := context.Background()

	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("holder")))
	err := svc.saveFAQImportProgressOrStop(ctx, &types.FAQImportProgress{
		TaskID: "mine", KBID: "kb-1", Status: types.FAQImportStatusProcessing,
	})
	require.ErrorIs(t, err, errFAQImportLeaseTaken)

	got, err := svc.GetFAQImportProgress(ctx, "mine")
	require.NoError(t, err)
	require.Equal(t, types.FAQImportStatusFailed, got.Status)
	require.NotEmpty(t, got.Error, "the record has to say why it stopped")

	info, err := svc.getRunningFAQImportInfo(ctx, "kb-1")
	require.NoError(t, err)
	require.Equal(t, "holder", info.TaskID, "the superseded task must not clear the new holder's marker")
}

// Finalization used to write the import result to the knowledge row first and
// only then notice a lost lease, leaving the old task's counts on a knowledge
// base another import now owns. The result write now happens only while the
// lease still names this run; on a takeover the record is closed out instead.
func TestFinalizationRefusesToWriteTheResultAfterLosingTheLease(t *testing.T) {
	f := newFAQWriteFixture(t)
	f.svc.memFAQRunningImport.Store(f.kb.ID, &runningFAQImportInfo{
		TaskID: "other", InstanceID: "other-1", EnqueuedAt: time.Now().Unix(),
	})

	payload := &types.FAQImportPayload{
		TaskID: "task", KBID: f.kb.ID, KnowledgeID: "faq",
		TenantID: f.kb.TenantID, Mode: types.FAQBatchModeAppend,
	}
	progress := &types.FAQImportProgress{
		TaskID: "task", KBID: f.kb.ID, KnowledgeID: "faq",
		TenantID: f.kb.TenantID, InstanceID: "mine",
	}

	require.ErrorIs(t, f.svc.finalizeFAQValidation(f.ctx, payload, progress, 1), asynq.SkipRetry)

	marker, err := f.svc.getRunningFAQImportInfo(f.ctx, f.kb.ID)
	require.NoError(t, err)
	require.NotNil(t, marker)
	require.Equal(t, "other", marker.TaskID)

	got, err := f.svc.GetFAQImportProgress(f.ctx, "task")
	require.NoError(t, err)
	require.Equal(t, types.FAQImportStatusFailed, got.Status,
		"the stopped task's initiator still has to see a terminal state")

	var knowledge types.Knowledge
	require.NoError(t, f.db.First(&knowledge, "id = ?", "faq").Error)
	result, err := knowledge.GetLastFAQImportResult()
	require.NoError(t, err)
	require.Nil(t, result, "a task that lost the lease must not touch the persisted result")
}

// A marker written by a modern run names one instance. A legacy run (payload
// without an instance id) must not renew or clear that marker just because the
// client reused the task id.
func TestLegacyRunCannotRenewOrClearAModernMarker(t *testing.T) {
	svc, _ := newLeaseService(t)
	ctx := context.Background()

	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("shared")))

	require.ErrorIs(t, svc.renewFAQImportLease(ctx, "kb-1", "shared", "", 0), errFAQImportLeaseTaken,
		"a run without an instance id cannot prove ownership of a modern marker")

	require.NoError(t, svc.clearRunningFAQImportInfoIfMatches(ctx, "kb-1", "shared", "", 0))
	info, err := svc.getRunningFAQImportInfo(ctx, "kb-1")
	require.NoError(t, err)
	require.NotNil(t, info, "the legacy clear must leave the modern marker in place")

	// The owning instance still clears it.
	require.NoError(t, svc.clearRunningFAQImportInfoIfMatches(ctx, "kb-1", "shared", "inst-shared", 0))
	info, err = svc.getRunningFAQImportInfo(ctx, "kb-1")
	require.NoError(t, err)
	require.Nil(t, info)
}

// The same ownership rule holds on the in-memory path.
func TestLegacyRunCannotRenewOrClearAModernMarkerWithoutRedis(t *testing.T) {
	svc := &knowledgeService{}
	ctx := context.Background()

	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("shared")))
	require.ErrorIs(t, svc.renewFAQImportLease(ctx, "kb-1", "shared", "", 0), errFAQImportLeaseTaken)
	require.NoError(t, svc.clearRunningFAQImportInfoIfMatches(ctx, "kb-1", "shared", "", 0))
	info, err := svc.getRunningFAQImportInfo(ctx, "kb-1")
	require.NoError(t, err)
	require.NotNil(t, info)
}

// The record is written only after the lease has been renewed: a run that has
// lost the knowledge base must not overwrite the record of the run that owns
// it, even if it still held the record key from an earlier save.
func TestProgressSaveDoesNotOverwriteAnotherTasksRecord(t *testing.T) {
	svc, _ := newLeaseService(t)
	ctx := context.Background()

	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("holder")))
	require.NoError(t, svc.writeFAQImportProgress(ctx, &types.FAQImportProgress{
		TaskID: "mine", KBID: "kb-1", InstanceID: "mine", Message: "before",
	}))

	err := svc.saveFAQImportProgress(ctx, &types.FAQImportProgress{
		TaskID: "mine", KBID: "kb-1", InstanceID: "mine", Message: "after",
	})
	require.ErrorIs(t, err, errFAQImportLeaseTaken)

	got, err := svc.GetFAQImportProgress(ctx, "mine")
	require.NoError(t, err)
	require.Equal(t, "before", got.Message, "a save that lost the lease must not write")
}

// The in-memory marker has no TTL of its own, so it carries one: a worker that
// died without clearing it must not block the knowledge base forever.
func TestMemoryMarkerExpiresAndIsReclaimable(t *testing.T) {
	svc := &knowledgeService{}
	ctx := context.Background()

	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", &runningFAQImportInfo{TaskID: "first"}))
	err := svc.claimFAQImportLease(ctx, "kb-1", &runningFAQImportInfo{TaskID: "second"})
	require.Error(t, err, "a fresh marker has to keep the lease")

	// Force the deadline into the past.
	held, ok := svc.memFAQRunningImport.Load("kb-1")
	require.True(t, ok)
	live := held.(*runningFAQImportInfo)
	expired := *live
	expired.ExpiresAt = time.Now().Add(-time.Second).UnixNano()
	svc.memFAQRunningImport.Store("kb-1", &expired)

	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", &runningFAQImportInfo{TaskID: "second"}),
		"an expired in-memory marker has to be reclaimable, like the Redis key")
	marker, err := svc.getRunningFAQImportInfo(ctx, "kb-1")
	require.NoError(t, err)
	require.Equal(t, "second", marker.TaskID)
}

// A batch embeds and indexes its whole set between two renewals. The heartbeat
// is what keeps the lease alive through such a batch: without it the lease can
// expire mid-batch and a new submission is admitted beside the running import.
func TestHeartbeatKeepsTheLeaseThroughALongBatch(t *testing.T) {
	svc, mr := newLeaseService(t)
	ctx := context.Background()

	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("running")))

	old := faqImportLeaseHeartbeatInterval
	faqImportLeaseHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { faqImportLeaseHeartbeatInterval = old })

	stop := svc.startFAQImportLeaseHeartbeat(ctx, "kb-1", "running", "inst-running", 0, nil)

	// The lease is nearly gone while the batch is still running: the next
	// heartbeat tick pushes the deadline out, exactly as a periodic renewal
	// would during a slow batch.
	mr.FastForward(faqImportLeaseTTL - time.Second)
	require.Eventually(t, func() bool {
		info, err := svc.getRunningFAQImportInfo(ctx, "kb-1")
		return err == nil && info != nil && info.TaskID == "running" &&
			mr.TTL(getFAQImportRunningKey("kb-1")) > faqImportLeaseTTL/2
	}, 2*time.Second, 5*time.Millisecond, "the heartbeat has to renew the lease")

	var held *faqImportLeaseHeldError
	require.ErrorAs(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("other")), &held,
		"while the run is alive its knowledge base stays reserved")

	// Once the run stops heartbeating, the lease is expired and reclaimable.
	stop()
	mr.FastForward(faqImportLeaseTTL + time.Second)
	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("other")))
}

// A heartbeat that ticks after the run cleared its marker must report the lease
// gone rather than recreate it: an orphan marker would block the knowledge base
// for a whole TTL after the import has already finished.
func TestHeartbeatDoesNotResurrectAClearedLease(t *testing.T) {
	svc, mr := newLeaseService(t)
	ctx := context.Background()

	info := leaseInfo("running")
	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", info))

	old := faqImportLeaseHeartbeatInterval
	faqImportLeaseHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { faqImportLeaseHeartbeatInterval = old })
	notLost := make(chan struct{}, 1)
	stop := svc.startFAQImportLeaseHeartbeat(ctx, "kb-1", "running", "inst-running", 0,
		func() {
			notLost <- struct{}{}
		})

	require.NoError(t, svc.clearRunningFAQImportInfoIfMatches(
		ctx, "kb-1", "running", "inst-running", info.EnqueuedAt))

	// Ticks keep coming after the clear; none of them may write the marker back.
	require.Never(t, func() bool {
		marker, err := svc.getRunningFAQImportInfo(ctx, "kb-1")
		return err == nil && marker != nil
	}, 100*time.Millisecond, 5*time.Millisecond,
		"a heartbeat that outlived its run must not resurrect the lease")

	require.ErrorIs(t, svc.renewFAQImportLeaseAllow(ctx, "kb-1", "running", "inst-running", 0, false),
		errFAQImportLeaseMissing, "a periodic renewal must not take a missing lease back")
	require.Equal(t, time.Duration(0), mr.TTL(getFAQImportRunningKey("kb-1")))
	select {
	case <-notLost:
		t.Fatal("a marker the run cleared itself is not a takeover")
	default:
	}

	stop()
}

// A run whose lease was taken by another task still owns its own progress key:
// the taker writes under a different task id. Its failure has to land there, or
// the initiator polls "processing" until the record expires.
func TestAFailedRunRecordsItsEndAfterLosingTheLease(t *testing.T) {
	svc, _ := newLeaseService(t)
	ctx := context.Background()
	const mine = "faq_import_mine"
	require.NoError(t, svc.writeFAQImportProgress(ctx, &types.FAQImportProgress{
		TaskID: mine, KBID: "kb-1", InstanceID: "inst-mine",
		Status: types.FAQImportStatusProcessing, Total: 1,
	}))
	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("taker")))

	require.NoError(t, svc.updateFAQImportProgressStatus(ctx, mine, "inst-mine", 0,
		types.FAQImportStatusFailed, 0, 1, 0, "stopped", "lease lost"))

	progress, err := svc.GetFAQImportProgress(ctx, mine)
	require.NoError(t, err)
	require.Equal(t, types.FAQImportStatusFailed, progress.Status)
	require.Equal(t, "lease lost", progress.Error)
}

// A run that loses the lease must stop working, not finish its batch beside the
// new owner. The heartbeat reports the takeover so the caller can cancel.
func TestHeartbeatCancelsTheRunWhenTheLeaseMoves(t *testing.T) {
	svc, _ := newLeaseService(t)
	ctx := context.Background()
	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("taker")))

	old := faqImportLeaseHeartbeatInterval
	faqImportLeaseHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { faqImportLeaseHeartbeatInterval = old })

	lost := make(chan struct{}, 1)
	stop := svc.startFAQImportLeaseHeartbeat(ctx, "kb-1", "running", "inst-running", 0,
		func() {
			lost <- struct{}{}
		})
	select {
	case <-lost:
	case <-time.After(2 * time.Second):
		t.Fatal("a takeover has to cancel the run")
	}
	marker, err := svc.getRunningFAQImportInfo(ctx, "kb-1")
	require.NoError(t, err)
	require.Equal(t, "taker", marker.TaskID, "the taker's marker must be left alone")
	stop()
}

// The in-memory progress map has no TTL of its own; a record must expire like
// its Redis key, both when read and when another write sweeps the map.
func TestMemoryProgressExpiresAndIsSwept(t *testing.T) {
	svc := &knowledgeService{}
	ctx := context.Background()
	expiredAt := time.Now().Add(-faqImportProgressTTL - time.Second).Unix()
	oldKey := getFAQImportProgressKey(ctx, "old")
	svc.memFAQProgress.Store(oldKey, &types.FAQImportProgress{TaskID: "old", UpdatedAt: expiredAt})

	_, err := svc.GetFAQImportProgress(ctx, "old")
	require.Error(t, err, "an expired record must look missing")
	_, loaded := svc.memFAQProgress.Load(oldKey)
	require.False(t, loaded)

	staleKey := getFAQImportProgressKey(ctx, "stale")
	svc.memFAQProgress.Store(staleKey, &types.FAQImportProgress{TaskID: "stale", UpdatedAt: expiredAt})
	require.NoError(t, svc.writeFAQImportProgress(ctx, &types.FAQImportProgress{TaskID: "new"}))
	_, loaded = svc.memFAQProgress.Load(staleKey)
	require.False(t, loaded, "a write has to sweep records nobody reads")
	_, loaded = svc.memFAQProgress.Load(getFAQImportProgressKey(ctx, "new"))
	require.True(t, loaded)
}

// Lazy expiry only runs for a knowledge base that is touched again. A claim
// sweeps markers whose lease expired so abandoned knowledge bases do not stay
// in the map for the life of the process.
func TestMemoryMarkersOfUntouchedKBsAreSwept(t *testing.T) {
	svc := &knowledgeService{}
	ctx := context.Background()
	svc.memFAQRunningImport.Store("kb-old", &runningFAQImportInfo{
		TaskID: "old", ExpiresAt: time.Now().Add(-time.Second).UnixNano(),
	})
	svc.memFAQRunningImport.Store("kb-live", &runningFAQImportInfo{
		TaskID: "live", ExpiresAt: time.Now().Add(time.Hour).UnixNano(),
	})

	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-new", leaseInfo("new")))

	_, loaded := svc.memFAQRunningImport.Load("kb-old")
	require.False(t, loaded, "an expired marker of another KB has to be swept")
	_, loaded = svc.memFAQRunningImport.Load("kb-live")
	require.True(t, loaded, "a live marker is untouched")
}

// The gate before a write must not act on a lease it could not read, at either
// tier: an ordinary batch writes rows just as an irreversible step does. Only
// the periodic progress renewal keeps the fail-open behaviour, so a transient
// outage does not freeze the record the initiator polls.
func TestWriteGateRefusesAnUnverifiableLease(t *testing.T) {
	svc, mr := newLeaseService(t)
	ctx := context.Background()
	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("mine")))

	mr.SetError("ERR lease store unavailable")
	t.Cleanup(func() { mr.SetError("") })

	require.ErrorIs(t,
		svc.ensureFAQImportLeaseForWriteStrict(ctx, "kb-1", "mine", "inst-mine", 0),
		errFAQImportLeaseUnverifiable)
	require.ErrorIs(t, svc.ensureFAQImportLeaseForWrite(ctx, "kb-1", "mine", "inst-mine", 0),
		errFAQImportLeaseUnverifiable)
	require.NoError(t, svc.renewFAQImportLease(ctx, "kb-1", "mine", "inst-mine", 0),
		"the progress renewal still logs and continues")

	mr.SetError("")
	require.NoError(t, svc.ensureFAQImportLeaseForWriteStrict(ctx, "kb-1", "mine", "inst-mine", 0))
	require.NoError(t, svc.ensureFAQImportLeaseForWrite(ctx, "kb-1", "mine", "inst-mine", 0))
}

// A takeover is final for the attempt; an unreadable lease store is not. The
// batch gate's caller writes its error straight into the asynq disposition, so
// an unverifiable lease must be retryable -- nothing of the batch was written,
// and a retry resumes from the progress cursor.
func TestWriteGateErrorKeepsAnUnverifiableLeaseRetryable(t *testing.T) {
	gateErr := faqImportLeaseGateError(fmt.Errorf("wrapped: %w", errFAQImportLeaseUnverifiable))
	require.NotErrorIs(t, gateErr, asynq.SkipRetry,
		"an unreadable lease store is retried, not archived")
	require.ErrorIs(t, gateErr, errFAQImportLeaseUnverifiable)

	takeover := faqImportLeaseGateError(errFAQImportLeaseTaken)
	require.ErrorIs(t, takeover, asynq.SkipRetry,
		"another import owns the knowledge base; retrying would only be refused again")
}

// An unreachable store is not proof of a takeover: the heartbeat keeps trying
// instead of cancelling a run that may still own the lease.
func TestHeartbeatTreatsAnUnverifiableLeaseAsTransient(t *testing.T) {
	svc, mr := newLeaseService(t)
	ctx := context.Background()
	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("running")))

	old := faqImportLeaseHeartbeatInterval
	faqImportLeaseHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { faqImportLeaseHeartbeatInterval = old })

	lost := make(chan struct{}, 1)
	stop := svc.startFAQImportLeaseHeartbeat(ctx, "kb-1", "running", "inst-running", 0,
		func() {
			lost <- struct{}{}
		})
	mr.SetError("ERR lease store unavailable")
	t.Cleanup(func() { mr.SetError("") })

	require.Never(t, func() bool {
		select {
		case <-lost:
			return true
		default:
			return false
		}
	}, 100*time.Millisecond, 5*time.Millisecond,
		"an unreadable lease is not a takeover")

	mr.SetError("")
	info, err := svc.getRunningFAQImportInfo(ctx, "kb-1")
	require.NoError(t, err)
	require.NotNil(t, info, "the heartbeat has to keep renewing once the store is back")
	stop()
}

// asynq cancels the task context on shutdown. The heartbeat has to exit with it,
// and stop must not hang waiting for a goroutine that already saw the cancel.
func TestHeartbeatExitsWhenTheRunContextIsCancelled(t *testing.T) {
	svc, _ := newLeaseService(t)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, svc.claimFAQImportLease(ctx, "kb-1", leaseInfo("running")))

	old := faqImportLeaseHeartbeatInterval
	faqImportLeaseHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { faqImportLeaseHeartbeatInterval = old })

	stop := svc.startFAQImportLeaseHeartbeat(ctx, "kb-1", "running", "inst-running", 0, nil)
	cancel()

	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop has to return after the run context is cancelled")
	}
	info, err := svc.getRunningFAQImportInfo(context.Background(), "kb-1")
	require.NoError(t, err)
	require.NotNil(t, info, "a cancelled heartbeat must not clear the marker")
}
