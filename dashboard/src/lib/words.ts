/**
 * Saying a list in a sentence: "a", "a and b", "a, b and c".
 *
 * ONE JOINER, because the dashboard had four private copies of it (the retention
 * banner's, the inbox's quiet band's, a work item's change row's and an
 * executor's audience line), two with the serial comma and two without, so the same
 * list read differently on two screens and differently again from the CLI,
 * which says a list through the engine's `internal/words`. The default is that
 * package's rule, so a list of logs or nodes reads alike on both surfaces.
 *
 * `serialComma` is for clauses that carry their own "and" ("the company's and
 * every seat's token budget"): without the comma before the last, the last two
 * run together and read as one item.
 *
 * NOT `Intl.ListFormat`, which joins in the BROWSER's locale, so a reader on a
 * Japanese one would get "、" between words that are themselves English.
 */
export function andList(
  items: readonly string[],
  { serialComma = false }: { serialComma?: boolean } = {},
): string {
  if (items.length <= 1) return items[0] ?? "";
  if (items.length === 2) return `${items[0]} and ${items[1]}`;
  const head = items.slice(0, -1).join(", ");
  return `${head}${serialComma ? "," : ""} and ${items.at(-1) ?? ""}`;
}
