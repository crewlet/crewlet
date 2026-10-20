/**
 * Recurring work as the engine keeps it: every schedule row resolved by its
 * rules (who a fire reaches, the default cap, why there is no next fire), a
 * schedule or a person refused where the engine refuses the write, and every
 * fire a scenario records written as the scheduler writes it to the dispatch
 * ledger, read back by the same questions the schedules screens ask.
 *
 * At a fixed clock, Monday 2026-10-12 at 14:00 Pacific, so every fire's
 * label and instant can be named.
 */

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import type { ScheduleRow, ScheduleRunRow, SchedulesAnswer } from "~/protocol/types.ts";
import { fairsplit } from "../fixtures/fairsplit.ts";
import {
  addPerson,
  addSchedule,
  addTeam,
  compose,
  scheduleRan,
  type DemoCompany,
  type Step,
} from "../fixtures/scenarios.ts";
import { documentOf } from "../harness/engine/company.ts";
import { derive } from "../harness/engine/derive.ts";
import { DemoEngine } from "../harness/engine/engine.ts";
import { agentIdFor } from "../harness/engine/ids.ts";
import { DemoRefusal } from "../harness/engine/refusal.ts";

const NOW = new Date("2026-10-12T21:00:00Z");
/** The Product and Engineering team's key, which a unit's schedule is filed and named under. */
const TEAM = "product-and-engineering";

let warned: unknown[][];

beforeEach(() => {
  warned = [];
  vi.spyOn(console, "warn").mockImplementation((...args) => void warned.push(args));
});

afterEach(() => {
  vi.restoreAllMocks();
});

const base = () => fairsplit({ now: NOW });
/** An engine over the steps, reading the fixture's own instant as now, so a next fire is a fixed date. */
const engineOf = (...steps: Step[]) => {
  const engine = new DemoEngine(compose(...steps)(base()));
  engine.now = () => new Date(NOW);
  return engine;
};
const schedules = (engine: DemoEngine) =>
  engine.answer("schedules", {}) as unknown as SchedulesAnswer;
/** Pm's agent id: the identity a seat's schedule is keyed on. */
const PM_ID = agentIdFor("FairSplit", "pm");
/** A schedule row by the scope a person reads it under: a seat's handle, a unit's key. */
const rowOf = (engine: DemoEngine, scope: string, scopeName: string, name: string): ScheduleRow =>
  schedules(engine).schedules.find(
    (r) => r.scope_type === scope && r.scope_name === scopeName && r.name === name,
  )!;

/** The message a step is refused with. */
function refusedWith(...steps: Step[]): string {
  try {
    compose(...steps)(base());
  } catch (err) {
    return (err as Error).message;
  }
  throw new Error("the steps were applied");
}

describe("a schedule row", () => {
  test("of a unit reaches each direct agent unless it names its lead, capped at three minutes", () => {
    const engine = engineOf(
      addSchedule({ unit: TEAM }, { name: "retro", cron: "0 16 * * 5", task: "Run the retro." }),
      addSchedule(
        { unit: TEAM },
        { name: "review", cron: "0 15 * * 5", task: "Review the week.", target: "lead" },
      ),
    );
    expect(rowOf(engine, "unit", TEAM, "retro")).toMatchObject({
      target: "each",
      runners: ["pm", "engineer"],
      timeout_seconds: 180,
      catchup: true,
      timezone: "America/Los_Angeles",
      next_run: "2026-10-16T23:00:00.000Z",
    });
    expect(rowOf(engine, "unit", TEAM, "review")).toMatchObject({
      target: "lead",
      runners: ["pm"],
    });
    expect(rowOf(engine, "role", "pm", "daily-standup")).toMatchObject({
      target: "",
      runners: ["pm"],
      timeout_seconds: 180,
    });
  });

  test("whose lead is a person reaches nobody, which only a disabled schedule may", () => {
    const ops = addTeam({
      name: "Ops",
      lead: "miles",
      agents: [{ name: "Crewlet-Ops", handle: "ops" }],
    });
    const weekly = { name: "weekly", cron: "0 9 * * 1", task: "Plan the week.", target: "lead" };
    expect(refusedWith(ops, addSchedule({ unit: "Ops" }, weekly))).toContain(
      'units[1].schedules[0]: unit "Ops": schedule "weekly": schedule has no runner: it targets the unit lead, but the effective lead "Miles" is a human seat: define the schedule on an agent seat instead',
    );
    const engine = engineOf(ops, addSchedule({ unit: "Ops" }, { ...weekly, enabled: false }));
    expect(rowOf(engine, "unit", "ops", "weekly")).toMatchObject({ enabled: false, runners: [] });
  });

  test("says why an enabled schedule has no next fire", () => {
    const engine = engineOf(
      addSchedule({ seat: "pm" }, { name: "leap", cron: "0 0 30 2 *", task: "Never." }),
    );
    const row = rowOf(engine, "role", "pm", "leap");
    expect(row.next_run).toBeUndefined();
    expect(row.problem).toBe("no fire within 70200h0m0s");
  });
});

describe("a written schedule", () => {
  test("is refused in the engine's words where the engine refuses it", () => {
    const pm = { seat: "pm" };
    const add = (spec: Record<string, unknown>) =>
      addSchedule(pm, { name: "x", cron: "0 9 * * *", task: "Do it.", ...spec } as never);
    expect(refusedWith(add({ cron: "61 * * * *" }))).toContain(
      'units[0].roles[0].schedules[1].cron: role "Crewlet-PM": schedule "x": invalid schedule: invalid cron expression "61 * * * *": minute field: value 61 is outside 0-59',
    );
    expect(refusedWith(add({ cron: "0 9 * * 5-1" }))).toContain(
      'day-of-week field: range "5-1" is descending: ranges do not wrap, so a weekend is 6-7, sat,sun or 0,6',
    );
    expect(refusedWith(add({ cron: "/5 * * * *" }))).toContain(
      'minute field: "" is not a number or a name for this field',
    );
    expect(refusedWith(add({ cron: "0 9 * *" }))).toContain(
      'invalid schedule: cron "0 9 * *" needs 5 fields (minute hour day-of-month month day-of-week)',
    );
    expect(refusedWith(add({ timezone: "Mars/Base" }))).toContain(
      'timezone "Mars/Base" is not an IANA time zone (Europe/Berlin, America/New_York, UTC): unknown time zone Mars/Base',
    );
    expect(refusedWith(add({ timezone: "Local" }))).toContain(
      "a host's own clock is not a calendar every node shares",
    );
    expect(refusedWith(add({ task: " " }))).toContain("invalid schedule: task must not be empty");
    expect(refusedWith(add({ target: "engineer" }))).toContain(
      'target "engineer" must be each or lead: to run recurring work as one specific seat, define the schedule on that seat',
    );
    expect(refusedWith(add({ name: "daily-standup" }))).toContain(
      'units[0].roles[0].schedules: role "Crewlet-PM": invalid schedule: duplicate schedule name "daily-standup": 2 schedules carry it (schedules[0], schedules[1])',
    );
    expect(
      refusedWith(addSchedule({ seat: "miles" }, { name: "x", cron: "0 9 * * *", task: "Do it." })),
    ).toContain(
      'roles[0].schedules: role "Miles": agent-only field set on a human seat: schedules',
    );
    expect(
      refusedWith(addSchedule({ seat: "nobody" }, { name: "x", cron: "0 9 * * *", task: "x" })),
    ).toBe('addSchedule: there is no seat "nobody".');
    expect(
      refusedWith(addSchedule({ unit: "Nowhere" }, { name: "x", cron: "0 9 * * *", task: "x" })),
    ).toBe('addSchedule: there is no unit "Nowhere".');
  });

  test("of a unit with no agent of its own to fan out to is refused", () => {
    const people = addTeam({ name: "Board", lead: "Miles" });
    expect(
      refusedWith(
        people,
        addSchedule({ unit: "Board" }, { name: "minutes", cron: "0 9 1 * *", task: "Minute it." }),
      ),
    ).toContain(
      'unit "Board": schedule "minutes": schedule has no runner: target each fans out to direct agent members only (never descendants, never human seats), and this unit has none',
    );
  });

  test("a person with no chat contact is advice, never a refusal, through the dashboard's own config write", () => {
    const engine = engineOf();
    const etag = engine.rest({
      method: "GET",
      path: "/config",
      query: new URLSearchParams(),
      headers: new Headers(),
      body: null,
    }).headers?.ETag;
    const reply = engine.rest({
      method: "PATCH",
      path: "/config",
      query: new URLSearchParams(),
      headers: new Headers({ "If-Match": etag ?? "" }),
      body: JSON.stringify({ roles: [{ name: "Miles", kind: "human", handle: "miles" }] }),
    });
    expect(reply.status).toBe(201);
    expect((reply.body as { warnings: unknown[] }).warnings).toContainEqual(
      expect.objectContaining({
        path: "roles[0].contact",
        kind: "advisory",
        message:
          "miles is a human seat with no chat contact, so chat messages to it reach nobody. Add one, or leave it if they only use the dashboard",
        seat: "miles",
      }),
    );
  });
});

describe("the dispatch ledger", () => {
  test("holds the fixture's one missed stand-up as the engine would have skipped it", () => {
    expect(schedules(engineOf()).recent_runs).toEqual([
      {
        scope_type: "role",
        scope_id: PM_ID,
        scope_name: "pm",
        schedule_name: "daily-standup",
        fire_label: "20261009T0930",
        target_handle: "",
        scheduled_at: "2026-10-09T16:30:00.000Z",
        fired_at: "2026-10-09T18:30:10.000Z",
        outcome: "skipped_catchup",
        trace_id: "",
      },
    ]);
  });

  test("records a seat's fire for that seat under its own trace, and the event it publishes", () => {
    const engine = engineOf(scheduleRan({ seat: "pm" }, "daily-standup", "fired", 2));
    const fired = schedules(engine).recent_runs![0]!;
    expect(fired).toMatchObject({
      fire_label: "20261012T0930",
      target_handle: "pm",
      scheduled_at: "2026-10-12T16:30:00.000Z",
      fired_at: "2026-10-12T16:30:05.000Z",
      outcome: "fired",
    });
    expect(fired.trace_id).not.toBe("");
    const runs = engine.answer("schedule_runs", {
      scope_type: "role",
      scope_id: PM_ID,
      name: "daily-standup",
    });
    expect(runs).toMatchObject({ scope_id: PM_ID, scope_name: "pm" });
    expect(runs.runs.map((r: ScheduleRunRow) => r.outcome)).toEqual(["fired", "skipped_catchup"]);
    expect(runs.truncated).toBe(false);
    const event = engine.company.events.find((e) => e.type === "scheduled_task_fired");
    expect(event).toMatchObject({
      timestamp: "2026-10-12T16:30:05.000Z",
      source: "scheduler",
      category: "task",
      summary: "Scheduled 'daily-standup' fired for pm",
      trace_id: fired.trace_id,
      topic: "crewlet.events.scheduled_task_fired",
      payload: {
        scope_type: "role",
        scope_id: PM_ID,
        scope_name: "pm",
        schedule_name: "daily-standup",
        target_handle: "pm",
        scheduled_at: "2026-10-12T16:30:00Z",
      },
    });
  });

  test("fans a unit's fire out to each agent, or to its lead, in the ledger's order", () => {
    const each = engineOf(
      addSchedule({ unit: TEAM }, { name: "retro", cron: "0 16 * * 5", task: "Run the retro." }),
      scheduleRan({ unit: TEAM }, "retro", "fired", 0),
    );
    const retro = schedules(each).recent_runs!.filter((r) => r.schedule_name === "retro");
    expect(retro.map((r) => [r.target_handle, r.fire_label])).toEqual([
      ["engineer", "20261009T1600"],
      ["pm", "20261009T1600"],
    ]);
    expect(new Set(retro.map((r) => r.trace_id)).size).toBe(2);
    const lead = engineOf(
      addSchedule(
        { unit: TEAM },
        { name: "review", cron: "0 16 * * 5", task: "Review.", target: "lead" },
      ),
      scheduleRan({ unit: TEAM }, "review", "skipped_paused", 0),
    );
    expect(schedules(lead).recent_runs!.filter((r) => r.schedule_name === "review")).toMatchObject([
      { target_handle: "pm", outcome: "skipped_paused", trace_id: "" },
    ]);
    expect(lead.company.events.some((e) => e.type === "scheduled_task_fired")).toBe(false);
  });

  test("labels a fire on the schedule's own clock", () => {
    const engine = engineOf(
      addSchedule(
        { seat: "pm" },
        { name: "tokyo", cron: "30 9 * * *", task: "Greet Tokyo.", timezone: "Asia/Tokyo" },
      ),
      scheduleRan({ seat: "pm" }, "tokyo", "fired", 0),
    );
    expect(schedules(engine).recent_runs![0]).toMatchObject({
      schedule_name: "tokyo",
      fire_label: "20261012T0930",
      scheduled_at: "2026-10-12T00:30:00.000Z",
    });
  });

  test("records a catchup skip for no seat, after the fire's window", () => {
    const engine = engineOf(
      addSchedule({ seat: "pm" }, { name: "hourly", cron: "0 * * * *", task: "Sync." }),
      scheduleRan({ seat: "pm" }, "hourly", "skipped_catchup", 3),
    );
    expect(schedules(engine).recent_runs![0]).toMatchObject({
      schedule_name: "hourly",
      fire_label: "20261012T1100",
      target_handle: "",
      scheduled_at: "2026-10-12T18:00:00.000Z",
      fired_at: "2026-10-12T18:30:10.000Z",
      outcome: "skipped_catchup",
      trace_id: "",
    });
  });

  test("refuses a fire the engine could not have written", () => {
    const hourly = addSchedule(
      { seat: "pm" },
      { name: "hourly", cron: "0 * * * *", task: "Sync." },
    );
    expect(refusedWith(scheduleRan({ seat: "pm" }, "nightly", "fired", 1))).toBe(
      'scheduleRan: seat "pm" declares no schedule "nightly".',
    );
    expect(refusedWith(scheduleRan({ seat: "pm" }, "daily-standup", "fired", -1))).toBe(
      "scheduleRan: hoursAgo is how long ago the fire was due, zero or more.",
    );
    expect(refusedWith(hourly, scheduleRan({ seat: "pm" }, "hourly", "fired", 0))).toContain(
      "which is still to come",
    );
    expect(
      refusedWith(scheduleRan({ seat: "pm" }, "daily-standup", "skipped_catchup", 72)),
    ).toContain("is already recorded, and the ledger claims each fire once");
    expect(
      refusedWith(
        addSchedule(
          { seat: "pm" },
          { name: "strict", cron: "0 9 * * *", task: "x", catchup: false },
        ),
        scheduleRan({ seat: "pm" }, "strict", "skipped_catchup", 3),
      ),
    ).toContain("does not catch up, so the engine records no catchup skip for it");
    expect(
      refusedWith(
        addSchedule({ seat: "pm" }, { name: "minutely", cron: "* * * * *", task: "x" }),
        scheduleRan({ seat: "pm" }, "minutely", "skipped_catchup", 1),
      ),
    ).toContain("fires too often for a missed fire to be skipped");
  });

  test("pages one schedule's history as the engine does, and refuses a question without its identity", () => {
    const engine = engineOf(
      addSchedule({ seat: "pm" }, { name: "hourly", cron: "0 * * * *", task: "Sync." }),
      scheduleRan({ seat: "pm" }, "hourly", "fired", 1),
      scheduleRan({ seat: "pm" }, "hourly", "fired", 2),
      scheduleRan({ seat: "pm" }, "hourly", "fired", 3),
    );
    const page = (limit?: number) =>
      engine.answer("schedule_runs", {
        scope_type: "role",
        scope_id: PM_ID,
        name: "hourly",
        ...(limit === undefined ? {} : { limit }),
      });
    expect(page(2).runs.map((r: ScheduleRunRow) => r.fire_label)).toEqual([
      "20261012T1300",
      "20261012T1200",
    ]);
    expect(page(2).truncated).toBe(true);
    expect(page(5).truncated).toBe(false);
    expect(page().runs).toHaveLength(3);
    const refusal = (params: Record<string, unknown>) => {
      try {
        engine.answer("schedule_runs", params);
      } catch (err) {
        if (err instanceof DemoRefusal) return [err.code, err.detail];
        throw err;
      }
      throw new Error("the question was answered");
    };
    expect(refusal({ scope_id: "pm", name: "hourly" })).toEqual([
      "bad_params",
      "queries: bad parameters: scope_type is required",
    ]);
    expect(refusal({ scope_type: "team", scope_id: "pm", name: "hourly" })).toEqual([
      "bad_params",
      'queries: bad parameters: scope_type="team" (want one of role, unit)',
    ]);
    expect(refusal({ scope_type: "role", name: "hourly" })).toEqual([
      "bad_params",
      "queries: bad parameters: scope_id is required",
    ]);
    expect(refusal({ scope_type: "role", scope_id: "pm" })).toEqual([
      "bad_params",
      "queries: bad parameters: name is required",
    ]);
    warned = [];
  });
});

describe("a person", () => {
  test("joins a unit as a human seat with no live state", () => {
    const c: DemoCompany = compose(
      addPerson({
        name: "Sara",
        handle: "sara",
        unit: TEAM,
        availability: "Mornings, Pacific time",
        manages: ["engineer"],
      }),
    )(base());
    const doc = documentOf(c);
    expect(doc.units?.[0]?.roles?.at(-1)).toEqual({
      name: "Sara",
      kind: "human",
      handle: "sara",
      availability: "Mornings, Pacific time",
      manages: ["engineer"],
    });
    expect(c.config.revisions.at(-1)?.meta.summary).toBe("Added Sara");
    expect(derive(doc).seats?.find((s) => s.handle === "sara")).toMatchObject({
      kind: "human",
      unit_path: "units[0]",
    });
    expect(c.seats.sara).toBeUndefined();
  });

  test("is refused in a unit that is not there", () => {
    expect(refusedWith(addPerson({ name: "Sara", handle: "sara", unit: "Nowhere" }))).toBe(
      'addPerson: there is no unit "Nowhere".',
    );
  });
});

afterEach(() => {
  expect(warned).toEqual([]);
});
