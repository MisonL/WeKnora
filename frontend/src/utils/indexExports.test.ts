import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { fixture } from '../test-utils/vueFixture.ts'

// The file caps and the file-size check live in the same module as the FAQ
// budget. A change to one of them once dropped the cap exports, and every
// caller of fileSizeVerification then threw a ReferenceError. This runs the
// real module and the real function, so a missing export fails here instead of
// in the file picker.

function loadUtils(t: any, runtime: Record<string, unknown> = {}) {
  const source = readFileSync(new URL('./index.ts', import.meta.url), 'utf8')
    // The module reads import.meta.env, which the sandbox has no notion of.
    .replace('import.meta.env.VITE_MAX_FILE_SIZE_MB', 'undefined')
    .replace('import.meta.env.VITE_MAX_SKILL_BUNDLE_SIZE_MB', 'undefined')
    .replace('import.meta.env.VITE_FAQ_IMPORT_MAX_ENTRIES', 'undefined')
  const f = fixture(t)
  return f.evaluate(source, {
    'tdesign-vue-next': { MessagePlugin: { error() {}, success() {}, warning() {} } },
    '@/i18n': { default: { global: { t: (k: string) => k } } },
    './fileTypeVerification': { shouldRejectKnowledgeFileType: () => false },
    './faqImportMaxEntries': { faqImportMaxEntries: (v: unknown) => (v === undefined ? 1000 : Number(v)) },
  }) as any
}

test('the module still exports every file cap and the FAQ budget', (t: any) => {
  const utils = loadUtils(t)
  assert.equal(utils.MAX_FILE_SIZE_MB, 50)
  assert.equal(utils.MAX_FILE_SIZE_BYTES, 50 * 1024 * 1024)
  assert.equal(utils.MAX_SKILL_BUNDLE_SIZE_MB, 256)
  assert.equal(utils.MAX_SKILL_BUNDLE_SIZE_BYTES, 256 * 1024 * 1024)
  assert.equal(utils.FAQ_IMPORT_MAX_ENTRIES, 1000)
})

test('fileSizeVerification runs against the caps it reads', (t: any) => {
  const utils = loadUtils(t)
  // true means "exceeds the deploy-time limit".
  assert.equal(utils.fileSizeVerification({ size: 1024 }, true), false)
  assert.equal(utils.fileSizeVerification({ size: 51 * 1024 * 1024 }, true), true)
})
