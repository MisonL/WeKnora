// Spreadsheet-safety marker used by the server's FAQ CSV export.
//
// A cell whose text starts with =, +, -, @, a tab or a CR is a formula to
// Excel and friends (CWE-1236), so the server prefixes such a cell with an
// apostrophe and the spreadsheet reads it as text. The marker is not part of
// the entry text, so the importers strip it back off: without that, exporting,
// editing and re-importing would attach a stray quote to every such question
// or answer.
//
// Text that already starts with an apostrophe and then a formula character is
// marked too (with a second apostrophe), so that stripping one on import is
// always the right move for a file this exporter produced. Only in a
// hand-authored file does `'=x` stay ambiguous, and there it loses the quote.

const FORMULA_LEAD = /^[=+\-@\t\r]/

function looksLikeFormula(value: string): boolean {
  return FORMULA_LEAD.test(value) || (value[0] === "'" && FORMULA_LEAD.test(value.slice(1)))
}

/** The exported spelling of one cell: prefixed only where it is dangerous. */
export function addFormulaMarker(value: string): string {
  return value !== '' && looksLikeFormula(value) ? `'${value}` : value
}

/** Reverse of addFormulaMarker; leaves any other text untouched. */
export function stripFormulaMarker(value: string): string {
  if (value.length < 2 || value[0] !== "'") return value
  return looksLikeFormula(value.slice(1)) ? value.slice(1) : value
}