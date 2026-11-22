import assert from 'node:assert/strict'
import test from 'node:test'
import {
  classifyUploadResult,
  estimateRate,
  estimateRemainingSeconds,
  itemPhase,
  needsParsePolling,
  pickNextToStart,
  splitDuration,
  summarizeItems,
  type UploadItem,
} from './uploadTasksState.ts'

let seq = 0
const item = (patch: Partial<UploadItem> = {}): UploadItem => ({
  id: `item-${seq++}`,
  batchId: 'batch',
  name: 'a.pdf',
  relativePath: '',
  size: 100,
  loaded: 0,
  transfer: 'queued',
  ...patch,
})

test('itemPhase folds transfer and parse state into one row status', () => {
  assert.equal(itemPhase(item()), 'waiting')
  assert.equal(itemPhase(item({ transfer: 'uploading', loaded: 40 })), 'uploading')
  assert.equal(itemPhase(item({ transfer: 'uploading', loaded: 100 })), 'saving')
  assert.equal(itemPhase(item({ transfer: 'uploaded', parseStatus: 'pending' })), 'parsing')
  assert.equal(itemPhase(item({ transfer: 'uploaded', parseStatus: 'processing' })), 'parsing')
  // finalizing is already searchable, so it counts as ready.
  assert.equal(itemPhase(item({ transfer: 'uploaded', parseStatus: 'finalizing' })), 'ready')
  assert.equal(itemPhase(item({ transfer: 'uploaded', parseStatus: 'completed' })), 'ready')
  assert.equal(itemPhase(item({ transfer: 'uploaded', parseStatus: 'failed' })), 'failed')
  assert.equal(itemPhase(item({ transfer: 'uploaded', parseStatus: 'deleted' })), 'cancelled')
  assert.equal(itemPhase(item({ transfer: 'failed' })), 'failed')
  assert.equal(itemPhase(item({ transfer: 'duplicate' })), 'duplicate')
  assert.equal(itemPhase(item({ transfer: 'cancelled' })), 'cancelled')
})

test('needsParsePolling keeps watching until the row leaves finalizing', () => {
  assert.equal(needsParsePolling(item({ transfer: 'uploaded', knowledgeId: 'k', parseStatus: 'pending' })), true)
  assert.equal(needsParsePolling(item({ transfer: 'uploaded', knowledgeId: 'k', parseStatus: 'finalizing' })), true)
  assert.equal(needsParsePolling(item({ transfer: 'uploaded', knowledgeId: 'k', parseStatus: 'completed' })), false)
  assert.equal(needsParsePolling(item({ transfer: 'uploaded', parseStatus: 'pending' })), false)
  assert.equal(needsParsePolling(item({ transfer: 'duplicate', knowledgeId: 'k' })), false)
})

test('summarizeItems reports the uploading stage while anything is queued or in flight', () => {
  const summary = summarizeItems([
    item({ transfer: 'uploaded', parseStatus: 'completed' }),
    item({ transfer: 'uploading', loaded: 50 }),
    item(),
    item({ transfer: 'failed' }),
  ])
  assert.equal(summary.stage, 'uploading')
  assert.equal(summary.transferSettled, 2)
  assert.equal(summary.transferTotal, 4)
  assert.equal(summary.loadedBytes, 250)
  // The failed file is settled for the remaining-bytes estimate but never counts as sent.
  assert.equal(summary.sentBytes, 150)
  assert.equal(summary.totalBytes, 400)
  assert.equal(summary.retryable, 1)
  assert.deepEqual(summary.bar, { ready: 0.25, active: 0.125, failed: 0.25, duplicate: 0 })
})

test('summarizeItems moves to parsing once every transfer settled, then to done', () => {
  const parsing = summarizeItems([
    item({ transfer: 'uploaded', parseStatus: 'processing' }),
    item({ transfer: 'uploaded', parseStatus: 'finalizing' }),
    item({ transfer: 'duplicate' }),
  ])
  assert.equal(parsing.stage, 'parsing')
  assert.equal(parsing.uploaded, 2)
  assert.equal(parsing.parsing, 1)
  assert.equal(parsing.ready, 1)

  const done = summarizeItems([
    item({ transfer: 'uploaded', parseStatus: 'completed' }),
    item({ transfer: 'uploaded', parseStatus: 'failed' }),
  ])
  assert.equal(done.stage, 'done')
  assert.equal(done.failed, 1)
  // A parse failure cannot be retried by uploading again.
  assert.equal(done.retryable, 0)
})

test('cancelled transfers leave the bar denominator and byte totals', () => {
  const summary = summarizeItems([
    item({ transfer: 'uploaded', parseStatus: 'completed' }),
    item({ transfer: 'cancelled' }),
  ])
  assert.equal(summary.stage, 'done')
  assert.equal(summary.transferTotal, 1)
  assert.equal(summary.totalBytes, 100)
  assert.equal(summary.retryable, 1)
  assert.equal(summary.bar.ready, 1)
})

test('a stored row counts as failed but not as retryable', () => {
  // Its bytes are on the server, so "retry N failed" would queue a second
  // upload of the same file: the row is marked failed, and failed rows are
  // exempt from the duplicate check. Reparse is the only way forward.
  const summary = summarizeItems([
    item({ transfer: 'failed', stored: true, knowledgeId: 'kn-1' }),
    item({ transfer: 'failed' }),
  ])
  assert.equal(summary.failed, 2)
  assert.equal(summary.retryable, 1)
})

test('pickNextToStart fills free slots in queue order', () => {
  const items = [
    item({ transfer: 'uploading' }),
    item({ transfer: 'uploaded' }),
    item({ id: 'q1' }),
    item({ id: 'q2' }),
    item({ id: 'q3' }),
  ]
  assert.deepEqual(pickNextToStart(items, 3).map(i => i.id), ['q1', 'q2'])
  assert.deepEqual(pickNextToStart(items, 1), [])
})

test('pickNextToStart skips items whose conflict key is already in flight', () => {
  const items = [
    item({ transfer: 'uploading', size: 10 }),
    item({ id: 'same-as-running', size: 10 }),
    item({ id: 'first-of-pair', size: 20 }),
    item({ id: 'second-of-pair', size: 20 }),
    item({ id: 'free', size: 30 }),
  ]
  const bySize = (i: UploadItem) => String(i.size)
  assert.deepEqual(pickNextToStart(items, 3, bySize).map(i => i.id), ['first-of-pair', 'free'])
})

test('classifyUploadResult reads success, duplicate rejections and failures', () => {
  assert.deepEqual(
    classifyUploadResult({ success: true, data: { id: 'k1', parse_status: 'pending' } }),
    { kind: 'uploaded', knowledgeId: 'k1', parseStatus: 'pending' },
  )
  assert.deepEqual(
    classifyUploadResult({ status: 409, success: false, code: 'duplicate_file', message: 'exists', data: { id: 'k0' } }),
    { kind: 'duplicate', knowledgeId: 'k0', message: 'exists' },
  )
  assert.deepEqual(
    classifyUploadResult({ status: 400, success: false, error: { code: 'bad', message: 'too large' } }),
    { kind: 'failed', message: 'too large' },
  )
  assert.deepEqual(classifyUploadResult({ message: 'Network error' }), { kind: 'failed', message: 'Network error' })
  assert.deepEqual(
    classifyUploadResult({ status: 429, success: false, error: { code: 1006, message: '服务器正在处理其他上传，请稍后重试' } }),
    { kind: 'busy', message: '服务器正在处理其他上传，请稍后重试', retryAfterMs: 0 },
  )
  assert.deepEqual(
    classifyUploadResult({ status: 429, success: false, retryAfter: 5, error: { code: 1006, message: 'busy' } }),
    { kind: 'busy', message: 'busy', retryAfterMs: 5000 },
  )
  assert.deepEqual(classifyUploadResult(undefined), { kind: 'failed', message: undefined })
})

// A document that was stored but never queued for parsing is its own outcome:
// re-uploading it would store the same bytes again (failed rows are exempt from
// the duplicate check), so the panel has to keep the row id and point at a
// reparse instead.
test('classifyUploadResult marks a stored-but-unqueued upload as stored, not failed', () => {
  assert.deepEqual(
    classifyUploadResult({
      status: 503,
      success: false,
      error: {
        code: 2500,
        message: '文件已保存，但解析任务未能入队，请稍后重新解析',
        details: { knowledge_id: 'kn-7', title: 'a.pdf', parse_status: 'failed' },
      },
    }),
    { kind: 'stored', knowledgeId: 'kn-7', message: '文件已保存，但解析任务未能入队，请稍后重新解析' },
  )

  // A 429 that carries the row is the same "stored, reparse later" case as the
  // 503: retrying it would store the bytes again.
  assert.deepEqual(
    classifyUploadResult({
      status: 429,
      success: false,
      error: { code: 1006, message: '文件已保存，但服务器正在处理的文档较多，暂未开始解析', details: { knowledge_id: 'kn-9' } },
    }),
    { kind: 'stored', knowledgeId: 'kn-9', message: '文件已保存，但服务器正在处理的文档较多，暂未开始解析' },
  )

  // A plain 503 is still a failure: only the typed code means "the row exists".
  assert.deepEqual(
    classifyUploadResult({ status: 503, success: false, error: { code: 1008, message: 'down' } }),
    { kind: 'failed', message: 'down' },
  )
})

test('estimateRate waits for a one second window and ignores stale samples', () => {
  assert.equal(estimateRate([{ at: 0, bytes: 0 }, { at: 500, bytes: 1000 }]), 0)
  assert.equal(estimateRate([{ at: 0, bytes: 0 }, { at: 2000, bytes: 4000 }]), 2000)
  const samples = [
    { at: 0, bytes: 0 },
    { at: 10_000, bytes: 100_000 },
    { at: 12_000, bytes: 104_000 },
  ]
  assert.equal(estimateRate(samples, 8000), 2000)
})

test('remaining time is rounded into a stable unit', () => {
  assert.equal(estimateRemainingSeconds(1000, 0), null)
  assert.equal(estimateRemainingSeconds(0, 100), null)
  assert.equal(estimateRemainingSeconds(1000, 300), 4)
  assert.deepEqual(splitDuration(4), { unit: 'seconds', value: 5 })
  assert.deepEqual(splitDuration(41), { unit: 'seconds', value: 45 })
  assert.deepEqual(splitDuration(57), { unit: 'minutes', value: 1 })
  assert.deepEqual(splitDuration(610), { unit: 'minutes', value: 11 })
  assert.deepEqual(splitDuration(5400), { unit: 'hours', value: 1.5 })
})

// The server admits at most one upload at or above UPLOAD_BIG_BYTES, so a
// second big file started beside it is a 429 the queue can avoid -- and it
// spends one of that file's five busy attempts on a wait it could have skipped.
test('pickNextToStart runs only one big upload at a time', () => {
  const big = 40
  const items = [
    item({ id: 'big-running', transfer: 'uploading', size: 100 }),
    item({ id: 'big-queued', size: 100 }),
    item({ id: 'small', size: 10 }),
    item({ id: 'big-fits', size: 100 }),
  ]
  const picked = pickNextToStart(items, 3, undefined, big).map(i => i.id)
  assert.deepEqual(picked, ['small'], 'no second big file while one is in flight')

  // With nothing big running, the first big file starts, and the slot it
  // occupies does not admit the next one.
  const idle = [item({ id: 'b1', size: 100 }), item({ id: 'b2', size: 100 }), item({ id: 's1', size: 1 })]
  assert.deepEqual(pickNextToStart(idle, 3, undefined, big).map(i => i.id), ['b1', 's1'])
})
