/**
 * A tidy layout for a forest of variable-size boxes, as a pure function.
 *
 * WHY IT EXISTS, AND WHY IT IS HAND-WRITTEN. The dashboard ships no layout
 * library: the ones that exist either lay out general graphs (and cost two
 * orders of magnitude more code and time for a tree) or assume every node is
 * the same size, and a unit card that stacks its seats inside it is as tall as
 * its membership. This is the Reingold-Tilford family, sized to that problem.
 *
 * THE RULES IT KEEPS.
 *
 * - SIZES ARE INPUT. Nothing here measures anything: the caller passes each
 *   box's width and height (measured in a browser, injected in a test), so the
 *   layout is deterministic and testable without a DOM.
 * - CHILDREN SIT AT THEIR PARENT'S BOTTOM PLUS `gapY`, not on a shared row per
 *   depth. Rows per depth would leave a short card's children hanging a tall
 *   neighbour's height below it.
 * - SEPARATION IS BY VERTICAL EXTENT, NOT BY DEPTH. Because depths no longer
 *   share a row, a tall card in one subtree can sit beside a grandchild in the
 *   next, and a contour indexed by depth would let the two overlap. Each
 *   subtree's left and right outline is kept as a function of y, and a
 *   neighbour is placed `gapX` clear of every y they share, including the band
 *   between a parent and its children where the connector runs.
 * - PARENTS ARE CENTRED over their first and last child.
 * - SIBLINGS ARE PACKED FROM BOTH SIDES AND AVERAGED. Packing only from the
 *   left pushes a small subtree between two large ones against its left
 *   neighbour. Both packings satisfy every separation, the constraints are
 *   linear, so their average does too; the result is also mirror symmetric,
 *   which is what reads as tidy.
 * - A ZERO-HEIGHT BOX STILL OCCUPIES ITS ROW, so two of them are never drawn
 *   on top of each other.
 *
 * Coordinates come back with the forest's top-left corner at the origin, and
 * boxes in pre-order. The outline merge costs time proportional to a subtree's
 * breakpoints, so a layout is roughly quadratic in the worst case; for the
 * few hundred boxes of an organization chart that is well under a millisecond
 * per hundred nodes, and it runs on a size change, never per frame.
 */

import type { Rect } from "./viewport.ts";

export interface TreeNode {
  id: string;
  width: number;
  height: number;
  children: readonly TreeNode[];
}

export interface Gaps {
  /** Horizontal space between neighbouring boxes that share any height. */
  gapX: number;
  /** Vertical space between a box's bottom and its children's tops. */
  gapY: number;
}

export interface Placed extends Rect {
  id: string;
  depth: number;
  parent: string | null;
}

export interface Layout {
  /** Every box, in pre-order. */
  nodes: Placed[];
  byId: Map<string, Placed>;
  /** The forest's extent: always at the origin. */
  bounds: Rect;
}

/** The height a zero-height box occupies in an outline: see the module doc. */
const MIN_OUTLINE_HEIGHT = 1;

/** One run of an outline: over `[top, bottom)` the extreme x is `value`. */
interface Segment {
  top: number;
  bottom: number;
  value: number;
}

/** Sorted, non-overlapping segments. */
type Outline = Segment[];

interface Subtree {
  node: TreeNode;
  kids: Subtree[];
  /** Each child's centre, relative to this box's centre. */
  offsets: number[];
  /** Relative to this box's centre and top. */
  left: Outline;
  right: Outline;
}

function shift(outline: Outline, dx: number, dy: number): Outline {
  return outline.map((s) => ({ top: s.top + dy, bottom: s.bottom + dy, value: s.value + dx }));
}

function mirror(outline: Outline): Outline {
  return outline.map((s) => ({ ...s, value: -s.value }));
}

/** Two outlines as one, taking `pick` of the two values wherever both have one. */
function merge(a: Outline, b: Outline, pick: (x: number, y: number) => number): Outline {
  const cuts = [...new Set([...a, ...b].flatMap((s) => [s.top, s.bottom]))].sort((x, y) => x - y);
  const out: Outline = [];
  let i = 0;
  let j = 0;
  for (let k = 0; k + 1 < cuts.length; k++) {
    const lo = cuts[k]!;
    const hi = cuts[k + 1]!;
    while (i < a.length && a[i]!.bottom <= lo) i++;
    while (j < b.length && b[j]!.bottom <= lo) j++;
    const va = i < a.length && a[i]!.top <= lo ? a[i]!.value : undefined;
    const vb = j < b.length && b[j]!.top <= lo ? b[j]!.value : undefined;
    const value = va !== undefined && vb !== undefined ? pick(va, vb) : (va ?? vb);
    if (value === undefined) continue;
    const last = out[out.length - 1];
    if (last && last.bottom === lo && last.value === value) last.bottom = hi;
    else out.push({ top: lo, bottom: hi, value });
  }
  return out;
}

/** How far right of `right` the outline `left` must start so they are `gap` apart. */
function separation(right: Outline, left: Outline, gap: number): number {
  let need = -Infinity;
  let i = 0;
  for (const s of left) {
    while (i < right.length && right[i]!.bottom <= s.top) i++;
    for (let k = i; k < right.length && right[k]!.top < s.bottom; k++) {
      need = Math.max(need, right[k]!.value - s.value);
    }
  }
  if (Number.isFinite(need)) return need + gap;
  // No shared height at all, which siblings (all starting at the same top)
  // never reach. Keeping the whole extents apart is safe and deterministic.
  const maxRight = right.reduce((m, s) => Math.max(m, s.value), -Infinity);
  const minLeft = left.reduce((m, s) => Math.min(m, s.value), Infinity);
  return maxRight - minLeft + gap;
}

/** Siblings packed left to right, each as close to the previous ones as its outline allows. */
function packFromLeft(kids: readonly { left: Outline; right: Outline }[], gap: number): number[] {
  const positions = [0];
  let reach = kids[0]!.right;
  for (let i = 1; i < kids.length; i++) {
    const at = separation(reach, kids[i]!.left, gap);
    positions.push(at);
    reach = merge(reach, shift(kids[i]!.right, at, 0), Math.max);
  }
  return positions;
}

/** Sibling centres: the average of packing from each side, centred on zero. */
function pack(kids: readonly Subtree[], gap: number): number[] {
  const fromLeft = packFromLeft(kids, gap);
  // Packing from the right is packing the mirror image from the left.
  const mirrored = [...kids]
    .reverse()
    .map((k) => ({ left: mirror(k.right), right: mirror(k.left) }));
  const fromRight = packFromLeft(mirrored, gap)
    .map((p) => -p)
    .reverse();
  const first = fromRight[0]!;
  const averaged = fromLeft.map((p, i) => (p + fromRight[i]! - first) / 2);
  const centre = (averaged[0]! + averaged[averaged.length - 1]!) / 2;
  return averaged.map((p) => p - centre);
}

function build(node: TreeNode, gaps: Gaps): Subtree {
  const kids = node.children.map((child) => build(child, gaps));
  const half = node.width / 2;
  const tall = Math.max(node.height, MIN_OUTLINE_HEIGHT);
  let left: Outline = [{ top: 0, bottom: tall, value: -half }];
  let right: Outline = [{ top: 0, bottom: tall, value: half }];
  if (kids.length === 0) return { node, kids, offsets: [], left, right };

  const offsets = pack(kids, gaps.gapX);
  const childTop = node.height + gaps.gapY;
  // THE CONNECTOR BAND: from this box's bottom to its children's tops, as wide
  // as the box or the bar joining the outermost children, whichever is wider.
  if (childTop > node.height) {
    const band = (value: number): Outline => [{ top: node.height, bottom: childTop, value }];
    left = merge(left, band(Math.min(-half, offsets[0]!)), Math.min);
    right = merge(right, band(Math.max(half, offsets[offsets.length - 1]!)), Math.max);
  }
  kids.forEach((kid, i) => {
    left = merge(left, shift(kid.left, offsets[i]!, childTop), Math.min);
    right = merge(right, shift(kid.right, offsets[i]!, childTop), Math.max);
  });
  return { node, kids, offsets, left, right };
}

function validate(roots: readonly TreeNode[], gaps: Gaps): void {
  const size = (v: number) => Number.isFinite(v) && v >= 0;
  if (!size(gaps.gapX) || !size(gaps.gapY)) {
    throw new RangeError(
      `tidytree: gaps must be finite and non-negative, got ${JSON.stringify(gaps)}`,
    );
  }
  const seen = new Set<string>();
  const visit = (node: TreeNode) => {
    if (seen.has(node.id)) {
      throw new RangeError(
        `tidytree: the id "${node.id}" appears twice; every box needs its own id`,
      );
    }
    seen.add(node.id);
    if (!size(node.width) || !size(node.height)) {
      throw new RangeError(
        `tidytree: "${node.id}" is ${node.width} by ${node.height}; a size must be finite and non-negative`,
      );
    }
    node.children.forEach(visit);
  };
  roots.forEach(visit);
}

/** Lays out a forest: see the module doc for the rules. */
export function layoutForest(roots: readonly TreeNode[], gaps: Gaps): Layout {
  validate(roots, gaps);
  const nodes: Placed[] = [];
  if (roots.length === 0) {
    return { nodes, byId: new Map(), bounds: { x: 0, y: 0, width: 0, height: 0 } };
  }
  const trees = roots.map((root) => build(root, gaps));
  const centres = pack(trees, gaps.gapX);

  const place = (
    tree: Subtree,
    centre: number,
    top: number,
    depth: number,
    parent: string | null,
  ) => {
    const { node } = tree;
    nodes.push({
      id: node.id,
      x: centre - node.width / 2,
      y: top,
      width: node.width,
      height: node.height,
      depth,
      parent,
    });
    const childTop = top + node.height + gaps.gapY;
    tree.kids.forEach((kid, i) =>
      place(kid, centre + tree.offsets[i]!, childTop, depth + 1, node.id),
    );
  };
  trees.forEach((tree, i) => place(tree, centres[i]!, 0, 0, null));

  // A loop rather than `Math.min(...xs)`: spreading a large array into
  // arguments overflows the call stack long before a layout is slow.
  let minX = Infinity;
  for (const n of nodes) minX = Math.min(minX, n.x);
  let maxX = -Infinity;
  let maxY = 0;
  for (const n of nodes) {
    n.x -= minX;
    maxX = Math.max(maxX, n.x + n.width);
    maxY = Math.max(maxY, n.y + n.height);
  }
  return {
    nodes,
    byId: new Map(nodes.map((n) => [n.id, n])),
    bounds: { x: 0, y: 0, width: maxX, height: maxY },
  };
}
