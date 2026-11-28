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
import { toastText } from "~/testing.tsx";
import { APPLIED_SENTENCE, APPLYING_SENTENCE } from "~/lib/savedToast.ts";
import {
  engineAnswer,
  mountSettings,
  READER_VIEWER,
  savedAnswer,
  stubEngine,
  validAnswer,
} from "~/test/settingsScreen.tsx";
import { limitSource } from "~/components/LimitSetting.tsx";
import { Limits } from "./Limits.tsx";

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
    row({ key: "turn_engine.max_iterations", group: "turn", range: { min: 1 } }),
    row({
      key: "turn_engine.onboarding_max_tool_rounds",
      group: "turn",
      value: 7,
      default: 10,
      range: { min: 0 },
    }),
    row({
      key: "notification_rate_limit",
      group: "notifications",
      unit: "per_second",
      value: 0,
      default: 0,
      off: true,
      default_off: true,
      range: { min: 0 },
    }),
    row({
      key: "notification_coalesce_max_batch",
      group: "notifications",
      value: 20,
      default: 20,
      range: { min: 1, max: 100 },
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

test("each cap carries its name, its key to copy, its value and its source", async () => {
  await show(LIMITS);
  const turn = screen.getByRole("table", { name: "Guardrails on every turn" });
  const changed = within(turn).getByRole("row", { name: /Tool rounds on a first turn/ });
  // THE KEY BEHIND THE ROW'S COPY CONTROL (kit 0.128.2), never a line of the
  // document's spelling under the name.
  expect(
    within(changed).getByRole("button", {
      name: "Copy the setting's key, turn_engine.onboarding_max_tool_rounds",
    }),
  ).toBeDefined();
  expect(within(changed).queryByText("turn_engine.onboarding_max_tool_rounds")).toBeNull();
  expect(
    within(changed)
      .getAllByRole("cell")
      .map((c) => c.textContent),
  ).toEqual(["7", "Changed from 10", ""]);
  // ONLY THE CHANGED ROW IS MARKED; a default says so in a word, never a tag.
  const resting = within(turn).getByRole("row", { name: /Review rounds per turn/ });
  expect(resting.hasAttribute("data-changed")).toBe(false);
  expect(changed.hasAttribute("data-changed")).toBe(true);
  expect(
    within(resting)
      .getAllByRole("cell")
      .map((c) => c.textContent),
  ).toEqual(["3", "Default", ""]);
});

test("a cap whose value switches it off says Off, as a word", async () => {
  await show(LIMITS);
  const wakeUps = screen.getByRole("row", { name: /Wake-ups per seat/ });
  expect(
    within(wakeUps)
      .getAllByRole("cell")
      .map((c) => c.textContent),
  ).toEqual(["Off", "Default", ""]);
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
  const field = screen.getByRole("spinbutton");
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
  // THE SAVED VALUE, drawn while this node applies it, and the save said by
  // its toast rather than a word beside the value.
  const row = screen.getByRole("row", { name: /Review rounds per turn/ });
  expect(within(row).getByText("5")).toBeTruthy();
  expect(toastText()).toContain(
    "Limits: Review rounds per turn set to 5 (turn_engine.max_iterations)",
  );
  expect(toastText()).toContain(APPLYING_SENTENCE);
  expect(within(row).queryByText(/applying/)).toBeNull();
  expect(screen.queryByRole("spinbutton")).toBeNull();
});

// ONCE THIS NODE APPLIED IT the page re-reads, and the toast says so in place.
test("the save's toast says applied once this node applied the revision", async () => {
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
  expect(within(row).getByText("Changed from 3")).toBeTruthy();
  expect(toastText()).toContain(APPLIED_SENTENCE);
  expect(toastText()).not.toContain(APPLYING_SENTENCE);
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

/** The Save button of the open field. */
const saveButton = () => screen.getByRole("button", { name: "Save" });

/** The sentence a held button is described by. */
function heldReason(button: HTMLElement): string {
  return (button.getAttribute("aria-describedby") ?? "")
    .split(/\s+/)
    .map((id) => document.getElementById(id)?.textContent ?? "")
    .join(" ");
}

// SAVE IS HELD WHILE NOTHING CHANGED: a press that could only be told the
// value is the limit already was a press offered for nothing.
test("Save is held while the value typed is the one in force", async () => {
  const calls = stubEngine(() => engineAnswer(DOC, 200, { ETag: '"r1"' }));
  await show(LIMITS);
  await change(/Change Tool rounds on a first turn/, "7");
  expect(calls).toEqual([]);
  expect(saveButton().getAttribute("aria-disabled")).toBe("true");
  expect(heldReason(saveButton())).toContain("This is the limit already.");
  // The default typed for a cap at its default is no change either.
  cleanup();
  await show(LIMITS);
  await change(/Change Review rounds per turn/, "3");
  expect(saveButton().getAttribute("aria-disabled")).toBe("true");
  fireEvent.change(screen.getByRole("spinbutton"), { target: { value: "4" } });
  expect(saveButton().getAttribute("aria-disabled")).not.toBe("true");
});

// THE ENGINE'S RANGE, SAID BEFORE THE SAVE. 0, -2 and 999999 went to the
// engine with Save enabled and came back as the document's raw line; the
// row carries the range the engine's validator holds the cap to, and the
// field says a value outside it as it is typed, in the cap's own words.
test.each([
  [/Change Review rounds per turn/, "0", "At least 1."],
  [/Change Review rounds per turn/, "-2", "At least 1."],
  [/Change Notices gathered into one wake-up/, "999999", "From 1 to 100."],
])("%s typed as %s is refused beside the field, and nothing is sent", async (name, typed, said) => {
  const calls = stubEngine(() => engineAnswer(DOC, 200, { ETag: '"r1"' }));
  await show(LIMITS);
  await change(name, typed);
  expect(calls).toEqual([]);
  expect(screen.getByRole("spinbutton").getAttribute("aria-invalid")).toBe("true");
  expect(screen.getAllByText(said).length).toBeGreaterThan(0);
  expect(saveButton().getAttribute("aria-disabled")).toBe("true");
});

// A PERSON'S WORDS, NEVER THE DOCUMENT'S. Whether a number fits the whole
// company is the engine's rule, and its refusal was drawn as its line for an
// operator: the key, "value out of range", a note for an operator, and the
// API's advice in capitals. A rule the engine words for a person is said in
// those words, a range is said as the field says it, and anything else as a
// value the rest of the configuration cannot take.
test("a value the engine refuses is said in a person's words, and nothing is saved", async () => {
  const line =
    "turn_engine.onboarding_max_tool_rounds_ceiling: value out of range: must be at least onboarding_max_tool_rounds (40), got 20";
  const said =
    "Tool rounds on a first turn can be at most 20, the most an extension may grant a first turn.";
  const calls = stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(DOC, 200, { ETag: '"r1"' });
    return engineAnswer(
      {
        error: "validation_error",
        detail: line,
        hint: "The whole company is checked on every change.",
        problems: [
          {
            path: "turn_engine.onboarding_max_tool_rounds_ceiling",
            segments: ["turn_engine", "onboarding_max_tool_rounds_ceiling"],
            kind: "out_of_range",
            message: line,
            said,
          },
        ],
      },
      400,
    );
  });
  await show(LIMITS);
  await change(/Change Tool rounds on a first turn/, "40");
  const refused = screen.getByText(said).closest('[role="status"]')!;
  expect(refused.textContent).not.toContain("turn_engine.");
  expect(refused.textContent).not.toContain("The whole company is checked");
  expect(calls.filter((c) => c.method === "PATCH" && !c.path.includes("dry_run"))).toEqual([]);
  // The field stays open on what was typed.
  expect((screen.getByRole("spinbutton") as HTMLInputElement).value).toBe("40");
});

test("the engine's range refusal is said as the field says it, and an unworded rule plainly", async () => {
  const line = "turn_engine.max_iterations: value out of range: must be at least 5, got 4";
  stubEngine((call) => {
    if (call.method === "GET") return engineAnswer(DOC, 200, { ETag: '"r1"' });
    return engineAnswer(
      {
        error: "validation_error",
        detail: line,
        problems: [
          {
            path: "turn_engine.max_iterations",
            segments: ["turn_engine", "max_iterations"],
            kind: "out_of_range",
            message: line,
            range: { min: 5 },
          },
          {
            path: "mcp_servers[0].name",
            segments: ["mcp_servers", 0, "name"],
            kind: "conflict",
            message: "mcp_servers[0].name: conflict: duplicate MCP server name",
          },
        ],
      },
      400,
    );
  });
  await show(LIMITS);
  await change(/Change Review rounds per turn/, "4");
  const refused = screen
    .getByText("At least 5. This value does not fit the rest of the company’s configuration.")
    .closest('[role="status"]')!;
  expect(refused.textContent).not.toContain("turn_engine.");
  expect(refused.textContent).not.toContain("mcp_servers");
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
  // THE FIELD IS REFUSED, in its own line under it, before anything is sent,
  // saying what the value is not. No sentence says how to type it: the
  // steppers, the bounds and the Default chip do.
  expect(screen.getByRole("spinbutton").getAttribute("aria-invalid")).toBe("true");
  expect(screen.getAllByText("“2.5” is not a whole number.").length).toBeGreaterThan(0);
  expect(screen.queryByText(/A whole number\./)).toBeNull();
  expect(screen.queryByText(/Empty is the default/)).toBeNull();
});

// A CAP NO DOCUMENT CARRIES IS NOT OFFERED A CHANGE: the engine says which
// those are (`settable`), and a patch of one would be refused as an unknown
// field.
test("the engine's constant has no pencil", async () => {
  await show(LIMITS);
  const row = screen.getByRole("row", { name: /A scheduled turn's time limit/ });
  // Only the key's copy control: no pencil.
  expect(
    within(row)
      .queryAllByRole("button")
      .map((b) => b.getAttribute("aria-label") ?? b.textContent),
  ).toEqual([expect.stringMatching(/^Copy the setting's key, /)]);
});

// NEVER HIDDEN: a reader who cannot write the document sees every pencil,
// disabled with the reason.
test("a reader without the grant sees the pencils disabled with the reason", async () => {
  await show(LIMITS, READER_VIEWER);
  const pencil = screen.getByRole("button", { name: /Change Review rounds per turn/ });
  expect(
    pencil.getAttribute("aria-disabled") ?? String((pencil as HTMLButtonElement).disabled),
  ).toBe("true");
  const described = (pencil.getAttribute("aria-describedby") ?? "")
    .split(" ")
    .map((id) => document.getElementById(id)?.textContent);
  expect(described).toContain(CONFIG_WRITE_REASONS.no_grant);
});

// THE CEILING A CAP IS REFUSED ABOVE IS A ROW BESIDE IT, named in words and
// changed by the same pencil: "Turns per worker can be at most 40" names a
// value the page shows and changes.
test("the ceilings and floors a cap is held to are named rows with pencils", async () => {
  await show({
    limits: [
      row({ key: "turn_engine.delegation.max_turns", group: "delegation", range: { min: 1 } }),
      row({
        key: "turn_engine.delegation.max_turns_ceiling",
        group: "delegation",
        value: 40,
        default: 40,
        range: { min: 1 },
      }),
      row({
        key: "turn_engine.delegation.min_tokens_per_task",
        group: "delegation",
        value: 500,
        default: 500,
        range: { min: 0 },
      }),
      row({
        key: "turn_engine.extension_round_step",
        group: "turn",
        value: 8,
        default: 8,
        range: { min: 1 },
      }),
      row({
        key: "turn_engine.executor_min_budget_tokens",
        group: "turn",
        value: 65000,
        default: 65000,
        range: { min: 0 },
      }),
    ],
  });
  for (const name of [
    "Most turns a worker may ask for",
    "Fewest tokens a worker is given",
    "Rounds one extension may grant a first turn",
    "Tokens an executor launch needs",
  ]) {
    const line = screen.getByRole("row", { name: new RegExp(name) });
    expect(within(line).getByRole("button", { name: `Change ${name}` })).toBeDefined();
  }
  expect(screen.getByText("65,000")).toBeTruthy();
});
