/**
 * The marks a builder node carries, on a chart node and in a table cell.
 *
 * NEUTRAL, EXCEPT WHERE A MARK IS A STATE. A seat's kind and its Datadog
 * fallback role are facts about identity and wiring, so they are drawn in the
 * neutral ink like every other identity on the dashboard (design rule 1). A
 * reference that names nothing is a caution, and a seat's live state is the
 * `StateBadge` every other screen uses.
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
 * chart's own reading of the draft, which holds them for exactly as long as
 * the node writes the reference. So a push twice per tool-loop round, and a
 * check after every edit, change a badge's text and never a measured size,
 * which is what the canvas and the table lay out by.
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
import { CrewletIcon } from "@crewlethq/icons";
import {
  NetworkGlyph,
  BuildingComplexGlyph,
  Repeat2Glyph,
  BellGlyph,
  UserGlyph,
  TriangleAlertGlyph,
  cssLength,
  type GlyphSize,
} from "@crewlethq/icons/glyphs";

/** "Agent seat" or "Human seat". */
export function seatKindLabel(view: Pick<SeatView, "kind">): string {
  return view.kind === "human" ? "Human seat" : "Agent seat";
}

/**
 * A unit's own type, capitalised: the word a chart node and a table row write
 * under the unit's name.
 *
 * CAPITALISED, and in ONE place. The type is the founder's own word from the
 * document, so the first letter is the only thing touched; written out on both
 * surfaces it drifted, and one draft read "Team" on the card and "team" on the
 * row beside it.
 */
export function unitTypeLabel(view: Pick<UnitView, "unitType">): string {
  const type = view.unitType.trim();
  if (type === "") return "Unit";
  return type.charAt(0).toUpperCase() + type.slice(1);
}

/** Which of the four things a node is, for the mark that stands for it. */
export type NodeGlyphKind = "company" | "unit" | "agent" | "human";

/** The kind of mark a node view wears. */
export function nodeGlyphKind(view: NodeView): NodeGlyphKind {
  if (view.type === "company") return "company";
  if (view.type === "unit") return "unit";
  return view.kind === "human" ? "human" : "agent";
}

/**
 * A node's mark: a building for the company, a tree for a unit, a person for a
 * human seat and the Crewlet figure for an agent seat.
 *
 * ONE MAPPING FOR EVERY SURFACE, because a node marked three ways is a node a
 * reader has to learn three times. The chart's cards, the table's rows and the
 * head of the editor over either of them all draw this: the editor's own head
 * used to draw a pencil on all four, which said the panel edits rather than
 * what it is editing, and the table and the chart each held a copy of the
 * mapping beside it.
 *
 * SIZE IS THE CALLER'S, and its absence means the mark takes the font size of
 * the zone it is in, which is how the chart gives an agent seat three quarters
 * of its icon zone and a unit half of it. Every drawing here answers `1em` by
 * default, the Crewlet figure included.
 *
 * A HUMAN'S FIGURE IS THE EXCEPTION, because it is the one mark drawn INSIDE
 * something: the dashed boundary that says this seat is a person outside the
 * system, held at the 24px target floor. At the zone's own step the figure
 * measured 20px inside that 24px ring and touched it on every side; the small
 * step leaves the air the boundary needs to read as a boundary. A caller that
 * says a size still gets it.
 */
export function NodeGlyph({ kind, size }: { kind: NodeGlyphKind; size?: GlyphSize }) {
  if (kind === "company") return <BuildingComplexGlyph size={size} />;
  if (kind === "unit") return <NetworkGlyph size={size} />;
  if (kind === "human") return <UserGlyph size={size ?? "sm"} />;
  const side = cssLength(size);
  return <CrewletIcon width={side} height={side} />;
}

/**
 * A seat's primary manager, "No manager", or what stands in while the engine
 * has not derived it for this draft: it derives the SAVED chart only, so a
 * draft that changed it has a manager nobody has worked out yet
 * (`chartModel.ts`).
 */
export function managerLabel(manager: string | null | undefined): string {
  if (manager === undefined) return "Not derived yet";
  return manager ?? "No manager";
}

/**
 * The live state of a saved agent seat. Looked up by the handle the SAVED
 * chart gives it, because that is what the running seat answers to until this
 * draft is saved and applied.
 */
export function LiveState({ api, view }: { api: BuilderApi; view: SeatView }) {
  if (!view.running || !view.saved) return null;
  // BY THE SAVED HANDLE, which is what the running seat answers to: a handle
  // this draft changed is not one the engine knows yet, and a name is prose
  // two seats may share.
  const { handle } = view.saved;
  const agent = api.agents.find((a) => a.handle === handle);
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
  const notes: string[] = [...view.danglingNotes];
  if (view.datadogFallback) notes.push("Alerts that name no seat wake this seat");
  return notes;
}

/**
 * A unit's lead that names no seat. The lead chip shows the handle as
 * written, and a handle that reads like a seat's must not look like one that
 * resolves. Read from the chart (`UnitView.danglingLead`), which holds it for
 * exactly as long as the unit writes it.
 *
 * A GLYPH, because this is drawn on a chart node one rank tall. It is named
 * for a reader who cannot see it, so the warning is not carried by the drawing
 * alone.
 */
export function UnitMarks({ view }: { view: UnitView }) {
  if (view.danglingLead === null) return null;
  return <Mark note={view.danglingNote ?? "Lead names no seat"} glyph={TriangleAlertGlyph} />;
}

/**
 * The wiring marks of a seat, as glyphs: see [UnitMarks]. A `manages:` entry
 * that names nothing is ONE mark however many there are, carrying every
 * sentence: a node one rank tall has room for a fixed number of glyphs.
 */
export function SeatMarks({ view }: { view: SeatView }) {
  const dangling = view.danglingNotes;
  if (dangling.length === 0 && !view.datadogFallback) return null;
  return (
    <>
      {dangling.length > 0 && <Mark note={dangling.join(" ")} glyph={TriangleAlertGlyph} />}
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
