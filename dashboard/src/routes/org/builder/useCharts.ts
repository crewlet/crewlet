/**
 * The React half the canvas and the outline share: the charts of the current
 * draft, where "Open seat" goes, and the state of a collapsible tree.
 *
 * ONE COPY OF EACH. Both views draw the same structure, open the same seat
 * screen and keep the same expansion and roving-focus rules over the shared
 * visible-order model (`ui/treeModel.ts`); only their ARIA patterns differ (a
 * tree of cards, a treegrid of rows), so everything below that line lives
 * here once.
 */

import { useCallback, useMemo, useRef, useState } from "react";
import { useNavigator } from "~/app/router.tsx";
import {
  allExpandable,
  ancestors,
  collapseOrAscend,
  createTree,
  expandOrDescend,
  firstVisible,
  isTypeAheadKey,
  lastVisible,
  nextVisible,
  previousVisible,
  typeAhead,
  typeAheadBuffer,
  visible,
  type TreeInput,
  type TreeModel,
  type TypeAheadState,
} from "~/ui/treeModel.ts";
import { chartInputs, reporting, structure, type Reporting, type Structure } from "./chartModel.ts";
import type { BuilderState } from "./model/reducer.ts";
import type { OpenScreen } from "./nodeActions.tsx";

/**
 * The structure chart of the current draft. A reading of the draft, the saved
 * base and the last check and of nothing else, so a live push, a refusal or a
 * pending update leaves it (and every layout built on it) as it was.
 */
export function useStructure(state: BuilderState): Structure {
  const { draft, baseDraft, check } = state;
  return useMemo(
    () => structure(chartInputs({ draft, baseDraft, check })),
    [draft, baseDraft, check],
  );
}

/** The reporting chart of the current draft, on the same terms as [useStructure]. */
export function useReporting(state: BuilderState): Reporting {
  const { draft, baseDraft, check } = state;
  return useMemo(
    () => reporting(chartInputs({ draft, baseDraft, check })),
    [draft, baseDraft, check],
  );
}

/** Opens a seat's own screen: a new screen, so it pushes. */
export function useOpenScreen(): OpenScreen {
  const nav = useNavigator();
  return useCallback((path) => nav.to(path), [nav]);
}

export interface TreeState {
  readonly model: TreeModel;
  readonly expanded: ReadonlySet<string>;
  /** Every visible node, in order. */
  readonly rows: readonly string[];
  /** The roving tab stop: always a visible node, or `null` for an empty tree. */
  readonly active: string | null;
  setActive(id: string): void;
  toggle(id: string): void;
  expandAll(): void;
  /** Collapses everything but the tops, so the tree keeps something to stand on. */
  collapseAll(): void;
  /** Opens every collapsed ancestor of `id`; true when any was closed. */
  open(id: string): boolean;
  /**
   * The node typing `key` at `now` finds after `from`: `null` when nothing
   * matches, `undefined` when the key is not a type-ahead key at all.
   */
  typed(key: KeyboardEventLike, from: string, now: number): string | null | undefined;
}

type KeyboardEventLike = { key: string; ctrlKey?: boolean; metaKey?: boolean; altKey?: boolean };

/**
 * A collapsible tree's state: what is collapsed, which node holds the tab
 * stop, and type-ahead.
 *
 * COLLAPSED, NOT EXPANDED, IS WHAT IS KEPT: a node nobody has closed is open,
 * which is also what a node an operation just added should be. The tab stop
 * is the node last focused, else the selection, else the first node, and is
 * moved to the nearest visible ancestor when its own node is hidden or gone,
 * so the tree always has exactly one way in.
 */
export function useTreeState(forest: readonly TreeInput[], selected: string | null): TreeState {
  const model = useMemo(() => createTree(forest), [forest]);
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(() => new Set());
  const expanded = useMemo(() => {
    const out = allExpandable(model);
    for (const id of collapsed) out.delete(id);
    return out;
  }, [model, collapsed]);
  const rows = useMemo(() => visible(model, expanded), [model, expanded]);

  const [wanted, setActive] = useState<string | null>(null);
  const active = useMemo(() => {
    const id = wanted ?? selected;
    if (id !== null && model.parent.has(id)) {
      if (rows.includes(id)) return id;
      const shown = ancestors(model, id)
        .reverse()
        .find((a) => rows.includes(a));
      if (shown) return shown;
    }
    return firstVisible(model);
  }, [wanted, selected, model, rows]);

  const toggle = useCallback((id: string) => {
    setCollapsed((was) => {
      const next = new Set(was);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const current = useRef({ model, collapsed });
  current.current = { model, collapsed };

  const expandAll = useCallback(() => setCollapsed(new Set()), []);
  const collapseAll = useCallback(() => {
    const { model: m } = current.current;
    const next = allExpandable(m);
    m.roots.forEach((r) => next.delete(r));
    setCollapsed(next);
  }, []);

  const open = useCallback((id: string) => {
    const { model: m, collapsed: closed } = current.current;
    const hidden = ancestors(m, id).filter((a) => closed.has(a));
    if (hidden.length === 0) return false;
    setCollapsed((was) => {
      const next = new Set(was);
      hidden.forEach((a) => next.delete(a));
      return next;
    });
    return true;
  }, []);

  const buffer = useRef<TypeAheadState>({ text: "", at: 0 });
  const typed = (e: KeyboardEventLike, from: string, now: number) => {
    if (!isTypeAheadKey(e, buffer.current, now)) return undefined;
    buffer.current = typeAheadBuffer(buffer.current, e.key, now);
    return typeAhead(model, expanded, from, buffer.current.text);
  };

  return {
    model,
    expanded,
    rows,
    active,
    setActive,
    toggle,
    expandAll,
    collapseAll,
    open,
    typed,
  };
}

/** What a navigation key asks of a tree: focus a node, or open or close one. */
export type TreeStep = { readonly focus: string } | { readonly toggle: string };

/**
 * What a navigation key pressed on `id` asks for: a step, `null` for a key
 * that is the tree's but leads nowhere (Up on the first node), or `undefined`
 * for a key that is not the tree's at all, which the caller leaves alone.
 *
 * Down and Up walk the visible order; Right opens a closed node or enters an
 * open one; Left closes an open node or climbs to the parent; Home and End
 * jump; a printable key types ahead. A chord with Ctrl, Command or Alt is
 * never the tree's.
 */
export function treeStep(
  tree: TreeState,
  id: string,
  e: KeyboardEventLike & { timeStamp: number },
): TreeStep | null | undefined {
  if (e.ctrlKey || e.metaKey || e.altKey) return undefined;
  const { model, expanded } = tree;
  const focus = (next: string | null): TreeStep | null => (next === null ? null : { focus: next });
  switch (e.key) {
    case "ArrowDown":
      return focus(nextVisible(model, expanded, id));
    case "ArrowUp":
      return focus(previousVisible(model, expanded, id));
    case "ArrowRight": {
      const step = expandOrDescend(model, expanded, id);
      if (step === null) return null;
      return "expand" in step ? { toggle: step.expand } : { focus: step.focus };
    }
    case "ArrowLeft": {
      const step = collapseOrAscend(model, expanded, id);
      if (step === null) return null;
      return "collapse" in step ? { toggle: step.collapse } : { focus: step.focus };
    }
    case "Home":
      return focus(firstVisible(model));
    case "End":
      return focus(lastVisible(model, expanded));
    default: {
      const found = tree.typed(e, id, e.timeStamp);
      return found === undefined ? undefined : focus(found);
    }
  }
}
