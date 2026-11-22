import assert from 'node:assert/strict'
import test from 'node:test'
import {
  createListRefreshThrottle,
  createUploadQueue,
  type KnowledgeStatusRow,
  type Timers,
  type TransferEnd,
  type UploadBatch,
  type UploadPayload,
  type UploadQueueDeps,
} from './uploadQueue.ts'
import type { UploadItem } from './uploadTasksState.ts'

type Call = {
  batch: UploadBatch
  payload: UploadPayload
  signal: AbortSignal
  progress: (ratio: number) => void
  resolve: (value: unknown) => void
  reject: (reason: unknown) => void
}

const file = (name: string, size = 100) => ({ name, size, webkitRelativePath: '' }) as unknown as File

const flush = () => new Promise(resolve => setImmediate(resolve))

/** Timers that only fire when the test says so. */
function manualTimers() {
  const pending = new Set<{ fn: () => void; ms: number }>()
  const timers: Timers = {
    setTimer: (fn, ms) => {
      const handle = { fn, ms }
      pending.add(handle)
      return handle
    },
    clearTimer: handle => {
      pending.delete(handle as { fn: () => void; ms: number })
    },
  }
  const runAll = () => {
    const due = [...pending]
    pending.clear()
    for (const handle of due) handle.fn()
  }
  return { timers, runAll, pending }
}

function setup(overrides: Partial<UploadQueueDeps> = {}) {
  const items: UploadItem[] = []
  const batches: UploadBatch[] = []
  const calls: Call[] = []
  const ends: Array<[string, TransferEnd]> = []
  const statusRows = new Map<string, KnowledgeStatusRow[] | null>()
  const clock = manualTimers()

  const queue = createUploadQueue(items, batches, {
    concurrency: 2,
    pollIntervalMs: 1000,
    pollChunkSize: 2,
    pollConcurrency: 4,
    upload: (batch, payload, progress, signal) => new Promise((resolve, reject) => {
      calls.push({ batch, payload, signal, progress, resolve, reject })
      signal.addEventListener('abort', () => reject({ message: 'Network error' }))
    }),
    queryStatus: async (kbId, ids) => {
      const rows = statusRows.get(kbId)
      return rows === null ? null : (rows ?? []).filter(row => ids.includes(row.id))
    },
    onTransferEnd: (kbId, end) => ends.push([kbId, end]),
    timers: clock.timers,
    ...overrides,
  })
  const batch = (kbId = 'kb1') => ({ kbId, kbName: 'KB', targetFolder: '' })
  const byName = (name: string) => items.find(item => item.name === name)!
  return { items, batches, calls, ends, statusRows, clock, queue, batch, byName }
}

test('runs at most `concurrency` transfers and starts the next as one settles', async () => {
  const { calls, ends, queue, batch, byName } = setup()
  queue.add(batch(), [{ file: file('a', 100) }, { file: file('b', 200) }, { file: file('c', 300) }])
  assert.deepEqual(calls.map(call => call.payload.file.name), ['a', 'b'])
  assert.equal(byName('c').transfer, 'queued')

  calls[0].progress(0.4)
  assert.equal(byName('a').loaded, 40)

  calls[0].resolve({ success: true, data: { id: 'k-a', parse_status: 'pending' } })
  await flush()
  assert.equal(byName('a').transfer, 'uploaded')
  assert.equal(byName('a').knowledgeId, 'k-a')
  assert.deepEqual(calls.map(call => call.payload.file.name), ['a', 'b', 'c'])
  assert.deepEqual(ends, [['kb1', { uploaded: true, settled: false }]])
})

test('never runs two same-size files for one knowledge base at once', async () => {
  const { calls, queue, batch } = setup({ concurrency: 3 })
  queue.add(batch(), [{ file: file('copy-1', 500) }, { file: file('copy-2', 500) }, { file: file('other', 7) }])
  queue.add(batch('kb2'), [{ file: file('elsewhere', 500) }])
  // copy-2 waits for copy-1, so the server's hash dedup sees copy-1's row.
  assert.deepEqual(calls.map(call => call.payload.file.name), ['copy-1', 'other', 'elsewhere'])

  calls[0].resolve({ success: true, data: { id: 'k1' } })
  await flush()
  assert.equal(calls[3].payload.file.name, 'copy-2')
})

test('classifies duplicates and failures, and retries a failed file', async () => {
  const { calls, ends, queue, batch, byName } = setup()
  queue.add(batch(), [{ file: file('dup', 1) }, { file: file('bad', 2) }])

  calls[0].reject({ status: 409, code: 'duplicate_file', data: { id: 'k-old' } })
  calls[1].reject({ status: 400, message: 'too large' })
  await flush()
  assert.equal(byName('dup').transfer, 'duplicate')
  assert.equal(byName('dup').knowledgeId, 'k-old')
  assert.equal(byName('bad').transfer, 'failed')
  assert.equal(byName('bad').error, 'too large')
  assert.deepEqual(ends.at(-1), ['kb1', { uploaded: false, settled: true }])

  queue.retryFailed()
  assert.equal(calls.length, 3)
  assert.equal(calls[2].payload.file.name, 'bad')
  // Duplicates are settled, not retryable.
  assert.equal(byName('dup').transfer, 'duplicate')
  calls[2].resolve({ success: true, data: { id: 'k-bad' } })
  await flush()
  assert.equal(byName('bad').transfer, 'uploaded')
  assert.equal(byName('bad').parseStatus, 'pending')
  assert.deepEqual(ends.at(-1), ['kb1', { uploaded: true, settled: true }])
})

test('cancelling aborts the request without reporting it as a failure', async () => {
  const { calls, ends, queue, batch, byName } = setup()
  queue.add(batch(), [{ file: file('a', 1) }, { file: file('b', 2) }, { file: file('c', 3) }])

  queue.cancel(byName('c').id)
  assert.equal(byName('c').transfer, 'cancelled')

  queue.cancel(byName('a').id)
  assert.equal(calls[0].signal.aborted, true)
  await flush()
  assert.equal(byName('a').transfer, 'cancelled')
  assert.equal(byName('a').error, undefined)
  // The freed slot doesn't resurrect the cancelled queued file.
  assert.equal(calls.length, 2)
  assert.deepEqual(ends, [['kb1', { uploaded: false, settled: false }]])

  // Fully sent: the server is already storing it, so it can't be cancelled.
  calls[1].progress(1)
  queue.cancel(byName('b').id)
  assert.equal(calls[1].signal.aborted, false)
  assert.equal(byName('b').transfer, 'uploading')
})

test('a late abort from a cancelled attempt cannot overwrite its retry', async () => {
  let rejectFirst!: (reason: unknown) => void
  const { calls, queue, batch, byName } = setup({
    // The first attempt ignores its signal, as a slow transport might.
    upload: (b, payload, progress, signal) => new Promise((resolve, reject) => {
      calls.push({ batch: b, payload, signal, progress, resolve, reject })
      if (calls.length === 1) rejectFirst = reject
    }),
  })
  queue.add(batch(), [{ file: file('a') }])
  queue.cancel(byName('a').id)
  queue.retry(byName('a').id)
  assert.equal(calls.length, 2)

  rejectFirst({ message: 'Network error' })
  await flush()
  assert.equal(byName('a').transfer, 'uploading')

  calls[1].resolve({ success: true, data: { id: 'k-a' } })
  await flush()
  assert.equal(byName('a').transfer, 'uploaded')
})

test('polls created rows until parsing settles', async () => {
  const { calls, clock, statusRows, queue, batch, byName } = setup()
  queue.add(batch(), [{ file: file('a', 1) }, { file: file('b', 2) }, { file: file('c', 3) }])
  calls[0].resolve({ success: true, data: { id: 'k-a', parse_status: 'pending' } })
  calls[1].resolve({ success: true, data: { id: 'k-b', parse_status: 'pending' } })
  await flush()
  calls[2].resolve({ success: true, data: { id: 'k-c', parse_status: 'pending' } })
  await flush()
  // One timer no matter how many uploads landed.
  assert.equal(clock.pending.size, 1)

  statusRows.set('kb1', null)
  await queue.pollNow()
  assert.equal(byName('a').parseStatus, 'pending', 'a failed request changes nothing')

  statusRows.set('kb1', [
    { id: 'k-a', parse_status: 'completed' },
    { id: 'k-b', parse_status: 'failed', error_message: 'broken pdf' },
  ])
  await queue.pollNow()
  assert.equal(byName('a').parseStatus, 'completed')
  assert.equal(byName('b').parseStatus, 'failed')
  assert.equal(byName('b').parseError, 'broken pdf')
  // Missing from one answer is not proof of deletion.
  assert.equal(byName('c').parseStatus, 'pending')
  await queue.pollNow()
  assert.equal(byName('c').parseStatus, 'pending')
  await queue.pollNow()
  assert.equal(byName('c').parseStatus, 'deleted')
})

test('a poll keeps at most `pollConcurrency` status requests in flight', async () => {
  // Thousands of files still parsing used to fire every chunk at once, one
  // database connection each on the server.
  const pending: Array<{ ids: string[]; resolve: (rows: KnowledgeStatusRow[]) => void }> = []
  const { items, batches, queue, byName } = setup({
    pollConcurrency: 2,
    queryStatus: (_kbId, ids) => new Promise(resolve => pending.push({ ids, resolve })),
  })
  batches.push({ id: 'b1', kbId: 'kb1', kbName: 'KB', targetFolder: '' })
  for (const name of ['a', 'b', 'c', 'd', 'e', 'f', 'g']) {
    items.push({
      id: `u-${name}`, batchId: 'b1', name, relativePath: '', size: 1, loaded: 1,
      transfer: 'uploaded', knowledgeId: `k-${name}`, parseStatus: 'pending',
    })
  }

  const poll = queue.pollNow()
  let done = false
  void poll.then(() => { done = true })
  const answered: string[][] = []
  let maxInFlight = 0
  while (!done) {
    await flush()
    maxInFlight = Math.max(maxInFlight, pending.length)
    const next = pending.shift()
    if (!next) continue
    answered.push(next.ids)
    next.resolve(next.ids.map(id => ({ id, parse_status: 'completed' })))
  }

  assert.equal(maxInFlight, 2)
  // pollChunkSize 2: seven ids in four requests, each id asked about once.
  assert.deepEqual(answered.flat().sort(), ['k-a', 'k-b', 'k-c', 'k-d', 'k-e', 'k-f', 'k-g'])
  assert.equal(answered.length, 4)
  assert.equal(byName('g').parseStatus, 'completed')
})

test('a row that reappears resets its missing count', async () => {
  const { calls, statusRows, queue, batch, byName } = setup()
  queue.add(batch(), [{ file: file('a') }])
  calls[0].resolve({ success: true, data: { id: 'k-a', parse_status: 'pending' } })
  await flush()

  statusRows.set('kb1', [])
  await queue.pollNow()
  await queue.pollNow()
  statusRows.set('kb1', [{ id: 'k-a', parse_status: 'processing' }])
  await queue.pollNow()
  statusRows.set('kb1', [])
  await queue.pollNow()
  await queue.pollNow()
  assert.equal(byName('a').parseStatus, 'processing')
})

test('clear() lets fully sent files finish and reports them, but aborts the rest', async () => {
  const { items, batches, calls, ends, queue, batch } = setup()
  queue.add(batch(), [{ file: file('sent', 1) }, { file: file('sending', 2) }])
  calls[0].progress(1)
  calls[1].progress(0.3)

  queue.clear()
  assert.equal(calls[0].signal.aborted, false)
  assert.equal(calls[1].signal.aborted, true)
  assert.equal(items.length, 0)
  assert.equal(batches.length, 0)
  assert.deepEqual(ends, [['kb1', { uploaded: false, settled: true }]])

  queue.add(batch(), [{ file: file('next', 3) }])
  calls[0].resolve({ success: true, data: { id: 'late' } })
  await flush()
  assert.deepEqual(items.map(item => item.name), ['next'])
  // The late file is new in the list; the new batch still has work queued.
  assert.deepEqual(ends.at(-1), ['kb1', { uploaded: true, settled: false }])
})

test('pruneFinished drops clean batches and keeps ones with issues', async () => {
  const { items, batches, calls, statusRows, queue, batch } = setup()
  queue.add(batch(), [{ file: file('ok', 1) }])
  queue.add(batch(), [{ file: file('broken', 2) }])
  calls[0].resolve({ success: true, data: { id: 'k-ok' } })
  calls[1].reject({ status: 500, message: 'boom' })
  await flush()
  statusRows.set('kb1', [{ id: 'k-ok', parse_status: 'completed' }])
  await queue.pollNow()

  queue.pruneFinished()
  assert.equal(batches.length, 1)
  assert.deepEqual(items.map(item => item.name), ['broken'])
})

test('list refresh throttle: mid-batch at most once per interval, one final refresh', () => {
  const emitted: Array<[string, boolean]> = []
  const clock = manualTimers()
  const notify = createListRefreshThrottle((kbId, settled) => emitted.push([kbId, settled]), 2000, 150, clock.timers)

  notify('kb1', { uploaded: true, settled: false })
  notify('kb1', { uploaded: true, settled: false })
  notify('kb1', { uploaded: false, settled: false })
  assert.deepEqual(emitted, [['kb1', false]])
  clock.runAll()
  assert.deepEqual(emitted, [['kb1', false], ['kb1', false]])

  // Several ends settling together (cancel all) make one final refresh.
  notify('kb1', { uploaded: false, settled: true })
  notify('kb1', { uploaded: false, settled: true })
  clock.runAll()
  assert.deepEqual(emitted.slice(2), [['kb1', true]])
  clock.runAll()
  assert.equal(emitted.length, 3)
})

test('a 429 hands the file back to the queue instead of failing it', async () => {
  const { calls, clock, queue, batch, byName } = setup({ concurrency: 1 })
  queue.add(batch(), [{ file: file('a', 100) }])
  calls[0].reject({ status: 429, success: false, error: { code: 1006, message: 'busy' } })
  await flush()

  assert.equal(byName('a').transfer, 'queued')
  assert.equal(calls.length, 1, 'the retry waits for the backoff, not for the next pump')
  clock.runAll()
  assert.equal(calls.length, 2, 'the file is offered again after the backoff')

  calls[1].resolve({ success: true, data: { id: 'k-a' } })
  await flush()
  assert.equal(byName('a').transfer, 'uploaded')
})

test('a server that stays busy eventually shows the failure, and retry starts over', async () => {
  const { calls, clock, queue, batch, byName } = setup({ concurrency: 1 })
  queue.add(batch(), [{ file: file('a', 100) }])
  for (let i = 0; i < 6; i++) {
    calls[i].reject({ status: 429, success: false, error: { code: 1006, message: 'busy' } })
    await flush()
    clock.runAll()
  }
  assert.equal(byName('a').transfer, 'failed')
  assert.equal(calls.length, 6)

  queue.retry(byName('a').id)
  assert.equal(calls.length, 7, 'a manual retry is not blocked by the spent attempts')
})

test('a file refused at the same moment another finishes still waits out its backoff', async () => {
  const { calls, clock, queue, batch, byName } = setup({ concurrency: 2 })
  queue.add(batch(), [{ file: file('a', 1) }, { file: file('b', 2) }])
  calls[0].reject({ status: 429, success: false, error: { code: 1006, message: 'busy' } })
  calls[1].resolve({ success: true, data: { id: 'k-b' } })
  await flush()

  assert.equal(byName('a').transfer, 'queued')
  assert.equal(calls.length, 2, 'the free slot must not restart the refused file before its backoff')
  clock.runAll()
  assert.equal(calls.length, 3)
  assert.equal(calls[2].payload.file.name, 'a')
})

test('a 429 that states Retry-After waits that long, within limits', async () => {
  const { calls, clock, queue, batch, byName } = setup({ concurrency: 1 })
  queue.add(batch(), [{ file: file('a', 100) }, { file: file('b', 200) }])
  calls[0].reject({ status: 429, success: false, retryAfter: 5, error: { code: 1006, message: 'busy' } })
  await flush()
  assert.equal(byName('a').transfer, 'queued')
  const delays = [...clock.pending].map(t => t.ms)
  assert.ok(delays.includes(5000), `expected a 5s timer, got ${delays}`)

  // A hostile or misconfigured header cannot park the file for an hour.
  clock.runAll() // the first backoff ends; b may start now
  assert.equal(calls.length, 2)
  calls[1].reject({ status: 429, success: false, retryAfter: 999999, error: { code: 1006, message: 'busy' } })
  await flush()
  const after = [...clock.pending].map(t => t.ms)
  assert.ok(after.every(ms => ms <= 60000), `delays must stay bounded, got ${after}`)
})

// While the server has said "wait", a new file must not start either. It is
// not the refused file, but sending it now collects the same 429.
test('a file added during a backoff window waits for the window', async () => {
  const { calls, clock, queue, batch } = setup({ concurrency: 2 })
  queue.add(batch(), [{ file: file('a', 100) }])
  calls[0].reject({ status: 429, success: false, retryAfter: 5, error: { code: 1006, message: 'busy' } })
  await flush()

  queue.add(batch(), [{ file: file('late', 300) }])
  assert.equal(calls.length, 1, 'the window blocks every start, not only the refused file')

  clock.runAll()
  assert.deepEqual(calls.map(c => c.payload.file.name), ['a', 'a', 'late'],
    'the window ends by offering the refused file and the new one')
})

// The server stored the file but could not queue its parse job (503 / 2500).
// The row exists (and is marked failed, which the duplicate check exempts), so
// the panel must keep its id and must not offer the file for another upload --
// that attempt would store the same bytes as a second document.
test('a stored-but-unqueued upload keeps its row and is not retried as a re-upload', async () => {
  const { calls, ends, queue, batch, byName } = setup()
  queue.add(batch(), [{ file: file('manual', 1) }])

  calls[0].reject({
    status: 503,
    success: false,
    error: {
      code: 2500,
      message: '文件已保存，但解析任务未能入队，请稍后重新解析',
      details: { knowledge_id: 'kn-9', title: 'manual', parse_status: 'failed' },
    },
  })
  await flush()

  assert.equal(byName('manual').transfer, 'failed')
  assert.equal(byName('manual').stored, true, 'the row is marked stored so the panel offers reparse, not retry')
  assert.equal(byName('manual').knowledgeId, 'kn-9', 'the stored row id must survive the failure')
  // "uploaded" here means "the server has a row", which is what the document
  // list refresh keys on -- the file did reach storage.
  assert.deepEqual(ends.at(-1), ['kb1', { uploaded: true, settled: true }])

  // The payload is dropped, so a retry cannot re-send the same bytes.
  queue.retryFailed()
  assert.equal(calls.length, 1, 'a stored row must not be uploaded again')
})

// The backoff timer is shared by the whole queue, so the first refusal used to
// fix the wait for every later one. A later 429 that asks for a longer wait has
// to replace it: otherwise the file is retried before the server said to,
// spends an attempt, and can fail while the queue is still full.
test('a later 429 with a longer Retry-After extends the shared backoff', async () => {
  const { calls, clock, queue, batch } = setup({ concurrency: 2 })
  queue.add(batch(), [{ file: file('a', 1) }, { file: file('b', 2) }])

  calls[0].reject({ status: 429, success: false, retryAfter: 5, error: { code: 1006, message: 'busy' } })
  await flush()
  assert.deepEqual([...clock.pending].map(t => t.ms), [5000])

  calls[1].reject({ status: 429, success: false, retryAfter: 20, error: { code: 1006, message: 'busy' } })
  await flush()
  assert.deepEqual([...clock.pending].map(t => t.ms), [20000],
    'the longer hint replaces the armed timer instead of being ignored')

  // A shorter hint does not shorten the wait either.
  clock.runAll()
  assert.equal(calls.length, 4)
})

// clear() ends the queue; a timer armed for the old files must not fire into
// the new one, and the per-file backoff bookkeeping must not outlive them.
test('clear() cancels a pending busy retry', async () => {
  const { calls, clock, queue, batch } = setup({ concurrency: 1 })
  queue.add(batch(), [{ file: file('a', 1) }])
  calls[0].reject({ status: 429, success: false, retryAfter: 5, error: { code: 1006, message: 'busy' } })
  await flush()
  assert.equal(clock.pending.size, 1)

  queue.clear()
  assert.equal(clock.pending.size, 0, 'the backoff timer belongs to the cleared queue')

  queue.add(batch(), [{ file: file('b', 2) }])
  assert.equal(calls.length, 2, 'the new file starts immediately')
})
