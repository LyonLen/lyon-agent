// XlsxViewer 按列排序的纯逻辑。单元格值是 SheetJS `raw:false` 之后的格式化
// 字符串（如 "1,234"、"12%"、"2026/03/27"），这里把它们智能解析回数字/日期
// 再比较。空值不在此处处理——由 sortRows 统一「恒垫底」。

function toText(v: unknown): string {
  if (v === null || v === undefined) return ''
  return typeof v === 'object' ? JSON.stringify(v) : String(v)
}

// 去千分位逗号/空白、首尾货币符号、百分号（÷100）。纯数值返回 number。
export function parseNumber(s: string): number | null {
  let t = s.replace(/[\s,]/g, '')
  let pct = false
  if (t.endsWith('%')) {
    pct = true
    t = t.slice(0, -1)
  }
  t = t.replace(/^[¥$€£]+/, '').replace(/[¥$€£]+$/, '')
  if (!/^[+-]?\d+(\.\d+)?$/.test(t)) return null
  const n = Number(t)
  return pct ? n / 100 : n
}

// 只认无歧义日期：YYYY<sep>M<sep>D（分隔符须一致，排除 2026/02-01 这类），
// 或中文「YYYY年M月D日」。可选时分秒。返回本地时间戳，非法返回 null。
const DATE_RE = /^(\d{4})([/\-.])(\d{1,2})\2(\d{1,2})(?:[ T](\d{1,2}):(\d{1,2})(?::(\d{1,2}))?)?$/
const CN_DATE_RE = /^(\d{4})年(\d{1,2})月(\d{1,2})日(?:\s*(\d{1,2})[时点](\d{1,2})分?(?:(\d{1,2})秒)?)?$/

export function parseDate(s: string): number | null {
  const trimmed = s.trim()
  // DATE_RE 组序：y, sep, mo, d, h, mi, se（sep 占一位，destructure 跳过）
  const m1 = trimmed.match(DATE_RE)
  if (m1) {
    const [, y, , mo, d, h = '0', mi = '0', se = '0'] = m1
    const dt = new Date(Number(y), Number(mo) - 1, Number(d), Number(h), Number(mi), Number(se))
    return Number.isNaN(dt.getTime()) ? null : dt.getTime()
  }
  // CN_DATE_RE 组序：y, mo, d, h, mi, se（无 sep）
  const m2 = trimmed.match(CN_DATE_RE)
  if (m2) {
    const [, y, mo, d, h = '0', mi = '0', se = '0'] = m2
    const dt = new Date(Number(y), Number(mo) - 1, Number(d), Number(h), Number(mi), Number(se))
    return Number.isNaN(dt.getTime()) ? null : dt.getTime()
  }
  return null
}

// 比较两个非空值：数字 → 日期 → 文本 localeCompare（混合类型也安全，保证全序）。
export function compareValues(a: unknown, b: unknown): number {
  const sa = toText(a)
  const sb = toText(b)
  const na = parseNumber(sa)
  const nb = parseNumber(sb)
  if (na !== null && nb !== null) return na - nb
  const da = parseDate(sa)
  const db = parseDate(sb)
  if (da !== null && db !== null) return da - db
  return sa.localeCompare(sb)
}

export type SortDir = 'asc' | 'desc'
export type Sort = { col: number; dir: SortDir } | null

function isEmpty(v: unknown): boolean {
  if (v === null || v === undefined) return true
  if (typeof v === 'string' && v === '') return true
  return false
}

// 返回排序后的 { row, rowNo(原始 1-based 行号) }。sort=null 时原序。
// 空值恒垫底（不乘 dir，避免降序翻到最前）；键相等保原序（稳定）。
// 不负责截断——截断由调用方对返回值 slice。
export function sortRows(
  rows: unknown[][],
  sort: Sort,
): { row: unknown[]; rowNo: number }[] {
  const indexed = rows.map((row, i) => ({ row, rowNo: i + 1 }))
  if (!sort) return indexed
  const dir = sort.dir === 'asc' ? 1 : -1
  const col = sort.col
  indexed.sort((a, b) => {
    const va = a.row[col]
    const vb = b.row[col]
    const aE = isEmpty(va)
    const bE = isEmpty(vb)
    if (aE || bE) {
      if (aE && bE) return a.rowNo - b.rowNo
      return aE ? 1 : -1 // 空值垫底，不乘 dir
    }
    const cmp = compareValues(va, vb)
    return cmp !== 0 ? cmp * dir : a.rowNo - b.rowNo
  })
  return indexed
}

// 列头点击三态：无 → 升序 → 降序 → 无；换列重置为升序。
export function cycleSort(prev: Sort, col: number): Sort {
  if (prev && prev.col === col) {
    return prev.dir === 'asc' ? { col, dir: 'desc' } : null
  }
  return { col, dir: 'asc' }
}
