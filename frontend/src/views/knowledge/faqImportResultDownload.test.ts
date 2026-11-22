import assert from 'node:assert/strict'
import test from 'node:test'
import { fixture, flush } from '../../test-utils/vueFixture.ts'
import { stripFormulaMarker } from '../../utils/faqCsvText.ts'
import { parseBooleanField, spreadsheetEntryFields } from '../../utils/faqSpreadsheet.ts'

// The import-result strip offers "Download Reasons" whenever the server kept a
// failed-entries CSV. Partial failures go into that same CSV, so the button has
// to appear for them too: an import whose entries all landed with conflicting
// questions stripped has failed_count 0 and a CSV nobody could reach, which is
// exactly the case a caller needs the file for.
async function mountManager(t: any) {
  const f = fixture(t)
  const mounted = f.component(new URL('./components/FAQEntryManager.vue', import.meta.url), {
    'vue-router': { useRouter: () => ({}) },
    'vue-i18n': { useI18n: () => ({ t: (key: string) => key }) },
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
      listFAQEntries: async () => ({ data: { data: [], total: 0 } }),
      listKnowledgeTags: async () => ({ data: { data: [], total: 0 } }),
      getKnowledgeBaseById: async () => ({ data: { id: 'kb', type: 'faq' } }),
      getFAQImportProgress: async () => ({ data: null }),
    },
    '@/utils/protectedFileAccess': { buildProtectedFileRequest: () => null },
    '@/utils/protectedResource': { responseFileName: () => 'faq_failed_entries.csv' },
    '@/utils': { FAQ_IMPORT_MAX_ENTRIES: 1000 },
    '@/utils/faqCsvText': { stripFormulaMarker },
    '@/utils/faqSpreadsheet': { parseBooleanField, spreadsheetEntryFields },
    xlsx: {}, papaparse: {},
  }, { kbId: 'kb-a' })
  await flush()
  return mounted.vm
}

const result = (patch: Record<string, unknown> = {}) => ({
  total_entries: 10,
  success_count: 10,
  failed_count: 0,
  skipped_count: 0,
  partial_failed_count: 0,
  merged_count: 0,
  added_count: 0,
  import_mode: 'append',
  imported_at: '',
  task_id: 'task-1',
  processing_time: 0,
  display_status: 'open',
  failed_entries_url: 'oss://bucket/faq_import/task-1_failed.csv',
  ...patch,
})

test('a partial-failure-only import still offers the failure CSV', async t => {
  const vm = await mountManager(t)
  vm.importResult = result({ success_count: 7, partial_failed_count: 3 })
  assert.equal(vm.canDownloadFailedEntries, true, 'the CSV holds the partial failures; the button must reach them')
})

test('an import with no failed or partial entries offers nothing to download', async t => {
  const vm = await mountManager(t)
  vm.importResult = result()
  assert.equal(vm.canDownloadFailedEntries, false)
})

test('an import without a stored CSV offers nothing to download', async t => {
  const vm = await mountManager(t)
  vm.importResult = result({ failed_count: 2, failed_entries_url: undefined })
  assert.equal(vm.canDownloadFailedEntries, false)
})