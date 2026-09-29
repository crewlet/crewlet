/**
 * What an operation will do before a dialog confirms it.
 *
 * A DIALOG PREVIEWS THE OPERATION ITSELF. The Move, Delete and Change kind
 * dialogs record the operation they are about to dispatch through the
 * reducer's own door (`recordIntent`) and apply it to a copy of the draft, so
 * the references it clears, the fields it strips and the seats it removes are
 * the ones the model will clear, strip and remove, not a second account of
 * the rules written for a dialog.
 *
 * SCHEDULE RUNNERS ARE THE ONE RULE RESTATED, and only as a warning. A move,
 * a removal or a kind change can leave a schedule nothing could ever run, and
 * nothing refuses that: the chart takes a structural write without reading the
 * schedules in a unit's runtime half, which is opaque to it, and the engine
 * builds the organization from the rows it holds. So a stranded schedule
 * simply never fires, often on a unit the operator never touched. The two
 * rules are `org.Unit.Validate` (an enabled schedule fanning out to members
 * needs a DIRECT agent member) and `org.Organization.validateLeadSchedules`
 * (an enabled schedule for the unit lead fails when the EFFECTIVE lead is a
 * human seat). They are read here on the draft before and after, and only a
 * schedule the operation strands is named. A schedule is in the runtime half,
 * so a reader who was not shown that half is told of none.
 */

import type { ScheduleSpec } from "~/protocol/index.ts";
import type { NodeKey } from "./model/keys.ts";
import { isRecord } from "./model/json.ts";
import {
  addressIndex,
  allSeats,
  allUnits,
  type Draft,
  type DraftSeat,
  type DraftUnit,
} from "./model/draft.ts";
import {
  apply,
  kindOf,
  type ApplyReport,
  type Intent,
  type Operation,
} from "./model/operations.ts";
import { recordIntent, type BuilderState, type RecordAnswer } from "./model/reducer.ts";
import { deriveChanges } from "./model/changes.ts";
import { effectiveLeads } from "./chartModel.ts";

/** An operation recorded against the state and applied to a copy of its draft. */
export type Simulated =
  | {
      readonly ok: true;
      readonly op: Operation;
      readonly after: Draft;
      readonly report: ApplyReport;
    }
  | Extract<RecordAnswer, { ok: false }>;

/** Records an intent as dispatching it would, and applies it to a copy of the draft. */
export function simulate(state: BuilderState, intent: Intent): Simulated {
  const answer = recordIntent(state, intent);
  if (!answer.ok) return answer;
  const { draft, report } = apply(state.draft, answer.op);
  return { ok: true, op: answer.op, after: draft, report };
}

// ---------------------------------------------------------------------------
// Schedules nothing could run
// ---------------------------------------------------------------------------

/** A unit schedule nothing could run. */
export interface StrandedSchedule {
  readonly unit: NodeKey;
  readonly unitName: string;
  readonly schedule: string;
  /** `members`: it fans out to direct agent members and there are none. `lead`: it runs as the lead, a human seat. */
  readonly reason: "members" | "lead";
}

/** Every enabled unit schedule in the draft that nothing could run, depth-first. */
export function strandedSchedules(draft: Draft): StrandedSchedule[] {
  const seats = addressIndex(draft).seats;
  const resolved = effectiveLeads(draft);
  const out: StrandedSchedule[] = [];
  for (const { unit } of allUnits(draft)) {
    for (const schedule of unitSchedules(unit)) {
      if (schedule.enabled === false) continue;
      const stranded = (reason: StrandedSchedule["reason"]) =>
        out.push({
          unit: unit.key,
          unitName: unit.data.name || unit.data.key,
          schedule: schedule.name,
          reason,
        });
      if (schedule.target === "lead") {
        const leadHandle = resolved.get(unit.key)?.lead ?? "";
        const lead: DraftSeat | undefined = leadHandle === "" ? undefined : seats.get(leadHandle);
        if (lead && kindOf(lead.data) === "human") stranded("lead");
      } else if (!unit.roles.some((seat) => kindOf(seat.data) === "agent")) {
        stranded("members");
      }
    }
  }
  return out;
}

/** A unit's named schedules, as its runtime half holds them. */
function unitSchedules(unit: DraftUnit): ScheduleSpec[] {
  const list = unit.data.runtime?.schedules;
  return (Array.isArray(list) ? list : []).filter(
    (s): s is ScheduleSpec => isRecord(s) && typeof s.name === "string",
  );
}

/**
 * The schedules an operation strands: nothing could run them after it, and
 * something could before. A schedule that was already stranded is not this
 * operation's consequence.
 */
export function newlyStranded(before: Draft, after: Draft): StrandedSchedule[] {
  const id = (s: StrandedSchedule) => `${s.unit}\u0000${s.schedule}`;
  const already = new Set(strandedSchedules(before).map(id));
  return strandedSchedules(after).filter((s) => !already.has(id(s)));
}

/** The sentence a dialog shows for a stranded schedule. */
export function strandedSentence(s: StrandedSchedule): string {
  const why =
    s.reason === "lead"
      ? "it runs as the unit's lead, and the lead would be a human seat"
      : "it runs on the unit's direct agent members, and the unit would have none";
  return `Schedule ${s.schedule} on ${s.unitName} would have no runner: ${why}. Nothing refuses the save, so it would simply never run: disable it, or give it a runner.`;
}

// ---------------------------------------------------------------------------
// Removals
// ---------------------------------------------------------------------------

/**
 * When an operation removes more than half the seats the saved company has,
 * how many of how many, as the review counts them (`model/changes.ts`).
 */
export function massRemoval(
  baseDraft: Draft,
  after: Draft,
): { removed: number; total: number } | null {
  return deriveChanges({ base: baseDraft, next: after, ops: [], reports: [] }).massRemoval;
}

/** The seats of `before` that `after` no longer holds, in the document's order. */
export function removedSeats(before: Draft, after: Draft): DraftSeat[] {
  const kept = new Set([...allSeats(after)].map(({ seat }) => seat.key));
  return [...allSeats(before)].map(({ seat }) => seat).filter((seat) => !kept.has(seat.key));
}

/** The units of `before` that `after` no longer holds, in the document's order. */
export function removedUnits(before: Draft, after: Draft): DraftUnit[] {
  const kept = new Set([...allUnits(after)].map(({ unit }) => unit.key));
  return [...allUnits(before)].map(({ unit }) => unit).filter((unit) => !kept.has(unit.key));
}
