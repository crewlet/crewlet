/**
 * Under the list of models: Ask Crewlet and the Advisor on one line, then the
 * seats, drawn from the models' own uses.
 *
 * The invariants: a seat on one model shows one picker, which sets all three
 * categories; "Split by job" shows a picker per category, and a seat whose jobs
 * differ shows its three, a category split by hand reading "Split"; every
 * model in a picker carries its harness's mark, on the trigger and each option;
 * no callout and no sentence repeated per row, a fast model marked inside the
 * pickers of a category with a deadline alone; coding (and the one picker)
 * offers only a harness that can code; every pick opens the picked model's
 * wizard on Use it for with exactly this seat and its categories; a member
 * reads values with no picker; and the table's heads are short and not sorted.
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
import { Helpers, SeatJobs } from "./SeatJobs.tsx";
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
function company({ admin = true } = {}): ModelsData {
  const others = (seat: string) =>
    CATEGORY_ORDER.filter((c) => c !== "coding").flatMap((c) => runs(seat, ...CATEGORY_JOBS[c]));
  return modelsData({
    admin,
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
          <Helpers data={data} />
          <SeatJobs data={data} onFlow={onFlow} />
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
  screen.getAllByRole("row").find((r) => r.textContent?.includes(seatName(handle)))!;
const optionsOf = (trigger: HTMLElement) => {
  fireEvent.click(trigger);
  return screen.getAllByRole("option");
};
const close = () => fireEvent.keyDown(document.activeElement!, { key: "Escape" });
const markOf = (el: HTMLElement) =>
  el.querySelector<HTMLElement>(".models-option-mark")?.dataset.harness ?? null;

beforeEach(() => {
  vi.useFakeTimers({ now: MODELS_NOW, toFake: ["Date"] });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

// ONE PICKER FOR A SEAT ON ONE MODEL: it sets all three categories, and the
// write is still per job, through the model's wizard.
test("a seat on one model shows one picker, which sets all three categories", () => {
  const onFlow = mount();
  const trigger = one("backend-engineer");
  expect(trigger.textContent).toContain("default");
  expect(within(rowOf("backend-engineer")).getAllByRole("combobox")).toHaveLength(1);
  const options = optionsOf(trigger);
  // IT SETS CODING TOO: only a harness that can code is offered.
  expect(options.some((o) => o.textContent?.startsWith("codex-review"))).toBe(false);
  fireEvent.mouseDown(options.find((o) => o.textContent?.startsWith("haiku"))!);
  expect(onFlow).toHaveBeenCalledWith("edit", "haiku", {
    step: "assign",
    add: CATEGORY_ORDER.map((job) => ({ job, seat: "backend-engineer" })),
  });
});

test("Split by job shows a picker per category, and One model folds them back", () => {
  const onFlow = mount();
  const row = rowOf("backend-engineer");
  fireEvent.click(within(row).getByRole("button", { name: "Split by job" }));
  for (const category of CATEGORY_ORDER)
    expect(picker(category, "backend-engineer").textContent).toContain("default");
  expect(within(row).getByRole("button", { name: "One model" }).getAttribute("aria-expanded")).toBe(
    "true",
  );
  // A PICK IN ONE CATEGORY adds that category alone.
  const reviews = picker("reviews", "backend-engineer");
  fireEvent.mouseDown(optionsOf(reviews).find((o) => o.textContent?.startsWith("haiku"))!);
  expect(onFlow).toHaveBeenCalledWith("edit", "haiku", {
    step: "assign",
    add: [{ job: "reviews", seat: "backend-engineer" }],
  });
  fireEvent.click(within(row).getByRole("button", { name: "One model" }));
  expect(within(row).getAllByRole("combobox")).toHaveLength(1);
});

test("a seat already split shows its three pickers, and a category split by hand reads Split", () => {
  mount();
  expect(picker("coding", "pm").textContent).toContain("default");
  expect(picker(TIMED, "pm").textContent).toContain("haiku");
  expect(picker("coding", "qa").textContent).toContain("sonnet");
  expect(picker(UNTIMED, "qa").textContent).toContain("Split");
  // NOTHING TO FOLD: its jobs differ, so there is no one model to show.
  expect(within(rowOf("pm")).queryByRole("button", { name: /Split by job|One model/ })).toBeNull();
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
  // ASK CREWLET'S AND THE ADVISOR'S pickers too.
  const ask = screen.getByRole("combobox", { name: "Ask Crewlet's model" });
  expect(markOf(ask)).toBe("claude-code");
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

test("each option names the model, its condition in a word and its model id, and coding offers only a harness that can code", () => {
  mount();
  const coding = optionsOf(picker("coding", "pm")).map((o) => o.textContent ?? "");
  expect(coding.some((t) => t.includes("codex-review"))).toBe(false);
  const sonnet = coding.find((t) => t.startsWith("sonnet"))!;
  expect(sonnet).toContain("Sign-in refused");
  expect(sonnet).toContain("claude-sonnet-5");
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
      const current = keys.find((k) => trigger.textContent?.trim() === k) ?? "";
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

test("a member reads every value, with its mark, and no picker", () => {
  mount(company({ admin: false }));
  expect(screen.queryAllByRole("combobox")).toHaveLength(0);
  expect(screen.queryByRole("button", { name: /Split by job/ })).toBeNull();
  expect(rowOf("backend-engineer").textContent).toContain("default");
  const qa = rowOf("qa");
  expect(qa.textContent).toContain("sonnet");
  expect(markOf(qa)).toBe("claude-code");
  // The value is body text, never the mono a key is set in elsewhere (Nit 7).
  expect(qa.querySelector(".mono")).toBeNull();
});

test("the seats' table has two short heads, each with a name, and no sorting", () => {
  mount();
  expect(screen.getByText("Seats")).toBeTruthy();
  const heads = screen.getAllByRole("columnheader");
  expect(heads.map((h) => h.textContent)).toEqual(["Seat", "Model"]);
  for (const head of heads) expect(within(head).queryByRole("button")).toBeNull();
});

// ONE LINE, NO CAPTION: each helper its mark, its name and its picker.
test("Ask Crewlet and the Advisor are on one line: name and picker, no caption", () => {
  mount(company());
  const rows = within(
    screen.getByRole("list", { name: "Ask Crewlet and the Advisor" }),
  ).getAllByRole("listitem");
  expect(rows).toHaveLength(2);
  expect(rows[0]!.textContent).toContain("Ask Crewlet");
  expect(within(rows[0]!).getByRole("combobox", { name: "Ask Crewlet's model" })).toBeDefined();
  expect(rows[1]!.textContent).toContain("Advisor");
  const said = document.body.textContent ?? "";
  expect(said).not.toMatch(/Answers you in the dashboard|Explains what it finds worth changing/);
  expect(said).not.toContain("Company-wide, so it has no seats");
  expect(said).not.toContain("no tools of its own");
});
