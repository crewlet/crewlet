/**
 * Saying a list in a sentence: "a", "a and b", "a, b and c".
 *
 * ONE JOINER, because the dashboard had four private copies of it (the retention
 * banner's, the inbox's quiet band's, a work item's change row's and an
 * executor's audience line), two with the serial comma and two without, so the same
 * list read differently on two screens and differently again from the harness,
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
  return joined(items, "and", serialComma);
}

/** The same list as a choice among its items, "a", "a or b", "a, b or c", by [andList]'s rule. */
export function orList(
  items: readonly string[],
  { serialComma = false }: { serialComma?: boolean } = {},
): string {
  return joined(items, "or", serialComma);
}

function joined(items: readonly string[], word: "and" | "or", serialComma: boolean): string {
  if (items.length <= 1) return items[0] ?? "";
  if (items.length === 2) return `${items[0]} ${word} ${items[1]}`;
  const head = items.slice(0, -1).join(", ");
  return `${head}${serialComma ? "," : ""} ${word} ${items.at(-1) ?? ""}`;
}

/**
 * A line about its actor, said under a column (or a heading) that names them:
 * the engine opens its summaries with the actor ("PM finished reflecting"), and
 * beside an Actor column the name was said twice, pushing the words that
 * mattered past the cut.
 *
 * Only from the FRONT, and only as a whole word: an actor named mid-sentence is
 * not the actor talking about itself, and a line that is nothing but the name
 * is left alone rather than emptied. The possessive the engine writes for a
 * seat's executor ("PM's executor launched") reads "Its executor launched".
 */
export function withoutActor(summary: string, actor: string): string {
  if (!actor) return summary;
  let line = summary;
  if (line.startsWith(`${actor} `)) line = line.slice(actor.length).trimStart();
  const owned = [`${actor}\u2019s `, `${actor}'s `].find((form) => line.startsWith(form));
  if (owned) line = `Its ${line.slice(owned.length)}`;
  return line.trim() ? line : summary;
}
