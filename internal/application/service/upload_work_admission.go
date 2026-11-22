package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hibiken/asynq"

	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

// Parse-job admission.
//
// The handler-side upload slot is released as soon as the HTTP request returns,
// but the parse job the upload enqueued keeps running. A caller that closes the
// connection after the upload was accepted does not cancel that job, so holding
// the job's budget on the request would let the next uploads pile onto a host
// that is still parsing the previous batch. This budget is therefore held by
// the job and released only when it reaches a terminal state.
//
// It is process-local, like the asynq queue it protects: this deployment runs
// its workers inside the same process that serves the API (RunAsynqServer), so
// the enqueue site and the worker share one counter. A deployment that splits
// them into separate processes would give each its own budget, and the env
// ceiling should then be read as "per process that runs parse jobs".
var (
	uploadWorkBytes atomic.Int64
	uploadWorkJobs  atomic.Int32
	uploadWorkIDs   sync.Map // knowledge id -> *heldUploadWork

	// uploadWorkMu serializes every transition of a reservation: joining one,
	// releasing a claim, freeing an idle row, reclaiming a stale uploader claim.
	//
	// Two interleavings made the counters alone insufficient. A worker could
	// load a row and be preempted before claiming it, while EndUploadWork or
	// the stale-claim sweep zeroed its claims and CompareAndDelete removed it:
	// the worker then charged a row no longer in the table, and freeing it
	// refunded nothing. In the other direction, the sweep could observe zero
	// claims, be preempted, and remove a row a worker had just joined, leaving
	// a running parse outside the budget. The lock closes both windows; it is
	// taken on the upload and parse-job paths only, at most a few times per
	// document, so serializing them costs nothing measurable.
	uploadWorkMu sync.Mutex
)

// parseJobUnknownCharge is what a parse job holds when the size of its input
// cannot be determined: a URL import before the page is fetched, an IM file
// handed over as a provider URL, or a reprocessed row whose source has moved.
// Charging zero would leave every such job outside the budget; charging a
// floor keeps the budget meaningful without pretending to know the size.
const parseJobUnknownCharge = int64(4 << 20)

// uploaderHandoffTTL bounds how long an enqueue-side claim waits for its
// worker. Normal handoff is seconds; the bound only matters when the job went
// to another replica, whose worker claim is not visible here.
const uploaderHandoffTTL = 10 * time.Minute

// heldUploadWork is one row's reservation. A row has at most one, however many
// claims reference it; the bytes are counted once and freed once.
//
// Ownership moves from the enqueue site to the worker. The enqueue site holds a
// claim only until the worker picks the row up (the success path never releases
// it: the row has been handed over). From then on the row lives and dies with
// the parse job, which is what keeps a cancelled-then-running parse inside the
// budget instead of freeing bytes it is still using.
//
// That handoff is process-local, while the queue is not: every replica runs a
// worker, so a job enqueued on one replica is often run on another, and the
// enqueue side's claim here would never be taken over or ended. uploaderDeadline
// bounds that: a claim whose worker has not arrived in uploaderHandoffTTL is
// dropped by the next reservation. When the worker does arrive later it charges
// the row itself, which is the state a queued-but-not-started job should hold
// anyway.
type heldUploadWork struct {
	bytes int64
	// uploaderClaims counts enqueue-side claims still outstanding. They keep
	// the row alive only until a worker claims it or the row is ended.
	uploaderClaims atomic.Int32
	// workerClaims counts parse jobs running or queued for this row.
	workerClaims atomic.Int32
	// uploaderDeadline is when the outstanding uploader claims expire, in unix
	// nanoseconds. Zero once the row is worker-owned: the running job owns it.
	uploaderDeadline atomic.Int64
	id               string
}

// Claim kinds: the enqueue site holds the row's bytes; the worker joins the
// claim that is already there (or starts one for a row no upload created).
const (
	requestUploadWork = true
	requestWorkerWork = false
)

// ReserveUploadWork claims parse-job budget for a knowledge row about to be
// enqueued, keyed by knowledge id.
//
// The worker's claim takes over the enqueue side's claim rather than joining
// it: the enqueue site never releases on the success path, and counting both
// would leave the row charged once no job can ever end it. Every returned
// function must be called exactly once by the side it was made for.
//
// A row also costs one job slot, so a flood of tiny or unattributable jobs
// stops at UPLOAD_MAX_BACKGROUND_JOBS instead of only at the byte budget.
func ReserveUploadWork(knowledgeID string, bytes int64, byUploader bool) (func(), error) {
	if bytes < 0 {
		bytes = 0
	}
	if bytes == 0 {
		bytes = parseJobUnknownCharge
	}

	uploadWorkMu.Lock()
	reclaimStaleUploaderClaimsLocked()
	if actual, ok := uploadWorkIDs.Load(knowledgeID); ok {
		joined := actual.(*heldUploadWork)
		joined.claimLocked(byUploader)
		uploadWorkMu.Unlock()
		return joined.release(byUploader), nil
	}
	held := &heldUploadWork{bytes: bytes, id: knowledgeID}
	if byUploader {
		held.uploaderClaims.Store(1)
		held.uploaderDeadline.Store(time.Now().Add(uploaderHandoffTTL).UnixNano())
	} else {
		held.workerClaims.Store(1)
	}
	merged := uploadWorkBytes.Add(bytes)
	jobs := uploadWorkJobs.Add(1)
	maxBytes := utils.GetUploadMaxBackgroundBytes()
	maxJobs := int32(utils.GetUploadMaxBackgroundJobs())
	if merged > maxBytes || jobs > maxJobs {
		if uploadWorkBytes.Add(-bytes) < 0 {
			uploadWorkBytes.Store(0)
		}
		uploadWorkJobs.Add(-1)
		uploadWorkMu.Unlock()
		log.Printf("[upload] refused parse work for %s: held=%d + %d > %d bytes, jobs=%d > %d",
			knowledgeID, merged-bytes, bytes, maxBytes, jobs, maxJobs)
		// A parse job runs for minutes, so a refusal here is not worth an
		// immediate retry: the hint is the coarsest useful unit, not seconds.
		return nil, apperrors.NewTooManyRequestsError(
			"服务器正在处理的文档过多，请稍后重试").
			WithDetails("parse work budget is full; retry after running documents finish").
			WithRetryAfter(60)
	}
	// Stored only after the charge landed: under this lock no release can run
	// before the row is visible, so a row in the table is always accounted for.
	uploadWorkIDs.Store(knowledgeID, held)
	uploadWorkMu.Unlock()
	return held.release(byUploader), nil
}

// claimLocked records one claim on an existing reservation. The caller holds
// uploadWorkMu.
func (h *heldUploadWork) claimLocked(byUploader bool) {
	if byUploader {
		h.uploaderClaims.Add(1)
		h.uploaderDeadline.Store(time.Now().Add(uploaderHandoffTTL).UnixNano())
		return
	}
	// Ownership moves to the worker. The enqueue side's claims are dropped
	// without a refund: the bytes stay, now charged to the parse job.
	h.uploaderClaims.Store(0)
	h.uploaderDeadline.Store(0)
	h.workerClaims.Add(1)
}

// reclaimStaleUploaderClaimsLocked drops enqueue-side claims whose worker never
// arrived, which happens whenever the job is run by another replica. Only rows
// that are not worker-owned here are candidates; a running parse keeps its row.
// The caller holds uploadWorkMu.
func reclaimStaleUploaderClaimsLocked() {
	now := time.Now().UnixNano()
	uploadWorkIDs.Range(func(_, value any) bool {
		held := value.(*heldUploadWork)
		if held.workerClaims.Load() > 0 || held.uploaderClaims.Load() == 0 {
			return true
		}
		deadline := held.uploaderDeadline.Load()
		if deadline == 0 || now < deadline {
			return true
		}
		held.uploaderClaims.Store(0)
		held.uploaderDeadline.Store(0)
		held.freeIfIdleLocked()
		return true
	})
}

// release ends one claim and frees the row when nothing is left. A claim whose
// side already handed the row over is a no-op.
func (h *heldUploadWork) release(byUploader bool) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			uploadWorkMu.Lock()
			defer uploadWorkMu.Unlock()
			if byUploader {
				// Storage may already have handed the claim to a worker or
				// ended the row; either way there is nothing left to release.
				if h.uploaderClaims.Load() > 0 {
					h.uploaderClaims.Add(-1)
				}
			} else if h.workerClaims.Load() > 0 {
				h.workerClaims.Add(-1)
			}
			h.freeIfIdleLocked()
		})
	}
}

// freeIfIdleLocked removes the row and refunds its bytes once no claim is left.
// The caller holds uploadWorkMu, so no claim can be added between the check and
// the delete -- the window that let a just-joined worker lose its row.
func (h *heldUploadWork) freeIfIdleLocked() {
	if h.uploaderClaims.Load() > 0 || h.workerClaims.Load() > 0 {
		return
	}
	if !uploadWorkIDs.CompareAndDelete(h.id, h) {
		return
	}
	if uploadWorkBytes.Add(-h.bytes) < 0 {
		uploadWorkBytes.Store(0)
	}
	if uploadWorkJobs.Add(-1) < 0 {
		uploadWorkJobs.Store(0)
	}
}

// uploadWorkHeld reports the bytes currently reserved by parse jobs.
func uploadWorkHeld() int64 { return uploadWorkBytes.Load() }

// uploadWorkJobsHeld reports how many parse jobs currently reserve budget.
func uploadWorkJobsHeld() int32 { return uploadWorkJobs.Load() }

// resetUploadWork clears the reservation table. Tests use it.
func resetUploadWork() {
	uploadWorkMu.Lock()
	defer uploadWorkMu.Unlock()
	uploadWorkBytes.Store(0)
	uploadWorkJobs.Store(0)
	uploadWorkIDs.Range(func(k, _ any) bool {
		uploadWorkIDs.Delete(k)
		return true
	})
}

// ParseJobWorkingBytes is the declared size a parse job holds budget for: the
// upload's declared body size, else the on-disk size of the file it will parse,
// else the bytes of the text passages carried in the payload. Zero when none is
// known: ReserveUploadWork then charges the unattributable floor.
//
// Paths and texts are separate parameters because the two cannot be told apart
// by shape. A local path that cannot be stat'ed (the row's source moved, or a
// retry raced the cleanup) says nothing about the bytes behind it -- charging
// the string length understated such a job by orders of magnitude -- so it is
// reported as unknown and the floor applies. A provider URL (local://,
// minio://, ...) is neither a readable path nor text and contributes nothing.
// Text passages are summed, including ones that happen to contain "://": a
// passage is content the worker must hold in memory however it is spelled.
func ParseJobWorkingBytes(declared int64, paths []string, texts []string) int64 {
	if declared > 0 {
		return declared
	}
	var held int64
	unknown := false
	for _, path := range paths {
		if path == "" {
			continue
		}
		if info, err := os.Stat(path); err == nil {
			held += info.Size()
			continue
		}
		if strings.Contains(path, "://") {
			continue
		}
		unknown = true
	}
	for _, text := range texts {
		if text == "" {
			continue
		}
		held += int64(len(text))
	}
	if unknown && held < parseJobUnknownCharge {
		held = parseJobUnknownCharge
	}
	return held
}

// declaredUploadBytesContextKey carries the size the HTTP handler admitted this
// upload for, so the enqueue site can charge the parse job the same figure the
// slot was sized on. Other upload paths leave it unset.
type declaredUploadBytesContextKey struct{}

// WithDeclaredUploadBytes records the admitted body size for an upload.
func WithDeclaredUploadBytes(ctx context.Context, n int64) context.Context {
	if n <= 0 {
		return ctx
	}
	return context.WithValue(ctx, declaredUploadBytesContextKey{}, n)
}

func declaredUploadBytes(ctx context.Context) int64 {
	n, _ := ctx.Value(declaredUploadBytesContextKey{}).(int64)
	return n
}

// declaredUploadFileBytes is the figure a file-backed row should budget: the
// size its upload was admitted for, or, when it was created by a path with no
// HTTP admission (IM, datasource, worker), the size recorded on the row. Zero
// when neither is known, so the unattributable floor applies.
func declaredUploadFileBytes(ctx context.Context, rowSize int64) int64 {
	if n := declaredUploadBytes(ctx); n > 0 {
		return n
	}
	return rowSize
}

// parseWorkKeyForTask names a parse job's reservation. The knowledge id is the
// stable identity: asynq ids change per enqueue and per retry, while the row is
// what the work belongs to.
func parseWorkKeyForTask(t *asynq.Task) string {
	var payload types.DocumentProcessPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil || payload.KnowledgeID == "" {
		// Key on the payload, not the type: every unattributable job sharing
		// "asynq:<type>" joined one reservation, so one job's release freed the
		// budget of all the others. Retries carry the same payload bytes, which
		// is what lets a retry rejoin its own row.
		sum := sha256.Sum256(t.Payload())
		return "asynq-task:" + hex.EncodeToString(sum[:8])
	}
	return payload.KnowledgeID
}

// EndUploadWork marks a knowledge row's parse work as finished. It is called
// from paths that do not run ProcessDocument themselves -- the cancellation and
// deletion guards, the enqueue-failure rollback -- and is safe when the row
// never claimed budget.
//
// A row whose parse is still running keeps its budget: a cancel can land after
// the worker dequeued the task and before it claimed the row, and ending the
// budget there would let the parse run while the next uploads count against
// bytes it no longer holds. The running job's own release frees it.
func EndUploadWork(knowledgeID string) {
	uploadWorkMu.Lock()
	defer uploadWorkMu.Unlock()
	v, ok := uploadWorkIDs.Load(knowledgeID)
	if !ok {
		return
	}
	held := v.(*heldUploadWork)
	held.uploaderClaims.Store(0)
	held.uploaderDeadline.Store(0)
	held.freeIfIdleLocked()
}
