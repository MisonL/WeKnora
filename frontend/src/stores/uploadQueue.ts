import type { KnowledgeProcessOverrides } from '@/types/knowledgeProcess'
import {
  classifyUploadResult,
  itemPhase,
  needsParsePolling,
  pickNextToStart,
  type UploadItem,
} from './uploadTasksState'

/**
 * The upload queue's behaviour — bounded concurrent transfers, cancel, retry
 * and polling the created rows until parsing settles — with the network and
 * timers injected, so the store only wires it to the API and Vue.
 *
 * `items` and `batches` are mutated in place (never reassigned) so a reactive
 * array handed in by the store stays the one the panel renders.
 */

export interface UploadBatch {
  id: string
  kbId: string
  kbName: string
  /** Destination folder, '' for the knowledge base root. */
  targetFolder: string
  tagIds?: string[]
  processConfig?: KnowledgeProcessOverrides
}

export interface UploadPayload {
  file: File
  /** Path-qualified name the backend splits into folder + file name. */
  fileName?: string
}

export interface KnowledgeStatusRow {
  id: string
  parse_status?: string
  error_message?: string
}

export interface TransferEnd {
  /** The file reached the server, so the knowledge base has a new row. */
  uploaded: boolean
  /** Nothing is queued or uploading for this knowledge base any more. */
  settled: boolean
}

export interface Timers {
  setTimer: (fn: () => void, ms: number) => unknown
  clearTimer: (handle: unknown) => void
  /** Injectable clock, for tests that need time to pass. Defaults to Date.now. */
  now?: () => number
}

export const defaultTimers: Timers = {
  setTimer: (fn, ms) => setTimeout(fn, ms),
  clearTimer: handle => clearTimeout(handle as ReturnType<typeof setTimeout>),
}

export interface UploadQueueDeps {
  concurrency: number
  pollIntervalMs: number
  /** Most knowledge ids asked about in one status request. */
  pollChunkSize: number
  /**
   * Most status requests one poll keeps in flight. Each one holds a database
   * connection on the server, so thousands of files still parsing must not
   * turn into a burst of hundreds of parallel requests.
   */
  pollConcurrency: number
  /** Resolves or rejects with the API's response body; the queue classifies either. */
  upload: (
    batch: UploadBatch,
    payload: UploadPayload,
    onProgress: (ratio: number) => void,
    signal: AbortSignal,
  ) => Promise<unknown>
  /** Current rows for the ids, or null when the request failed. */
  queryStatus: (kbId: string, knowledgeIds: string[]) => Promise<KnowledgeStatusRow[] | null>
  /** Called whenever a transfer ends, including one that outlived clear(). */
  onTransferEnd: (kbId: string, end: TransferEnd) => void
  timers?: Timers
}

/**
 * Consecutive successful polls a row must be missing from before it counts as
 * deleted, so one odd answer can't strand a live document as "Deleted".
 */
const MISSING_POLLS_BEFORE_DELETED = 3

/** Attempts a 429-refused upload gets before it is shown as failed. */
const BUSY_RETRIES = 5
/** Gap before a 429-refused upload is offered to the queue again. */
const BUSY_RETRY_DELAY_MS = 2000
/** Upper bound on a single 429 backoff, whatever the header said. */
const BUSY_MAX_RETRY_DELAY_MS = 60 * 1000

let idSeq = 0
const nextId = (prefix: string) => `${prefix}-${Date.now().toString(36)}-${(idSeq++).toString(36)}`

export function createUploadQueue(items: UploadItem[], batches: UploadBatch[], deps: UploadQueueDeps) {
  const { setTimer, clearTimer } = deps.timers ?? defaultTimers
  const now = deps.timers?.now ?? (() => Date.now())

  // File payloads and abort handles stay out of the (possibly reactive) items:
  // proxying a File buys nothing and the panel never renders them.
  const payloads = new Map<string, UploadPayload>()
  const controllers = new Map<string, AbortController>()
  const missingPolls = new Map<string, number>()
  // Bumped by clear(), so work started before it cannot touch what comes after.
  let generation = 0
  let pollTimer: unknown = null

  const batchOf = (item: UploadItem) => batches.find(batch => batch.id === item.batchId)

  // A 429 hands the file back to the queue instead of failing it. The server
  // admits a limited number of uploads at once; a caller that treats the
  // refusal as a failure shows an error for a file that would have gone
  // through moments later. Entries mark files that are waiting out a backoff;
  // attempts keeps the total bounded so a server that stays full eventually
  // shows the failure instead of holding the file forever.
  const busyRetries = new Set<string>()
  const busyAttempts = new Map<string, number>()
  const findItem = (id: string) => items.find(item => item.id === id)

  const isBusy = (kbId: string) => items.some(item =>
    (item.transfer === 'queued' || item.transfer === 'uploading') && batchOf(item)?.kbId === kbId)

  // ---- transfer -----------------------------------------------------------

  // Identical files always have the same size. Keeping same-size files for one
  // knowledge base out of flight together lets the server's content-hash dedup,
  // which only sees rows already written, catch the second copy.
  const dedupKey = (item: UploadItem) => `${batchOf(item)?.kbId}:${item.size}`

  // Mirrors the server's UPLOAD_BIG_BYTES default: a file this size or larger
  // is admitted only on its own, so the queue runs one at a time. A deployment
  // that raises the server knob simply gets the 429 path it already handles.
  const uploadBigBytes = 32 << 20

  // One timer for the whole queue: the attempt that already runs at the end of
  // the backoff pumps every waiting item, so N refused files do not each arm
  // their own timer. Kept out of the poll timer, which only runs while rows
  // are parsing.
  let busyRetryTimer: unknown = null
  // When the armed timer is due. A later refusal is counted from the moment it
  // arrives, not from when the timer was armed: a hint that lands near the end
  // of the window still has to wait its full length, or the file is retried
  // before the server said to, spends one of its attempts on a queue that is
  // still full, and can fail for a reason the hint already predicted.
  let busyRetryDueAt = 0
  // A server that states how long to wait gets that wait; the default is for
  // the 429s that carry no hint. Clamped so a misconfigured or hostile header
  // cannot park a file for an hour.
  const clampBusyDelay = (ms: number) => Math.min(Math.max(ms, BUSY_RETRY_DELAY_MS), BUSY_MAX_RETRY_DELAY_MS)
  const scheduleBusyRetry = (delayMs = BUSY_RETRY_DELAY_MS) => {
    const dueAt = now() + clampBusyDelay(delayMs)
    if (busyRetryTimer !== null) {
      if (dueAt <= busyRetryDueAt) return
      clearTimer(busyRetryTimer)
    }
    busyRetryDueAt = dueAt
    busyRetryTimer = setTimer(() => {
      busyRetryTimer = null
      busyRetryDueAt = 0
      // The backoff has passed for every refused file, so they are eligible
      // again and the attempt itself clears the mark.
      busyRetries.clear()
      pump()
    }, Math.max(0, dueAt - now()))
  }

  // Files waiting out a 429 backoff are never eligible: starting one early is
  // exactly what the refusal asked not to happen.
  //
  // Nothing starts while the window is open, not only the refused files: a file
  // added or retried mid-window would otherwise send at once and collect the
  // same refusal. The timer clears the marks before it pumps, so a backoff that
  // has passed still starts.
  const pump = () => {
    if (busyRetryTimer !== null && now() < busyRetryDueAt) return
    const eligible = items.filter(item => !busyRetries.has(item.id))
    for (const item of pickNextToStart(eligible, deps.concurrency, dedupKey, uploadBigBytes)) void transfer(item)
  }

  const transfer = async (item: UploadItem) => {
    const batch = batchOf(item)
    const payload = payloads.get(item.id)
    if (!batch || !payload) {
      item.transfer = 'failed'
      return
    }
    const startedIn = generation
    const controller = new AbortController()
    controllers.set(item.id, controller)
    item.transfer = 'uploading'
    item.loaded = 0
    item.error = undefined

    let result: unknown
    try {
      result = await deps.upload(batch, payload, ratio => {
        if (item.transfer === 'uploading') item.loaded = Math.min(item.size, Math.round(ratio * item.size))
      }, controller.signal)
    } catch (error) {
      result = error
    }
    // A retry after a cancel starts a new attempt under the same id; only the
    // latest attempt may write the item.
    const latestAttempt = controllers.get(item.id) === controller
    if (latestAttempt) controllers.delete(item.id)
    const aborted = controller.signal.aborted

    // Outlived clear(): the row is gone from the panel, but a file that still
    // made it (clear() lets fully sent files finish) is new in the list.
    if (startedIn !== generation) {
      if (!aborted && classifyUploadResult(result).kind === 'uploaded') {
        deps.onTransferEnd(batch.kbId, { uploaded: true, settled: !isBusy(batch.kbId) })
      }
      return
    }
    if (!latestAttempt) return

    // A stored-but-unqueued upload still created a row, so the document list
    // needs the same refresh an ordinary upload triggers.
    let rowCreated = false
    if (aborted || item.transfer !== 'uploading') {
      // Cancelled mid-flight; the abort rejection carries nothing worth showing.
      if (item.transfer === 'uploading') item.transfer = 'cancelled'
    } else {
      const outcome = classifyUploadResult(result)
      if (outcome.kind === 'uploaded') {
        item.transfer = 'uploaded'
        item.loaded = item.size
        item.knowledgeId = outcome.knowledgeId
        item.parseStatus = outcome.parseStatus || 'pending'
        payloads.delete(item.id)
        schedulePoll()
      } else if (outcome.kind === 'duplicate') {
        item.transfer = 'duplicate'
        item.knowledgeId = outcome.knowledgeId
        payloads.delete(item.id)
      } else if (outcome.kind === 'busy' && (busyAttempts.get(item.id) ?? 0) < BUSY_RETRIES) {
        busyAttempts.set(item.id, (busyAttempts.get(item.id) ?? 0) + 1)
        busyRetries.add(item.id)
        item.transfer = 'queued'
        item.loaded = 0
        scheduleBusyRetry(outcome.retryAfterMs)
      } else if (outcome.kind === 'stored') {
        // The bytes are safely stored; only the parse job is missing. Show it
        // as a failed transfer so it is not silently counted as uploaded, but
        // keep the row id: the stored row is already marked failed, and failed
        // rows do not block the duplicate check -- re-uploading would create a
        // second row for the same bytes. The document list is where the
        // reparse happens.
        item.transfer = 'failed'
        item.stored = true
        item.knowledgeId = outcome.knowledgeId
        item.error = outcome.message
        payloads.delete(item.id)
        rowCreated = true
      } else {
        item.transfer = 'failed'
        item.error = outcome.message
      }
    }
    deps.onTransferEnd(batch.kbId, {
      uploaded: item.transfer === 'uploaded' || rowCreated,
      settled: !isBusy(batch.kbId),
    })
    // Anything else that is waiting may start now. A file refused with 429 is
    // already 'queued' but must wait for the backoff: starting it here would
    // hammer the full server, which is the opposite of what a refusal asks for.
    pump()
  }

  const add = (batchInput: Omit<UploadBatch, 'id'>, uploads: UploadPayload[]) => {
    const batch: UploadBatch = { ...batchInput, id: nextId('batch') }
    batches.push(batch)
    for (const upload of uploads) {
      const id = nextId('upload')
      payloads.set(id, upload)
      items.push({
        id,
        batchId: batch.id,
        name: upload.file.name,
        relativePath: upload.file.webkitRelativePath || '',
        size: upload.file.size,
        loaded: 0,
        transfer: 'queued',
      })
    }
    pump()
    return batch
  }

  const cancel = (id: string) => {
    const item = findItem(id)
    if (!item || (item.transfer !== 'queued' && item.transfer !== 'uploading')) return
    // Once the whole body is sent the server is already storing the file;
    // aborting now would hide a row that still gets created.
    if (itemPhase(item) === 'saving') return
    item.transfer = 'cancelled'
    item.loaded = 0
    const controller = controllers.get(id)
    if (controller) {
      // Its transfer reports the end once the abort lands.
      controller.abort()
      return
    }
    const kbId = batchOf(item)?.kbId
    if (kbId && !isBusy(kbId)) deps.onTransferEnd(kbId, { uploaded: false, settled: true })
  }

  const cancelAll = () => {
    for (const item of items) cancel(item.id)
  }

  const retry = (id: string) => {
    const item = findItem(id)
    if (!item || (item.transfer !== 'failed' && item.transfer !== 'cancelled') || !payloads.has(id)) return
    item.transfer = 'queued'
    item.loaded = 0
    item.error = undefined
    busyAttempts.delete(id)
    busyRetries.delete(id)
    pump()
  }

  const retryFailed = () => {
    for (const item of items) retry(item.id)
  }

  /** Drop batches whose every file is searchable; anything with an issue stays. */
  const pruneFinished = () => {
    for (const batch of [...batches]) {
      const own = items.filter(item => item.batchId === batch.id)
      if (!own.every(item => itemPhase(item) === 'ready')) continue
      for (const item of own) {
        items.splice(items.indexOf(item), 1)
        missingPolls.delete(item.id)
      }
      batches.splice(batches.indexOf(batch), 1)
    }
  }

  // ---- parse polling ------------------------------------------------------

  function schedulePoll() {
    if (pollTimer !== null || !items.some(needsParsePolling)) return
    pollTimer = setTimer(() => {
      pollTimer = null
      void pollNow()
    }, deps.pollIntervalMs)
  }

  const pollNow = async () => {
    const startedIn = generation
    const byKb = new Map<string, UploadItem[]>()
    for (const item of items) {
      const kbId = needsParsePolling(item) ? batchOf(item)?.kbId : undefined
      if (!kbId) continue
      if (!byKb.has(kbId)) byKb.set(kbId, [])
      byKb.get(kbId)!.push(item)
    }

    const chunks: Array<{ kbId: string; chunk: UploadItem[] }> = []
    for (const [kbId, group] of byKb) {
      for (let i = 0; i < group.length; i += deps.pollChunkSize) {
        chunks.push({ kbId, chunk: group.slice(i, i + deps.pollChunkSize) })
      }
    }

    const queryChunk = async ({ kbId, chunk }: { kbId: string; chunk: UploadItem[] }) => {
      const rows = await deps.queryStatus(kbId, chunk.map(item => item.knowledgeId!)).catch(() => null)
      // A failed request just waits for the next tick.
      if (!rows || startedIn !== generation) return
      const byId = new Map(rows.map(row => [row.id, row]))
      for (const item of chunk) {
        const row = byId.get(item.knowledgeId!)
        if (!row) {
          const misses = (missingPolls.get(item.id) ?? 0) + 1
          missingPolls.set(item.id, misses)
          if (misses >= MISSING_POLLS_BEFORE_DELETED) item.parseStatus = 'deleted'
          continue
        }
        missingPolls.delete(item.id)
        item.parseStatus = row.parse_status
        item.parseError = row.error_message || undefined
      }
    }

    let next = 0
    const worker = async () => {
      while (next < chunks.length && startedIn === generation) await queryChunk(chunks[next++])
    }
    await Promise.all(Array.from({ length: Math.min(Math.max(1, deps.pollConcurrency), chunks.length) }, worker))
    if (startedIn === generation) schedulePoll()
  }

  // ---- lifecycle ----------------------------------------------------------

  /**
   * Forget everything. Transfers still sending are aborted; ones whose body is
   * fully sent are left to finish, since the server stores them regardless.
   */
  const clear = () => {
    generation++
    for (const item of items) {
      if (itemPhase(item) !== 'saving') controllers.get(item.id)?.abort()
    }
    const busyKbs = new Set(items.filter(item => item.transfer === 'queued' || item.transfer === 'uploading')
      .map(item => batchOf(item)?.kbId)
      .filter((kbId): kbId is string => !!kbId))
    controllers.clear()
    payloads.clear()
    missingPolls.clear()
    items.splice(0)
    batches.splice(0)
    if (pollTimer !== null) {
      clearTimer(pollTimer)
      pollTimer = null
    }
    // A pending busy timer would fire against the next queue and clear marks
    // that no longer exist; the per-file backoff state would outlive its files.
    if (busyRetryTimer !== null) {
      clearTimer(busyRetryTimer)
      busyRetryTimer = null
    }
    busyRetryDueAt = 0
    busyRetries.clear()
    busyAttempts.clear()
    // Files that landed before the clear may still be waiting for a refresh.
    for (const kbId of busyKbs) deps.onTransferEnd(kbId, { uploaded: false, settled: true })
  }

  return { add, cancel, cancelAll, retry, retryFailed, pruneFinished, pollNow, clear }
}

/**
 * Turns transfer ends into `knowledgeFileUploaded` refreshes: while a knowledge
 * base still has work queued, at most one mid-batch (`settled: false`) refresh
 * per `intervalMs`; once it settles, a single final (`settled: true`) one.
 * Ends that settle within `coalesceMs` of each other (cancel-all aborting
 * several requests) produce one final refresh.
 */
export function createListRefreshThrottle(
  emit: (kbId: string, settled: boolean) => void,
  intervalMs: number,
  coalesceMs: number,
  timers: Timers = defaultTimers,
) {
  const throttles = new Map<string, unknown>()
  const pending = new Set<string>()
  const finals = new Map<string, unknown>()

  return (kbId: string, end: TransferEnd) => {
    if (end.settled) {
      const throttle = throttles.get(kbId)
      if (throttle !== undefined) timers.clearTimer(throttle)
      throttles.delete(kbId)
      pending.delete(kbId)
      if (finals.has(kbId)) return
      finals.set(kbId, timers.setTimer(() => {
        finals.delete(kbId)
        emit(kbId, true)
      }, coalesceMs))
      return
    }
    if (!end.uploaded) return
    if (throttles.has(kbId)) {
      pending.add(kbId)
      return
    }
    emit(kbId, false)
    const tick = () => {
      if (pending.delete(kbId)) {
        emit(kbId, false)
        throttles.set(kbId, timers.setTimer(tick, intervalMs))
      } else {
        throttles.delete(kbId)
      }
    }
    throttles.set(kbId, timers.setTimer(tick, intervalMs))
  }
}
