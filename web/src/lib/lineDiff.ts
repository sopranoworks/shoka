export type DiffRowType = 'same' | 'add' | 'del'
export interface DiffRow {
  type: DiffRowType
  text: string
}

// A minimal LCS-based line diff for the conflict "Show diff" view (§3.5).
// `left` is the server-latest content, `right` is the editor buffer: lines only
// on the server are `del`, lines only in the buffer are `add`, shared lines are
// `same`. Hand-rolled (no diff dependency) — sufficient for a document store at
// this scale.
export function lineDiff(left: string, right: string): DiffRow[] {
  const a = left.split('\n')
  const b = right.split('\n')
  const n = a.length
  const m = b.length

  // dp[i][j] = LCS length of a[i:] and b[j:].
  const dp: number[][] = Array.from({ length: n + 1 }, () =>
    new Array<number>(m + 1).fill(0),
  )
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      dp[i][j] =
        a[i] === b[j]
          ? dp[i + 1][j + 1] + 1
          : Math.max(dp[i + 1][j], dp[i][j + 1])
    }
  }

  const rows: DiffRow[] = []
  let i = 0
  let j = 0
  while (i < n && j < m) {
    if (a[i] === b[j]) {
      rows.push({ type: 'same', text: a[i] })
      i++
      j++
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      rows.push({ type: 'del', text: a[i] })
      i++
    } else {
      rows.push({ type: 'add', text: b[j] })
      j++
    }
  }
  while (i < n) rows.push({ type: 'del', text: a[i++] })
  while (j < m) rows.push({ type: 'add', text: b[j++] })
  return rows
}
