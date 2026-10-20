/**
 * `work_items` answers as the engine's grammar does: a parameter it does not
 * read is refused rather than ignored, a preset's defaults sit under what the
 * caller asked, finished work is in an answer only when asked for, a column
 * narrowing is a column and never an axis, and a board's columns, its totals
 * and a task's neighbours are all drawn from one matched set.
 */

import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import type { WorkItemsAnswer } from "~/protocol/types.ts";
import { expensiveTasksParams, medianTaskParams } from "~/routes/spend/model.ts";
import { fairsplit } from "../fixtures/fairsplit.ts";
import { DemoEngine } from "../harness/engine/engine.ts";
import { DemoRefusal } from "../harness/engine/refusal.ts";

let engine: DemoEngine;
let warned: unknown[][];

beforeEach(() => {
  engine = new DemoEngine(fairsplit());
  warned = [];
  vi.spyOn(console, "warn").mockImplementation((...args) => void warned.push(args));
});

afterEach(() => {
  vi.restoreAllMocks();
});

const FAIR = { container: "project:FAIR" };
const OPEN = ["FAIR-1", "FAIR-2", "FAIR-3", "FAIR-4", "FAIR-5", "FAIR-6"];

const items = (params: Record<string, unknown>): WorkItemsAnswer =>
  engine.answer("work_items", params);
const keys = (params: Record<string, unknown>) => items(params).items.map((i) => i.key);

function refusal(params: Record<string, unknown>): DemoRefusal {
  try {
    items(params);
  } catch (err) {
    if (err instanceof DemoRefusal) return err;
    throw err;
  }
  throw new Error("the query was answered");
}

function act(tool: string, args: Record<string, unknown>) {
  return engine.rest({
    method: "POST",
    path: `/operator/act/${tool}`,
    query: new URLSearchParams(),
    headers: new Headers(),
    body: JSON.stringify({ request_id: "0192f0c4-0000-7000-8000-000000000000", args }),
  });
}

describe("what the grammar refuses", () => {
  test("a parameter it does not read, named, rather than a wider answer", () => {
    const refused = refusal({ ...FAIR, open: "true" });
    expect(refused.code).toBe("bad_params");
    expect(refused.detail).toMatch(/"open" is not a query parameter/);
  });

  test("a value it does not read", () => {
    expect(refusal({ status: "shipped" }).detail).toMatch(/six statuses/);
    expect(refusal({ sort: "colour" }).detail).toMatch(/not a sort key/);
    expect(refusal({ flag: "orphan" }).detail).toMatch(/not an attention flag/);
    expect(refusal({ updated: "soon" }).detail).toMatch(/names no comparison/);
    expect(refusal({ group: "todo" }).detail).toMatch(/without group_by/);
    expect(refusal({ closed_since: "sow", show_closed: "true" }).detail).toMatch(/both say/);
    expect(refusal({ any: JSON.stringify([{ limit: 1 }]) }).detail).toMatch(/about the answer/);
  });
});

describe("finished work", () => {
  test("is in an answer only when asked for, however it is named", () => {
    expect(keys({ ...FAIR, status_group: "done" })).toEqual([]);
    expect(keys({ ...FAIR, status_group: "done", show_closed: "true" })).toEqual(["FAIR-7"]);
    expect(keys({ ...FAIR, show_closed: true })).toEqual([...OPEN, "FAIR-7"]);
    // FAIR-7 finished two days ago: inside a window from three days back, outside one from yesterday.
    expect(keys({ ...FAIR, closed_since: "-1d" })).toEqual(OPEN);
    expect(keys({ ...FAIR, closed_since: "-3d" })).toEqual([...OPEN, "FAIR-7"]);
  });

  test("is stamped by group on the way in and cleared by a reopen", () => {
    act("update_work_item", { item: "FAIR-4", status: "cancelled" });
    const finished = { ...FAIR, finished: "range:today..tomorrow", show_closed: "true" };
    expect(keys(finished)).toEqual(["FAIR-4"]);
    act("update_work_item", { item: "FAIR-4", status: "todo" });
    expect(keys(finished)).toEqual([]);
  });
});

describe("presets and the viewer", () => {
  test("a preset's defaults sit under the caller's own parameters", () => {
    expect(keys({ ...FAIR, preset: "triage" })).toEqual(["FAIR-5"]);
    const mine = items({ ...FAIR, preset: "triage", assignee: "engineer" });
    expect(mine.preset).toBe("triage");
    expect(new Set(mine.items.map((i) => i.key))).toEqual(
      new Set(["FAIR-2", "FAIR-3", "FAIR-4", "FAIR-6"]),
    );
  });

  test("my queue is the viewer's own unblocked work and what is unclaimed in their project", () => {
    expect(refusal({ preset: "my_queue" }).detail).toMatch(/names nobody/);
    const queue = keys({ preset: "my_queue", viewer: "engineer" });
    expect(new Set(queue)).toEqual(new Set(["FAIR-2", "FAIR-4", "FAIR-5", "FAIR-6"]));
    expect(queue[0]).toBe("FAIR-2");
    expect(queue.at(-1)).toBe("FAIR-6");
  });
});

describe("a board", () => {
  test("narrowed to a column is that column, not an axis named after it", () => {
    const column = items({ ...FAIR, group_by: "status", group: "in_review" });
    expect(column.groups?.map((g) => [g.key, g.count, g.rows.map((r) => r.key)])).toEqual([
      ["in_review", 1, ["FAIR-2"]],
    ]);
    expect(column.total_hint).toBe(1);
  });

  test("draws every admitted column, counts each whole and carries group_limit rows", () => {
    const board = items({ ...FAIR, group_by: "status", group_limit: 2 });
    expect(board.groups?.map((g) => [g.key, g.count, g.rows.length])).toEqual([
      ["todo", 4, 2],
      ["in_progress", 1, 1],
      ["in_review", 1, 1],
    ]);
    const all = items({ ...FAIR, group_by: "status", show_closed: "true" });
    expect(all.groups?.map((g) => [g.key, g.count])).toEqual([
      ["todo", 4],
      ["in_progress", 1],
      ["in_review", 1],
      ["done", 1],
      ["cancelled", 0],
      ["closed", 0],
    ]);
  });

  test("a task's neighbours are read in the order the board draws", () => {
    expect(items({ ...FAIR, around: "FAIR-3" }).around).toEqual({
      position: 3,
      prev: "FAIR-2",
      next: "FAIR-4",
      total_hint: 6,
    });
    expect(items({ ...FAIR, group_by: "status", around: "FAIR-1" }).around).toEqual({
      position: 5,
      prev: "FAIR-6",
      next: "FAIR-2",
      total_hint: 6,
    });
    expect(items({ ...FAIR, around: "FAIR-7" }).around).toBeNull();
  });
});

describe("what the Spend screen asks", () => {
  test("work with spend that changed or finished in a window, its totals over the whole set", () => {
    const changed = items({
      closed_since: "-7d",
      updated: "range:-7d..tomorrow",
      spend_tokens: "gt:0",
      sort: "-spend_tokens",
      totals: "spend_tokens:median,spend_tokens:sum,spend_tokens:avg,tasks:count",
    });
    expect(changed.items.map((i) => i.key)).toEqual(["FAIR-2", "FAIR-1", "FAIR-7"]);
    // The nearest rank of three is the second, never the mean of them.
    expect(changed.totals?.map((t) => t.value)).toEqual([380_000, 1_034_800, 1_034_800 / 3, 3]);
    const finished = items({
      finished: "range:-7d..tomorrow",
      spend_tokens: "gt:0",
      show_closed: "true",
      totals: "spend_tokens:median,spend_tokens:count,finished_at:max",
    });
    expect(finished.items.map((i) => i.key)).toEqual(["FAIR-7"]);
    expect(finished.totals?.map((t) => [t.key, t.value ?? t.at?.slice(0, 10)])).toEqual([
      ["spend_tokens:median", 142_800],
      ["spend_tokens:count", 1],
      ["finished_at:max", engine.company.work.items[6]?.task.done_at?.slice(0, 10)],
    ]);
  });
});

describe("what the Spend screen asks, built by the screen's own code", () => {
  test("over the window its spend answer carries, instants written to the millisecond", () => {
    const { since, until } = engine.answer("tokens", { days: 30 });
    expect(since).toMatch(/\.\d{3}Z$/);
    expect(keys(expensiveTasksParams(since, until, 10))).toEqual(["FAIR-2", "FAIR-1", "FAIR-7"]);
    expect(items(medianTaskParams(since, until)).totals?.map((t) => t.value)).toEqual([142_800, 1]);
    expect(warned).toEqual([]);
  });
});

describe("subtasks", () => {
  test("ride along with their root unless every row is filtered on its own", () => {
    act("create_work_item", { project: "FAIR", title: "Write the glossary", parent: "FAIR-1" });
    act("update_work_item", { item: "FAIR-8", status: "done" });
    expect(keys(FAIR)).toContain("FAIR-8");
    expect(keys({ ...FAIR, subtasks: "separate" })).toEqual(OPEN);
    expect(keys({ ...FAIR, parent: engine.company.work.items[0]!.task.id })).toEqual([]);
  });
});

describe("paging", () => {
  test("a cursor resumes the same order, and is refused on another", () => {
    const first = items({ ...FAIR, limit: 2 });
    expect(first.items.map((i) => i.key)).toEqual(["FAIR-1", "FAIR-2"]);
    expect(first.total_hint).toBe(6);
    const second = items({ ...FAIR, limit: 2, cursor: first.next_cursor });
    expect(second.items.map((i) => i.key)).toEqual(["FAIR-3", "FAIR-4"]);
    expect(refusal({ ...FAIR, limit: 2, cursor: first.next_cursor, sort: "title" }).code).toBe(
      "bad_params",
    );
  });
});

describe("what the demo company holds no data for", () => {
  test("a custom field is refused unless declared, and a declared one is said to be left out", () => {
    expect(refusal({ ...FAIR, "f.effort": "gt:1" }).detail).toMatch(/names no field/);
    expect(String(warned[0]?.[1])).toMatch(/was refused bad_params/);
    warned = [];
    engine.company.work.fields.push({
      id: "0192f0c4-0000-7000-8000-00000000f1e1",
      slug: "effort",
      name: "Effort",
      type: "number",
    });
    expect(keys({ ...FAIR, "f.effort": "gt:1" })).toEqual(OPEN);
    expect(warned).toHaveLength(1);
    expect(String(warned[0]?.[1])).toMatch(/"f\.effort" is not modelled/);
  });
});

describe("freshness", () => {
  test("is served at the level asked for, the dashboard's own default when none was", () => {
    expect(items(FAIR).read_level).toBe("stale");
    expect(items({ ...FAIR, read_level: "linearizable" }).read_level).toBe("linearizable");
    expect(refusal({ ...FAIR, read_level: "session" }).detail).toMatch(/min_position/);
  });
});
