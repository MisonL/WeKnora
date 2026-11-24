package service

// 上传预算复核缺陷修复回归：declared 谎报不得压低真实行大小；handoff 回收
// 不得释放仍在队列里的作业的预算。

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/utils"
)

// The enqueue-side charge must never trust the declared (Content-Length based)
// figure below what the row actually stores: a client can understate
// Content-Length, while knowledge.FileSize is computed by the multipart
// parser as bytes hit the disk. A larger declaration stays (chunked uploads
// pre-charge the cap); zero on both means the floor applies later.
func TestDeclaredUploadFileBytesNeverUnderstatesTheRow(t *testing.T) {
	ctx := context.Background()

	// 谎报 5MB 的声明 vs 真实 50MB 行：按大的计。
	require.Equal(t, int64(50<<20),
		declaredUploadFileBytes(WithDeclaredUploadBytes(ctx, 5<<20), 50<<20))

	// 声明更保守（分块上传按 cap 预扣）时保留声明。
	require.Equal(t, int64(64<<20),
		declaredUploadFileBytes(WithDeclaredUploadBytes(ctx, 64<<20), 50<<20))

	// 无声明走行大小；两者皆零回 0（ReserveUploadWork 的 floor 兜底）。
	require.Equal(t, int64(50<<20), declaredUploadFileBytes(ctx, 50<<20))
	require.Zero(t, declaredUploadFileBytes(ctx, 0))
	_ = utils.EnvUploadMaxBackgroundBytes
}

// A reservation whose worker has not arrived is only an orphan once the job
// has left the queue: on the shipped single-process deployment a busy worker
// leaves queued jobs unclaimed for longer than the handoff TTL, and freeing
// their budget then loosens the guard exactly while the host is backed up.
func TestHandoffReclaimKeepsBudgetForQueuedJobs(t *testing.T) {
	withUploadBackgroundBudget(t, 30<<20)

	releaseA, err := ReserveUploadWork("row-a", 10<<20, requestUploadWork)
	require.NoError(t, err)
	defer releaseA()

	// Force row-a past its handoff deadline, as a backlog would.
	raw, ok := uploadWorkIDs.Load("row-a")
	require.True(t, ok)
	raw.(*heldUploadWork).uploaderDeadline.Store(time.Now().Add(-time.Second).UnixNano())

	// The queue still holds row-a's job: the sweep must keep charging it.
	setUploadWorkQueueProbe(t, map[string]bool{"row-a": true})

	_, err = ReserveUploadWork("row-b", 10<<20, requestUploadWork)
	require.NoError(t, err)
	require.Equal(t, int64(20<<20), uploadWorkHeld(),
		"row-a stays charged while its job is still in the queue")
	require.EqualValues(t, 2, uploadWorkJobsHeld())

	// Once the job is gone from the queue (a replica that never shows it ran
	// or dropped it), reclaiming is correct: row-c's reservation proves the
	// sweep ran and freed row-a.
	setUploadWorkQueueProbe(t, nil)
	releaseC, err := ReserveUploadWork("row-c", 1<<20, requestUploadWork)
	require.NoError(t, err)
	defer releaseC()
	require.Equal(t, int64(11<<20), uploadWorkHeld(),
		"an orphan with no queue entry is reclaimed at the deadline")
}

// A probe failure cannot prove the job is gone: the sweep keeps the charge
// (conservative direction, the worker will settle the account when it arrives).
func TestHandoffReclaimKeepsBudgetWhenTheQueueProbeFails(t *testing.T) {
	withUploadBackgroundBudget(t, 30<<20)

	releaseA, err := ReserveUploadWork("row-a", 10<<20, requestUploadWork)
	require.NoError(t, err)
	defer releaseA()
	raw, ok := uploadWorkIDs.Load("row-a")
	require.True(t, ok)
	raw.(*heldUploadWork).uploaderDeadline.Store(time.Now().Add(-time.Second).UnixNano())

	setUploadWorkQueueProbeError(t, true)
	_, err = ReserveUploadWork("row-b", 10<<20, requestUploadWork)
	require.NoError(t, err)
	require.Equal(t, int64(20<<20), uploadWorkHeld(),
		"an unreadable queue is not proof to release budget")
}

// setUploadWorkQueueProbe installs a queue-liveness probe for the sweep and
// restores the unprobed behaviour on cleanup.
func setUploadWorkQueueProbe(t *testing.T, queued map[string]bool) {
	t.Helper()
	SetUploadWorkQueueProbe(func(context.Context) (map[string]bool, error) {
		return queued, nil
	})
	t.Cleanup(func() { SetUploadWorkQueueProbe(nil) })
}

// A reservation must not pay for a queue scan when no claim is a reclaim
// candidate. The probe answers a question about expired rows; an empty
// candidate set has no question, and the scan runs under the reservation lock.
func TestReserveSkipsTheQueueProbeWhenNothingIsExpired(t *testing.T) {
	withUploadBackgroundBudget(t, 30<<20)

	releaseA, err := ReserveUploadWork("row-a", 1<<20, requestUploadWork)
	require.NoError(t, err)
	defer releaseA()

	var mu sync.Mutex
	calls := 0
	SetUploadWorkQueueProbe(func(context.Context) (map[string]bool, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return nil, nil
	})
	t.Cleanup(func() { SetUploadWorkQueueProbe(nil) })

	releaseB, err := ReserveUploadWork("row-b", 1<<20, requestUploadWork)
	require.NoError(t, err)
	defer releaseB()

	mu.Lock()
	defer mu.Unlock()
	require.Zero(t, calls, "no expired claim means no queue scan")
}

// The queue read is the slow part of the sweep and it must not hold the
// reservation lock: a release arriving while the probe is still reading has to
// finish, or every upload in flight waits behind one scan.
func TestQueueProbeRunsOutsideTheReservationLock(t *testing.T) {
	withUploadBackgroundBudget(t, 30<<20)

	// One expired uploader claim makes the next reservation consult the probe.
	releaseStale, err := ReserveUploadWork("row-stale", 1<<20, requestUploadWork)
	require.NoError(t, err)
	t.Cleanup(releaseStale)

	releaseOther, err := ReserveUploadWork("row-other", 1<<20, requestUploadWork)
	require.NoError(t, err)
	t.Cleanup(releaseOther)

	// Install the probe before the row expires: without one the sweep itself
	// reclaims the row and there would be nothing left to ask about.
	entered := make(chan struct{})
	leave := make(chan struct{})
	SetUploadWorkQueueProbe(func(context.Context) (map[string]bool, error) {
		close(entered)
		<-leave
		return map[string]bool{"row-stale": true}, nil
	})
	t.Cleanup(func() { SetUploadWorkQueueProbe(nil) })

	raw, ok := uploadWorkIDs.Load("row-stale")
	require.True(t, ok)
	raw.(*heldUploadWork).uploaderDeadline.Store(time.Now().Add(-time.Second).UnixNano())

	reserved := make(chan error, 1)
	go func() {
		release, err := ReserveUploadWork("row-next", 1<<20, requestUploadWork)
		if err == nil {
			t.Cleanup(release)
		}
		reserved <- err
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the sweep never reached the probe")
	}

	// The probe is blocked. A release must not wait behind it.
	released := make(chan struct{})
	go func() {
		releaseOther()
		close(released)
	}()
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("a release blocked on the reservation lock while the queue probe was reading")
	}

	close(leave)
	require.NoError(t, <-reserved)
}

// The probe runs without the lock, so a row collected as expired can be alive
// again by the time its verdict lands: a new uploader claim refreshes the
// deadline. Reclaiming it then would free budget for a job that just arrived.
func TestSweepDoesNotReclaimARowAClaimRevivedDuringTheProbe(t *testing.T) {
	withUploadBackgroundBudget(t, 30<<20)

	releaseA, err := ReserveUploadWork("row-a", 1<<20, requestUploadWork)
	require.NoError(t, err)
	t.Cleanup(releaseA)

	entered := make(chan struct{})
	leave := make(chan struct{})
	var probeCalls atomic.Int32
	SetUploadWorkQueueProbe(func(context.Context) (map[string]bool, error) {
		if probeCalls.Add(1) == 1 {
			close(entered)
			<-leave
		}
		return nil, nil // the queue says the job is gone
	})
	t.Cleanup(func() { SetUploadWorkQueueProbe(nil) })

	raw, ok := uploadWorkIDs.Load("row-a")
	require.True(t, ok)
	raw.(*heldUploadWork).uploaderDeadline.Store(time.Now().Add(-time.Second).UnixNano())

	swept := make(chan struct{})
	go func() {
		_, err := ReserveUploadWork("row-b", 1<<20, requestUploadWork)
		require.NoError(t, err)
		close(swept)
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the sweep never reached the probe")
	}
	// While the probe reads, the same row gets a fresh upload claim.
	releaseAgain, err := ReserveUploadWork("row-a", 1<<20, requestUploadWork)
	require.NoError(t, err)
	t.Cleanup(releaseAgain)

	close(leave)
	<-swept

	// The stale charge was released (its own sweep saw the probe say "gone"),
	// then the revived claim added its own; sweep #1 must not free that one.
	require.Equal(t, int64(2<<20), uploadWorkHeld(),
		"the revived claim survives the in-flight sweep's verdict")
	held, ok := uploadWorkIDs.Load("row-a")
	require.True(t, ok, "the revived row is still accounted for")
	require.Positive(t, held.(*heldUploadWork).uploaderClaims.Load())
}

func setUploadWorkQueueProbeError(t *testing.T, failing bool) {
	t.Helper()
	SetUploadWorkQueueProbe(func(context.Context) (map[string]bool, error) {
		if failing {
			return nil, errors.New("queue unavailable")
		}
		return nil, nil
	})
	t.Cleanup(func() { SetUploadWorkQueueProbe(nil) })
}
