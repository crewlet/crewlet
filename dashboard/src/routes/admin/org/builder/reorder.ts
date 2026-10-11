/**
 * Moving a seat or a unit among the siblings it is drawn beside, and saying
 * what that did to the reporting lines.
 *
 * A MODULE OF ITS OWN because the operation and its consequence are two
 * different things happening at two different times, and only one of them is a
 * button press. The reorder is recorded at once; what it did to who reports to
 * whom is the ENGINE's to derive, and the answer arrives with the next check of
 * that draft, which may be a second later and may never come at all (another
 * edit lands first, or the check is refused). The view that offered the button
 * has no business holding that state machine, and two views offering the same
 * button would have held two copies of it.
 *
 * WHY THE ORDER MATTERS AT ALL. The engine's primary manager of a seat is the
 * FIRST seat that lists it, so moving a seat past its sibling can change who it
 * reports to without anything about reporting being edited. That is a change
 * nobody asked for and nobody would see, so it is announced.
 */

import { useEffect, useRef } from "react";
import type { Derived } from "~/protocol/index.ts";

import type { BuilderApi } from "./BuilderContext.tsx";
import type { Structure } from "./chartModel.ts";
import { deriveChanges } from "./model/changes.ts";
import { locate, type Draft } from "./model/draft.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";

/** A reorder whose effect on the reporting lines is waiting for the check of its draft. */
interface PendingReorder {
  /** The generation the reorder was recorded on. */
  readonly from: number;
  /** The generation it produced, once it was recorded. */
  produced?: number;
  readonly target: NodeKey;
  readonly before: { readonly draft: Draft; readonly derived: Derived };
}

/** Which way a row moves among its siblings. */
export type Direction = -1 | 1;

/** What a surface offering the move needs back from it. */
export interface Reorder {
  /** Moves `id` one place among the siblings it is drawn beside. */
  move(id: NodeKey, direction: Direction): void;
  /**
   * Whether a node can move at all: every seat and unit but the company, and
   * never a seat a unit reference placed (it lives in the company's list, so
   * its place among the rows it is drawn beside is not a place to write).
   */
  movable(id: NodeKey): boolean;
}

const nameOf = (structure: Structure, key: NodeKey): string =>
  structure.nodes.get(key)?.name ?? "this organization";

/**
 * The reorder operation, and the listener that announces what the next check
 * says it did to the reporting lines.
 *
 * `announce` carries both halves: a move nobody could see ("already the first
 * seat in Engineering") and a consequence nobody asked for ("Dev now reports to
 * Lead instead of no one").
 */
export function useReorder(api: BuilderApi, structure: Structure): Reorder {
  const pending = useRef<PendingReorder | null>(null);
  const { state, announce } = api;

  useEffect(() => {
    const p = pending.current;
    if (!p) return;
    if (p.produced === undefined) {
      const last = state.last;
      if (
        state.generation === p.from + 1 &&
        last?.kind === "applied" &&
        last.op.type === "reorder" &&
        last.op.target === p.target
      ) {
        p.produced = state.generation;
      } else {
        // Refused, or something else moved the draft first.
        pending.current = null;
        return;
      }
    }
    if (state.generation !== p.produced) {
      // Another change landed before the check answered. Generations only
      // move forward, so no check of the reordered draft alone will ever
      // answer now (the next one describes both changes, and the review
      // lists them): the record is released rather than kept waiting.
      pending.current = null;
      return;
    }
    if (state.check.generation !== p.produced) return;
    pending.current = null;
    if (!state.check.derived) return;
    const changes = deriveChanges({
      base: p.before,
      next: { draft: state.draft, derived: state.check.derived },
      ops: [],
      reports: [],
    });
    const lines = changes.reportsTo.map(
      (r) =>
        `${r.ref.name} now reports to ${r.after?.name ?? "no one"} instead of ${r.before?.name ?? "no one"}.`,
    );
    if (lines.length > 0) announce(`The new order changes a primary manager. ${lines.join(" ")}`);
  }, [state, announce]);

  /*
   * WHETHER THIS NODE HAS SIBLINGS TO PASS, which is a question about the
   * organization and not about the posture. READ-ONLY DISABLES, IT NEVER
   * HIDES (`nodeActions`), so a guarded draft's menu draws the two moves and
   * refuses them; answering false here instead took them out of the menu
   * while Move to beside them stayed and said it was unavailable, which told
   * an operator that this node cannot be reordered at all.
   */
  const movable = (id: NodeKey): boolean => {
    const view = structure.nodes.get(id);
    if (!view || view.type === "company") return false;
    return !(view.type === "seat" && view.placedByRef);
  };

  const move = (id: NodeKey, direction: Direction) => {
    const view = structure.nodes.get(id);
    if (!view || view.type === "company" || api.readOnly) return;
    if (view.type === "seat" && view.placedByRef) {
      announce(
        `${view.name} is placed in this unit by its unit reference. Move it into the unit to reorder it.`,
      );
      return;
    }
    const found = locate(state.draft, id);
    if (!found) return;
    // A ROW MOVES PAST THE ROW THE OPERATOR SEES BESIDE IT. The company's list
    // of root seats also holds the ones the engine placed in units by
    // reference, drawn inside those units, so counting in the document's list
    // would step over an invisible sibling: a keypress that changes nothing
    // on screen. The step is counted among the siblings as drawn and written
    // as a place in the document's list.
    const parentView = structure.nodes.get(found.parent);
    const drawn: readonly NodeKey[] =
      found.kind === "seat" && parentView?.type === "company"
        ? parentView.seats
        : found.siblings.map((s) => s.key);
    const past = drawn[drawn.indexOf(id) + direction];
    if (past === undefined) {
      const kind = found.kind === "seat" ? "seat" : "unit";
      const where = found.parent === COMPANY_KEY ? "the company" : nameOf(structure, found.parent);
      announce(
        `${view.name} is already the ${direction < 0 ? "first" : "last"} ${kind} in ${where}.`,
      );
      return;
    }
    // Down: straight after the row it passes. Up: straight before it, which
    // the document writes as after whatever precedes that row there.
    const before = found.siblings.findIndex((s) => s.key === past) - 1;
    const after = direction > 0 ? past : before < 0 ? null : found.siblings[before]!.key;
    const current = state.check.generation === state.generation ? state.check.derived : null;
    pending.current = current
      ? { from: state.generation, target: id, before: { draft: state.draft, derived: current } }
      : null;
    api.dispatch({
      type: "record",
      intent: { type: "reorder", target: id, to: { parent: found.parent, after } },
    });
  };

  return { move, movable };
}
