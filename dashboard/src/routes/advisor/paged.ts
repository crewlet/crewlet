/**
 * The rows already shown, then a page's rows not among them. Until the next
 * page answers, the question still holds the page before, whose rows are
 * already held: drawn twice, they would repeat on screen and share a key.
 */
export function pagedOn<T>(
  held: readonly T[],
  page: readonly T[] | undefined,
  key: (row: T) => string,
): T[] {
  const seen = new Set(held.map(key));
  return [...held, ...(page ?? []).filter((row) => !seen.has(key(row)))];
}
