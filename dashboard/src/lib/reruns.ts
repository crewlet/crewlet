/**
 * Which triggers ran more than once, over the turns a screen holds.
 *
 * A turn id names one RUN (`adr/0017`): a trigger that fails without reaching
 * outside the engine is redelivered and runs again, so one unit of work is
 * several rows, and unmarked they read as the company having done — or paid
 * for — the work twice. The turns list, the cost screen and a seat's cost tab
 * all mark a re-run, and they took this count three times over.
 *
 * A PLAIN RECORD rather than a `Map`, because each of those screens closes its
 * column list over it and passes it through `useShared` (`./share.ts`): a
 * count taken afresh on every poll is a new value each time, and a column list
 * that moves with it draws every row again for counts that did not move. The
 * walk that keeps a value whose content is unchanged reads arrays and object
 * literals only.
 *
 * AND ONLY THE TRIGGERS THAT RE-RAN, which is the only count a cell draws.
 * Holding every key, each new turn was a new key and so a new record, and the
 * turns list drew all two hundred rows for one turn that re-ran nothing.
 */

/** Runs per work key, for every key that ran more than once over `rows`. */
export function rerunCounts(rows: readonly { work_key?: string }[]): Record<string, number> {
  const all = new Map<string, number>();
  for (const row of rows) {
    // AN EMPTY WORK KEY IS THE ABSENCE OF AN IDENTITY — a trigger with nothing
    // to collapse on — so counting those together would report every such
    // turn as a re-run of every other.
    if (row.work_key) all.set(row.work_key, (all.get(row.work_key) ?? 0) + 1);
  }
  const counts: Record<string, number> = {};
  for (const [key, runs] of all) if (runs > 1) counts[key] = runs;
  return counts;
}

/**
 * How many runs `key`'s trigger got where it re-ran, and 0 where it did not.
 *
 * OWN KEYS ONLY: a record inherits `toString` and its kind, and a lookup that
 * read through to them would count a function.
 */
export function runsOf(counts: Record<string, number>, key: string | undefined): number {
  return key !== undefined && Object.hasOwn(counts, key) ? (counts[key] ?? 0) : 0;
}
