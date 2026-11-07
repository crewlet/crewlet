/**
 * A grid head over an order the engine reads in one direction.
 *
 * The kit's head steps ascending, descending, then back to the default, but a
 * listing the engine orders offers a fixed set (titles A to Z, the newest
 * first) and no reverse of any. Passed straight on, a press would ask for an
 * order nobody can answer, so it is read here against what the engine reads.
 */

const columnOf = (sort: string): string => (sort.startsWith("-") ? sort.slice(1) : sort);

/**
 * What a press on a head asks for, as the grid's `sort` (`""` for the default):
 * the one order its column has, and the default again for the head already in
 * force. `asked` is what the kit stepped to, `orders` every order a head can
 * ask for as `key` or `-key`, `inForce` the order the rows are in now.
 */
export function oneWayHead(
  asked: string,
  orders: readonly string[],
  inForce: string,
  fallback: string,
): string {
  const order = orders.find((candidate) => columnOf(candidate) === columnOf(asked));
  if (asked === "" || order === undefined || order === inForce || order === fallback) return "";
  return order;
}

/**
 * A grid's order props over a listing the engine orders: the head asks, and the
 * rows stay as answered. Sorted here, a head would order what is loaded under
 * an arrow that reads as the whole. `onOrder` takes the order chosen, `""` for
 * the default, so an address holds the reader's choice and nothing else.
 */
export function engineOrder(
  inForce: string,
  orders: readonly string[],
  fallback: string,
  onOrder: (order: string) => void,
) {
  return {
    serverSorted: true,
    defaultSort: fallback,
    sort: inForce === fallback ? "" : inForce,
    onSortChange: (asked: string) => onOrder(oneWayHead(asked, orders, inForce, fallback)),
  };
}
