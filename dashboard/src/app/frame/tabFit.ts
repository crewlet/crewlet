/**
 * Which tabs of a strip fold into its "More" — the arithmetic, apart from any
 * DOM it is measured in, so it can be held to its rules directly.
 *
 * TWO STRIPS FOLD, AND BY ONE RULE: a workspace's section tabs
 * (`SectionTabs` in `app/header/PageHeader.tsx`) and an object's own tabs
 * (`ObjectTabs`, a seat's Overview … Settings). Each measures in its own way —
 * the section strip keeps its folded links in the document to measure them,
 * the object strip measures a twin of the kit's row — and both hand the widths
 * here, so a tab leaves either strip by the same order and the one the reader
 * is on stays in both.
 */

/**
 * Which tabs fold into "More", from their widths — the arithmetic, apart from
 * the DOM it is measured in, so it can be held to its rules directly.
 *
 * `widths` are the tabs' own, in order; `space` is the strip's inner width,
 * `gap` the space between two items and `more` the "More" trigger's width.
 * Returns the INDEXES that fold. Everything fits → none. Otherwise the strip
 * keeps the tab the reader is on (`current`) and, before it, as long a run of
 * the others FROM THE START as fits beside it and the trigger — a run, never a
 * pick of whichever narrower tabs would squeeze in further along, because a
 * strip whose order changes with its width is one a reader cannot learn.
 */
export function foldTabs({
  widths,
  current,
  space,
  gap,
  more,
}: {
  widths: readonly number[];
  current: number;
  space: number;
  gap: number;
  more: number;
}): number[] {
  const all = widths.reduce((sum, w) => sum + w, 0) + gap * Math.max(0, widths.length - 1);
  if (all <= space) return [];
  let used = more + (current >= 0 ? (widths[current] ?? 0) + gap : 0);
  const kept = new Set<number>(current >= 0 ? [current] : []);
  for (let i = 0; i < widths.length; i++) {
    if (i === current) continue;
    const next = used + (widths[i] ?? 0) + gap;
    if (next > space) break;
    used = next;
    kept.add(i);
  }
  return widths.map((_, i) => i).filter((i) => !kept.has(i));
}
