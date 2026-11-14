/**
 * A schedule changed from the dashboard is a write of the ONE entity that
 * declares it, its `schedules` changed and nothing else, addressed as the
 * configuration routes address it, under the revision it was read from.
 */

import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";
import {
  NEW_DRAFT,
  applyChange,
  draftOf,
  draftProblems,
  locatedProblems,
  ownerOf,
  writtenOf,
  type ScheduleOwner,
} from "./scheduleWrite.ts";
import { useScheduleWrite } from "./useScheduleWrite.ts";
import { runHold } from "./ScheduleControls.tsx";

afterEach(() => {
  vi.unstubAllGlobals();
});

const core: ScheduleOwner = { scope: "unit", id: "core" };

/** The unit `core` as its route answers it: a seat and a child unit inside, two schedules. */
function coreUnit(): Record<string, unknown> {
  return {
    name: "Core",
    id: "core",
    lead: "lead",
    roles: [
      { name: "Lead", handle: "lead", schedules: [{ name: "own", cron: "0 8 * * *", task: "x" }] },
    ],
    children: [{ name: "Infra", id: "infra" }],
    schedules: [
      { name: "daily-standup", cron: "30 9 * * 1-5", task: "Post your standup." },
      {
        name: "weekly-report",
        cron: "0 16 * * 5",
        task: "Sum up the week.",
        target: "lead",
        timezone: "Asia/Tokyo",
        timeout_seconds: 90,
        future_field: "kept",
      },
    ],
  };
}

// THE ROUTE ADDRESSES A SEAT BY ITS HANDLE, never by the agent id a
// `/schedules` row carries as its scope's identity: a write sent to the id
// would be refused as a seat the company does not have. A unit is its key
// either way. Mutation: address a role by `scope_id`.
test("a seat is written by its handle and a unit by its key", () => {
  expect(
    ownerOf({
      scope_type: "role",
      scope_id: "0b6f6c1e-5d1a-5c8e-9a51-2f0c3e7a9b10",
      scope_name: "pm",
    }),
  ).toEqual({ scope: "role", id: "pm" });
  expect(ownerOf({ scope_type: "unit", scope_id: "core", scope_name: "core" })).toEqual(core);
});

// A UNIT GOES BACK WHOLE, its seats, children and other schedules as they
// came, and a field a newer build wrote on the schedule survives the edit.
test("an edit changes the one schedule's fields and nothing else of the entity", () => {
  const entity = coreUnit();
  const written = (entity.schedules as Record<string, unknown>[])[1]!;
  const draft = { ...draftOf(written), cron: "0 17 * * 5" };
  const applied = applyChange(entity, core, { kind: "edit", name: "weekly-report", draft });
  if (applied.kind !== "changed") throw new Error(`not changed: ${JSON.stringify(applied)}`);
  expect(applied.entity.roles).toEqual(entity.roles);
  expect(applied.entity.children).toEqual(entity.children);
  const schedules = applied.entity.schedules as Record<string, unknown>[];
  expect(schedules[0]).toEqual((entity.schedules as unknown[])[0]);
  // THE CAP AS IT WAS: 90 seconds, drawn as 1.5 minutes and not touched.
  expect(schedules[1]).toEqual({ ...written, cron: "0 17 * * 5" });
  expect(applied.summary).toBe(
    "Change the schedule “weekly-report” on the unit core: when it runs",
  );
  expect(applied.index).toBe(1);
});

// A DEFAULT STAYS UNWRITTEN: a schedule that never said `enabled`, `catchup`
// or `target` does not start saying it because the form drew the default.
test("a new schedule writes what the form says and leaves the defaults unsaid", () => {
  const draft = { ...NEW_DRAFT, name: " nightly ", cron: "0  2 * * *", task: "Tidy up. " };
  expect(writtenOf(draft, "unit")).toEqual({
    name: "nightly",
    cron: "0 2 * * *",
    task: "Tidy up.",
  });
  expect(
    writtenOf(
      { ...draft, timezone: "Europe/Berlin", target: "lead", catchup: false, timeoutMinutes: "45" },
      "unit",
    ),
  ).toEqual({
    name: "nightly",
    cron: "0 2 * * *",
    task: "Tidy up.",
    timezone: "Europe/Berlin",
    target: "lead",
    catchup: false,
    timeout_seconds: 2700,
  });
  // A SEAT'S SCHEDULE HAS NO TARGET.
  expect(writtenOf({ ...draft, target: "lead" }, "role")).not.toHaveProperty("target");
});

// PAUSE AND RESUME ARE THE DOCUMENT'S OWN SWITCH, and nothing else moves.
test("pause is enabled false, resume enabled true, and a second press changes nothing", () => {
  const paused = applyChange(coreUnit(), core, {
    kind: "enable",
    name: "daily-standup",
    enabled: false,
  });
  if (paused.kind !== "changed") throw new Error("not changed");
  expect((paused.entity.schedules as Record<string, unknown>[])[0]).toEqual({
    name: "daily-standup",
    cron: "30 9 * * 1-5",
    task: "Post your standup.",
    enabled: false,
  });
  expect(paused.summary).toBe("Pause the schedule “daily-standup” on the unit core");
  const again = applyChange(paused.entity, core, {
    kind: "enable",
    name: "daily-standup",
    enabled: false,
  });
  expect(again.kind).toBe("unchanged");
  const resumed = applyChange(paused.entity, core, {
    kind: "enable",
    name: "daily-standup",
    enabled: true,
  });
  if (resumed.kind !== "changed") throw new Error("not changed");
  expect(resumed.summary).toBe("Resume the schedule “daily-standup” on the unit core");
});

test("a removal drops the one schedule, and the last one takes the list with it", () => {
  const removed = applyChange(coreUnit(), core, { kind: "remove", name: "daily-standup" });
  if (removed.kind !== "changed") throw new Error("not changed");
  expect((removed.entity.schedules as { name: string }[]).map((s) => s.name)).toEqual([
    "weekly-report",
  ]);
  const last = applyChange(removed.entity, core, { kind: "remove", name: "weekly-report" });
  if (last.kind !== "changed") throw new Error("not changed");
  expect(last.entity).not.toHaveProperty("schedules");
  expect(last.summary).toBe("Remove the schedule “weekly-report” from the unit core");
});

test("a schedule gone from the document, or a name already taken, is refused before anything is sent", () => {
  expect(applyChange(coreUnit(), core, { kind: "remove", name: "nope" }).kind).toBe("refused");
  const dup = applyChange(coreUnit(), core, {
    kind: "create",
    draft: { ...NEW_DRAFT, name: "daily-standup", cron: "0 9 * * *", task: "x" },
  });
  expect(dup.kind).toBe("refused");
});

describe("what the form refuses before anything is sent", () => {
  test("each field says what it needs", () => {
    const problems = draftProblems(
      {
        ...NEW_DRAFT,
        name: "daily-standup",
        cron: "61 * * * *",
        timezone: "Mars/Olympus",
        timeoutMinutes: "soon",
      },
      ["daily-standup"],
    );
    expect(Object.keys(problems).sort()).toEqual(["cron", "name", "task", "timeout", "timezone"]);
    expect(draftProblems({ ...NEW_DRAFT, name: "n", cron: "0 9 * * 1-5", task: "t" }, [])).toEqual(
      {},
    );
  });
});

// THE ENGINE'S PROBLEM IS DRAWN BESIDE ITS FIELD where it is about the
// changed schedule, by its segments, and only on the owner it names: a
// seat's schedule inside the unit at the same index is somebody else's.
test("a located problem lands on the field of the changed schedule, and the rest stay whole", () => {
  const { fields, general } = locatedProblems(
    [
      {
        path: "units[0].schedules[1].cron",
        segments: ["units", 0, "schedules", 1, "cron"],
        kind: "invalid",
        message: "units[0].schedules[1].cron: bad minute 61",
        unit: "Core",
      },
      {
        path: "units[0].roles[0].schedules[1].cron",
        segments: ["units", 0, "roles", 0, "schedules", 1, "cron"],
        kind: "invalid",
        message: "a seat's own problem",
        seat: "lead",
      },
      { path: "", segments: null, kind: "invalid", message: "the document's own problem" },
    ],
    core,
    1,
    "Core",
  );
  expect(fields).toEqual({ cron: "units[0].schedules[1].cron: bad minute 61" });
  expect(general).toEqual(["a seat's own problem", "the document's own problem"]);
});

// RUN NOW IS HELD FOR A SCHEDULE THAT WOULD BE REFUSED, and for a reader who
// may not wake every seat it reaches, in words that say why.
test("run now is held on a paused schedule, one nobody runs, and a seat the reader does not lead", () => {
  const all = () => true;
  expect(runHold({ enabled: true, runners: ["a", "b"] }, all)).toBeUndefined();
  expect(runHold({ enabled: false, runners: ["a"] }, all)).toMatch(/Paused/);
  expect(runHold({ enabled: true, runners: [] }, all)).toMatch(/Nobody/);
  expect(runHold({ enabled: true, runners: ["a", "b"] }, (h) => h === "a")).toMatch(
    /leading every seat/,
  );
});

const json = (payload: unknown, status: number, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { ...ENGINE_HEADERS, "Content-Type": "application/json", ...headers },
  });

/** The configuration routes for one unit, recording every request. */
function stubUnitRoutes(dryRun: (body: Record<string, unknown>) => Response) {
  const calls: {
    method: string;
    url: string;
    headers: Headers;
    body: Record<string, unknown> | null;
  }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      const method = init.method ?? "GET";
      const body =
        typeof init.body === "string" ? (JSON.parse(init.body) as Record<string, unknown>) : null;
      calls.push({ method, url, headers: new Headers(init.headers), body });
      if (method === "GET") return json(coreUnit(), 200, { ETag: '"rev-7"' });
      if (url.includes("dry_run=true")) return dryRun(body!);
      return json({ revision_id: "rev-8", epoch: 8 }, 201);
    }),
  );
  return calls;
}

// THE WRITE IS THE ENTITY'S, UNDER THE TAG IT WAS READ WITH: read, checked,
// then stored with the summary, at the unit's own address.
test("a pause reads the unit, checks it, and stores it under the revision it read", async () => {
  const calls = stubUnitRoutes(() => json({ base_revision_id: "rev-7", warnings: [] }, 200));
  const saved = vi.fn();
  const { result } = renderHook(() => useScheduleWrite(core, { onSaved: saved }));
  act(() => result.current.submit({ kind: "enable", name: "daily-standup", enabled: false }));
  await waitFor(() => expect(result.current.state.kind).toBe("saved"));
  const put = calls.find((c) => c.method === "PUT" && !c.url.includes("dry_run"))!;
  expect(put.url).toContain("/config/units/core");
  expect(put.headers.get("If-Match")).toBe('"rev-7"');
  expect(put.body?._summary).toBe("Pause the schedule “daily-standup” on the unit core");
  const { _summary: _, ...entity } = put.body!;
  expect(entity).toEqual({
    ...coreUnit(),
    schedules: [
      { name: "daily-standup", cron: "30 9 * * 1-5", task: "Post your standup.", enabled: false },
      (coreUnit().schedules as unknown[])[1],
    ],
  });
  expect(saved).toHaveBeenCalledWith("Paused “daily-standup”");
});

// A REFUSED CHECK STORES NOTHING, and says where.
test("a change the engine's check refuses is not stored, its problem beside its field", async () => {
  const calls = stubUnitRoutes((body) =>
    (body.schedules as unknown[]).length > 2
      ? json(
          {
            error: "invalid_config",
            problems: [
              {
                path: "units[0].schedules[2].cron",
                segments: ["units", 0, "schedules", 2, "cron"],
                kind: "invalid",
                message: "units[0].schedules[2].cron: 61 is not a minute",
                unit: "Core",
              },
            ],
          },
          422,
        )
      : json({ base_revision_id: "rev-7", warnings: [] }, 200),
  );
  const { result } = renderHook(() => useScheduleWrite(core));
  act(() =>
    result.current.submit({
      kind: "create",
      draft: { ...NEW_DRAFT, name: "nightly", cron: "61 2 * * *", task: "Tidy up." },
    }),
  );
  await waitFor(() => expect(result.current.state.kind).toBe("problems"));
  const state = result.current.state;
  if (state.kind !== "problems") throw new Error(state.kind);
  expect(state.fields.cron).toMatch(/61 is not a minute/);
  expect(calls.some((c) => c.method === "PUT" && !c.url.includes("dry_run"))).toBe(false);
});
