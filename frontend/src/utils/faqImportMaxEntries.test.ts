import assert from 'node:assert/strict'
import test from 'node:test'

import { FAQ_IMPORT_DEFAULT_MAX_ENTRIES, faqImportMaxEntries } from './faqImportMaxEntries.ts'

// The server reads WEKNORA_FAQ_IMPORT_MAX_ENTRIES with strconv.Atoi and keeps
// its default on anything else. The client has to agree, because a value the
// server rejects must not become a huge budget here: that would make the
// pre-submit check accept batches the server will refuse.
test('faqImportMaxEntries accepts what the server accepts', () => {
  assert.equal(faqImportMaxEntries(undefined), FAQ_IMPORT_DEFAULT_MAX_ENTRIES)
  assert.equal(faqImportMaxEntries(''), FAQ_IMPORT_DEFAULT_MAX_ENTRIES)
  assert.equal(faqImportMaxEntries('250'), 250)
  assert.equal(faqImportMaxEntries(' 250 '), 250)
  assert.equal(faqImportMaxEntries('+250'), 250)
  assert.equal(faqImportMaxEntries('0250'), 250)
  assert.equal(faqImportMaxEntries(250), 250)
  assert.equal(faqImportMaxEntries('2147483647'), 2147483647)
})

test('faqImportMaxEntries falls back where the server would too', () => {
  for (const value of ['0', '-5', 'abc', '2.5', '1e3', '20 00', '１２３', '99999999999999999999', '2147483648', true, {}]) {
    assert.equal(faqImportMaxEntries(value), FAQ_IMPORT_DEFAULT_MAX_ENTRIES, `value=${JSON.stringify(value)}`)
  }
})
