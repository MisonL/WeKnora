import assert from 'node:assert/strict'
import test from 'node:test'

import { addFormulaMarker, stripFormulaMarker } from './faqCsvText.ts'

// A leading formula character is what spreadsheets evaluate, so it is exactly
// those cells the export prefixes and exactly those the import un-prefixes.
test('the marker is added and removed only for formula-looking cells', () => {
  for (const dangerous of ['=1+1', '+x', '-2', '@SUM(A1)', '\ttab', '\rcr']) {
    assert.equal(addFormulaMarker(dangerous), `'${dangerous}`)
    assert.equal(stripFormulaMarker(`'${dangerous}`), dangerous)
  }
  assert.equal(addFormulaMarker('plain'), 'plain')
  assert.equal(stripFormulaMarker('plain'), 'plain')
  assert.equal(addFormulaMarker(''), '')
})

// Text that already starts with an apostrophe and then a formula character is
// itself a formula-looking cell once the apostrophe is read as text, so it is
// marked with a second apostrophe. Stripping one on import then returns the
// original text exactly, which is what makes the round trip lossless.
test('a leading apostrophe plus formula character is marked twice', () => {
  assert.equal(addFormulaMarker("'=x"), "''=x")
  assert.equal(stripFormulaMarker("''=x"), "'=x")
  assert.equal(stripFormulaMarker("'quoted"), "'quoted")
  assert.equal(stripFormulaMarker("'"), "'")
})