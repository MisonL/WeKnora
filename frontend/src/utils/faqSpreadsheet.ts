// Reading FAQ spreadsheet columns, shared by the CSV and XLSX import paths.
//
// The server's own CSV export writes 是否全部回复 and 是否禁止被推荐. Ignoring
// them made an export → spreadsheet → import round trip silently reset the
// answer strategy and un-recommend every entry.

export type AnswerStrategy = 'all' | 'random'

export interface SpreadsheetEntryFields {
  answer_strategy?: AnswerStrategy
  is_recommended?: boolean
}

/** 解析布尔字段（支持多种格式：TRUE/FALSE, true/false, 是/否, 1/0等） */
export function parseBooleanField(value?: string, defaultValue?: boolean): boolean | undefined {
  // A blank or whitespace-only cell is "not stated", not the fallback: the
  // templates leave optional columns empty.
  const normalized = value?.trim().toUpperCase()
  if (!normalized) return undefined
  if (normalized === 'TRUE' || normalized === '1' || normalized === '是' || normalized === 'YES') {
    return true
  }
  if (normalized === 'FALSE' || normalized === '0' || normalized === '否' || normalized === 'NO') {
    return false
  }
  // An unrecognised word is "not understood", not a guess: with no fallback it
  // stays unset, so the server keeps the stored value instead of the import
  // flipping a setting on whatever the cell happened to contain.
  return defaultValue
}

/**
 * Reads the first of `keys` whose cell actually has a value. A plain `??` is
 * not enough: a spreadsheet that carries both header spellings usually leaves
 * one of them empty, and an empty string is not null -- it would mask the
 * column that does hold the value.
 */
function firstFilledField(row: Record<string, string>, keys: string[]): string | undefined {
  for (const key of keys) {
    const value = row[key]
    if (value !== undefined && value.trim() !== '') return value
  }
  return undefined
}

/**
 * The two boolean columns the FAQ export carries beyond enablement. A missing
 * column yields no field at all, so an update keeps the current value.
 */
export function spreadsheetEntryFields(row: Record<string, string>): SpreadsheetEntryFields {
  const answerAll = parseBooleanField(firstFilledField(row, ['是否全部回复', 'answer_all']))
  const banned = parseBooleanField(firstFilledField(row, ['是否禁止被推荐', 'not_recommended']))
  return {
    ...(answerAll === undefined ? {} : { answer_strategy: (answerAll ? 'all' : 'random') as AnswerStrategy }),
    ...(banned === undefined ? {} : { is_recommended: !banned }),
  }
}