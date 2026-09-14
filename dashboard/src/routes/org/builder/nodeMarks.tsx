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
 * A LIVE PUSH NEVER MOVES A CARD, AND NEITHER DOES A CHECK. The live state
 * badge and the problem count each sit in a slot of their own on the card's
 * first line that does not wrap and does not grow it (`.bnode-state` and
 * `.bnode-count` in `screens.css`), and the marks of a reference that names
 * nothing come from the chart, which holds them while the node still writes
 * what the check warned about. So a push twice per tool-loop round, and a
 * check after every edit, change a badge's text and never a card's measured
 * size, which is what the canvas lays out by.
 */

import { StateBadge } from "~/components/common.tsx";
import { plural } from "~/lib/format.ts";
import { Badge } from "~/ui/primitives.tsx";
import type { BuilderApi } from "./BuilderContext.tsx";
import type { SeatView, UnitView } from "./chartModel.ts";
import type { NodeKey } from "./model/keys.ts";

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
 * The problem count badge, in a slot of its own: empty when the last check
 * of the current draft placed nothing on the node.
 *
 * A CHECK NEVER MOVES A CARD. The count is the last check of the CURRENT
 * draft's, so it is absent while a check is out, which is after every edit.
 * Drawn in a line of its own, that line collapsed and came back, changing the
 * card's measured height twice per edit and moving every card beside it. The
 * slot (`.bnode-count`) sits on the card's first line beside the name, which
 * truncates instead, and that line is as tall with the badge as without it.
 */
export function ProblemCount({ api, nodeKey }: { api: BuilderApi; nodeKey: NodeKey }) {
  const count = api.problemsFor(nodeKey).length;
  return (
    <span className="bnode-count">
      {count > 0 && (
        <Badge tone="critical" icon="alert">
          {plural(count, "problem")}
        </Badge>
      )}
    </span>
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
 * one that resolves. Read from the chart (`UnitView.danglingLead`), which
 * holds it while the unit still writes what the check warned about, so a
 * check going out does not take it off the card and change its height.
 */
export function UnitMarks({ view }: { view: UnitView }) {
  if (view.danglingLead === null) return null;
  return (
    <Badge tone="caution" icon="alert" title={view.danglingNote}>
      Lead names no seat
    </Badge>
  );
}

/** The wiring marks of a seat: placement by reference, a dangling reference, the Datadog fallback. */
export function SeatMarks({ view }: { view: SeatView }) {
  const dangling = view.danglingUnitRef;
  if (!view.placedByRef && !dangling && !view.datadogFallback) return null;
  return (
    <>
      {view.placedByRef && (
        <Badge outline icon="link" title="Declared at the root with a unit reference">
          Placed by unit reference
        </Badge>
      )}
      {dangling && (
        <Badge tone="caution" icon="alert" title={view.danglingNote}>
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
