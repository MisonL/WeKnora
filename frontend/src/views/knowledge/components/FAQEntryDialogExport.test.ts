import assert from 'node:assert/strict'
import test from 'node:test'
import { fixture, flush } from '../../../test-utils/vueFixture.ts'
import { stripFormulaMarker } from '../../../utils/faqCsvText.ts'
import { parseBooleanField, spreadsheetEntryFields } from '../../../utils/faqSpreadsheet.ts'

// Single-entry edit and export round-trips. The batch paths got answer_strategy
// and id passthrough from the start; the dialog and the real export blob did
// not, so editing one entry or re-importing an export silently rewrote data
// server-side. These mount the real component the way the batch tests do and
// call the dialog handlers directly, asserting on the request bodies.

type Row = {
  id: number
  standard_question: string
  similar_questions: string[]
  negative_questions: string[]
  answers: string[]
  is_enabled: boolean
  tag_id: number | null
  answer_strategy?: 'all' | 'random'
}

function mountManager(t: any, opts: { entries?: Row[] } = {}) {
  const f = fixture(t)
  const rows: Row[] = opts.entries ?? []
  const listFAQEntries = t.mock.fn(async (_id: string, params: any) => {
    const page = Number(params?.page ?? 1)
    const size = Number(params?.page_size ?? 20)
    const start = (page - 1) * size
    return { data: { data: rows.slice(start, start + size), total: rows.length } }
  })
  const createFAQEntry = t.mock.fn(async () => ({ data: {} }))
  const updateFAQEntry = t.mock.fn(async () => ({ data: {} }))
  // The export path builds a real Blob and hands it to downloadExportBlob; the
  // stub records what URL.createObjectURL returned and when revoke fired, and
  // what href was live at the moment of the click.
  const created: string[] = []
  const revoked: string[] = []
  const clicks: Array<{ href: string; revokedAtClick: string[] }> = []
  const url = {
    createObjectURL: () => {
      const next = `blob:export-${created.length + 1}`
      created.push(next)
      return next
    },
    revokeObjectURL: (u: string) => { revoked.push(u) },
  }
  const doc = {
    documentElement: { clientHeight: 900 },
    querySelector: () => null,
    createElement: () => ({
      href: '',
      download: '',
      click() { clicks.push({ href: this.href, revokedAtClick: [...revoked] }) },
    }),
    body: { appendChild() {}, removeChild() {} },
  }
  // Template refs never bind in a no-render mount, so the submit-time form
  // validation has to be satisfied the same way t-form would satisfy it:
  // a passing validate().
  const mounted = f.component(new URL('./FAQEntryManager.vue', import.meta.url), {
    'vue-router': { useRouter: () => ({}) },
    'vue-i18n': { useI18n: () => ({ t: (key: string, params?: any) => (params ? key + JSON.stringify(params) : key) }) },
    'tdesign-vue-next': {
      MessagePlugin: { error: () => {}, success: () => {}, warning: () => {} },
      DialogPlugin: { confirm: () => ({}) },
      Icon: {},
    },
    '@/components/settings/useConfirmDelete': { useConfirmDelete: () => ({}) },
    '@/stores/auth': { useAuthStore: () => ({ hasRole: () => true }) },
    '@/stores/ui': { useUIStore: () => ({ clearSelectedTagIds() {} }) },
    '@/stores/organization': { useOrganizationStore: () => ({ sharedKnowledgeBases: [], async fetchSharedKnowledgeBases() {} }) },
    '@/stores/chatResources': { useChatResourcesStore: () => ({ rawKnowledgeBases: [], async ensureKnowledgeBases() {} }) },
    '@/api/knowledge-base': {
      listFAQEntries,
      createFAQEntry,
      updateFAQEntry,
      listKnowledgeTags: async () => ({ data: { data: [], total: 0 } }),
      getKnowledgeBaseById: async () => ({ data: { id: 'kb', type: 'faq' } }),
      getFAQImportProgress: async () => ({ data: null }),
      deleteFAQEntries: async () => ({ data: {} }),
      updateFAQEntryTagBatch: async () => ({ data: {} }),
      updateFAQEntryFieldsBatch: async () => ({ data: {} }),
      exportFAQEntries: async () => new Blob(['[]'], { type: 'application/json' }),
      updateFAQImportResultDisplayStatus: async () => ({ data: {} }),
    },
    '@/utils/protectedFileAccess': { buildProtectedFileRequest: () => null },
    '@/utils/protectedResource': { responseFileName: () => 'faq_failed_entries.csv' },
    '@/utils': { FAQ_IMPORT_MAX_ENTRIES: 1000 },
    '@/utils/faqCsvText': { stripFormulaMarker },
    '@/utils/faqSpreadsheet': { parseBooleanField, spreadsheetEntryFields },
    xlsx: {}, papaparse: {},
  }, { kbId: 'kb-a' }, { URL: url, document: doc })
  mounted.vm.editorFormRef = { validate: async () => true, clearValidate() {} }
  return { f, mounted, listFAQEntries, createFAQEntry, updateFAQEntry, created, revoked, clicks }
}

const entry = (patch: Partial<Row> = {}): Row => ({
  id: 7,
  standard_question: 'q',
  similar_questions: [],
  negative_questions: [],
  answers: ['a'],
  is_enabled: true,
  tag_id: null,
  ...patch,
})

// Sanitize-on-the-server semantics: a PUT payload that omits answer_strategy
// is read as "all". So an edit dialog opened on a "random" entry must send the
// value back, or saving any unrelated field flips the strategy with no error.
test('editing a random-strategy entry keeps answer_strategy=random in the PUT payload', async t => {
  const m = mountManager(t, { entries: [entry({ answer_strategy: 'random' })] })
  await flush()
  m.mounted.vm.openEditor(m.mounted.vm.entries[0])
  assert.equal(m.mounted.vm.editorForm.answer_strategy, 'random', 'the dialog has to carry the loaded value')
  await m.mounted.vm.handleSubmitEntry()
  assert.equal(m.updateFAQEntry.mock.calls.length, 1)
  const payload = m.updateFAQEntry.mock.calls[0].arguments[2] as FAQPayloadShape
  assert.equal(payload.answer_strategy, 'random')
})

// The list may omit the field for legacy rows; the dialog must then hold the
// default and send it explicitly rather than leave the choice to the server.
test('editing an entry without an answer_strategy sends the default all', async t => {
  const m = mountManager(t, { entries: [entry()] })
  await flush()
  m.mounted.vm.openEditor(m.mounted.vm.entries[0])
  assert.equal(m.mounted.vm.editorForm.answer_strategy, 'all')
  await m.mounted.vm.handleSubmitEntry()
  const payload = m.updateFAQEntry.mock.calls[0].arguments[2] as FAQPayloadShape
  assert.equal(payload.answer_strategy, 'all')
})

test('creating an entry always sends answer_strategy', async t => {
  const m = mountManager(t)
  await flush()
  m.mounted.vm.openEditor()
  m.mounted.vm.editorForm.standard_question = 'new q'
  m.mounted.vm.editorForm.answers = ['new a']
  await m.mounted.vm.handleSubmitEntry()
  assert.equal(m.createFAQEntry.mock.calls.length, 1)
  const payload = m.createFAQEntry.mock.calls[0].arguments[1] as FAQPayloadShape
  assert.equal(payload.answer_strategy, 'all')
})

type FAQPayloadShape = { answer_strategy?: string; id?: number; standard_question?: string }

// The export→edit→re-import loop is what FAQExportEntry.ID exists for (the
// server maps it back onto chunk.SeqID). normalizePayload rebuilds every
// imported row field by field, so a dropped id here loses the mapping.
test('normalizePayload keeps the id on imported entries and omits it on new ones', async t => {
  const m = mountManager(t)
  await flush()
  const roundTrip = m.mounted.vm.normalizePayload(entry()) as FAQPayloadShape
  assert.equal(roundTrip.id, 7, 'an exported entry keeps its id through JSON import')
  const fresh = m.mounted.vm.normalizePayload({ standard_question: 'q', answers: ['a'] }) as FAQPayloadShape
  assert.equal('id' in fresh, false, 'a created entry must not carry an invented id')
})

// The failure-CSV path already waits 1s before revoking, with a comment
// saying the click needs to be processed first; the real export had the same
// bug fixed everywhere else but here.
test('export revokes the object URL only after the click has been processed', async t => {
  const m = mountManager(t)
  await flush()
  await m.mounted.vm.handleExportFAQ('json')
  assert.equal(m.clicks.length, 1, 'the download anchor was clicked')
  assert.deepEqual(m.clicks[0].revokedAtClick, [], 'URL must still be valid at click time')
  assert.equal(m.clicks[0].href, m.created[0])
  // The revoke is scheduled with the component's setTimeout; the fixture
  // enables mock timers on the shared globals, so ticking advances it.
  t.mock.timers.tick(999)
  assert.deepEqual(m.revoked, [], 'nothing revoked before the 1s grace window ends')
  t.mock.timers.tick(1)
  assert.deepEqual(m.revoked, m.created, 'revoked exactly once the grace window passes')
})
