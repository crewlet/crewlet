/**
 * The marks a builder node carries, on a chart node and in a table cell.
 *
 * NEUTRAL, EXCEPT WHERE A MARK IS A STATE. A seat's kind, its placement by a
 * unit reference and its Datadog fallback role are facts about identity and
 * wiring, so they are drawn in the neutral ink like every other identity on
 * the dashboard (design rule 1). A problem count is a state and takes the
 * critical tone; a reference that names no unit is a caution; a seat's live
 * state is the `StateBadge` every other screen uses.
 *
 * TWO DRAWINGS OF ONE FACT, because the two surfaces have different room. A
 * table cell has a line to itself, so a wiring mark is a `Tag` with the
 * sentence written out. A chart node is ONE RANK TALL and as wide as its own
 * name, so the same fact is a glyph riding the caption, named for a reader who
 * cannot see it and titled for one who can. Both come from this module, so
 * what the chart marks and what the table marks cannot drift apart.
 *
 * A LIVE PUSH NEVER MOVES A NODE, AND NEITHER DOES A CHECK. The live state
 * badge and the problem count sit in slots that keep their room whether or not
 * they hold anything (`.bnode-state` and `.bnode-count`, and the chart node's
 * own trailing slot), every glyph mark is a fixed size, and the marks of a
 * reference that names nothing come from the chart, which holds them while the
 * node still writes what the check warned about. So a push twice per tool-loop
 * round, and a check after every edit, change a badge's text and never a
 * node's measured size, which is what the canvas lays out by.
 */

import type { ReactNode } from "react";
import { StateBadge } from "~/components/common.tsx";
import { plural } from "~/lib/format.ts";
import { runState, stateLabel, toneOf } from "~/lib/seats.ts";
import type { BuilderApi } from "./BuilderContext.tsx";
import type { ReportingItem, SeatView, UnitView } from "./chartModel.ts";
import type { NodeKey } from "./model/keys.ts";
import { CycleGlyph, LinkGlyph, NotificationsGlyph, WarningGlyph } from "@crewlethq/icons/glyphs";
import { StatusDot, Tag, VisuallyHidden } from "@crewlethq/ui";

/** "Agent seat" or "Human seat". */
export function seatKindLabel(view: Pick<SeatView, "kind">): string {
  return view.kind === "human" ? "Human seat" : "Agent seat";
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
 * The problem count, in a slot of its own: empty when the last check of the
 * current draft placed nothing on the node.
 *
 * A CHECK NEVER MOVES A CARD. The count is the last check of the CURRENT
 * draft's, so it is absent while a check is out, which is after every edit.
 * Drawn in a line of its own, that line collapsed and came back, changing the
 * card's measured height twice per edit and moving every card beside it. The
 * slot (`.bnode-count`) sits on the card's first line beside the name, which
 * truncates instead, and that line is as tall with the badge as without it.
 *
 * THE COUNT RATHER THAN THE SENTENCE, because a chart node is one rank tall
 * and as wide as its own name, and the slot a push arrives in is one control
 * step wide however many marks it holds: "3 problems" written out was clipped
 * inside it. It is the rule this module already keeps for a wiring mark, for
 * the same reason, and the sentence is still said, as the badge's own name and
 * as the tooltip a pointer gets. The table beside this chart has a line to
 * spare and writes it out.
 */
export function ProblemCount({ api, nodeKey }: { api: BuilderApi; nodeKey: NodeKey }) {
  const count = api.problemsFor(nodeKey).length;
  return (
    <span className="bnode-count">
      {count > 0 && (
        <Tag
          variant="danger"
          title={plural(count, "problem")}
          aria-label={plural(count, "problem")}
        >
          {count}
        </Tag>
      )}
    </span>
  );
}

/**
 * The live state of a saved agent seat. Looked up by the handle and name the
 * SAVED company gives it, because that is what the running seat is called
 * until this draft is saved and applied.
 */
export function LiveState({
  api,
  view,
  compact = false,
}: {
  api: BuilderApi;
  view: SeatView;
  /**
   * The chart's drawing: the state as its tone and nothing written out.
   *
   * A NODE HAS NO ROOM FOR THE WORD. The slot a push arrives in is one control
   * step wide whatever it holds, which is what stops a badge appearing from
   * relaying the chart; the word "awaiting sandbox" in it would be clipped
   * rather than read. The state is still said, as the mark's own name and as
   * the tooltip a pointer gets, and the table beside the chart writes it out.
   */
  compact?: boolean;
}) {
  if (!view.running || !view.saved) return null;
  const { handle, name } = view.saved;
  const agent =
    (handle ? api.agents.find((a) => a.handle === handle) : undefined) ??
    api.agents.find((a) => a.role === name);
  if (compact) {
    const state = runState(agent, api.sandboxes);
    const said = stateLabel(state);
    return (
      <span className="bnode-state" title={said}>
        <StatusDot tone={toneOf(state)} />
        <VisuallyHidden>{said}</VisuallyHidden>
      </span>
    );
  }
  return (
    <span className="bnode-state">
      <StateBadge agent={agent} sandboxes={api.sandboxes} />
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
  return <Mark note={view.danglingNote ?? "Lead names no seat"} glyph={WarningGlyph} />;
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
        <Mark note={view.danglingNote ?? `No unit named ${dangling}`} glyph={WarningGlyph} />
      )}
      {view.datadogFallback && (
        <Mark note="Alerts that name no seat wake this seat" glyph={NotificationsGlyph} />
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
    <Mark note={`In a reporting cycle of ${plural(item.cycleSize, "seat")}`} glyph={CycleGlyph} />
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

/**
 * The same marks as tags, for a surface with a line to spare: the table's name
 * cell. The words are the glyphs' own names, so the two surfaces say the same
 * thing about one fact.
 */
export function UnitTags({ view }: { view: UnitView }) {
  if (view.danglingLead === null) return null;
  return (
    <Tag variant="warning" leadingIcon={<WarningGlyph />} title={view.danglingNote}>
      Lead names no seat
    </Tag>
  );
}

/** The wiring marks of a seat, as tags: see [UnitTags]. */
export function SeatTags({ view }: { view: SeatView }) {
  const dangling = view.danglingUnitRef;
  if (!view.placedByRef && !dangling && !view.datadogFallback) return null;
  return (
    <>
      {view.placedByRef && (
        <Tag
          appearance="outline"
          leadingIcon={<LinkGlyph />}
          title="Declared at the root with a unit reference"
        >
          Placed by unit reference
        </Tag>
      )}
      {dangling && (
        <Tag variant="warning" leadingIcon={<WarningGlyph />} title={view.danglingNote}>
          {`No unit named ${dangling}`}
        </Tag>
      )}
      {view.datadogFallback && (
        <Tag
          appearance="outline"
          leadingIcon={<NotificationsGlyph />}
          title="Alerts that name no seat wake this seat"
        >
          Datadog fallback
        </Tag>
      )}
    </>
  );
}
