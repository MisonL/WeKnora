// The FAQ import entry budget, mirrored from the server.
//
// The server reads WEKNORA_FAQ_IMPORT_MAX_ENTRIES with strconv.Atoi and keeps
// its default on anything it cannot read. The server's int is 64-bit, so a
// configured value above int32 is still honoured there; this client clamps to
// int32 on purpose -- such a budget is meaningless as a UI pre-check, and
// staying inside int32 avoids any precision surprise in the comparison. The
// clamp only ever makes the pre-submit check stricter than the server, so a
// value this function lowers can refuse a batch early but never lets one
// through.
export const FAQ_IMPORT_DEFAULT_MAX_ENTRIES = 1000

export function faqImportMaxEntries(value: unknown): number {
  const text = typeof value === 'number' ? String(value) : typeof value === 'string' ? value.trim() : ''
  if (!/^\+?\d+$/.test(text)) return FAQ_IMPORT_DEFAULT_MAX_ENTRIES
  const n = Number(text.replace(/^\+/, ''))
  if (!Number.isSafeInteger(n) || n <= 0 || n > 2147483647) return FAQ_IMPORT_DEFAULT_MAX_ENTRIES
  return n
}
