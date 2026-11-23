package service

// 上传预算复核缺陷修复回归：declared 谎报不得压低真实行大小；handoff 回收
// 不得释放仍在队列里的作业的预算。

import (
	"context"
	"errors"
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
