/**
 * By job draws what each seat runs each category of work on from the models'
 * own uses, says each model's condition where it is named, and a pick opens
 * that model's wizard rather than writing anything.
 *
 * The invariants: a cell names the model the engine reports, and a category
 * the document split says which job runs where; a cell on a model that will
 * not answer says so (QA M19); a category with a deadline notes a slow model;
 * coding offers only a CLI that can code, and the Assistant only a model that
 * denies web tools; every pick opens the picked model's wizard on Use it for
 * with exactly this seat and category (the deep-link walk); a member reads
 * values, with no picker, in the type an admin's picker draws (QA Nit 7); and
 * each column head is named once (QA Minor 20).
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
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
import { ByJob } from "./ByJob.tsx";
import type { ModelsData } from "./data.ts";
import { WIZARD_STEPS } from "./parts.tsx";
import { CATEGORY_WORDS, FAST_NOTE } from "./words.ts";

/** The categories with a deadline, and one without: the fast note's two sides. */
const TIMED = CATEGORY_ORDER.find((c) => FAST_NOTE[c])!;
const UNTIMED = CATEGORY_ORDER.find((c) => c !== "coding" && !FAST_NOTE[c])!;
const [FIRST_JOB, SECOND_JOB] = CATEGORY_JOBS[UNTIMED];

/**
 * Backend Engineer runs every job on `default` (a strong, slow model).
 * Product Manager codes on `default` and runs every other job on `haiku`.
 * Quality Lead codes on `sonnet`, which cannot sign in; its untimed category
 * is split by hand between `sonnet` and `codex-review` (a CLI that cannot
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
        agent: "codex",
        model: "gpt-6.1",
        used_by: runs("qa", SECOND_JOB!),
      }),
      modelEntry({ key: "guarded", model: "claude-sonnet-5", used_by: [ASSISTANT_CHAT] }),
    ],
    doc: modelsDoc({
      default: {},
      haiku: {},
      sonnet: {},
      "codex-review": { agent: "codex" },
      guarded: { overrides: { web_tools: "denied" } },
    }),
  });
}

function mount(data: ModelsData = company()) {
  const onFlow = vi.fn();
  render(
    <TestClient>
      <LayerHost>
        <Router>
          <ByJob data={data} onFlow={onFlow} />
        </Router>
      </LayerHost>
    </TestClient>,
  );
  return onFlow;
}

const seatName = (handle: string) => MODELS_SEATS.find((s) => s.handle === handle)!.name;
const picker = (category: JobCategory, handle: string) =>
  screen.getByRole("combobox", {
    name: `${CATEGORY_WORDS[category].label} for ${seatName(handle)}`,
  });
/** The cell a picker (or a member's value) stands in. */
const cellOf = (el: HTMLElement) => el.closest<HTMLElement>(".models-byjob__cell")!;
const optionsOf = (trigger: HTMLElement) => {
  fireEvent.click(trigger);
  const found = screen.getAllByRole("option");
  return found;
};

beforeEach(() => {
  vi.useFakeTimers({ now: MODELS_NOW, toFake: ["Date"] });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

test("each cell names the model the engine reports the seat's category runs on", () => {
  mount();
  expect(picker("coding", "pm").textContent).toContain("default");
  expect(picker(TIMED, "pm").textContent).toContain("haiku");
  expect(picker("coding", "qa").textContent).toContain("sonnet");
  // ONE MODEL FOR EVERY JOB is said once, on the seat.
  const rows = screen.getAllByRole("row");
  const backend = rows.find((r) => r.textContent?.includes("Backend Engineer"))!;
  expect(within(backend).getByText("Everything")).toBeDefined();
  const pm = rows.find((r) => r.textContent?.includes("Product Manager"))!;
  expect(within(pm).queryByText("Everything")).toBeNull();
});

test("a category the document split says which job runs where, and claims no one model", () => {
  mount();
  const split = picker(UNTIMED, "qa");
  expect(split.textContent).toContain("Split");
  const words = cellOf(split).querySelector(".models-byjob__notes")!.textContent ?? "";
  expect(words).toMatch(/on sonnet/);
  expect(words).toMatch(/on codex-review/);
});

test("a cell on a model that cannot sign in says so, to the eye and to the picker's description", () => {
  mount();
  const refused = picker("coding", "qa");
  const notes = cellOf(refused).querySelector<HTMLElement>(".models-byjob__notes")!;
  expect(notes.textContent).toContain("Sign-in refused");
  expect(cellOf(refused).classList).toContain("models-byjob__cell--alarm");
  expect(refused.getAttribute("aria-describedby")).toContain(notes.id);
  expect(notes.textContent).toMatch(/cannot sign in/);
  // A READY MODEL'S CELL carries no condition.
  expect(cellOf(picker("coding", "pm")).textContent).not.toContain("Ready");
});

test("a category with a deadline notes a slow model, and not a fast one", () => {
  mount();
  expect(cellOf(picker(TIMED, "qa")).textContent).toContain("Fast model recommended");
  expect(cellOf(picker(TIMED, "pm")).textContent).not.toContain("Fast model recommended");
  expect(cellOf(picker("coding", "pm")).textContent).not.toContain("Fast model recommended");
});

test("each option names the model, its condition in a word and its model id, and coding offers only a CLI that can code", () => {
  mount();
  const coding = optionsOf(picker("coding", "pm")).map((o) => o.textContent ?? "");
  expect(coding.some((t) => t.includes("codex-review"))).toBe(false);
  const sonnet = coding.find((t) => t.startsWith("sonnet"))!;
  expect(sonnet).toContain("Sign-in refused");
  expect(sonnet).toContain("claude-sonnet-5");
  fireEvent.keyDown(document.activeElement!, { key: "Escape" });
  const other = optionsOf(picker(TIMED, "pm")).map((o) => o.textContent ?? "");
  expect(other.some((t) => t.startsWith("codex-review"))).toBe(true);
});

/**
 * THE DEEP-LINK WALK: every cell, picked to another model, opens THAT model's
 * wizard on Use it for with this seat and this category, and the address
 * carries nothing else. Picking the model a cell already runs opens nothing.
 */
test("every pick opens the picked model's wizard at Use it for, with only the seat and the category", () => {
  const onFlow = mount();
  const keys = company().models.map((m) => m.key);
  const keyOf = (text: string | null) => keys.find((k) => text?.startsWith(k)) ?? "";
  for (const seat of MODELS_SEATS) {
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
        seat: seat.handle,
        job: category,
      });
      const at = onFlow.mock.calls[0]![2] as Record<string, unknown>;
      expect(Object.keys(at).sort()).toEqual(["job", "seat", "step"]);
      expect(WIZARD_STEPS).toContain(at.step);
    }
  }
  // THE MODEL ALREADY THERE is no change.
  onFlow.mockClear();
  const options = optionsOf(picker("coding", "pm"));
  fireEvent.mouseDown(options.find((o) => o.textContent?.startsWith("default"))!);
  expect(onFlow).not.toHaveBeenCalled();
});

test("the assistant's picker offers only a model that denies web tools, and opens its wizard for the assistant", () => {
  const data = company();
  data.models.find((m) => m.key === "haiku")!.webDenied = true;
  const onFlow = mount(data);
  const trigger = screen.getByRole("combobox", { name: "Model the Dashboard Assistant runs on" });
  expect(trigger.textContent).toContain("guarded");
  const offered = optionsOf(trigger).map((o) => o.textContent ?? "");
  expect(offered.map((t) => t.split(/\s|claude/)[0])).toEqual(["haiku", "guarded"]);
  fireEvent.mouseDown(
    screen.getAllByRole("option").find((o) => o.textContent?.startsWith("haiku"))!,
  );
  expect(onFlow).toHaveBeenCalledWith("edit", "haiku", { step: "assign", job: "assistant" });
});

test("a member reads every value, in the picker's type, with no picker", () => {
  mount(company({ admin: false }));
  expect(screen.queryAllByRole("combobox")).toHaveLength(0);
  // The value is body text, never the mono a key is set in elsewhere (Nit 7).
  const values = screen.getAllByText("sonnet", { selector: ".crewlet-text" });
  expect(values.length).toBeGreaterThan(0);
  expect(values.every((v) => !v.classList.contains("mono"))).toBe(true);
  // THE CONDITION reaches a member too: it is why their seat waits.
  expect(document.body.textContent).toContain("Sign-in refused");
});

test("each category's column head is named once", () => {
  mount();
  for (const category of CATEGORY_ORDER) {
    const label = CATEGORY_WORDS[category].label;
    const heads = screen.getAllByRole("columnheader").filter((h) => h.textContent?.includes(label));
    expect(heads).toHaveLength(1);
    expect(within(heads[0]!).getByRole("button").getAttribute("aria-label")).toBe(label);
  }
});
