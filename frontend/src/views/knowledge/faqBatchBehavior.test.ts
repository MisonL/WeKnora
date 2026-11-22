import assert from 'node:assert/strict'
import test from 'node:test'
import { fixture, flush } from '../../test-utils/vueFixture.ts'
import { stripFormulaMarker } from '../../utils/faqCsvText.ts'
import { parseBooleanField, spreadsheetEntryFields } from '../../utils/faqSpreadsheet.ts'

// Behavioural coverage for the batch operations that the FAQ entry manager
// drives. The component has no exported API for these, so the test mounts it
// with the real script block and calls the exposed handlers the way the batch
// bar does. A test that only reads the source string cannot catch the two
// regressions that matter here: a chunk size that drifts from the server cap,
// and a target knowledge base read live instead of snapshotted.

type Row = { id: number; standard_question: string; similar_questions: string[]; answers: string[]; is_enabled: boolean; tag_id: number | null }

function mountManager(t: any, opts: {
  kbId?: string
  entries?: number[]
  listEntries?: (id: string, params: any) => Promise<any>
  deleteEntries?: (id: string, ids: number[]) => Promise<any>
  updateTagBatch?: (id: string, body: any) => Promise<any>
  updateFieldsBatch?: (id: string, body: any) => Promise<any>
}) {
  const f = fixture(t)
  const rows: Row[] = (opts.entries ?? []).map(id => ({
    id,
    standard_question: `q${id}`,
    similar_questions: [],
    answers: [`a${id}`],
    is_enabled: true,
    tag_id: null,
  }))
  // The real endpoint pages, so the stub must too: a stub that returns every
  // row cannot tell a restore that follows the loaded window from one that
  // invents ids the component never fetched.
  const paged = async (id: string, params: any) => {
    if (opts.listEntries) return opts.listEntries(id, params)
    const page = Number(params?.page ?? 1)
    const size = Number(params?.page_size ?? 20)
    const start = (page - 1) * size
    return { data: { data: rows.slice(start, start + size), total: rows.length } }
  }
  // removeRows teaches the stub that a delete really removed the entries, so a
  // later refresh sees a shorter list the way the server would show it.
  const removeRows = (gone: number[]) => {
    const drop = new Set(gone)
    for (let i = rows.length - 1; i >= 0; i -= 1) if (drop.has(rows[i].id)) rows.splice(i, 1)
  }
  const listFAQEntries = t.mock.fn(paged)
  const deleteFAQEntries = t.mock.fn(opts.deleteEntries ?? (async () => ({ data: {} })))
  const updateFAQEntryTagBatch = t.mock.fn(opts.updateTagBatch ?? (async () => ({ data: {} })))
  const updateFAQEntryFieldsBatch = t.mock.fn(opts.updateFieldsBatch ?? (async () => ({ data: {} })))
  const messages: string[] = []
  const mounted = f.component(new URL('./components/FAQEntryManager.vue', import.meta.url), {
    'vue-router': { useRouter: () => ({}) },
    'vue-i18n': { useI18n: () => ({ t: (key: string, params?: any) => params ? key + JSON.stringify(params) : key }) },
    'tdesign-vue-next': {
      MessagePlugin: {
        error: (m: string) => messages.push(String(m)),
        success: (m: string) => messages.push(String(m)),
      },
    },
    '@/components/settings/useConfirmDelete': { useConfirmDelete: () => ({}) },
    '@/stores/auth': { useAuthStore: () => ({ hasRole: () => true }) },
    '@/stores/ui': { useUIStore: () => ({ clearSelectedTagIds() {} }) },
    '@/stores/organization': { useOrganizationStore: () => ({ sharedKnowledgeBases: [], async fetchSharedKnowledgeBases() {} }) },
    '@/stores/chatResources': { useChatResourcesStore: () => ({ rawKnowledgeBases: [], async ensureKnowledgeBases() {} }) },
    '@/api/knowledge-base': {
      listFAQEntries,
      listKnowledgeTags: async () => ({ data: { data: [], total: 0 } }),
      getKnowledgeBaseById: async () => ({ data: { id: 'kb', type: 'faq' } }),
      deleteFAQEntries,
      updateFAQEntryTagBatch,
      updateFAQEntryFieldsBatch,
    },
    '@/utils/protectedFileAccess': { buildProtectedFileRequest: () => null },
    '@/utils/protectedResource': { responseFileName: () => 'faq_failed_entries.csv' },
    '@/utils': { FAQ_IMPORT_MAX_ENTRIES: 1000 },
    '@/utils/faqCsvText': { stripFormulaMarker },
    '@/utils/faqSpreadsheet': { parseBooleanField, spreadsheetEntryFields },
    xlsx: {}, papaparse: {},
  }, { kbId: opts.kbId ?? 'kb-a' })
  return { f, mounted, rows, removeRows, listFAQEntries, deleteFAQEntries, updateFAQEntryTagBatch, updateFAQEntryFieldsBatch, messages }
}

function selected(vm: any, ids: number[]) {
  vm.selectedRowKeys = ids.slice()
}

// The server rejects more than 20 ids per request, so a larger selection has to
// leave in chunks. Submitting it whole is the regression this pins: the server
// answer would be a 400 and half the selection would silently stay untouched.
test('a large batch leaves in server-sized chunks, serially, against the original KB', async t => {
  const ids = Array.from({ length: 45 }, (_, i) => i + 1)
  const m = mountManager(t, {
    entries: ids,
    deleteEntries: async () => {
      await flush()
      return { data: {} }
    },
  })
  await flush()
  selected(m.mounted.vm, ids)

  await m.mounted.vm.handleBatchDelete()

  const sizes = m.deleteFAQEntries.mock.calls.map((call: { arguments: unknown[] }) => (call.arguments[1] as number[]).length)
  assert.deepEqual(sizes, [20, 20, 5])
  for (const call of m.deleteFAQEntries.mock.calls as Array<{ arguments: unknown[] }>) {
    assert.equal(call.arguments[0], 'kb-a')
  }
})

// A partially failing batch keeps its unconfirmed entries recoverable after
// the refresh. The failed chunk is past the first page, which is the case the
// real endpoint produces: the list comes back newest-first, twenty at a time,
// and entries the batch did succeed on can push the failed ones out of the
// window. A restore that only looks at the loaded page would treat "not
// loaded" as "deleted" and silently drop the retry.
test('entries left unconfirmed stay pending across pages after the refresh', async t => {
  const ids = Array.from({ length: 45 }, (_, i) => i + 1)
  let call = 0
  const m = mountManager(t, {
    entries: ids,
    deleteEntries: async (_kb: string, chunk: number[]) => {
      call += 1
      if (call === 2) throw new Error('chunk failed')
      return { data: {} }
    },
  })
  await flush()
  selected(m.mounted.vm, ids)
  await m.mounted.vm.handleBatchDelete()

  assert.ok(m.messages.some((m: string) => m.includes('batchPartial')), 'the partial outcome has to be reported')

  // Page one succeeded, so nothing on it is selected; the three failed ids are
  // remembered, not lost.
  assert.deepEqual([...m.mounted.vm.selectedRowKeys], [])
  const pending = m.mounted.vm.pendingRetry
  assert.ok(pending, 'the failed ids must survive the refresh')
  assert.equal(pending.kbId, 'kb-a')
  const secondChunk = Array.from({ length: 20 }, (_, i) => i + 21)
  assert.deepEqual([...pending.ids], secondChunk)

  // Loading more pages restores the failed rows as they arrive, page by page.
  // Loading the next page is what a scroll does. The load path itself calls
  // restorePendingRetry on append, so this also covers the wiring.
  await m.mounted.vm.loadEntries(true)
  // Every failed id is on the page that just arrived, so the whole page is
  // selected and nothing is left pending.
  assert.deepEqual([...m.mounted.vm.selectedRowKeys], secondChunk,
    'the failed page is selected once its rows load')
  assert.equal(m.mounted.vm.pendingRetry, null, 'once every failed id is loaded nothing stays pending')

})

// The prop the component reads is `props.kbId`, and the fixture mounts with a
// plain object, which Vue copies into a reactive props object at mount time.
// Writing that object during the run is therefore a real prop change on the
// component -- it is the same object the setup closure's `props` points at.
test('a knowledge base change mid-run does not retarget the remaining chunks', async t => {
  const ids = Array.from({ length: 45 }, (_, i) => i + 1)
  const gates: Array<() => void> = []
  let firstSent: () => void = () => {}
  const firstChunk = new Promise<void>(resolve => { firstSent = resolve })

  const m = mountManager(t, {
    entries: ids,
    deleteEntries: async () => {
      firstSent()
      await new Promise<void>(resolve => gates.push(resolve))
      return { data: {} }
    },
  })
  await flush()
  selected(m.mounted.vm, ids)

  const running = m.mounted.vm.handleBatchDelete()
  await firstChunk
  // Route change lands while the first chunk is still in flight.
  m.mounted.vm.$.props.kbId = 'kb-b'
  for (let i = 0; i < 4; i += 1) {
    gates.shift()?.()
    await flush()
  }
  await running

  assert.equal(m.deleteFAQEntries.mock.calls.length, 3)
  for (const call of m.deleteFAQEntries.mock.calls as Array<{ arguments: unknown[] }>) {
    assert.equal(call.arguments[0], 'kb-a', 'every chunk belongs to the KB the selection was made in')
  }
})

// The tag and status handlers record every id the batch sent as unconfirmed
// when any chunk fails, because both operations are idempotent: re-running a
// chunk that succeeded changes nothing, while trying to remember exactly which
// ids a failed chunk already covered does not.
test('a partially failing tag update keeps everything it sent recoverable', async t => {
  const ids = Array.from({ length: 45 }, (_, i) => i + 1)
  let call = 0
  const m = mountManager(t, {
    entries: ids,
    updateTagBatch: async () => {
      call += 1
      if (call === 3) throw new Error('chunk failed')
      return { data: {} }
    },
  })
  await flush()
  selected(m.mounted.vm, ids)
  m.mounted.vm.batchTagValue = '7'
  await m.mounted.vm.handleBatchTag()

  const diag = `tag calls ${m.updateFAQEntryTagBatch.mock.calls.length}, pending ${m.mounted.vm.pendingRetry ? m.mounted.vm.pendingRetry.ids.length : 'null'}, selected ${[...m.mounted.vm.selectedRowKeys].length}, entries ${m.mounted.vm.entries.length}`
  assert.equal([...m.mounted.vm.selectedRowKeys].length, 20, diag)
  assert.deepEqual([...m.mounted.vm.selectedRowKeys], ids.slice(0, 20))
  assert.deepEqual([...m.mounted.vm.pendingRetry.ids], ids.slice(20))
  assert.equal(m.mounted.vm.pendingRetry.kbId, 'kb-a')

  // Every further page only adds to the selection.
  await m.mounted.vm.loadEntries(true)
  assert.deepEqual([...m.mounted.vm.selectedRowKeys], ids.slice(0, 40))
  await m.mounted.vm.loadEntries(true)
  assert.deepEqual([...m.mounted.vm.selectedRowKeys], ids)
  assert.equal(m.mounted.vm.pendingRetry, null, 'every id is loaded, nothing stays queued')
})

test('a partially failing status change keeps everything it sent recoverable', async t => {
  const ids = Array.from({ length: 25 }, (_, i) => i + 1)
  let call = 0
  const m = mountManager(t, {
    entries: ids,
    updateFieldsBatch: async () => {
      call += 1
      if (call === 2) throw new Error('chunk failed')
      return { data: {} }
    },
  })
  await flush()
  selected(m.mounted.vm, ids)
  await m.mounted.vm.handleBatchStatusChange(false)

  assert.deepEqual([...m.mounted.vm.selectedRowKeys], ids.slice(0, 20))
  assert.deepEqual([...m.mounted.vm.pendingRetry.ids], [21, 22, 23, 24, 25])

  await m.mounted.vm.loadEntries(true)
  assert.deepEqual([...m.mounted.vm.selectedRowKeys], ids)
})

// Restoring page by page must accumulate. Each page's ids leave the queue when
// they are restored, so a later page cannot re-add them: assigning the present
// set instead of merging would silently drop everything already selected.
test('the selection accumulates across every page that loads', async t => {
  const ids = Array.from({ length: 45 }, (_, i) => i + 1)
  let call = 0
  const m = mountManager(t, {
    entries: ids,
    deleteEntries: async () => {
      call += 1
      if (call >= 2) throw new Error('chunk failed')
      return { data: {} }
    },
  })
  await flush()
  selected(m.mounted.vm, ids)
  await m.mounted.vm.handleBatchDelete()

  // Pages two and three both fail; each page's ids must survive the next one's
  // restore.
  await m.mounted.vm.loadEntries(true)
  assert.deepEqual([...m.mounted.vm.selectedRowKeys].sort((a: number, b: number) => a - b), ids.slice(20, 40))
  await m.mounted.vm.loadEntries(true)
  assert.deepEqual([...m.mounted.vm.selectedRowKeys].sort((a: number, b: number) => a - b), ids.slice(20),
    'page three must not erase page two')
  assert.equal(m.mounted.vm.pendingRetry, null)
})

// Retrying the rows that are on screen says nothing about the rows that are
// not. The queue has to MERGE, not be replaced: a second batch settles only the
// ids it actually sent, and ids left behind the loaded window by the first
// batch stay queued.
//
// The retried chunk fails too, so the refresh keeps the same twenty rows on
// screen and 41..45 stay off it -- which is the only shape where "this batch's
// failures" and "everything still unconfirmed" can differ.
test('a retry of the loaded rows keeps the unloaded failures queued', async t => {
  const ids = Array.from({ length: 45 }, (_, i) => i + 1)
  let call = 0
  const sent: number[][] = []
  const m = mountManager(t, {
    entries: ids,
    deleteEntries: async (_kb: string, chunk: number[]) => {
      call += 1
      sent.push(chunk)
      // The first chunk succeeds; the next two fail. The retry fails as well,
      // so nothing is removed from the list after the refresh.
      if (call >= 2) throw new Error('chunk failed')
      m.removeRows(chunk)
      return { data: {} }
    },
  })
  await flush()
  selected(m.mounted.vm, ids)
  await m.mounted.vm.handleBatchDelete()

  // The refresh reloaded page one, which now holds 21..40: those were restored
  // and left the queue, so only the never-fetched ids are still pending.
  assert.deepEqual([...m.mounted.vm.selectedRowKeys].sort((a: number, b: number) => a - b), ids.slice(20, 40),
    'the failed rows on the reloaded page come back selected')
  assert.deepEqual([...m.mounted.vm.pendingRetry.ids], ids.slice(40), 'the never-loaded ids stay queued')

  // Retry exactly what is on screen.
  await m.mounted.vm.handleBatchDelete()
  assert.equal(sent.length, 4, 'the retry is one more request, not a new walk over the list')
  assert.deepEqual([...sent[3]].sort((a: number, b: number) => a - b), ids.slice(20, 40),
    'only the loaded failures are re-sent')

  // The retry did not send 41..45, so replacing the queue with "this batch's
  // failures" would silently drop them: they would no longer be queued, and a
  // page that later shows them would not select them.
  assert.ok(m.mounted.vm.pendingRetry, 'the unloaded ids must still be recoverable')
  assert.deepEqual([...m.mounted.vm.pendingRetry.ids], ids.slice(40),
    'the unloaded ids survive a retry that never mentioned them')
  assert.deepEqual(m.rows.map((row: Row) => row.id), ids.slice(20), 'the first chunk is gone from the list')
})

// Clearing the selection means "forget this batch", so the retry queue goes
// with it and the next page load must not tick the failed rows again.
test('retrying the loaded failures keeps the unloaded ones queued', async t => {
  const ids = Array.from({ length: 45 }, (_, i) => i + 1)
  let call = 0
  const m = mountManager(t, {
    entries: ids,
    deleteEntries: async (_kb: string, chunk: number[]) => {
      call += 1
      // First batch: chunk 1 succeeds, chunks 2 and 3 fail. Everything after
      // that (the retry) succeeds.
      if (call === 2 || call === 3) throw new Error('chunk failed')
      m.removeRows(chunk)
      return { data: {} }
    },
  })
  await flush()
  selected(m.mounted.vm, ids)
  await m.mounted.vm.handleBatchDelete()
  assert.ok(m.mounted.vm.pendingRetry, 'pages two and three are both queued; got=' + JSON.stringify(m.mounted.vm.pendingRetry) + ' calls=' + m.deleteFAQEntries.mock.calls.length)

  // The first chunk's deletion shortened the list, so the next load brings the
  // tail of the queue into view as well. Retrying that selection settles
  // everything it sent; what matters is that nothing is lost on the way, not
  // which ids happen to be on screen.
  await m.mounted.vm.loadEntries(true)
  const retried = [...m.mounted.vm.selectedRowKeys].sort((a: number, b: number) => a - b)
  assert.ok(retried.length > 0, 'there is something to retry')
  await m.mounted.vm.handleBatchDelete()

  // Everything sent either succeeded (gone) or failed (queued): nothing that
  // was selected may simply disappear from both, and nothing that still exists
  // may stay ticked as if the delete had taken it.
  const queued = m.mounted.vm.pendingRetry ? [...m.mounted.vm.pendingRetry.ids] : []
  const stillThere = m.rows.map((row: Row) => row.id)
  for (const id of retried) {
    assert.ok(queued.includes(id) || !stillThere.includes(id), `id ${id} was neither deleted nor kept recoverable`)
  }
  assert.deepEqual([...m.mounted.vm.selectedRowKeys].filter((id: number) => stillThere.includes(id)), [],
    'no row that still exists may remain ticked after a successful delete')

  // Finally, a page load of the survivors ticks back whatever is still queued.
  await m.mounted.vm.loadEntries(true)
  for (const id of queued) {
    assert.ok(stillThere.includes(id), `queued id ${id} no longer exists`)
  }
})

// Clearing the selection means "forget this batch", so the retry queue goes
// with it and the next page load must not tick the failed rows again.
test('clearing the selection drops the retry queue', async t => {
  const ids = Array.from({ length: 25 }, (_, i) => i + 1)
  let call = 0
  const m = mountManager(t, {
    entries: ids,
    updateFieldsBatch: async () => {
      call += 1
      if (call === 2) throw new Error('chunk failed')
      return { data: {} }
    },
  })
  await flush()
  selected(m.mounted.vm, ids)
  await m.mounted.vm.handleBatchStatusChange(false)
  assert.ok(m.mounted.vm.pendingRetry, 'the unloaded failed ids are queued')

  m.mounted.vm.clearFAQSelection()
  assert.equal(m.mounted.vm.pendingRetry, null, 'clearing cancels the queue too')

  await m.mounted.vm.loadEntries(true)
  assert.deepEqual([...m.mounted.vm.selectedRowKeys], [], 'a cleared queue must not reselect anything')
})

// A retry has to repeat the operation it retries. The tag dialog starts empty,
// so reopening it for a failed batch and confirming would send null -- which
// removes the tag -- instead of the tag the batch was setting.
test('a tag retry reopens with the tag the failed batch was setting', async t => {
  const ids = Array.from({ length: 25 }, (_, i) => i + 1)
  let call = 0
  const m = mountManager(t, {
    entries: ids,
    updateTagBatch: async () => {
      call += 1
      if (call === 2) throw new Error('chunk failed')
      return { data: {} }
    },
  })
  await flush()
  selected(m.mounted.vm, ids)
  m.mounted.vm.batchTagValue = '7'
  await m.mounted.vm.handleBatchTag()

  m.mounted.vm.openBatchTagDialog()
  assert.equal(m.mounted.vm.batchTagValue, '7',
    'reopening the dialog for a retry must restore the tag, not start empty')
  await m.mounted.vm.handleBatchTag()

  const retry = m.updateFAQEntryTagBatch.mock.calls.at(-1) as { arguments: unknown[] }
  const updates = (retry.arguments[1] as { updates: Record<string, number | null> }).updates
  for (const [id, tag] of Object.entries(updates)) {
    assert.equal(tag, 7, `retry sent ${String(tag)} for ${id}; a null removes the tag the first chunk had already set`)
  }
})

// A successful single-row deletion refreshes the list, and the rows that were
// restored from the queue for the failed pages have to survive that refresh:
// they are still waiting to be retried, not confirmed.
test('a single-row deletion keeps the other restored failures recoverable', async t => {
  const ids = Array.from({ length: 45 }, (_, i) => i + 1)
  let call = 0
  const m = mountManager(t, {
    entries: ids,
    deleteEntries: async (_kb: string, chunk: number[]) => {
      call += 1
      if (call === 2 || call === 3) throw new Error('chunk failed')
      m.removeRows(chunk)
      return { data: {} }
    },
  })
  await flush()
  selected(m.mounted.vm, ids)
  await m.mounted.vm.handleBatchDelete()
  await m.mounted.vm.loadEntries(true)
  const restored = [...m.mounted.vm.selectedRowKeys].sort((a: number, b: number) => a - b)
  assert.ok(restored.length > 1, 'the failed pages were restored and are waiting to be retried')

  await m.mounted.vm.deleteFAQEntryById(restored[0])
  await m.mounted.vm.loadEntries(false)
  const survivors = m.rows.map((row: Row) => row.id)
  const queued = m.mounted.vm.pendingRetry ? [...m.mounted.vm.pendingRetry.ids] : []
  const expected = restored.slice(1).filter(id => survivors.includes(id) || queued.includes(id))
  for (const id of expected) {
    assert.ok(queued.includes(id) || m.mounted.vm.selectedRowKeys.includes(id),
      `id ${id} was waiting to be retried but a single-row delete dropped it from both the queue and the selection`)
  }
  assert.ok(!m.mounted.vm.selectedRowKeys.includes(restored[0]), 'the deleted row must not come back ticked')
})

// Nothing a refresh does may silently discard rows the operator still has to
// retry: a plain reload resets the selection, so the queue has to carry them.
test('a plain refresh after a partial batch keeps the restored failures queued', async t => {
  const ids = Array.from({ length: 45 }, (_, i) => i + 1)
  let call = 0
  const m = mountManager(t, {
    entries: ids,
    deleteEntries: async (_kb: string, chunk: number[]) => {
      call += 1
      if (call === 2 || call === 3) throw new Error('chunk failed')
      m.removeRows(chunk)
      return { data: {} }
    },
  })
  await flush()
  selected(m.mounted.vm, ids)
  await m.mounted.vm.handleBatchDelete()
  await m.mounted.vm.loadEntries(true)
  const restored = [...m.mounted.vm.selectedRowKeys]
  assert.ok(restored.length > 0)

  await m.mounted.vm.loadEntries(false)
  const queued = m.mounted.vm.pendingRetry ? [...m.mounted.vm.pendingRetry.ids] : []
  const ticked = m.mounted.vm.selectedRowKeys
  for (const id of restored) {
    assert.ok(queued.includes(id) || ticked.includes(id),
      `id ${id} still needs a retry but a plain refresh dropped it from both the queue and the selection`)
  }
})
