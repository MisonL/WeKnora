import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

// The contract worth pinning is a cross-layer one: the UI submits batch
// operations in FAQ_BATCH_CHUNK_SIZE-sized chunks because the server rejects
// anything larger. The component has no other behavioural test harness, but
// the two numbers drifting apart is exactly the regression that turns a
// successful batch into a 400, so read both constants out of their sources.
// The constant may be written as a literal or as a reference to another
// constant in the same file (`= SOME_OTHER`), so a refactor that hoists the
// number does not break this contract test. A reference is followed one step;
// anything the test cannot resolve is reported rather than guessed.
function batchChunkSize(): number {
  const source = readFileSync(new URL('./components/FAQEntryManager.vue', import.meta.url), 'utf8')
  const match = source.match(/const FAQ_BATCH_CHUNK_SIZE\s*=\s*([\w.]+)/)
  assert.ok(match, 'FAQ_BATCH_CHUNK_SIZE should stay readable from its definition')
  if (/^\d+$/.test(match[1])) {
    return Number(match[1])
  }
  const name = match[1].split('.').pop()
  const indirect = source.match(new RegExp(`const ${name}\\s*=\\s*(\\d+)`))
  assert.ok(indirect, `FAQ_BATCH_CHUNK_SIZE refers to ${match[1]}, which the test could not resolve to a number`)
  return Number(indirect[1])
}

function serverCap(file: string, field: string): number {
  const source = readFileSync(new URL('../../../../internal/' + file, import.meta.url), 'utf8')
  const line = source.split('\n').find(l => l.includes(field) && l.includes('binding:'))
  assert.ok(line, `${field} should carry a binding tag`)
  const match = line.match(/max=(\d+)/)
  assert.ok(match, `${field} should carry a max tag`)
  return Number(match[1])
}

// exclude_ids is capped conditionally -- only a by_tag update reads it -- so the
// constant is checked through the function that enforces that rule rather than
// through a binding tag that no longer exists.
function conditionalExcludeCap(): number {
  const source = readFileSync(new URL('../../../../internal/types/faq.go', import.meta.url), 'utf8')
  const fn = source.match(/func ValidateFAQEntryFieldsBatch\([^)]*\)[^{]*\{([\s\S]*?)\n\}/)
  assert.ok(fn, 'ValidateFAQEntryFieldsBatch should exist')
  assert.ok(/len\(r\.ByTag\)\s*>\s*0/.test(fn[1]), 'the exclusion cap must be conditional on by_tag')
  assert.ok(/len\(r\.ExcludeIDs\)\s*>\s*FAQBatchMaxEntriesPerRequest/.test(fn[1]),
    'the exclusion cap must use the shared per-request constant')
  const constant = source.match(/FAQBatchMaxEntriesPerRequest\s*=\s*(\d+)/)
  assert.ok(constant, 'FAQBatchMaxEntriesPerRequest should stay a literal the test can read')
  return Number(constant[1])
}

// The service layer repeats the by_id bound as a guard for paths that never go
// through the HTTP handler. It must use the same constant: a literal there
// would let the two caps drift, and the UI would fill a chunk the service
// refuses.
function serviceByIDCap(): number {
  const source = readFileSync(new URL('../../../../internal/application/service/knowledge_faq.go', import.meta.url), 'utf8')
  const guard = source.match(/len\(req\.ByID\)\s*>=?\s*([A-Za-z0-9_.]+)/)
  assert.ok(guard, 'UpdateFAQEntryFieldsBatch should bound req.ByID before use')
  assert.equal(guard[1], 'types.FAQBatchMaxEntriesPerRequest',
    'the service by_id cap must use the shared constant, not a literal')
  return conditionalExcludeCap()
}

test('FAQ batch chunk size matches every server-side per-request cap', () => {
  const chunk = batchChunkSize()
  assert.equal(chunk, serverCap('handler/faq.go', 'IDs []int64'))
  assert.equal(chunk, serverCap('handler/faq.go', 'Updates map[int64]*int64'))
  assert.equal(chunk, serverCap('types/faq.go', 'ByID map[int64]FAQEntryFieldsUpdate'))
  assert.equal(chunk, conditionalExcludeCap())
  assert.equal(chunk, serviceByIDCap())
})
