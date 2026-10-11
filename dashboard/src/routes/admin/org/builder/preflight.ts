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
 * the engine refuses the whole company for it (`ErrUnrunnableSchedule`), often
 * on a unit the operator never touched. The builder authors no schedules, so
 * the refusal would arrive with no way out in view. The two rules are
 * `org.Unit.Validate` (an enabled schedule fanning out to members needs a
 * DIRECT agent member, a root seat placed in the unit by its `unit:`
 * reference included) and `org.Organization.validateLeadSchedules` (an
 * enabled schedule for the unit lead fails when the EFFECTIVE lead is a human
 * seat). They are read here on the draft before and after, and only a
 * schedule the operation strands is named. The engine still validates the
 * result: if the two readings ever differ, the check says so.
 */

import type { ScheduleSpec } from "~/protocol/index.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import { allSeats, allUnits, type Draft, type DraftSeat, type DraftUnit } from "./model/draft.ts";
import { isRecord } from "./model/json.ts";
import {
  apply,
  kindOf,
  type ApplyReport,
  type Intent,
  type Operation,
} from "./model/operations.ts";
import { recordIntent, type BuilderState, type RecordAnswer } from "./model/reducer.ts";
import { deriveChanges } from "./model/changes.ts";

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

/** Every enabled unit schedule in the draft that nothing could run, in the document's order. */
export function strandedSchedules(draft: Draft): StrandedSchedule[] {
  const units = [...allUnits(draft)];
  const firstUnitNamed = (name: string) => units.find(({ unit }) => unit.data.name === name)?.unit;

  // Root seats the engine places in a unit by their `unit:` reference are
  // that unit's direct members (`attachRootSeats`), and leave the root.
  const attached = new Map<NodeKey, DraftSeat[]>();
  const placed = new Set<NodeKey>();
  for (const seat of draft.roles) {
    const ref = seat.data.unit;
    const target = typeof ref === "string" && ref !== "" ? firstUnitNamed(ref) : undefined;
    if (!target) continue;
    attached.set(target.key, [...(attached.get(target.key) ?? []), seat]);
    placed.add(seat.key);
  }
  const membersOf = (unit: DraftUnit) => [...unit.roles, ...(attached.get(unit.key) ?? [])];

  // A lead names a seat by its name, the first seat of that name in the
  // engine's order: root seats that stayed at the root, then each unit's
  // members depth-first.
  const seatNamed = (name: string): DraftSeat | undefined =>
    draft.roles.find((s) => !placed.has(s.key) && s.data.name === name) ??
    units.flatMap(({ unit }) => membersOf(unit)).find((s) => s.data.name === name);

  const parentOf = new Map(units.map(({ unit, parent }) => [unit.key, parent]));
  const byKey = new Map(units.map(({ unit }) => [unit.key, unit]));
  const effectiveLead = (unit: DraftUnit): string => {
    const declared = unit.data.lead;
    if (typeof declared === "string" && declared !== "") return declared;
    const parent = parentOf.get(unit.key);
    const up = parent === undefined || parent === COMPANY_KEY ? undefined : byKey.get(parent);
    return up ? effectiveLead(up) : "";
  };

  const out: StrandedSchedule[] = [];
  for (const { unit } of units) {
    const schedules = (Array.isArray(unit.data.schedules) ? unit.data.schedules : []).filter(
      (s): s is ScheduleSpec => isRecord(s) && typeof s.name === "string",
    );
    for (const schedule of schedules) {
      if (schedule.enabled === false) continue;
      const stranded = (reason: StrandedSchedule["reason"]) =>
        out.push({ unit: unit.key, unitName: unit.data.name, schedule: schedule.name, reason });
      if (schedule.target === "lead") {
        const leadName = effectiveLead(unit);
        const lead = leadName === "" ? undefined : seatNamed(leadName);
        if (lead && kindOf(lead.data) === "human") stranded("lead");
      } else if (!membersOf(unit).some((seat) => kindOf(seat.data) === "agent")) {
        stranded("members");
      }
    }
  }
  return out;
}

/**
 * The schedules an operation strands: nothing could run them after it, and
 * something could before. A schedule that was already stranded is the
 * check's to report, not this operation's consequence.
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
  return `Schedule ${s.schedule} on ${s.unitName} would have no runner: ${why}. The engine refuses the save until it is disabled or has a runner.`;
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
  return deriveChanges({
    base: { draft: baseDraft, derived: null },
    next: { draft: after, derived: null },
    ops: [],
    reports: [],
  }).massRemoval;
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
