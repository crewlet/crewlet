/**
 * Settings › Limits draws the engine's answer and nothing of its own: each
 * group a table named by its card, each cap its name, its key, its value and
 * where the value comes from.
 *
 * And what it WRITES: a cap the document carries is changed where it is read,
 * checked by the engine before it is stored, and one it does not carry is not
 * offered a change.
 */

import { act, cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import type { LimitsAnswer } from "~/contract/limits.ts";
import { CONFIG_WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { healthFrame } from "~/test/health.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import {
  engineAnswer,
  mountSettings,
  READER_VIEWER,
  savedAnswer,
  stubEngine,
  validAnswer,
} from "~/test/settingsScreen.tsx";
import { Limits, limitSource } from "./Limits.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

/** Mounts the page over one `limits` answer, read as `viewer`. */
async function show(limits: LimitsAnswer, viewer?: Record<string, unknown>) {
  const mounted = mountSettings(<Limits />, { limits }, { hash: "#/settings/limits", viewer });
  await act(afterMicrotasks);
  return mounted;
}

type Row = LimitsAnswer["limits"][number];

function row(over: Partial<Row> & Pick<Row, "key" | "group">): Row {
  return {
    unit: "count",
    value: 3,
    default: 3,
    off: false,
    default_off: false,
    settable: true,
    ...over,
  };
}

const LIMITS: LimitsAnswer = {
  limits: [
    row({ key: "turn_engine.max_iterations", group: "turn" }),
    row({ key: "turn_engine.onboarding_max_tool_rounds", group: "turn", value: 7, default: 10 }),
    row({
      key: "notification_rate_limit",
      group: "notifications",
      unit: "per_second",
      value: 0,
      default: 0,
      off: true,
      default_off: true,
    }),
    row({
      key: "schedules.timeout_seconds",
      group: "schedules",
      unit: "seconds",
      value: 180,
      default: 180,
      settable: false,
    }),
  ],
};

test("draws each group as a table named by its card, in the engine's order", async () => {
  await show(LIMITS);
  expect(screen.getAllByRole("table").map((t) => t.getAttribute("aria-labelledby"))).toEqual([
    "limits-turn",
    "limits-notifications",
    "limits-schedules",
  ]);
  expect(screen.getByRole("table", { name: "Guardrails on every turn" })).toBeDefined();
  expect(screen.getByRole("table", { name: "Schedules" })).toBeDefined();
});

test("each cap carries its name, its key, its value and its source", async () => {
  await show(LIMITS);
  const turn = screen.getByRole("table", { name: "Guardrails on every turn" });
  const changed = within(turn).getByRole("row", { name: /Tool rounds on a first turn/ });
  expect(within(changed).getByText("turn_engine.onboarding_max_tool_rounds")).toBeDefined();
  expect(
    within(changed)
      .getAllByRole("cell")
      .map((c) => c.textContent),
  ).toEqual(["7", "Changed from 10"]);
  // ONLY THE CHANGED ROW IS MARKED; a default says so in a word, never a tag.
  const resting = within(turn).getByRole("row", { name: /Review rounds per turn/ });
  expect(resting.hasAttribute("data-changed")).toBe(false);
  expect(changed.hasAttribute("data-changed")).toBe(true);
  expect(
    within(resting)
      .getAllByRole("cell")
      .map((c) => c.textContent),
  ).toEqual(["3", "Default"]);
});

test("a cap whose value switches it off says Off, as a word", async () => {
  await show(LIMITS);
  const wakeUps = screen.getByRole("row", { name: /Wake-ups per seat/ });
  expect(
    within(wakeUps)
      .getAllByRole("cell")
      .map((c) => c.textContent),
  ).toEqual(["Off", "Default"]);
});

test("a set value names the default it replaced, Off where the default is no limit", () => {
  expect(
    limitSource(
      row({
        key: "notification_rate_limit",
        group: "notifications",
        unit: "per_second",
        value: 2,
        default: 0,
        default_off: true,
      }),
    ),
  ).toEqual({ changed: true, words: "Changed from Off" });
  expect(
    limitSource(
      row({
        key: "turn_engine.delegation.task_timeout_seconds",
        group: "delegation",
        unit: "seconds",
        value: 600,
        default: 300,
      }),
    ),
  ).toEqual({ changed: true, words: "Changed from 5 minutes" });
  expect(limitSource(row({ key: "x", group: "turn" }))).toEqual({
    changed: false,
    words: "Default",
  });
});

test("a node running no company says why there is nothing to show", async () => {
  await show({ limits: [] });
  expect(screen.getByText("No limits to show")).toBeDefined();
  expect(screen.queryAllByRole("table")).toHaveLength(0);
});

// ---------------------------------------------------------------------------
// Changing a cap
// ---------------------------------------------------------------------------

const DOC = { name: "Acme", turn_engine: { onboarding_max_tool_rounds: 7 } };

/** Opens one cap's field, types `value` and submits it. */
async function change(name: RegExp, value: string) {
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name }));
  });
  const field = screen.getByRole("textbox");
  fireEvent.change(field, { target: { value } });
  await act(async () => {
    fireEvent.submit(field.closest("form")!);
  });
  await act(afterMicrotasks);
}

const sent = (calls: { method: string; path: string }[]) =>
  calls.map((c) => `${c.method} ${c.path.replace(/^.*\/config/, "/config")}`);

// READ, CHECKED, THEN SAVED, on the revision the read named: the patch names
// the one key, so a colleague's change to anything else survives it, and the
// summary says what was set for the revision history.
test("a cap is checked by the engine, then saved as a patch of its one key", async () => {
  const calls = stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(DOC, 200, { ETag: '"r1"' });
    if (call.path.includes("dry_run=true")) return validAnswer();
    return savedAnswer();
  });
  await show(LIMITS);
  await change(/Change Review rounds per turn/, "5");

  expect(sent(calls)).toEqual([
    "GET /config",
    "PATCH /config?dry_run=true",
    "PATCH /config?dry_run=true",
    "PATCH /config",
  ]);
  const save = calls[3]!;
  expect(save.headers.get("If-Match")).toBe('"r1"');
  expect(save.body).toEqual({
    turn_engine: { max_iterations: 5 },
    _summary: "Limits: Review rounds per turn set to 5 (turn_engine.max_iterations)",
  });
  // THE SAVED VALUE, MARKED AS APPLYING: the answer the page draws is what
  // this node applied, which is still the value before.
  const row = screen.getByRole("row", { name: /Review rounds per turn/ });
  expect(within(row).getByText("5")).toBeTruthy();
  expect(within(row).getByRole("status").textContent).toBe("applying…");
  expect(screen.queryByRole("textbox")).toBeNull();
});

// ONCE THIS NODE APPLIED IT the page re-reads, and the mark goes when the
// answer carries the value.
test("the applying mark goes once this node applied the revision", async () => {
  stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(DOC, 200, { ETag: '"r1"' });
    if (call.path.includes("dry_run=true")) return validAnswer();
    return savedAnswer();
  });
  let served = LIMITS;
  const { store, asked } = mountSettings(
    <Limits />,
    { limits: () => served },
    { hash: "#/settings/limits" },
  );
  await act(afterMicrotasks);
  await change(/Change Review rounds per turn/, "5");
  const before = asked.filter((q) => q === "limits").length;
  served = {
    limits: LIMITS.limits.map((l) =>
      l.key === "turn_engine.max_iterations" ? { ...l, value: 5 } : l,
    ),
  };
  await act(async () => {
    store.applyHealth(healthFrame({ applied_epoch: 5 }));
  });
  await act(afterMicrotasks);
  expect(asked.filter((q) => q === "limits").length).toBeGreaterThan(before);
  const row = screen.getByRole("row", { name: /Review rounds per turn/ });
  expect(within(row).getByText("5")).toBeTruthy();
  expect(within(row).queryByRole("status")).toBeNull();
  expect(within(row).getByText("Changed from 3")).toBeTruthy();
});

// THE DEFAULT IS THE KEY'S ABSENCE: an emptied field removes the key rather
// than writing today's default over it.
test("an emptied field removes the key, which is the default", async () => {
  const calls = stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(DOC, 200, { ETag: '"r1"' });
    if (call.path.includes("dry_run=true")) return validAnswer();
    return savedAnswer();
  });
  await show(LIMITS);
  await change(/Change Tool rounds on a first turn/, "");
  expect(calls[3]!.body).toEqual({
    turn_engine: { onboarding_max_tool_rounds: null },
    _summary:
      "Limits: Tool rounds on a first turn back to the default (turn_engine.onboarding_max_tool_rounds)",
  });
});

test("a value the document already holds is not saved again", async () => {
  const calls = stubEngine(() => engineAnswer(DOC, 200, { ETag: '"r1"' }));
  await show(LIMITS);
  await change(/Change Tool rounds on a first turn/, "7");
  expect(sent(calls)).toEqual(["GET /config"]);
  expect(screen.getByText("Already the limit.")).toBeTruthy();
});

// THE ENGINE'S OWN WORDS. Whether a number is one the company can run is the
// engine's rule: the check refuses it, the refusal is drawn as written, and
// no save is sent.
test("a value the engine refuses is said in its words, and nothing is saved", async () => {
  const refusal =
    "turn_engine.onboarding_max_tool_rounds_ceiling: must be at least onboarding_max_tool_rounds (40), got 20";
  const calls = stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(DOC, 200, { ETag: '"r1"' });
    return engineAnswer(
      {
        error: "validation_error",
        detail: refusal,
        problems: [
          {
            path: "turn_engine.onboarding_max_tool_rounds_ceiling",
            segments: ["turn_engine", "onboarding_max_tool_rounds_ceiling"],
            kind: "out_of_range",
            message: refusal,
          },
        ],
      },
      400,
    );
  });
  await show(LIMITS);
  await change(/Change Tool rounds on a first turn/, "40");
  expect(screen.getByText(refusal)).toBeTruthy();
  expect(calls.filter((c) => c.method === "PATCH" && !c.path.includes("dry_run"))).toEqual([]);
  // The field stays open on what was typed.
  expect((screen.getByRole("textbox") as HTMLInputElement).value).toBe("40");
});

// A COLLEAGUE'S SAVE IN BETWEEN is a conflict to re-read, never an overwrite.
test("a revision that moved since the read offers a reload", async () => {
  stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(DOC, 200, { ETag: '"r1"' });
    return engineAnswer({ error: "revision_advanced", detail: "the revision moved" }, 412);
  });
  await show(LIMITS);
  await change(/Change Review rounds per turn/, "5");
  expect(screen.getByText(/changed since this was read, so nothing was saved/)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Reload" })).toBeTruthy();
});

test("what is typed is held to the cap's unit before any request", async () => {
  const calls = stubEngine(() => engineAnswer(DOC, 200, { ETag: '"r1"' }));
  await show(LIMITS);
  await change(/Change Review rounds per turn/, "2.5");
  expect(calls).toEqual([]);
  // THE FIELD IS REFUSED, in its own line under it, before anything is sent.
  expect(screen.getByRole("textbox").getAttribute("aria-invalid")).toBe("true");
  expect(screen.getAllByText(/A whole number\./).length).toBeGreaterThan(1);
});

// A CAP NO DOCUMENT CARRIES IS NOT OFFERED A CHANGE: the engine says which
// those are (`settable`), and a patch of one would be refused as an unknown
// field.
test("the engine's constant has no pencil", async () => {
  await show(LIMITS);
  const row = screen.getByRole("row", { name: /A scheduled turn's time limit/ });
  expect(within(row).queryByRole("button")).toBeNull();
});

// NEVER HIDDEN: a reader who cannot write the document sees every pencil,
// disabled with the reason.
test("a reader without the grant sees the pencils disabled with the reason", async () => {
  await show(LIMITS, READER_VIEWER);
  const pencil = screen.getByRole("button", { name: /Change Review rounds per turn/ });
  expect(
    pencil.getAttribute("aria-disabled") ?? String((pencil as HTMLButtonElement).disabled),
  ).toBe("true");
  expect(pencil.getAttribute("title")).toBe(CONFIG_WRITE_REASONS.no_grant);
});
