/**
 * Recurring work and its dispatch ledger, as the engine's `org` and
 * `schedule` packages keep them: the rules a written schedule is held to, the
 * label a fire is claimed under, and what the scheduler writes when one comes
 * due.
 *
 * PORTS, each named after the function it ports. The cron grammar's errors
 * are the engine's own (`cron.Parse`): the dashboard's reader
 * (`~/lib/cron.ts`) says only whether it can read an expression, and a
 * refusal has to say what the engine says. The engine's sentences that join
 * clauses with an em dash are joined with a colon or parentheses here.
 */

import { nextFires } from "~/lib/cron.ts";
import { zoneOffset } from "~/lib/format.ts";
import type { EventRecord, ScheduleRow, ScheduleRunRow, ScheduleSpec } from "~/protocol/types.ts";
import { fixtureId } from "./ids.ts";
import { goQuote as q } from "./refusal.ts";

// ---- the cron grammar ---------------------------------------------------------

const INVALID = "invalid cron expression";
const MONTHS: Record<string, number> = {
  jan: 1,
  feb: 2,
  mar: 3,
  apr: 4,
  may: 5,
  jun: 6,
  jul: 7,
  aug: 8,
  sep: 9,
  oct: 10,
  nov: 11,
  dec: 12,
};
const WEEKDAYS: Record<string, number> = { sun: 0, mon: 1, tue: 2, wed: 3, thu: 4, fri: 5, sat: 6 };

/** Go's `strconv.Atoi`: an optional sign and decimal digits, else NaN. */
function atoi(s: string): number {
  return /^[+-]?\d+$/.test(s) ? Number(s) : NaN;
}

/** One value of a field, or why it is not one (`cron.parseValue`). */
function valueOf(
  expr: string,
  name: string,
  token: string,
  lo: number,
  hi: number,
  aliases: Record<string, number>,
): number | string {
  const key = token.trim().toLowerCase();
  if (key in aliases) return aliases[key]!;
  const v = atoi(key);
  if (Number.isNaN(v)) {
    return `${INVALID} ${q(expr)}: ${name} field: ${q(token)} is not a number or a name for this field`;
  }
  if (v < lo || v > hi)
    return `${INVALID} ${q(expr)}: ${name} field: value ${v} is outside ${lo}-${hi}`;
  return v;
}

/** Why one field does not parse, or null (`cron.parseField`). */
function fieldProblem(
  expr: string,
  name: string,
  raw: string,
  lo: number,
  hi: number,
  aliases: Record<string, number> = {},
): string | null {
  const spec = raw.trim();
  if (spec === "") return `${INVALID} ${q(expr)}: empty ${name} field`;
  for (const part of spec.split(",")) {
    const term = part.trim();
    if (term === "") return `${INVALID} ${q(expr)}: empty term in ${name} field ${q(spec)}`;
    const slash = term.indexOf("/");
    if (slash >= 0) {
      const stepText = term.slice(slash + 1);
      const step = atoi(stepText.trim());
      if (Number.isNaN(step))
        return `${INVALID} ${q(expr)}: ${name} field: invalid step ${q(stepText)}`;
      if (step <= 0)
        return `${INVALID} ${q(expr)}: ${name} field: step must be positive, got ${step}`;
    }
    // An empty base (`/5`) is refused as the empty value it is. The engine's
    // parser slices it without checking its length, which panics.
    const base = (slash >= 0 ? term.slice(0, slash) : term).trim();
    if (base === "*") continue;
    if (base.slice(1).includes("-")) {
      const dash = base.indexOf("-");
      const start = valueOf(expr, name, base.slice(0, dash), lo, hi, aliases);
      if (typeof start === "string") return start;
      const end = valueOf(expr, name, base.slice(dash + 1), lo, hi, aliases);
      if (typeof end === "string") return end;
      if (end < start) {
        return `${INVALID} ${q(expr)}: ${name} field: range ${q(base)} is descending: ranges do not wrap, so a weekend is 6-7, sat,sun or 0,6`;
      }
      continue;
    }
    const value = valueOf(expr, name, base, lo, hi, aliases);
    if (typeof value === "string") return value;
  }
  return null;
}

/** Why a cron expression does not parse, or null (`cron.Parse`). */
export function cronProblem(expr: string): string | null {
  const fields = expr.split(/\s+/).filter(Boolean);
  if (fields.length !== 5) {
    return `${INVALID} ${q(expr)}: expected 5 fields (minute hour day-of-month month day-of-week), got ${fields.length}`;
  }
  return (
    fieldProblem(expr, "minute", fields[0]!, 0, 59) ??
    fieldProblem(expr, "hour", fields[1]!, 0, 23) ??
    fieldProblem(expr, "day-of-month", fields[2]!, 1, 31) ??
    fieldProblem(expr, "month", fields[3]!, 1, 12, MONTHS) ??
    fieldProblem(expr, "day-of-week", fields[4]!, 0, 7, WEEKDAYS)
  );
}

/**
 * Why a zone name is not a calendar every node shares, or null
 * (`period.LoadZone`): a host's own clock by name, or a name the zone
 * database does not hold.
 */
export function zoneProblem(name: string): string | null {
  if (/^(local|localtime)$/i.test(name)) {
    return `a host's own clock is not a calendar every node shares: ${q(name)} is whatever zone the host reading it is set to, so two nodes would cut two different days from it; write the IANA name of the zone you mean (Europe/Berlin, America/New_York) or UTC`;
  }
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: name });
    return null;
  } catch {
    return `${q(name)} is not an IANA time zone (Europe/Berlin, America/New_York, UTC): unknown time zone ${name}`;
  }
}

// ---- what a written schedule is held to ---------------------------------------------

/** A rule an owner's schedules break, at the field it is about under the owner. */
export interface ScheduleFault {
  /** Under the seat or unit that owns the schedules: `["schedules", 0, "cron"]`. */
  field: (string | number)[];
  kind: "shape";
  message: string;
}

/**
 * Every shape rule an owner's schedules break (`validateSchedules`, with each
 * expression's grammar, `cronGrammar`). `owner` is how the engine names it:
 * `role "Crewlet-PM"` or `unit "Product and Engineering"`. Whether anybody
 * can run a schedule is a question about its owner, answered there.
 */
export function scheduleFaults(owner: string, schedules: readonly ScheduleSpec[]): ScheduleFault[] {
  const out: ScheduleFault[] = [];
  schedules.forEach((s, i) => {
    const shape = (field: string, message: string) =>
      out.push({ field: ["schedules", i, field], kind: "shape", message });
    const name = (s.name ?? "").trim();
    if (!name) shape("name", `${owner}: invalid schedule: name must not be empty`);
    const about = `${owner}: schedule ${q(name)}: invalid schedule`;
    if (!(s.task ?? "").trim()) shape("task", `${about}: task must not be empty`);
    const cron = s.cron ?? "";
    const fields = cron.split(/\s+/).filter(Boolean).length;
    if (fields !== 5) {
      shape(
        "cron",
        `${about}: cron ${q(cron)} needs 5 fields (minute hour day-of-month month day-of-week)`,
      );
    }
    if (s.timezone) {
      const why = zoneProblem(s.timezone);
      if (why) shape("timezone", `${about}: timezone ${why}`);
    }
    if ((s.timeout_seconds ?? 0) < 0)
      shape("timeout_seconds", `${about}: timeout_seconds must be positive`);
    if (s.target && s.target !== "each" && s.target !== "lead") {
      shape(
        "target",
        `${about}: target ${q(s.target)} must be each or lead: to run recurring work as one specific seat, define the schedule on that seat`,
      );
    }
    if (fields === 5) {
      const why = cronProblem(cron);
      if (why) shape("cron", `${about}: ${why}`);
    }
  });
  const named = new Map<string, number[]>();
  schedules.forEach((s, i) => {
    const name = s.name ?? "";
    if (name.trim()) named.set(name, [...(named.get(name) ?? []), i]);
  });
  for (const [name, at] of named) {
    if (at.length < 2) continue;
    out.push({
      field: ["schedules"],
      kind: "shape",
      message: `${owner}: invalid schedule: duplicate schedule name ${q(name)}: ${at.length} schedules carry it (${at.map((i) => `schedules[${i}]`).join(", ")}). The name is half of each fire's idempotency key, so these would suppress each other's fire at the same minute; give each its own name`,
    });
  }
  return out;
}

// ---- the dispatch ledger ---------------------------------------------------------

/**
 * When a due fire is claimed. The scheduler polls every ten seconds
 * (`schedule.DefaultTick`), so a fire is written within ten seconds of its
 * minute; the demo's are written five seconds in.
 */
const CLAIMED_AFTER_MS = 5_000;
const POLL_MS = 10_000;
/** The catchup window's clamp (`DefaultCatchupMin`, `DefaultCatchupMax`). */
const CATCHUP_MIN_MS = 2 * 60_000;
const CATCHUP_MAX_MS = 2 * 3_600_000;

/** What the ledger records about a fire (`schedule.Outcome`). */
export type FireOutcome = "fired" | "skipped_catchup" | "skipped_paused";

/** The label a fire is claimed under: its minute on the schedule's own clock (`schedule.FireLabel`). */
export function fireLabel(fire: Date, zone: string): string {
  const local = new Date(fire.getTime() + zoneOffset(fire.getTime(), zone));
  const two = (n: number) => String(n).padStart(2, "0");
  return `${String(local.getUTCFullYear()).padStart(4, "0")}${two(local.getUTCMonth() + 1)}${two(local.getUTCDate())}T${two(local.getUTCHours())}${two(local.getUTCMinutes())}`;
}

/** The ledger's order (`Ledger.Recent`): newest written first, then the fire's identity ascending. */
export function ledgerOrder(a: ScheduleRunRow, b: ScheduleRunRow): number {
  const by = (x: string, y: string) => (x < y ? -1 : x > y ? 1 : 0);
  return (
    by(b.fired_at, a.fired_at) ||
    by(a.scope_type, b.scope_type) ||
    by(a.scope_id, b.scope_id) ||
    by(a.schedule_name, b.schedule_name) ||
    by(a.fire_label, b.fire_label) ||
    by(a.target_handle, b.target_handle)
  );
}

/** The latest instant `cron` fires at in `zone`, at or before `at`, looking back up to two years. */
function fireAtOrBefore(cron: string, at: Date, zone: string): Date | null {
  for (const days of [1 / 24, 1, 8, 32, 367, 733]) {
    let from = new Date(at.getTime() - days * 86_400_000);
    let last: Date | null = null;
    for (;;) {
      const next = nextFires(cron, from, zone, 1)[0];
      if (!next || next.getTime() > at.getTime()) break;
      last = next;
      from = next;
    }
    if (last) return last;
  }
  return null;
}

/** RFC 3339 to the second, as the engine formats an instant in an event payload. */
function rfc3339(at: Date): string {
  return at.toISOString().replace(/\.\d{3}Z$/, "Z");
}

/**
 * What the scheduler writes for one fire of a schedule (`Scheduler.fire`,
 * `recordPaused`, `recordSkip`), the fire being the latest tick of its cron
 * at or before `due`:
 *
 * - `fired`: one ledger row per seat the schedule runs on, each claimed under
 *   its own trace, and the `scheduled_task_fired` event each dispatch
 *   publishes.
 * - `skipped_paused`: one row per seat, claimed and not dispatched, so it
 *   carries no trace.
 * - `skipped_catchup`: one row with no seat, written by the first tick after
 *   the engine came back just after the fire's catchup window closed (half
 *   the interval to the next fire, clamped to two minutes and two hours).
 *   Only a schedule that catches up records one, and only one whose fires are
 *   far enough apart for a restart to fall between the window and the next.
 *
 * Throws, naming why, for a fire the engine could not have recorded.
 */
export function fireRecords(
  row: ScheduleRow,
  outcome: FireOutcome,
  due: Date,
): { runs: ScheduleRunRow[]; events: EventRecord[] } {
  const what = `${row.scope_type} ${q(row.scope_name)}'s schedule ${q(row.name)}`;
  const tick = fireAtOrBefore(row.cron, due, row.timezone);
  if (!tick) throw new Error(`${what} has no fire in the two years before ${due.toISOString()}.`);
  const label = fireLabel(tick, row.timezone);
  const base = {
    scope_type: row.scope_type,
    scope_id: row.scope_id,
    scope_name: row.scope_name,
    schedule_name: row.name,
    fire_label: label,
    scheduled_at: tick.toISOString(),
  };
  if (outcome === "skipped_catchup") {
    if (!row.catchup) {
      throw new Error(`${what} does not catch up, so the engine records no catchup skip for it.`);
    }
    const [next, after] = nextFires(row.cron, tick, row.timezone, 2);
    const window =
      next && after
        ? Math.min(Math.max((after.getTime() - next.getTime()) / 2, CATCHUP_MIN_MS), CATCHUP_MAX_MS)
        : CATCHUP_MIN_MS;
    const written = new Date(tick.getTime() + window + POLL_MS);
    if (next && written.getTime() >= next.getTime()) {
      throw new Error(
        `${what} fires too often for a missed fire to be skipped: its catchup window reaches the next fire.`,
      );
    }
    return {
      runs: [
        { ...base, target_handle: "", fired_at: written.toISOString(), outcome, trace_id: "" },
      ],
      events: [],
    };
  }
  if (row.runners.length === 0) {
    throw new Error(`${what} reaches no seat, so a fire of it is never claimed.`);
  }
  const written = new Date(tick.getTime() + CLAIMED_AFTER_MS);
  const runs: ScheduleRunRow[] = [];
  const events: EventRecord[] = [];
  for (const handle of row.runners) {
    const key = `${row.scope_type}-${row.scope_id}-${row.name}-${label}-${handle}`;
    const trace = outcome === "fired" ? fixtureId(`schedule-trace-${key}`) : "";
    runs.push({
      ...base,
      target_handle: handle,
      fired_at: written.toISOString(),
      outcome,
      trace_id: trace,
    });
    if (outcome !== "fired") continue;
    events.push({
      id: fixtureId(`schedule-fired-${key}`),
      type: "scheduled_task_fired",
      timestamp: written.toISOString(),
      source: "scheduler",
      actor: "",
      summary: `Scheduled '${row.name}' fired for ${handle}`,
      category: "task",
      trace_id: trace,
      span_id: fixtureId(`schedule-span-${key}`).slice(0, 16),
      parent_span_id: "",
      topic: "crewlet.events.scheduled_task_fired",
      payload: {
        scope_type: row.scope_type,
        scope_id: row.scope_id,
        scope_name: row.scope_name,
        schedule_name: row.name,
        target_handle: handle,
        scheduled_at: rfc3339(tick),
      },
    });
  }
  return { runs, events };
}
