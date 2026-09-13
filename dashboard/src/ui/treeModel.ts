/**
 * The visible order of a collapsible tree, as pure functions.
 *
 * WHY IT EXISTS. Two views of one hierarchy need the same answers: a canvas
 * drawn as a `tree` and an outline drawn as a `treegrid`. They are different
 * ARIA patterns (a treegrid has cells and row actions a tree must not contain)
 * so they cannot share one keyboard hook, but "what is the next visible row",
 * "where does Left go" and "which row does typing `eng` land on" must not have
 * two answers. The model is that shared half; each view owns its roles, its
 * focus and its keys.
 *
 * THE RULES IT KEEPS.
 *
 * - VISIBLE ORDER IS PRE-ORDER, skipping the descendants of collapsed nodes.
 * - RIGHT EXPANDS A CLOSED NODE, or moves to the first child of an open one;
 *   LEFT COLLAPSES AN OPEN NODE, or moves to the parent. A leaf only moves.
 * - TYPE-AHEAD matches the start of a visible label, case-insensitively, from
 *   the row after the current one and wrapping. Typing one character again
 *   cycles through the rows that start with it. A word never STARTS with `+`,
 *   `-` or `0`: those are a canvas's zoom and fit keys, and a press that means
 *   "fit" one moment and "find the row starting with zero" the next is a press
 *   nobody can predict. Inside a word they are ordinary characters, so
 *   "Q3 2026" is still typed whole.
 * - NOTHING IS MUTATED. Expansion is a set the view owns and passes in.
 */

export interface TreeInput {
  id: string;
  /** What type-ahead matches against: the text a person reads on the row. */
  label: string;
  children?: readonly TreeInput[];
}

export interface TreeModel {
  roots: readonly string[];
  children: ReadonlyMap<string, readonly string[]>;
  parent: ReadonlyMap<string, string | null>;
  label: ReadonlyMap<string, string>;
  /** Zero for a root. */
  depth: ReadonlyMap<string, number>;
  /** Every node in pre-order, collapsed or not. */
  order: readonly string[];
}

/** Builds the model. Ids must be unique across the whole forest. */
export function createTree(forest: readonly TreeInput[]): TreeModel {
  const children = new Map<string, string[]>();
  const parent = new Map<string, string | null>();
  const label = new Map<string, string>();
  const depth = new Map<string, number>();
  const order: string[] = [];
  const visit = (node: TreeInput, up: string | null, level: number) => {
    if (parent.has(node.id)) {
      throw new RangeError(
        `treeModel: the id "${node.id}" appears twice; every row needs its own id`,
      );
    }
    parent.set(node.id, up);
    label.set(node.id, node.label);
    depth.set(node.id, level);
    order.push(node.id);
    const kids = node.children ?? [];
    children.set(
      node.id,
      kids.map((k) => k.id),
    );
    kids.forEach((k) => visit(k, node.id, level + 1));
  };
  forest.forEach((root) => visit(root, null, 0));
  return { roots: forest.map((r) => r.id), children, parent, label, depth, order };
}

const kids = (tree: TreeModel, id: string): readonly string[] => tree.children.get(id) ?? [];

function siblings(tree: TreeModel, id: string): readonly string[] {
  const up = tree.parent.get(id);
  return up == null ? tree.roots : kids(tree, up);
}

export function isExpandable(tree: TreeModel, id: string): boolean {
  return kids(tree, id).length > 0;
}

/** `aria-level`: one for a root. */
export function level(tree: TreeModel, id: string): number {
  return (tree.depth.get(id) ?? 0) + 1;
}

/** `aria-setsize`: how many siblings, itself included. */
export function setSize(tree: TreeModel, id: string): number {
  return siblings(tree, id).length;
}

/** `aria-posinset`: one-based position among its siblings. */
export function posInSet(tree: TreeModel, id: string): number {
  return siblings(tree, id).indexOf(id) + 1;
}

export function parentOf(tree: TreeModel, id: string): string | null {
  return tree.parent.get(id) ?? null;
}

export function firstChildOf(tree: TreeModel, id: string): string | null {
  return kids(tree, id)[0] ?? null;
}

/** Its ancestors, the root first. */
export function ancestors(tree: TreeModel, id: string): string[] {
  const out: string[] = [];
  for (let up = tree.parent.get(id) ?? null; up !== null; up = tree.parent.get(up) ?? null) {
    out.unshift(up);
  }
  return out;
}

/** The expanded set with every ancestor of `id` opened, so the node is visible. */
export function expandTo(tree: TreeModel, expanded: ReadonlySet<string>, id: string): Set<string> {
  return new Set([...expanded, ...ancestors(tree, id)]);
}

/** Every node that can be expanded: what "Expand all" sets. */
export function allExpandable(tree: TreeModel): Set<string> {
  return new Set(tree.order.filter((id) => isExpandable(tree, id)));
}

const open = (tree: TreeModel, expanded: ReadonlySet<string>, id: string) =>
  expanded.has(id) && isExpandable(tree, id);

/** Every visible node, in order. */
export function visible(tree: TreeModel, expanded: ReadonlySet<string>): string[] {
  const out: string[] = [];
  const walk = (ids: readonly string[]) => {
    for (const id of ids) {
      out.push(id);
      if (open(tree, expanded, id)) walk(kids(tree, id));
    }
  };
  walk(tree.roots);
  return out;
}

export function firstVisible(tree: TreeModel): string | null {
  return tree.roots[0] ?? null;
}

export function lastVisible(tree: TreeModel, expanded: ReadonlySet<string>): string | null {
  let last = tree.roots[tree.roots.length - 1] ?? null;
  while (last !== null && open(tree, expanded, last)) {
    const children = kids(tree, last);
    last = children[children.length - 1]!;
  }
  return last;
}

/** The next visible node after `id`, or null at the end. */
export function nextVisible(
  tree: TreeModel,
  expanded: ReadonlySet<string>,
  id: string,
): string | null {
  if (open(tree, expanded, id)) return kids(tree, id)[0]!;
  for (let at: string | null = id; at !== null; at = parentOf(tree, at)) {
    const row = siblings(tree, at);
    const next = row[row.indexOf(at) + 1];
    if (next !== undefined) return next;
  }
  return null;
}

/** The visible node before `id`, or null at the start. */
export function previousVisible(
  tree: TreeModel,
  expanded: ReadonlySet<string>,
  id: string,
): string | null {
  const row = siblings(tree, id);
  const at = row.indexOf(id);
  if (at <= 0) return parentOf(tree, id);
  let last = row[at - 1]!;
  while (open(tree, expanded, last)) {
    const children = kids(tree, last);
    last = children[children.length - 1]!;
  }
  return last;
}

/** What Right means on `id`: expand it, move to its first child, or nothing. */
export function expandOrDescend(
  tree: TreeModel,
  expanded: ReadonlySet<string>,
  id: string,
): { expand: string } | { focus: string } | null {
  if (!isExpandable(tree, id)) return null;
  return expanded.has(id) ? { focus: kids(tree, id)[0]! } : { expand: id };
}

/** What Left means on `id`: collapse it, move to its parent, or nothing. */
export function collapseOrAscend(
  tree: TreeModel,
  expanded: ReadonlySet<string>,
  id: string,
): { collapse: string } | { focus: string } | null {
  if (open(tree, expanded, id)) return { collapse: id };
  const up = parentOf(tree, id);
  return up === null ? null : { focus: up };
}

/** How long a pause ends a type-ahead word: the WAI-ARIA practices' own examples use half a second. */
export const TYPE_AHEAD_RESET_MS = 500;

/** Keys a type-ahead word never starts with: a canvas's zoom and fit keys. */
const RESERVED = new Set(["+", "-", "0"]);

/** Whether a key press is a type-ahead character rather than a command. */
export function isTypeAheadKey(
  e: { key: string; ctrlKey?: boolean; metaKey?: boolean; altKey?: boolean },
  buffer = "",
): boolean {
  if (e.ctrlKey || e.metaKey || e.altKey) return false;
  if ([...e.key].length !== 1) return false;
  if (buffer !== "") return true;
  // A leading space is activation (Space on a row), not the start of a word.
  return e.key !== " " && !RESERVED.has(e.key);
}

export interface TypeAheadState {
  text: string;
  at: number;
}

/** The buffer after a type-ahead key at time `now` (milliseconds). */
export function typeAheadBuffer(state: TypeAheadState, key: string, now: number): TypeAheadState {
  const fresh = now - state.at > TYPE_AHEAD_RESET_MS;
  return { text: (fresh ? "" : state.text) + key, at: now };
}

/** The visible node whose label starts with `text`, searching after `from` and wrapping. */
export function typeAhead(
  tree: TreeModel,
  expanded: ReadonlySet<string>,
  from: string | null,
  text: string,
): string | null {
  if (text === "") return null;
  const rows = visible(tree, expanded);
  if (rows.length === 0) return null;
  const lower = text.toLocaleLowerCase();
  // "sss" is somebody pressing S three times to reach the third S row.
  const cycling = [...lower].every((c) => c === lower[0]);
  const needle = cycling ? lower[0]! : lower;
  const at = from === null ? -1 : rows.indexOf(from);
  // A multi-character word may still match the row it started on: typing "en"
  // after "e" landed on "Engineering" must stay there. With no current row the
  // search simply starts at the top.
  const begin = at < 0 ? 0 : at + (cycling ? 1 : 0);
  for (let step = 0; step < rows.length; step++) {
    const id = rows[(begin + step) % rows.length]!;
    if ((tree.label.get(id) ?? "").toLocaleLowerCase().startsWith(needle)) return id;
  }
  return null;
}
