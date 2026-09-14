/**
 * The marks a builder node carries, drawn the same on a canvas card and an
 * outline row.
 *
 * NEUTRAL, EXCEPT WHERE A MARK IS A STATE. A seat's kind, its placement by a
 * unit reference and its Datadog fallback role are facts about identity and
 * wiring, so they are drawn in the neutral ink like every other identity on
 * the dashboard (design rule 1). A problem count is a state and takes the
 * critical tone; a reference that names no unit is a caution; a seat's live
 * state is the `StateBadge` every other screen uses.
 *
 * A LIVE PUSH NEVER MOVES A CARD. The live state badge sits in a slot of its
 * own that does not wrap and does not grow the row (`.bnode-state` in
 * `screens.css`), and nothing else here reads live data, so a push twice per
 * tool-loop round changes a badge's text and never a card's measured size,
 * which is what the canvas lays out by.
 */

import { StateBadge } from "~/components/common.tsx";
import { plural } from "~/lib/format.ts";
import { Badge } from "~/ui/primitives.tsx";
import type { BuilderApi } from "./BuilderContext.tsx";
import type { SeatView } from "./chartModel.ts";
import type { NodeKey } from "./model/keys.ts";

/** "Agent seat" or "Human seat". */
export function seatKindLabel(view: Pick<SeatView, "kind">): string {
  return view.kind === "human" ? "Human seat" : "Agent seat";
}

/** A seat's handle as written beside its name, or what stands in for one not reported yet. */
export function handleLabel(handle: string | undefined): string {
  return handle ? `@${handle}` : "Handle after the check";
}

/** The problem count badge: nothing when the last check placed none on the node. */
export function ProblemCount({ api, nodeKey }: { api: BuilderApi; nodeKey: NodeKey }) {
  const count = api.problemsFor(nodeKey).length;
  if (count === 0) return null;
  return (
    <Badge tone="critical" icon="alert">
      {plural(count, "problem")}
    </Badge>
  );
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
      <StateBadge agent={agent} sandboxes={api.sandboxes} />
    </span>
  );
}

/**
 * A unit's lead that names no seat, as the engine warned. The lead chip shows
 * the name as written, and a name that reads like a seat must not look like
 * one that resolves.
 */
export function UnitMarks({ api, nodeKey }: { api: BuilderApi; nodeKey: NodeKey }) {
  const warning = api.warningsFor(nodeKey).find((w) => w.ref === "lead");
  if (!warning) return null;
  return (
    <Badge tone="caution" icon="alert" title={warning.message}>
      Lead names no seat
    </Badge>
  );
}

/** The wiring marks of a seat: placement by reference, a dangling reference, the Datadog fallback. */
export function SeatMarks({ api, view }: { api: BuilderApi; view: SeatView }) {
  const dangling = view.danglingUnitRef;
  const warning = dangling
    ? api.warningsFor(view.key).find((w) => w.ref === "unit")?.message
    : undefined;
  if (!view.placedByRef && !dangling && !view.datadogFallback) return null;
  return (
    <>
      {view.placedByRef && (
        <Badge outline icon="link" title="Declared at the root with a unit reference">
          Placed by unit reference
        </Badge>
      )}
      {dangling && (
        <Badge tone="caution" icon="alert" title={warning}>
          {`No unit named ${dangling}`}
        </Badge>
      )}
      {view.datadogFallback && (
        <Badge outline icon="bell" title="Alerts that name no seat wake this seat">
          Datadog fallback
        </Badge>
      )}
    </>
  );
}
