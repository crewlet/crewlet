/**
 * Who runs on which model, under the list: one card, one row anatomy for the
 * helpers and the seats, drawn from the models' own uses.
 *
 * The invariants: Helpers then Seats, each row its identity and its model's
 * picker in one column, with no header row; every model named in full (its
 * harness's mark, its key and the model it runs by name, the id where nothing
 * names it); a seat on one model shows one picker, which sets all three
 * categories; "Split by job" and "Use one model" live in the row's menu, and a
 * seat whose jobs differ shows its three, a hand split reading "Split"; no
 * callout and no sentence repeated per row, a fast model marked inside the
 * Routines picker alone; coding (and the one picker) offers only a harness that
 * can code; every pick opens the picked model's wizard on Use it for with
 * exactly this seat and its categories; a member reads values with no picker.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { CATEGORY_JOBS, CATEGORY_ORDER, type JobCategory } from "~/lib/modelUses.ts";
import { TestClient } from "~/test/client.tsx";
import {
  ASSISTANT_CHAT,
  EVERY_JOB,
  MODELS_NOW,
  MODELS_SEATS,
  modelEntry,
  modelsData,
  modelsDoc,
  runs,
} from "~/routes/settings/models/fixtures.ts";
import { WhoRuns } from "./SeatJobs.tsx";
import { nameKey } from "./modelNames.ts";
import type { ModelsData } from "./data.ts";
import { WIZARD_STEPS } from "./parts.tsx";
import { CATEGORY_WORDS, FAST_NOTE } from "./words.ts";

// A HELPER'S MODEL IS WRITTEN IN ONE STEP, by the configuration transport:
// held here so the line can be read before and after the write.
const writes = vi.hoisted(() => ({ patches: [] as unknown[] }));
vi.mock("~/protocol/configWrite.ts", async (actual) => ({
  ...(await actual<typeof import("~/protocol/configWrite.ts")>()),
  getConfig: async () => ({
    kind: "document",
    document: { assistant: { model: "guarded" } },
    etag: '"r1"',
  }),
  dryRunPatch: async () => ({ kind: "valid", baseRevisionId: "r1", warnings: [], derived: null }),
  savePatch: async (patch: unknown) => {
    writes.patches.push(patch);
    return { kind: "saved", revisionId: "r2", epoch: 2, warnings: [], derived: null };
  },
}));

/** The categories with a deadline, and one without: the fast note's two sides. */
const TIMED = CATEGORY_ORDER.find((c) => FAST_NOTE[c])!;
const UNTIMED = CATEGORY_ORDER.find((c) => c !== "coding" && !FAST_NOTE[c])!;
const [FIRST_JOB, SECOND_JOB] = CATEGORY_JOBS[UNTIMED];

/**
 * Backend Engineer runs every job on `default` (a strong, slow model).
 * Product Manager codes on `default` and runs every other job on `haiku`.
 * Quality Lead codes on `sonnet`, which cannot sign in; its untimed category
 * is split by hand between `sonnet` and `codex-review` (a harness that cannot
 * code); its timed one runs on `default`. The assistant chats on `guarded`,
 * the one model that denies web tools.
 */
/** The names the engine's catalogue gives two of the ids; `gpt-6.1` and `claude-sonnet-5` it names not. */
const NAMES = new Map([
  [nameKey("claude-code", "claude-opus-5"), "Claude Opus 5"],
  [nameKey("claude-code", "claude-haiku-5"), "Claude Haiku 5"],
]);

function company({ admin = true } = {}): ModelsData {
  const others = (seat: string) =>
    CATEGORY_ORDER.filter((c) => c !== "coding").flatMap((c) => runs(seat, ...CATEGORY_JOBS[c]));
  return modelsData({
    admin,
    names: NAMES,
    entries: [
      modelEntry({
        key: "default",
        model: "claude-opus-5",
        used_by: [
          ...runs("backend-engineer", ...EVERY_JOB),
          ...runs("pm", "executor"),
          ...runs("qa", ...CATEGORY_JOBS[TIMED]),
        ],
      }),
      modelEntry({ key: "haiku", model: "claude-haiku-5", used_by: others("pm") }),
      modelEntry({
        key: "sonnet",
        health: "rejected",
        used_by: [...runs("qa", "executor"), ...runs("qa", FIRST_JOB!)],
      }),
      modelEntry({
        key: "codex-review",
        harness: "codex",
        model: "gpt-6.1",
        used_by: runs("qa", SECOND_JOB!),
        helper_problem: "This model's tool keeps a tool Crewlet cannot turn off.",
      }),
      modelEntry({ key: "guarded", model: "claude-sonnet-5", used_by: [ASSISTANT_CHAT] }),
    ],
    doc: {
      ...modelsDoc({
        default: {},
        haiku: {},
        sonnet: {},
        "codex-review": { harness: "codex" },
        guarded: {},
      }),
      assistant: { model: "guarded" },
    } as ModelsData["doc"] & object,
  });
}

function mount(data: ModelsData = company()) {
  const onFlow = vi.fn();
  render(
    <TestClient>
      <LayerHost>
        <Router>
          <WhoRuns data={data} onFlow={onFlow} />
        </Router>
      </LayerHost>
    </TestClient>,
  );
  return onFlow;
}

const seatName = (handle: string) => MODELS_SEATS.find((s) => s.handle === handle)!.name;
/** A split seat's picker for one category, by its short word. */
const picker = (category: JobCategory, handle: string) =>
  screen.getByRole("combobox", {
    name: `${CATEGORY_WORDS[category].short} for ${seatName(handle)}`,
  });
/** A seat's one picker, for a seat on one model. */
const one = (handle: string) =>
  screen.getByRole("combobox", { name: `${seatName(handle)}'s model` });
const rowOf = (handle: string) =>
  within(screen.getByRole("list", { name: "Seats" }))
    .getAllByRole("listitem")
    .find((r) => r.textContent?.includes(seatName(handle)))!;
const optionsOf = (trigger: HTMLElement) => {
  fireEvent.click(trigger);
  return screen.getAllByRole("option");
};
const close = () => fireEvent.keyDown(document.activeElement!, { key: "Escape" });
const markOf = (el: HTMLElement) =>
  el.querySelector<HTMLElement>(".models-option-mark")?.dataset.harness ?? null;
/** Chooses an item of a seat's row menu. */
const fromMenu = (handle: string, item: string) => {
  fireEvent.click(screen.getByRole("button", { name: `More for ${seatName(handle)}` }));
  fireEvent.click(screen.getByRole("menuitem", { name: item }));
};

beforeEach(() => {
  vi.useFakeTimers({ now: MODELS_NOW, toFake: ["Date"] });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

// ONE CARD, ONE ROW ANATOMY: Helpers then Seats, each row who on the left and
// its model in the one column, and no header row saying so.
test("helpers and seats are rows of one anatomy on one card, with no header row", () => {
  mount();
  const helpers = screen.getByRole("list", { name: "Helpers" });
  const seats = screen.getByRole("list", { name: "Seats" });
  expect(helpers.closest(".crewlet-card")).toBe(seats.closest(".crewlet-card"));
  expect(helpers.compareDocumentPosition(seats) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  const rows = [
    ...within(helpers).getAllByRole("listitem"),
    ...within(seats).getAllByRole("listitem"),
  ];
  expect(rows).toHaveLength(2 + MODELS_SEATS.length);
  for (const row of rows) {
    expect(row.querySelector(":scope > .models-run__who")).not.toBeNull();
    expect(
      row.querySelector(":scope > .models-run__model, :scope .models-run__model"),
    ).not.toBeNull();
  }
  expect(
    within(helpers)
      .getAllByRole("listitem")
      .map((r) => r.textContent),
  ).toEqual([expect.stringContaining("Ask Crewlet"), expect.stringContaining("Advisor")]);
  // A SEAT'S NAME LINKS TO THE SEAT.
  expect(within(rowOf("pm")).getByRole("link", { name: /Product Manager/ })).toBeDefined();
  expect(screen.queryByRole("table")).toBeNull();
  expect(screen.queryAllByRole("columnheader")).toHaveLength(0);
});

// THE MODEL IN FULL: its key and, quieter, the model it runs by name, read
// from the id; an id nothing names is said as itself.
test("a picker names the model in full from its id, and an unknown id as itself", () => {
  mount();
  expect(one("backend-engineer").textContent).toContain("default · Claude Opus 5");
  const options = optionsOf(one("backend-engineer")).map((o) => o.textContent ?? "");
  expect(options.find((t) => t.startsWith("haiku"))).toContain("haiku · Claude Haiku 5");
  // NOTHING NAMES claude-sonnet-5 here: its id stands for it.
  expect(options.find((t) => t.startsWith("sonnet"))).toContain("sonnet · claude-sonnet-5");
  close();
  const routines = optionsOf(picker(TIMED, "pm")).map((o) => o.textContent ?? "");
  expect(routines.find((t) => t.startsWith("codex-review"))).toContain("codex-review · gpt-6.1");
});

// ONE PICKER FOR A SEAT ON ONE MODEL: it sets all three categories, and the
// write is still per job, through the model's wizard.
test("a seat on one model shows one picker, which sets all three categories", () => {
  const onFlow = mount();
  expect(within(rowOf("backend-engineer")).getAllByRole("combobox")).toHaveLength(1);
  const options = optionsOf(one("backend-engineer"));
  // IT SETS CODING TOO: only a harness that can code is offered.
  expect(options.some((o) => o.textContent?.startsWith("codex-review"))).toBe(false);
  fireEvent.mouseDown(options.find((o) => o.textContent?.startsWith("haiku"))!);
  expect(onFlow).toHaveBeenCalledWith("edit", "haiku", {
    step: "assign",
    add: CATEGORY_ORDER.map((job) => ({ job, seat: "backend-engineer" })),
  });
});

// SPLIT AND MERGE FROM THE ROW'S MENU, never a button on every row.
test("the row's menu splits a seat by job and uses one model again", () => {
  const onFlow = mount();
  expect(screen.queryByRole("button", { name: /Split by job/ })).toBeNull();
  fromMenu("backend-engineer", "Split by job");
  for (const category of CATEGORY_ORDER)
    expect(picker(category, "backend-engineer").textContent).toContain("default");
  // EACH CATEGORY'S SHORT WORD stands under the seat's name.
  expect(
    [...rowOf("backend-engineer").querySelectorAll(".models-run__label")].map((l) => l.textContent),
  ).toEqual(["Coding", "Reviews", "Routines"]);
  // A PICK IN ONE CATEGORY adds that category alone.
  fireEvent.mouseDown(
    optionsOf(picker("reviews", "backend-engineer")).find((o) =>
      o.textContent?.startsWith("haiku"),
    )!,
  );
  expect(onFlow).toHaveBeenCalledWith("edit", "haiku", {
    step: "assign",
    add: [{ job: "reviews", seat: "backend-engineer" }],
  });
  fromMenu("backend-engineer", "Use one model");
  expect(within(rowOf("backend-engineer")).getAllByRole("combobox")).toHaveLength(1);
});

test("a seat already split shows its three pickers, and a category split by hand reads Split", () => {
  mount();
  expect(picker("coding", "pm").textContent).toContain("default");
  expect(picker(TIMED, "pm").textContent).toContain("haiku");
  expect(picker("coding", "qa").textContent).toContain("sonnet");
  expect(picker(UNTIMED, "qa").textContent).toContain("Split");
  // ITS MENU OFFERS ONE MODEL; the next pick on it sets all three.
  fromMenu("pm", "Use one model");
  expect(one("pm").textContent).toContain("Split");
});

test("every model in a picker carries its harness's mark, on the trigger and on each option", () => {
  mount();
  const trigger = one("backend-engineer");
  expect(markOf(trigger)).toBe("claude-code");
  for (const option of optionsOf(trigger)) expect(markOf(option)).not.toBeNull();
  close();
  const routines = optionsOf(picker(TIMED, "pm"));
  expect(markOf(routines.find((o) => o.textContent?.startsWith("codex-review"))!)).toBe("codex");
  close();
  // ASK CREWLET'S AND THE ADVISOR'S pickers too, its model named in full.
  const ask = screen.getByRole("combobox", { name: "Ask Crewlet's model" });
  expect(markOf(ask)).toBe("claude-code");
  expect(ask.textContent).toContain("guarded · claude-sonnet-5");
  for (const option of optionsOf(ask).filter((o) => !o.textContent?.startsWith("None")))
    expect(markOf(option)).not.toBeNull();
});

// NOTHING SAID PER ROW: no callout, no sentence a row repeats, seen or hidden.
// A fast model is marked inside the pickers of a category with a deadline.
test("no callout and no sentence repeated per row; a fast model is marked in the Routines picker alone", () => {
  mount();
  expect(document.querySelector(".crewlet-callout")).toBeNull();
  expect(document.body.textContent).not.toMatch(/fast model is recommended|Fast model recommended/);
  expect(screen.queryByText("Everything")).toBeNull();
  const fast = optionsOf(picker(TIMED, "pm"));
  expect(fast.find((o) => o.textContent?.startsWith("haiku"))!.textContent).toContain("Fast model");
  expect(fast.find((o) => o.textContent?.startsWith("default"))!.textContent).not.toContain(
    "Fast model",
  );
  close();
  for (const option of optionsOf(picker("coding", "pm")))
    expect(option.textContent).not.toContain("Fast model");
});

test("each option names the model and its condition in a word, and coding offers only a harness that can code", () => {
  mount();
  const coding = optionsOf(picker("coding", "pm")).map((o) => o.textContent ?? "");
  expect(coding.some((t) => t.includes("codex-review"))).toBe(false);
  expect(coding.find((t) => t.startsWith("sonnet"))!).toContain("Sign-in refused");
  close();
  const other = optionsOf(picker(TIMED, "pm")).map((o) => o.textContent ?? "");
  expect(other.some((t) => t.startsWith("codex-review"))).toBe(true);
});

/**
 * THE DEEP-LINK WALK: every category picker of a split seat, picked to another
 * model, opens THAT model's wizard on Use it for with this seat and this
 * category, and the address carries nothing else. Picking the model it already
 * runs opens nothing.
 */
test("every pick opens the picked model's wizard at Use it for, with only the seat and its categories", () => {
  const onFlow = mount();
  const keys = company().models.map((m) => m.key);
  const keyOf = (text: string | null) => keys.find((k) => text?.startsWith(k)) ?? "";
  for (const seat of MODELS_SEATS.filter((s) => s.handle !== "backend-engineer")) {
    for (const category of CATEGORY_ORDER) {
      const trigger = picker(category, seat.handle);
      const current = keyOf(trigger.textContent?.trim() ?? "");
      const other = optionsOf(trigger).find((o) => keyOf(o.textContent) !== current)!;
      const key = keyOf(other.textContent);
      onFlow.mockClear();
      fireEvent.mouseDown(other);
      expect(onFlow).toHaveBeenCalledTimes(1);
      expect(onFlow).toHaveBeenCalledWith("edit", key, {
        step: "assign",
        add: [{ job: category, seat: seat.handle }],
      });
      const at = onFlow.mock.calls[0]![2] as Record<string, unknown>;
      expect(Object.keys(at).sort()).toEqual(["add", "step"]);
      expect(WIZARD_STEPS).toContain(at.step);
    }
  }
  // THE MODEL ALREADY THERE is no change.
  onFlow.mockClear();
  fireEvent.mouseDown(
    optionsOf(picker("coding", "pm")).find((o) => o.textContent?.startsWith("default"))!,
  );
  expect(onFlow).not.toHaveBeenCalled();
});

// ANY MODEL A HELPER CAN RUN ON, as the engine says of each: an ordinary entry
// qualifies as it is, and one whose harness keeps a tool that cannot be turned
// off is listed disabled beside the engine's reason. CHOOSING ONE WRITES
// `assistant.model` IN ONE STEP, as the helpers' own pickers do: it never
// opens the model wizard, which has nothing more to ask of a company-wide helper.
test("Ask Crewlet's picker writes its one model in a step, never the wizard", async () => {
  writes.patches = [];
  const onFlow = mount(company());
  const trigger = screen.getByRole("combobox", { name: "Ask Crewlet's model" });
  expect(trigger.textContent).toContain("guarded");
  const options = optionsOf(trigger);
  const codex = options.find((o) => o.textContent?.startsWith("codex-review"))!;
  expect(codex.getAttribute("aria-disabled")).toBe("true");
  expect(codex.textContent).toContain("cannot turn off");
  await act(async () => {
    fireEvent.mouseDown(options.find((o) => o.textContent?.startsWith("haiku"))!);
  });
  expect(onFlow).not.toHaveBeenCalled();
  expect(await screen.findByText(/Ask Crewlet now runs on haiku/)).toBeDefined();
  expect(writes.patches).toEqual([{ assistant: { model: "haiku" } }]);
});

// THE ADVISOR IS A USE LIKE ASK CREWLET: a row of its own, its
// model chosen the same way, and drawn when it has none, since its checks
// run without one.
test("the Advisor has a line of its own, chosen in a step like Ask Crewlet's", async () => {
  writes.patches = [];
  const onFlow = mount({ ...company(), advisor: { key: null } });
  const trigger = screen.getByRole("combobox", { name: "Advisor's model" });
  expect(trigger.textContent).toContain("None");
  const options = optionsOf(trigger);
  await act(async () => {
    fireEvent.mouseDown(options.find((o) => o.textContent?.startsWith("default"))!);
  });
  expect(onFlow).not.toHaveBeenCalled();
  expect(await screen.findByText(/Advisor now runs on default/)).toBeDefined();
  expect(writes.patches).toEqual([{ advisor: { model: "default" } }]);
  cleanup();
  mount({ ...company({ admin: false }), advisor: { key: "haiku" } });
  expect(screen.getAllByText("haiku").length).toBeGreaterThan(0);
  expect(screen.queryByRole("combobox", { name: "Advisor's model" })).toBeNull();
});

// ONE MODEL, NO CHAIN: the line names no fallback, and is drawn when Ask
// Crewlet has no model too, so the company sees that it is off.
test("Ask Crewlet's line names its one model, and is drawn with none", () => {
  mount(company());
  expect(document.body.textContent).not.toContain("Falls back");
  cleanup();
  const off = company();
  mount({ ...off, doc: modelsDoc({ guarded: {} }), assistant: { key: null } });
  expect(screen.getByRole("combobox", { name: "Ask Crewlet's model" }).textContent).toContain(
    "None",
  );
  cleanup();
  mount({ ...company({ admin: false }), assistant: { key: null } });
  // ASK CREWLET'S LINE AND THE ADVISOR'S each say they have no model.
  expect(screen.getAllByText("Not set")).toHaveLength(2);
});

test("a member reads every value in full, with its mark, and no picker or menu", () => {
  mount(company({ admin: false }));
  expect(screen.queryAllByRole("combobox")).toHaveLength(0);
  expect(screen.queryByRole("button", { name: /^More for/ })).toBeNull();
  expect(rowOf("backend-engineer").textContent).toContain("default · Claude Opus 5");
  const qa = rowOf("qa");
  expect(qa.textContent).toContain("sonnet");
  expect(markOf(qa)).toBe("claude-code");
});

// NO CAPTION on a helper: its mark, its name and its picker.
test("Ask Crewlet and the Advisor are rows with no caption", () => {
  mount(company());
  const said = document.body.textContent ?? "";
  expect(said).not.toMatch(/Answers you in the dashboard|Explains what it finds worth changing/);
  expect(said).not.toContain("Company-wide, so it has no seats");
  expect(said).not.toContain("no tools of its own");
});
