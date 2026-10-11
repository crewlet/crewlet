/**
 * How projects are ordered against what a reader typed: an exact key, then
 * keys that start with it, then a name with a word that starts with it, then
 * keys and names that contain it. Ties go by key, and case does not matter.
 *
 * ONE ORDER FOR THE SWITCHER AND ⌘K, so the project one puts first is the one
 * the other puts first. A word starts after a space, a dash, an underscore, a
 * slash or a middle dot, which is where the kit draws a name's matched
 * letters bold, so the row ranked for a word is the row with that word lit.
 */

/** What separates the words of a project's name. */
const WORD_BREAK = /[\s\-_/·]+/;

/** A project as ranking reads it: its key and its name. */
export interface Rankable {
  key: string;
  name: string;
}

/**
 * How well a project matches, lower is better: 0 for an exact key down to 4
 * for a name that contains the query, or -1 for no match. An empty query
 * ranks every project 0.
 */
export function projectRank(project: Rankable, query: string): number {
  const q = query.trim().toLowerCase();
  if (q === "") return 0;
  const key = project.key.toLowerCase();
  const name = project.name.toLowerCase();
  if (key === q) return 0;
  if (key.startsWith(q)) return 1;
  if (name.split(WORD_BREAK).some((word) => word.startsWith(q))) return 2;
  if (key.includes(q)) return 3;
  if (name.includes(q)) return 4;
  return -1;
}

/**
 * The projects that match, best first, ties by key. An empty query keeps the
 * order they came in.
 *
 * `matched` is for rows the ENGINE already matched (a listing asked with
 * `q`), which matches a project's purpose as well: one that matches only
 * there is kept, after every row that matches by key or name, rather than
 * dropped for a reason the reader cannot see.
 */
export function rankProjects<T extends Rankable>(
  projects: readonly T[],
  query: string,
  { matched = false }: { matched?: boolean } = {},
): T[] {
  if (query.trim() === "") return [...projects];
  const unmatched = 5;
  return projects
    .map((project) => {
      const rank = projectRank(project, query);
      return [rank < 0 && matched ? unmatched : rank, project] as const;
    })
    .filter(([rank]) => rank >= 0)
    .sort(([ra, a], [rb, b]) => ra - rb || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0))
    .map(([, project]) => project);
}
