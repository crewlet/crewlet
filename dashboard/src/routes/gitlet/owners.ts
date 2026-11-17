/**
 * Path owners' asks in words, shared by the change rows' reasons and a
 * change's timeline, so both say the same paths the same way.
 */

/** Names in a sentence, with how many more: "e2e", "e2e and lint", "e2e, lint and 2 more". */
export function namesSaid(names: readonly string[], more = 0): string {
  const all = more > 0 ? [...names, `${more} more`] : [...names];
  if (all.length <= 1) return all[0] ?? "";
  return `${all.slice(0, -1).join(", ")} and ${all[all.length - 1]}`;
}

/** A path owner's ask, as far as its paths go. */
interface OwnerAsk {
  readonly owned_paths?: readonly string[] | undefined;
  readonly owned_paths_omitted?: number | undefined;
}

/** The paths an owner's ask is for, and how many more; none where the facts name none. */
export function ownedPathList(ask: OwnerAsk): { paths: readonly string[]; more: number } {
  const paths = ask.owned_paths ?? [];
  return { paths, more: paths.length ? (ask.owned_paths_omitted ?? 0) : 0 };
}

/** The paths an owner's ask is for, in a sentence; empty where the facts name none. */
export function ownedPaths(ask: OwnerAsk): string {
  const { paths, more } = ownedPathList(ask);
  return namesSaid(paths, more);
}
