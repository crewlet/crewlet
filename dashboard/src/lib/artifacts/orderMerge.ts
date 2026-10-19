/**
 * The order of a list two people edited from one ancestor (a deck's slides, a
 * doc's tabs), merged: the moves one side made, the items each side added put
 * back after the item they followed there, and the items deleted left out.
 *
 * WHAT COUNTS AS A MOVE is the order of the items all three still hold: an
 * item one side added or deleted is not a reorder of the others. When both
 * sides moved items differently, the newer version's order stands and the
 * clash is reported, for the person to keep theirs or take the newer one.
 *
 * PURE, and in `lib/` for the reason the rest of this directory gives.
 */

export interface OrderMerge {
  order: string[];
  /** Both sides moved items, differently: the newer version's order was kept. */
  conflict: boolean;
}

/**
 * Merge `mine` (started from `base`) onto `live`, the newer version, leaving
 * out every id in `deleted` (what this edit deleted and the newer version did
 * not change).
 */
export function mergeOrder(
  base: readonly string[],
  mine: readonly string[],
  live: readonly string[],
  deleted: ReadonlySet<string>,
): OrderMerge {
  const same = (a: readonly string[], b: readonly string[]) => a.join("\n") === b.join("\n");
  const kept = (o: readonly string[]) =>
    o.filter((id) => base.includes(id) && live.includes(id) && mine.includes(id));
  const mineMoved = !same(kept(mine), kept(base));
  const liveMoved = !same(kept(live), kept(base));
  // THE ORDER TO START FROM is the side that moved items (the newer version
  // when neither or both did); the other side's additions are put back after
  // the item they followed there.
  const fromMine = mineMoved && (!liveMoved || same(kept(mine), kept(live)));
  const start = (fromMine ? mine : live).filter(
    (id) => !deleted.has(id) && (live.includes(id) || !base.includes(id)),
  );
  const order = [...start];
  const other = fromMine ? live : mine;
  other.forEach((id, i) => {
    if (order.includes(id) || deleted.has(id) || base.includes(id)) return;
    order.splice(placeAfter(order, other.slice(0, i)), 0, id);
  });
  return { order, conflict: mineMoved && liveMoved && !fromMine };
}

/**
 * Where an item goes back into `order`: just after the nearest of `before`
 * (the items it followed, in their order) that `order` holds, else first.
 */
export function placeAfter(order: readonly string[], before: readonly string[]): number {
  const anchor = [...before].reverse().find((n) => order.includes(n));
  return anchor === undefined ? 0 : order.indexOf(anchor) + 1;
}
