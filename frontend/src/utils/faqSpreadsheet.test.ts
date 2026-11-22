import assert from 'node:assert/strict'
import test from 'node:test'

import { parseBooleanField, spreadsheetEntryFields } from './faqSpreadsheet.ts'

// The server's export writes these two columns; importing must apply them or a
// round trip silently resets the answer strategy and the recommendation state.
test('the answer-strategy and recommendation columns survive an import', () => {
  assert.deepEqual(
    spreadsheetEntryFields({ '是否全部回复': 'TRUE', '是否禁止被推荐': 'FALSE' }),
    { answer_strategy: 'all', is_recommended: true },
  )
  assert.deepEqual(
    spreadsheetEntryFields({ '是否全部回复': 'FALSE', '是否禁止被推荐': 'TRUE' }),
    { answer_strategy: 'random', is_recommended: false },
  )
  // Lower-case English headers, as a spreadsheet made by the app itself may use.
  assert.deepEqual(
    spreadsheetEntryFields({ answer_all: '1', not_recommended: '0' }),
    { answer_strategy: 'all', is_recommended: true },
  )
})

// A sheet that never mentions the columns must not claim a value at all: the
// server keeps the current one on update, and a default would overwrite it.
test('missing columns produce no field', () => {
  assert.deepEqual(spreadsheetEntryFields({}), {})
  assert.deepEqual(spreadsheetEntryFields({ '问题': 'q', '是否停用': 'FALSE' }), {})
})

// A sheet can carry both header spellings at once, with only one filled in.
// The empty one is not "the value": reading it first would hide the real cell.
test('an empty alias does not mask the filled one', () => {
  assert.deepEqual(
    spreadsheetEntryFields({ '是否全部回复': '', answer_all: '1' }),
    { answer_strategy: 'all' },
  )
  assert.deepEqual(
    spreadsheetEntryFields({ '是否禁止被推荐': '  ', not_recommended: 'TRUE' }),
    { is_recommended: false },
  )
})

// The shared reader keeps the behaviour the component had for 停用: empty is
// "unset", recognised words are booleans, and anything else is unset unless the
// caller names a fallback -- an import must never guess a setting.
test('parseBooleanField keeps its accepted spellings and fallback', () => {
  assert.equal(parseBooleanField(''), undefined)
  assert.equal(parseBooleanField('  '), undefined)
  for (const truthy of ['TRUE', 'true', '1', '是', 'yes']) assert.equal(parseBooleanField(truthy), true)
  for (const falsy of ['FALSE', 'false', '0', '否', 'no']) assert.equal(parseBooleanField(falsy), false)
  assert.equal(parseBooleanField('maybe'), undefined)
  assert.equal(parseBooleanField('maybe', false), false)
  assert.equal(parseBooleanField('maybe', true), true)
})