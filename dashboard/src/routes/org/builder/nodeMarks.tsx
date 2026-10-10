/**
 * The marks a builder node carries, on a chart node and in a table cell.
 *
 * NEUTRAL, EXCEPT WHERE A MARK IS A STATE. A seat's kind, its placement by a
 * unit reference and its Datadog fallback role are facts about identity and
 * wiring, so they are drawn in the neutral ink like every other identity on
 * the dashboard (design rule 1). A reference that names no unit is a caution,
 * and a seat's live state is the `StateBadge` every other screen uses.
 *
 * ONE DRAWING OF ONE FACT, on both surfaces. A wiring mark is a fixed-size
 * glyph riding the caption, named for a reader who cannot see it and titled
 * for one who can. It used to be a `Tag` with the sentence written out in the
 * table, on the reading that a table cell has a line to spare; the treegrid
 * row is a RANK tall, so a tag in it took the row from 36px to 36.6 and made a
 * row as tall as its own wiring, which this module promises it is not.
 *
 * A LIVE PUSH NEVER MOVES A ROW, AND NEITHER DOES A CHECK. The live state
 * badge sits in a slot that keeps its room whether or not it holds anything
 * (`.bnode-state`, in the table row's trailing slot), every glyph mark is a
 * fixed size, and the marks of a reference that names nothing come from the
 * chart, which holds them while the node still writes what the check warned
 * about. So a push twice per tool-loop round, and a check after every edit,
 * change a badge's text and never a measured size, which is what the canvas
 * and the table lay out by.
 *
 * WHAT A CHART NODE DOES NOT CARRY. The chart drew the live state and the
 * problem count in every node's trailing slot once — a column of "idle" dots
 * down a company where nothing was running, and an empty box beside every
 * node with no problem — and stopped (see `CanvasView.tsx`). The compact
 * drawing of the state and the count's badge went with it: the table writes
 * the state out and counts the problems in a column of its own, and the
 * toolbar counts the whole draft's.
 */

import type { ReactNode } from "react";
import { StateBadge } from "~/components/common.tsx";
import { plural } from "~/lib/format.ts";
import type { BuilderApi } from "./BuilderContext.tsx";
import type { NodeView, ReportingItem, SeatView, UnitView } from "./chartModel.ts";
import type { TreeCardTone } from "@crewlethq/ui";
import { seatTone, type NodeGlyphKind } from "~/ui/orgNodes.tsx";
import { Repeat2Glyph, LinkGlyph, BellGlyph, TriangleAlertGlyph } from "@crewlethq/icons/glyphs";

/** The kind of mark a node view wears. */
export function nodeGlyphKind(view: NodeView): NodeGlyphKind {
  if (view.type === "company") return "company";
  if (view.type === "unit") return "unit";
  return view.kind === "human" ? "human" : "agent";
}

/** The hue a node is drawn in: an agent seat's, or none. */
export function nodeTone(view: NodeView | undefined): TreeCardTone | undefined {
  return view?.type === "seat" ? seatTone(view.kind) : undefined;
}

/** A seat's handle as written beside its name, or what stands in for one not reported yet. */
export function handleLabel(handle: string | undefined): string {
  return handle ? `@${handle}` : "Handle after the check";
}

/** A seat's primary manager, "No manager", or what stands in while no check of this draft has said. */
export function managerLabel(manager: string | null | undefined): string {
  if (manager === undefined) return "Manager after the check";
  return manager ?? "No manager";
}

/**
 * The live state of a saved agent seat. Looked up by the handle and name the
 * SAVED company gives it, because that is what the running seat is called
 * until this draft is saved and applied.
 */
export function LiveState({ api, view }: { api: BuilderApi; view: SeatView }) {
  if (!view.running || !view.saved) return null;
  const { handle, name } = view.saved;
  const agent =
    (handle ? api.agents.find((a) => a.handle === handle) : undefined) ??
    api.agents.find((a) => a.role === name);
  return (
    <span className="bnode-state">
      <StateBadge agent={agent} />
    </span>
  );
}

/** What a unit's marks say, as sentences: one list, drawn two ways. */
export function unitMarkNotes(view: UnitView): string[] {
  if (view.danglingLead === null) return [];
  return [view.danglingNote ?? "Lead names no seat"];
}

/** What a seat's marks say, as sentences: one list, drawn two ways. */
export function seatMarkNotes(view: SeatView): string[] {
  const notes: string[] = [];
  if (view.placedByRef) notes.push("Declared at the root with a unit reference");
  if (view.danglingUnitRef) {
    notes.push(view.danglingNote ?? `No unit named ${view.danglingUnitRef}`);
  }
  if (view.datadogFallback) notes.push("Alerts that name no seat wake this seat");
  return notes;
}

/**
 * A unit's lead that names no seat, as the engine warned. The lead chip shows
 * the name as written, and a name that reads like a seat must not look like
 * one that resolves. Read from the chart (`UnitView.danglingLead`), which
 * holds it while the unit still writes what the check warned about, so a
 * check going out does not take it off the node and change its size.
 *
 * A GLYPH, because this is drawn on a chart node one rank tall. It is named
 * for a reader who cannot see it, so the warning is not carried by the drawing
 * alone.
 */
export function UnitMarks({ view }: { view: UnitView }) {
  if (view.danglingLead === null) return null;
  return <Mark note={view.danglingNote ?? "Lead names no seat"} glyph={TriangleAlertGlyph} />;
}

/** The wiring marks of a seat, as glyphs: see [UnitMarks]. */
export function SeatMarks({ view }: { view: SeatView }) {
  const dangling = view.danglingUnitRef;
  if (!view.placedByRef && !dangling && !view.datadogFallback) return null;
  return (
    <>
      {view.placedByRef && (
        <Mark note="Declared at the root with a unit reference" glyph={LinkGlyph} />
      )}
      {dangling && (
        <Mark note={view.danglingNote ?? `No unit named ${dangling}`} glyph={TriangleAlertGlyph} />
      )}
      {view.datadogFallback && (
        <Mark note="Alerts that name no seat wake this seat" glyph={BellGlyph} />
      )}
    </>
  );
}

/**
 * What a reporting chart item is marked with: that it is in a cycle.
 *
 * ONLY THE CYCLE. That a seat has no manager is said by WHERE IT SITS, at the
 * top of the forest with nothing above it, so a mark saying it again would be
 * a second drawing of the same fact on every top of the chart; it is said to a
 * reader who cannot see where it sits and to nobody else. A cycle is not said
 * by where a seat sits at all, because every seat in a loop has one above it,
 * so that one is drawn.
 */
export function ReportingMarks({ item }: { item: Pick<ReportingItem, "cycleSize"> }) {
  if (item.cycleSize === undefined) return null;
  return (
    <Mark note={`In a reporting cycle of ${plural(item.cycleSize, "seat")}`} glyph={Repeat2Glyph} />
  );
}

/**
 * One glyph mark: the sentence twice, once for each reader.
 *
 * The glyph carries the sentence as its accessible name, and the element
 * around it carries the same sentence as the tooltip a pointer gets. A drawing
 * with no words anywhere is a fact only the person who wrote it can read, and
 * these sentences are the engine's own.
 */
function Mark({
  note,
  glyph: Drawing,
}: {
  note: string;
  glyph: (props: { title?: string }) => ReactNode;
}) {
  return (
    <span className="bnode-mark" title={note}>
      <Drawing title={note} />
    </span>
  );
}
