// @vitest-environment node
/**
 * The tidy tree's promises, over hand-built cases and random forests.
 *
 * The properties are what a reader of the chart relies on: no card is drawn
 * over another, a parent sits over its children, children hang from their
 * parent's bottom, the order given is the order drawn, and the same input
 * always draws the same chart. The random forests come from a seeded PRNG in
 * this file, and a failure names its seed so the case can be replayed.
 */

import { describe, expect, test } from "vitest";
import { layoutForest, type Gaps, type Layout, type Placed, type TreeNode } from "./tidytree.ts";

const EPS = 1e-6;

/** mulberry32. */
function prng(seed: number): () => number {
  let s = seed >>> 0;
  return () => {
    s = (s + 0x6d2b79f5) >>> 0;
    let t = s;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function box(id: string, width: number, height: number, children: TreeNode[] = []): TreeNode {
  return { id, width, height, children };
}

/** A random forest: 1 to 3 roots, fan-out shrinking with depth, sizes like real cards. */
function forest(seed: number): { roots: TreeNode[]; gaps: Gaps } {
  const rand = prng(seed);
  const int = (lo: number, hi: number) => lo + Math.floor(rand() * (hi - lo + 1));
  let next = 0;
  const grow = (depth: number): TreeNode => {
    const fanout = depth >= 5 ? 0 : int(0, Math.max(0, 4 - depth));
    return box(
      `n${next++}`,
      int(40, 320),
      // Unit cards stack their seats, so heights vary far more than widths.
      int(1, 400),
      Array.from({ length: fanout }, () => grow(depth + 1)),
    );
  };
  const roots = Array.from({ length: int(1, 3) }, () => grow(0));
  return { roots, gaps: { gapX: int(0, 40), gapY: int(0, 60) } };
}

function all(roots: readonly TreeNode[]): TreeNode[] {
  return roots.flatMap((r) => [r, ...all(r.children)]);
}

function mirrorTree(node: TreeNode): TreeNode {
  return { ...node, children: [...node.children].reverse().map(mirrorTree) };
}

/** Every broken promise in one layout, as sentences. */
function violations(roots: TreeNode[], gaps: Gaps, layout: Layout): string[] {
  const out: string[] = [];
  const input = all(roots);
  if (layout.nodes.length !== input.length)
    out.push(`${layout.nodes.length} placed of ${input.length}`);
  for (const node of input) {
    const p = layout.byId.get(node.id);
    if (!p) {
      out.push(`${node.id} was not placed`);
      continue;
    }
    if (p.width !== node.width || p.height !== node.height) out.push(`${node.id} changed size`);
    if (node.children.length > 0) {
      const kids = node.children.map((c) => layout.byId.get(c.id)!);
      const centre = (n: Placed) => n.x + n.width / 2;
      const middle = (centre(kids[0]!) + centre(kids[kids.length - 1]!)) / 2;
      if (Math.abs(centre(p) - middle) > EPS)
        out.push(`${node.id} is not centred over its children`);
      for (const kid of kids) {
        if (Math.abs(kid.y - (p.y + p.height + gaps.gapY)) > EPS) {
          out.push(`${kid.id} does not hang from ${node.id}'s bottom`);
        }
        if (kid.parent !== node.id || kid.depth !== p.depth + 1)
          out.push(`${kid.id} lost its parent`);
      }
      for (let i = 1; i < kids.length; i++) {
        if (centre(kids[i]!) <= centre(kids[i - 1]!)) out.push(`${kids[i]!.id} is out of order`);
      }
    }
  }
  for (const root of roots) {
    if (layout.byId.get(root.id)!.y !== 0) out.push(`root ${root.id} is not at the top`);
  }
  // No two boxes that share any height are closer than gapX.
  const placed = layout.nodes;
  for (let i = 0; i < placed.length; i++) {
    for (let j = i + 1; j < placed.length; j++) {
      const a = placed[i]!;
      const b = placed[j]!;
      const shared = Math.min(a.y + a.height, b.y + b.height) - Math.max(a.y, b.y);
      if (shared <= EPS) continue;
      const apart = Math.max(b.x - (a.x + a.width), a.x - (b.x + b.width));
      if (apart < gaps.gapX - EPS) out.push(`${a.id} and ${b.id} are ${apart} apart`);
    }
  }
  const { bounds } = layout;
  const minX = Math.min(...placed.map((n) => n.x));
  if (Math.abs(minX) > EPS) out.push(`the forest starts at x ${minX}`);
  for (const n of placed) {
    if (n.x + n.width > bounds.width + EPS || n.y + n.height > bounds.height + EPS) {
      out.push(`${n.id} is outside the bounds`);
    }
  }
  return out;
}

describe("hand-built cases", () => {
  test("two children under a parent, parent centred, children at its bottom plus the gap", () => {
    const layout = layoutForest([box("r", 100, 50, [box("a", 100, 50), box("b", 100, 50)])], {
      gapX: 20,
      gapY: 10,
    });
    expect(layout.byId.get("a")).toMatchObject({ x: 0, y: 60 });
    expect(layout.byId.get("b")).toMatchObject({ x: 120, y: 60 });
    expect(layout.byId.get("r")).toMatchObject({ x: 60, y: 0 });
    expect(layout.bounds).toEqual({ x: 0, y: 0, width: 220, height: 110 });
  });

  test("a tall card pushes a neighbour's wide grandchild clear, which a per-depth contour would not", () => {
    // A is a unit card 300 tall. B is short, and its child C (at depth two)
    // sits beside A's lower half. Compared only depth by depth, C would be
    // drawn over A.
    const gaps = { gapX: 10, gapY: 10 };
    const layout = layoutForest(
      [box("root", 60, 20, [box("A", 100, 300), box("B", 100, 50, [box("C", 300, 50)])])],
      gaps,
    );
    const a = layout.byId.get("A")!;
    const c = layout.byId.get("C")!;
    expect(c.x).toBeGreaterThanOrEqual(a.x + a.width + gaps.gapX - EPS);
  });

  test("a small subtree between two large ones is spread evenly, not packed against its left", () => {
    const wide = (id: string) => box(id, 40, 40, [box(`${id}1`, 200, 40), box(`${id}2`, 200, 40)]);
    const layout = layoutForest([box("r", 40, 40, [wide("L"), box("m", 40, 40), wide("R")])], {
      gapX: 20,
      gapY: 20,
    });
    const centre = (id: string) => layout.byId.get(id)!.x + layout.byId.get(id)!.width / 2;
    expect(centre("m") - centre("L")).toBeCloseTo(centre("R") - centre("m"));
  });

  test("a neighbour is kept clear of the connector bar between a parent and its children", () => {
    // P is short with two children far apart, so the bar joining them runs
    // well past P's own edges through the gap below it. Q is just tall enough
    // to reach into that gap and no further.
    const gaps = { gapX: 10, gapY: 60 };
    const layout = layoutForest(
      [
        box("root", 40, 20, [
          box("P", 40, 20, [box("P1", 200, 40), box("P2", 200, 40)]),
          box("Q", 40, 50),
        ]),
      ],
      gaps,
    );
    const p2 = layout.byId.get("P2")!;
    const q = layout.byId.get("Q")!;
    expect(q.x).toBeGreaterThanOrEqual(p2.x + p2.width / 2 + gaps.gapX - EPS);
  });

  test("zero-height boxes in one row are still kept apart", () => {
    const layout = layoutForest([box("a", 50, 0), box("b", 50, 0)], { gapX: 8, gapY: 0 });
    expect(layout.byId.get("b")!.x - (layout.byId.get("a")!.x + 50)).toBeCloseTo(8);

    // The case that needs it: zero-height parents whose narrow children would
    // otherwise be the only thing the outline knows about.
    const parents = layoutForest(
      [box("p", 50, 0, [box("p1", 10, 10)]), box("q", 50, 0, [box("q1", 10, 10)])],
      { gapX: 8, gapY: 0 },
    );
    expect(parents.byId.get("q")!.x - (parents.byId.get("p")!.x + 50)).toBeGreaterThanOrEqual(
      8 - EPS,
    );
  });

  test("an empty forest has empty bounds", () => {
    expect(layoutForest([], { gapX: 1, gapY: 1 })).toEqual({
      nodes: [],
      byId: new Map(),
      bounds: { x: 0, y: 0, width: 0, height: 0 },
    });
  });

  test("invalid input is refused with the id that caused it", () => {
    expect(() => layoutForest([box("x", 10, 10), box("x", 10, 10)], { gapX: 0, gapY: 0 })).toThrow(
      /"x" appears twice/,
    );
    expect(() => layoutForest([box("nan", Number.NaN, 10)], { gapX: 0, gapY: 0 })).toThrow(/"nan"/);
    expect(() => layoutForest([box("a", 10, 10)], { gapX: -1, gapY: 0 })).toThrow(/gaps/);
  });
});

describe("properties over random forests", () => {
  const SEEDS = Array.from({ length: 250 }, (_, i) => 0x5eed + i);

  test("no overlap, centred parents, hanging children, input order, bounds at the origin", () => {
    const failures: string[] = [];
    for (const seed of SEEDS) {
      const { roots, gaps } = forest(seed);
      const found = violations(roots, gaps, layoutForest(roots, gaps));
      if (found.length) failures.push(`seed ${seed}: ${found.slice(0, 3).join("; ")}`);
    }
    expect(failures).toEqual([]);
  });

  test("the same input always draws the same chart", () => {
    const failures: string[] = [];
    for (const seed of SEEDS.slice(0, 50)) {
      const { roots, gaps } = forest(seed);
      if (
        JSON.stringify(layoutForest(roots, gaps).nodes) !==
        JSON.stringify(layoutForest(roots, gaps).nodes)
      ) {
        failures.push(`seed ${seed}`);
      }
    }
    expect(failures).toEqual([]);
  });

  test("a mirrored forest draws as the mirror image", () => {
    const failures: string[] = [];
    for (const seed of SEEDS.slice(0, 100)) {
      const { roots, gaps } = forest(seed);
      const plain = layoutForest(roots, gaps);
      const flipped = layoutForest([...roots].reverse().map(mirrorTree), gaps);
      const width = plain.bounds.width;
      for (const n of plain.nodes) {
        const m = flipped.byId.get(n.id)!;
        if (Math.abs(m.x - (width - n.x - n.width)) > 1e-6 || m.y !== n.y) {
          failures.push(`seed ${seed}: ${n.id}`);
          break;
        }
      }
    }
    expect(failures).toEqual([]);
  });
});
