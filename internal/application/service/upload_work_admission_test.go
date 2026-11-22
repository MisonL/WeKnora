package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/utils"
)

func withUploadBackgroundBudget(t *testing.T, bytes int64) {
	t.Helper()
	t.Setenv(utils.EnvUploadMaxBackgroundBytes, itoaDecimal(bytes))
	resetUploadWork()
	t.Cleanup(resetUploadWork)
}

func itoaDecimal(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// The parse-job budget is held until the job ends, not until the request that
// enqueued it does: a client that disconnects after the upload was accepted
// must not free budget for the job it left running.
func TestReserveUploadWorkIsHeldUntilReleased(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	a, err := ReserveUploadWork("doc-a", 60, requestUploadWork)
	require.NoError(t, err)
	require.Equal(t, int64(60), uploadWorkHeld())

	_, err = ReserveUploadWork("doc-b", 60, requestUploadWork)
	require.Error(t, err, "the running job still owns its bytes")

	a()
	require.Equal(t, int64(0), uploadWorkHeld())

	b, err := ReserveUploadWork("doc-b", 60, requestUploadWork)
	require.NoError(t, err)
	b()
}

// A release left on a defer and one from an error path must not subtract twice.
func TestEndUploadWorkIsIdempotentWithRelease(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	release, err := ReserveUploadWork("doc-a", 40, requestUploadWork)
	require.NoError(t, err)
	EndUploadWork("doc-a")
	release()

	require.Equal(t, int64(0), uploadWorkHeld())
	again, err := ReserveUploadWork("doc-a", 40, requestUploadWork)
	require.NoError(t, err)
	again()
}

// Enqueue site and worker both claim the same knowledge id. The second claim
// must reuse the first reservation: charging twice would halve the effective
// budget for every document.
func TestReserveUploadWorkIsKeyedByKnowledgeID(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	first, err := ReserveUploadWork("doc-a", 80, requestUploadWork)
	require.NoError(t, err)
	second, err := ReserveUploadWork("doc-a", 0, requestWorkerWork)
	require.NoError(t, err, "a second claim of a working row is not new work")
	require.Equal(t, int64(80), uploadWorkHeld())

	// A second uploader claim borrows the row's releases rather than adding
	// one: the job that follows is the row's only parse.
	borrowed, err := ReserveUploadWork("doc-a", 80, requestUploadWork)
	require.NoError(t, err)
	borrowed()
	second()
	first()
	require.Equal(t, int64(0), uploadWorkHeld(), "borrowed releases must not leave bytes behind")
}

// A task that cannot be attributed to a knowledge row still takes its own slot,
// so an unparseable payload cannot bypass the budget -- and two such jobs must
// not share one reservation, where one release would free the other's budget.
func TestParseWorkKeyForTaskFallsBackWithoutAKnowledgeID(t *testing.T) {
	first := asynq.NewTask("document:process", []byte("not json"))
	key := parseWorkKeyForTask(first)
	require.True(t, strings.HasPrefix(key, "asynq-task:"), "got %q", key)
	require.Equal(t, key, parseWorkKeyForTask(asynq.NewTask("document:process", []byte("not json"))),
		"a retry carries the same payload, so it rejoins its own row")
	require.NotEqual(t, key, parseWorkKeyForTask(asynq.NewTask("document:process", []byte("other payload"))))

	task := asynq.NewTask("document:process", []byte(`{"knowledge_id":"doc-1"}`))
	require.Equal(t, "doc-1", parseWorkKeyForTask(task))
}

// The job's size is the admitted upload size when known, and the file on disk
// otherwise; with neither it is zero rather than a guess.
func TestParseJobWorkingBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello world"), 0o600))

	require.Equal(t, int64(4096), ParseJobWorkingBytes(4096, []string{path}, nil), "the admitted size wins")
	require.Equal(t, int64(11), ParseJobWorkingBytes(0, []string{path}, nil))
	// A local path that does not resolve says nothing about its size; its
	// string length would understate the job by orders of magnitude, so the
	// unattributable floor applies instead.
	missing := filepath.Join(dir, "missing")
	require.Equal(t, parseJobUnknownCharge, ParseJobWorkingBytes(0, []string{missing}, nil))
	require.Equal(t, parseJobUnknownCharge, ParseJobWorkingBytes(0, []string{path, missing}, nil),
		"a known file must not lower the floor for a path whose size is unknown")
	require.Equal(t, int64(0), ParseJobWorkingBytes(0, nil, nil))
	// Text passages are content, not paths.
	require.Equal(t, int64(5), ParseJobWorkingBytes(0, nil, []string{"hello"}))
	require.Equal(t, int64(16), ParseJobWorkingBytes(0, []string{path}, []string{"hello"}), "the sources add up")
	require.Equal(t, int64(11), ParseJobWorkingBytes(0, nil, []string{"hello", " world"}),
		"every passage the worker holds is charged")
	require.Equal(t, int64(23), ParseJobWorkingBytes(0, nil, []string{"https://example.com/doc"}),
		"a passage is text even when it contains a URL")
	second := filepath.Join(dir, "second.txt")
	require.NoError(t, os.WriteFile(second, []byte("four"), 0o600))
	require.Equal(t, int64(15), ParseJobWorkingBytes(0, []string{path, second}, nil),
		"several files are all held at once")
}

// A reparse or a URL-imported row has no uploader charge. Its worker must
// budget the bytes it will actually work on, not zero.
func TestWorkerClaimBudgetsRowsNoUploadCharged(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	worker, err := ReserveUploadWork("doc-reparse", ParseJobWorkingBytes(0, nil, []string{"0123456789"}), requestWorkerWork)
	require.NoError(t, err)
	require.Equal(t, int64(10), uploadWorkHeld())
	worker()
	require.Equal(t, int64(0), uploadWorkHeld())
}

// The knobs are documented with defaults; an operator's value is honoured.
func TestUploadBackgroundBudgetComesFromTheEnv(t *testing.T) {
	t.Setenv(utils.EnvUploadMaxBackgroundBytes, "7")
	require.Equal(t, int64(7), utils.GetUploadMaxBackgroundBytes())

	t.Setenv(utils.EnvUploadMaxBackgroundBytes, "not-a-number")
	require.Equal(t, utils.DefaultUploadMaxBackgroundBytes, utils.GetUploadMaxBackgroundBytes())
}

// The worker's claim is the uploader's reservation, not a second one: while
// the parse runs, neither the uploader's release nor the worker's own end may
// free the row's bytes.
func TestWorkerClaimDoesNotDoubleChargeOrFreeEarly(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	uploader, err := ReserveUploadWork("doc-a", 70, requestUploadWork)
	require.NoError(t, err)
	worker, err := ReserveUploadWork("doc-a", 0, requestWorkerWork)
	require.NoError(t, err)
	require.Equal(t, int64(70), uploadWorkHeld(), "the worker must not charge the row again")

	uploader()
	require.Equal(t, int64(70), uploadWorkHeld(), "the job is still running")

	// A retry makes the worker claim again; only the last end frees the row.
	retry, err := ReserveUploadWork("doc-a", 0, requestWorkerWork)
	require.NoError(t, err)
	worker()
	require.Equal(t, int64(70), uploadWorkHeld())
	retry()
	require.Equal(t, int64(0), uploadWorkHeld())
}

// The worker usually starts after the handler already returned. That row's
// bytes are already freed, so the worker starts its own reservation instead of
// running the parse unbudgeted -- one parse, one budget.
func TestWorkerClaimAfterUploaderReleaseStartsItsOwnReservation(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	uploader, err := ReserveUploadWork("doc-a", 90, requestUploadWork)
	require.NoError(t, err)
	uploader()
	require.Equal(t, int64(0), uploadWorkHeld())

	worker, err := ReserveUploadWork("doc-a", ParseJobWorkingBytes(90, nil, nil), requestWorkerWork)
	require.NoError(t, err)
	require.Equal(t, int64(90), uploadWorkHeld(), "the running parse must be budgeted")
	worker()
	require.Equal(t, int64(0), uploadWorkHeld())
}

// A cancel can land after the worker dequeued the task and before it claimed
// the row. Ending the budget there must not free it under the running parse.
func TestEndUploadWorkKeepsARunningWorkerClaim(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	worker, err := ReserveUploadWork("doc-a", 70, requestWorkerWork)
	require.NoError(t, err)
	require.Equal(t, int64(70), uploadWorkHeld())

	EndUploadWork("doc-a")
	require.Equal(t, int64(70), uploadWorkHeld(), "the parse is still running")

	worker()
	require.Equal(t, int64(0), uploadWorkHeld())
}

// The handler and the worker release in either order; the row is freed once,
// on the later one, and never twice.
func TestUploaderAndWorkerReleaseInEitherOrder(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	for _, when := range []string{"uploader first", "worker first"} {
		resetUploadWork()
		uploader, err := ReserveUploadWork("doc-a", 40, requestUploadWork)
		require.NoError(t, err)
		worker, err := ReserveUploadWork("doc-a", 0, requestWorkerWork)
		require.NoError(t, err)
		require.Equal(t, int64(40), uploadWorkHeld(), when)
		if when == "uploader first" {
			uploader()
			require.Equal(t, int64(40), uploadWorkHeld(), when)
			worker()
		} else {
			// The worker's claim already took the row over from the enqueue
			// side, so the parse ending is the row ending.
			worker()
			require.Equal(t, int64(0), uploadWorkHeld(), when)
			uploader()
		}
		require.Equal(t, int64(0), uploadWorkHeld(), when)
		uploader()
		worker()
		require.Equal(t, int64(0), uploadWorkHeld(), when+": a repeated release must not subtract twice")
	}
}

// A reparse of an existing row claims for both uploader and worker; the bytes
// are freed once, when the parse ends.
func TestReparseClaimsAreCountedOnce(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	uploader, err := ReserveUploadWork("doc-a", 30, requestUploadWork)
	require.NoError(t, err)
	worker, err := ReserveUploadWork("doc-a", 0, requestWorkerWork)
	require.NoError(t, err)
	require.Equal(t, int64(30), uploadWorkHeld())

	uploader()
	worker()
	require.Equal(t, int64(0), uploadWorkHeld())
}

// A running parse keeps its budget through a cancel: the worker is still
// using the bytes, and ending the row early would let the next uploads count
// against budget the parse no longer holds.
func TestEndUploadWorkKeepsBudgetWhileAParseRuns(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	uploader, err := ReserveUploadWork("doc-a", 50, requestUploadWork)
	require.NoError(t, err)
	worker, err := ReserveUploadWork("doc-a", 0, requestWorkerWork)
	require.NoError(t, err)
	// The success path hands the row to the worker, which is the run this test
	// stands in for; ending the row consumes the enqueue side's claim.
	uploader()
	require.Equal(t, int64(50), uploadWorkHeld())
	EndUploadWork("doc-a")
	require.Equal(t, int64(50), uploadWorkHeld(), "the parse is still running")

	worker()
	require.Equal(t, int64(0), uploadWorkHeld())
	worker()
	require.Equal(t, int64(0), uploadWorkHeld(), "a repeated release must not push the budget negative")
}

// A queued task that is dequeued (cancel, delete, enqueue rollback) never
// reaches ProcessDocument, so nothing else would end its reservation.
func TestEndUploadWorkRepresentsADequeuedTask(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	_, err := ReserveUploadWork("doc-a", 70, requestUploadWork)
	require.NoError(t, err)
	require.Equal(t, int64(70), uploadWorkHeld())

	EndUploadWork("doc-a")
	require.Equal(t, int64(0), uploadWorkHeld())

	// The row can be parsed again afterwards, as a fresh reservation.
	again, err := ReserveUploadWork("doc-a", 70, requestUploadWork)
	require.NoError(t, err, "an ended row must not keep its budget")
	again()
	require.Equal(t, int64(0), uploadWorkHeld())
}

// Every replica runs a worker over one shared queue, so a job enqueued here can
// be run by another replica. Nothing on this process would then take the
// enqueue-side claim over or end it, and the budget would shrink with every
// upload until the process restarts. An uploader claim whose worker never
// arrived has to expire.
func TestStaleUploaderClaimsAreReclaimed(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	_, err := ReserveUploadWork("doc-a", 90, requestUploadWork)
	require.NoError(t, err)
	require.Equal(t, int64(90), uploadWorkHeld())

	held, ok := uploadWorkIDs.Load("doc-a")
	require.True(t, ok)
	held.(*heldUploadWork).uploaderDeadline.Store(time.Now().Add(-time.Second).UnixNano())

	next, err := ReserveUploadWork("doc-b", 90, requestUploadWork)
	require.NoError(t, err, "the abandoned claim must not pin the budget")
	next()
	require.Equal(t, int64(0), uploadWorkHeld())

	// A claim still inside its handoff window keeps its bytes.
	_, err = ReserveUploadWork("doc-c", 90, requestUploadWork)
	require.NoError(t, err)
	_, err = ReserveUploadWork("doc-d", 90, requestUploadWork)
	require.Error(t, err, "a fresh claim has not expired yet")
}

// A row a worker has taken over is the running parse's, however old it looks:
// the sweep must not free bytes the job is still using.
func TestSweepDoesNotTouchWorkerOwnedRows(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	worker, err := ReserveUploadWork("doc-a", 90, requestWorkerWork)
	require.NoError(t, err)

	held, _ := uploadWorkIDs.Load("doc-a")
	held.(*heldUploadWork).uploaderDeadline.Store(time.Now().Add(-time.Hour).UnixNano())

	_, err = ReserveUploadWork("doc-b", 90, requestUploadWork)
	require.Error(t, err, "the running parse still owns its bytes")
	worker()
	require.Equal(t, int64(0), uploadWorkHeld())
}

// The budget is what keeps a queue of documents from outrunning the host, so
// a row that arrives when it is full must be refused, not wait.
func TestReserveUploadWorkRefusesWhenBackgroundBudgetIsFull(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	first, err := ReserveUploadWork("doc-a", 100, requestUploadWork)
	require.NoError(t, err)

	_, err = ReserveUploadWork("doc-b", 1, requestUploadWork)
	require.Error(t, err)
	require.Equal(t, int64(100), uploadWorkHeld(), "a refusal must not leak its bytes")

	first()
	second, err := ReserveUploadWork("doc-b", 1, requestUploadWork)
	require.NoError(t, err)
	second()
}

// A cancel lands after the worker dequeued the task: the worker then joins the
// row the cancel already ended. It must budget its run, and must end the row
// when it finishes -- otherwise every cancelled-then-retried row would pin its
// bytes for the life of the process.
func TestWorkerJoiningAnEndedRowStillEndsIt(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	uploader, err := ReserveUploadWork("doc-a", 60, requestUploadWork)
	require.NoError(t, err)
	EndUploadWork("doc-a")
	require.Equal(t, int64(0), uploadWorkHeld())

	// The queued task had been dequeued before the cancel was seen; it claims
	// now, after the uploader's side ended.
	worker, err := ReserveUploadWork("doc-a", 60, requestWorkerWork)
	require.NoError(t, err)
	require.Equal(t, int64(60), uploadWorkHeld(), "the parse must be budgeted")

	uploader()
	worker()
	require.Equal(t, int64(0), uploadWorkHeld(), "the finished run must release the row")
}

// The exact cancel/worker interleaving that used to strand bytes: the task is
// enqueued (enqueue site claims, then hands over), the cancel ends the row
// before the worker claims, and the worker then runs anyway because the queue
// entry could not be removed. Both the run's budget and its release must hold.
func TestWorkerAfterCancelStillBudgetsAndEndsItsRun(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	_, err := ReserveUploadWork("doc-a", 40, requestUploadWork)
	require.NoError(t, err)
	// The success path does not release: the row is handed to the worker.
	require.Equal(t, int64(40), uploadWorkHeld())

	EndUploadWork("doc-a") // cancel lands before the worker starts
	require.Equal(t, int64(0), uploadWorkHeld())

	worker, err := ReserveUploadWork("doc-a", ParseJobWorkingBytes(40, nil, nil), requestWorkerWork)
	require.NoError(t, err)
	require.Equal(t, int64(40), uploadWorkHeld(), "the still-running parse must be budgeted")
	worker()
	require.Equal(t, int64(0), uploadWorkHeld(), "the ended run must not strand its bytes")

	// A later reparse of the same row: enqueued, and then cancelled before any
	// worker picked it up. The cancel has to end that enqueue side's claim --
	// the success path never releases it -- or the row is charged forever.
	_, err = ReserveUploadWork("doc-a", 40, requestUploadWork)
	require.NoError(t, err)
	require.Equal(t, int64(40), uploadWorkHeld())
	EndUploadWork("doc-a")
	require.Equal(t, int64(0), uploadWorkHeld())

	// And the next upload is not charged against that cancelled row.
	next, err := ReserveUploadWork("doc-b", 90, requestUploadWork)
	require.NoError(t, err, "a cancelled row must not exhaust the budget")
	next()
	EndUploadWork("doc-b")
	require.Equal(t, int64(0), uploadWorkHeld())
}

// The enqueue side releases in its error paths (enqueue failed, rollback) while
// the worker of a previous run is still parsing the row. That release must not
// free bytes the running parse is using.
func TestUploaderReleaseDoesNotFreeARunningParse(t *testing.T) {
	withUploadBackgroundBudget(t, 100)

	worker, err := ReserveUploadWork("doc-a", 60, requestWorkerWork)
	require.NoError(t, err)
	require.Equal(t, int64(60), uploadWorkHeld())

	rollback, err := ReserveUploadWork("doc-a", 60, requestUploadWork)
	require.NoError(t, err)
	rollback()
	require.Equal(t, int64(60), uploadWorkHeld(), "the parse is still running")

	worker()
	require.Equal(t, int64(0), uploadWorkHeld())
}

// A URL/minio provider URL is not text: its length says nothing about what the
// job will read, so it must not be charged as a few dozen bytes.
func TestParseJobWorkingBytesIgnoresProviderURLs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello world"), 0o600))

	require.Equal(t, int64(0), ParseJobWorkingBytes(0, []string{"local://bucket/key"}, nil),
		"a provider URL is neither a readable path nor text")
	require.Equal(t, int64(0), ParseJobWorkingBytes(0, []string{"local://bucket/key", "minio://bucket/other"}, nil))
	require.Equal(t, int64(11), ParseJobWorkingBytes(0, []string{"local://bucket/key", path}, nil),
		"a readable source still wins")
}

// A job whose input size cannot be determined must still hold budget; with
// zero it used to ride for free and the budget never bound URL/IM imports.
func TestUnattributableJobsChargeTheFloor(t *testing.T) {
	withUploadBackgroundBudget(t, parseJobUnknownCharge+1)

	release, err := ReserveUploadWork("url-doc", ParseJobWorkingBytes(0, []string{"local://kb/doc.pdf"}, nil), requestWorkerWork)
	require.NoError(t, err)
	require.Equal(t, parseJobUnknownCharge, uploadWorkHeld(),
		"an unattributable job holds the floor, not zero")

	_, err = ReserveUploadWork("url-doc-2", ParseJobWorkingBytes(0, []string{"minio://kb/doc.pdf"}, nil), requestWorkerWork)
	require.Error(t, err, "the floor has to consume the budget it was charged")
	release()

	again, err := ReserveUploadWork("url-doc-2", ParseJobWorkingBytes(0, []string{"minio://kb/doc.pdf"}, nil), requestWorkerWork)
	require.NoError(t, err)
	again()
	require.Equal(t, int64(0), uploadWorkHeld())
	require.Equal(t, int32(0), uploadWorkJobsHeld())
}

// Small documents can be many; the byte budget alone would never stop them, so
// the job count is capped too.
func TestBackgroundJobCountIsCapped(t *testing.T) {
	withUploadBackgroundBudget(t, 1<<30)
	t.Setenv(utils.EnvUploadMaxBackgroundJobs, "2")
	resetUploadWork()

	first, err := ReserveUploadWork("doc-a", 10, requestUploadWork)
	require.NoError(t, err)
	second, err := ReserveUploadWork("doc-b", 10, requestUploadWork)
	require.NoError(t, err)
	require.Equal(t, int32(2), uploadWorkJobsHeld())

	_, err = ReserveUploadWork("doc-c", 10, requestUploadWork)
	require.Error(t, err, "the third job exceeds the job count cap")
	require.Equal(t, int32(2), uploadWorkJobsHeld(), "a refused job leaves the count alone")

	// A release frees a slot for the next job.
	first()
	third, err := ReserveUploadWork("doc-c", 10, requestUploadWork)
	require.NoError(t, err)

	second()
	third()
	require.Equal(t, int64(0), uploadWorkHeld())
	require.Equal(t, int32(0), uploadWorkJobsHeld())
}

// Every transition of a reservation shares one process-wide table. Claims,
// releases, cancels and stale sweeps used to race on it: a claim could land on
// a row already removed from the table (its budget never refunded), and the
// sweep could remove a row a worker had just joined. Hammer every path at once
// and require the budget to come back to exactly zero with no row left.
func TestConcurrentClaimsOnOneRowReturnTheBudgetToZero(t *testing.T) {
	resetUploadWork()
	t.Cleanup(resetUploadWork)

	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				release, err := ReserveUploadWork("kb-concurrent", 1<<20, (worker+j)%2 == 0)
				if err != nil {
					return
				}
				if j%3 == 0 {
					EndUploadWork("kb-concurrent")
				}
				release()
			}
		}(i)
	}
	wg.Wait()

	require.Zero(t, uploadWorkHeld(), "every reservation has to be refunded")
	require.Zero(t, uploadWorkJobsHeld())
	_, left := uploadWorkIDs.Load("kb-concurrent")
	require.False(t, left, "the row has to be gone once every claim is released")
}

// A row created without an HTTP admission (IM, datasource, reparse) has only
// the size recorded on it; a row that went through admission keeps the figure
// its slot was sized on.
func TestDeclaredUploadFileBytesPrefersTheAdmission(t *testing.T) {
	ctx := context.Background()
	require.Equal(t, int64(2048), declaredUploadFileBytes(ctx, 2048))
	require.Equal(t, int64(4096), declaredUploadFileBytes(WithDeclaredUploadBytes(ctx, 4096), 2048))
}
