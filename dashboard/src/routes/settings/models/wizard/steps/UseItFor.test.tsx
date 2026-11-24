/**
 * Use it for: what a model does, and for which seats.
 *
 * The invariants: a category writes exactly the jobs `CATEGORY_JOBS` gives
 * it; an edit's choice keeps the seats the model serves, never every seat;
 * no choice row carries a "Replaces" line, Review lists the changes; seats
 * are named, never keyed; a document that split a category reads back half-checked;
 * a harness that cannot code refuses the coding box of a seat without it; the
 * fast-model note is quiet and only where a category carries one; and the
 * web tools rule is said wherever a choice meets it, with its one-click fix.
 */

import { useState } from "react";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import {
  CATEGORY_JOBS,
  CATEGORY_ORDER,
  SEAT_JOBS,
  noUses,
  usesForIntent,
  usesOf,
  type Uses,
} from "~/lib/modelUses.ts";
import {
  EVERY_JOB,
  EXECUTORS,
  modelEntry,
  modelsData,
  runs,
  seat,
} from "~/routes/settings/models/fixtures.ts";
import type { ModelsData } from "../../data.ts";
import { CATEGORY_WORDS, EVERYTHING, FAST_NOTE } from "../../words.ts";
import { emptyDraft, type Draft } from "../draft.ts";
import { UseItFor, everythingHelpers, seatChangesOf, summaryOf } from "./UseItFor.tsx";

const data = modelsData({
  entries: [
    modelEntry({
      key: "default",
      used_by: [...runs("backend-engineer", ...EVERY_JOB), ...runs("pm", ...EVERY_JOB)],
    }),
  ],
  assistant: { key: "default" },
});

let latest: Draft;
function Harness({
  start,
  served = null,
  company = data,
}: {
  start: Partial<Draft>;
  served?: Uses | null;
  company?: ModelsData;
}) {
  const [draft, setDraft] = useState<Draft>({
    ...emptyDraft(EXECUTORS),
    key: "haiku",
    model: "haiku",
    ...start,
  });
  latest = draft;
  return (
    <UseItFor
      draft={draft}
      data={company}
      served={served}
      onChange={(p) => setDraft((d) => ({ ...d, ...p }))}
    />
  );
}

/**
 * Acme's split: `default` runs every job for the engineers and QA,
 * `default-host` (its coding on the engine's host) every job for the PM and
 * the designer.
 */
const split = modelsData({
  entries: [
    modelEntry({
      key: "default",
      used_by: [...runs("backend-engineer", ...EVERY_JOB), ...runs("qa", ...EVERY_JOB)],
    }),
    modelEntry({
      key: "default-host",
      used_by: [...runs("pm", ...EVERY_JOB), ...runs("designer", ...EVERY_JOB)],
    }),
  ],
  seats: [
    seat("backend-engineer", "Backend Engineer"),
    seat("qa", "Quality Lead"),
    seat("pm", "Product Manager"),
    seat("designer", "Designer"),
  ],
});

afterEach(cleanup);

test("a category writes the jobs CATEGORY_JOBS gives it, for the seats it starts with", () => {
  render(<Harness start={{}} />);
  for (const category of CATEGORY_ORDER) {
    fireEvent.click(
      screen.getByRole("radio", { name: new RegExp(CATEGORY_WORDS[category].label) }),
    );
    for (const job of SEAT_JOBS) {
      const on = CATEGORY_JOBS[category].includes(job);
      expect(latest.uses.jobs[job].length > 0).toBe(on);
    }
    expect(latest.intent).toBe(category);
  }
});

test("no choice says what it replaces, and seats are named rather than keyed", () => {
  render(<Harness start={{ intent: "all", uses: { ...noUses(), jobs: { ...noUses().jobs } } }} />);
  // REVIEW LISTS EVERY CHANGE BY SEAT; a row's "Replaces" line read as a description and was missed.
  expect(screen.queryByText(/^Replaces /)).toBeNull();
  fireEvent.click(screen.getByRole("radio", { name: /Everything/ }));
  expect(screen.getAllByText("Backend Engineer").length).toBeGreaterThan(0);
  expect(screen.queryByText("backend-engineer")).toBeNull();
});

/**
 * AN EDIT KEEPS ITS SEATS. Editing `default` for its sign-in and choosing
 * Coding moved the PM and the designer off `default-host`, because a choice
 * reset the seats to every seat. In an edit a choice starts from the seats
 * the model serves today; only an add starts from a sensible set.
 */
test("in an edit, choosing a category keeps the seats the model serves, never every seat", () => {
  const today = usesOf(split.models.find((m) => m.key === "default")!.entry.used_by);
  render(
    <Harness
      company={split}
      served={today}
      start={{ key: "default", intent: "all", uses: today }}
    />,
  );
  fireEvent.click(screen.getByRole("radio", { name: new RegExp(CATEGORY_WORDS.coding.label) }));
  expect(latest.intent).toBe("coding");
  expect(latest.uses.jobs.executor).toEqual(["backend-engineer", "qa"]);
  // AN ADD takes no seat that already codes on a model: every seat here does.
  cleanup();
  render(<Harness company={split} start={{ key: "new" }} />);
  fireEvent.click(screen.getByRole("radio", { name: new RegExp(CATEGORY_WORDS.coding.label) }));
  expect(latest.uses.jobs.executor).toEqual([]);
});

/**
 * A NEW MODEL TAKES ONLY SEATS WITH NO MODEL FOR IT. Adding a model and
 * choosing what it does selected every seat, so a save moved seats off the
 * models they ran on. The Backend Engineer runs everything on `default`, the
 * PM reviews on `reviewer` and nothing else, and the Quality Lead has no
 * model at all.
 */
const partial = modelsData({
  entries: [
    modelEntry({ key: "default", used_by: runs("backend-engineer", ...EVERY_JOB) }),
    modelEntry({ key: "reviewer", used_by: runs("pm", "review") }),
  ],
});

const choose = (label: RegExp | string) =>
  fireEvent.click(
    screen.getByRole("radio", { name: label instanceof RegExp ? label : new RegExp(label) }),
  );

test("an add preselects, per job, only the seats with no model for any job it chooses", () => {
  render(<Harness company={partial} start={{ key: "haiku" }} />);
  // CODING: the PM codes on nothing, so it is picked beside the Quality Lead.
  choose(CATEGORY_WORDS.coding.label);
  expect(latest.uses.jobs.executor).toEqual(["pm", "qa"]);
  // REVIEWS: the PM reviews on `reviewer`, so only the Quality Lead.
  choose(CATEGORY_WORDS.reviews.label);
  for (const job of CATEGORY_JOBS.reviews) expect(latest.uses.jobs[job]).toEqual(["qa"]);
  // EVERYTHING: a seat with a model for even one job is left alone.
  choose(/Everything/);
  for (const job of SEAT_JOBS) expect(latest.uses.jobs[job]).toEqual(["qa"]);
  const chip = (name: string) => screen.getByRole("button", { name: new RegExp(name) });
  expect(chip("Quality Lead").getAttribute("aria-pressed")).toBe("true");
  expect(chip("Product Manager").getAttribute("aria-pressed")).toBe("false");
  expect(chip("Backend Engineer").getAttribute("aria-pressed")).toBe("false");
});

test("the switch picks exactly the seats without a model, and off picks none", () => {
  render(<Harness company={partial} start={{ key: "haiku" }} />);
  choose(CATEGORY_WORDS.coding.label);
  const toggle = screen.getByRole("switch", { name: "Seats without a model" }) as HTMLInputElement;
  expect(toggle.checked).toBe(true);
  expect(screen.queryByRole("switch", { name: "All seats" })).toBeNull();
  fireEvent.click(toggle);
  expect(latest.uses.jobs.executor).toEqual([]);
  expect(toggle.checked).toBe(false);
  fireEvent.click(toggle);
  expect(latest.uses.jobs.executor).toEqual(["pm", "qa"]);
  // A HAND-PICKED SEAT WITH A MODEL is more than the switch picks: it reads off.
  fireEvent.click(screen.getByRole("button", { name: /Backend Engineer/ }));
  expect(latest.uses.jobs.executor).toEqual(["backend-engineer", "pm", "qa"]);
  expect(toggle.checked).toBe(false);
});

test("a seat that has a model may still be picked, and its chip says picking it replaces that", async () => {
  render(<Harness company={partial} start={{ key: "haiku" }} />);
  choose(/Everything/);
  const engineer = screen.getByRole("button", { name: /Backend Engineer/ });
  expect(engineer.getAttribute("aria-pressed")).toBe("false");
  // THE CHIP SAYS IT, in a few words: what the seat is on, and what picking does.
  const tipOf = async (chip: HTMLElement) => {
    fireEvent.pointerOver(chip);
    const text = (await screen.findByRole("tooltip", { hidden: true })).textContent;
    fireEvent.pointerOut(chip);
    fireEvent.keyDown(document.body, { key: "Escape" });
    return text;
  };
  expect(await tipOf(engineer)).toBe("On default now. Picking it replaces that.");
  expect(await tipOf(screen.getByRole("button", { name: /Product Manager/ }))).toBe(
    "On reviewer now. Picking it replaces that.",
  );
  // A seat with no model carries no such tip.
  fireEvent.pointerOver(screen.getByRole("button", { name: /Quality Lead/ }));
  await new Promise((r) => setTimeout(r, 50));
  expect(screen.queryByRole("tooltip", { hidden: true })).toBeNull();
  fireEvent.click(engineer);
  for (const job of SEAT_JOBS) expect(latest.uses.jobs[job]).toEqual(["backend-engineer", "qa"]);
});

test("when every seat has a model for it, no seat is picked, the switch is gone, and nothing asks for one", () => {
  render(<Harness company={split} start={{ key: "new" }} />);
  choose(/Everything/);
  for (const job of SEAT_JOBS) expect(latest.uses.jobs[job]).toEqual([]);
  expect(screen.queryByRole("switch", { name: "Seats without a model" })).toBeNull();
  expect(screen.getByText("For which seats?")).toBeDefined();
  expect(screen.queryByRole("alert")).toBeNull();
});

test("a line separates Everything from the categories, as one separates Not yet", () => {
  render(<Harness start={{}} />);
  const item = (name: RegExp) =>
    screen.getByRole("radio", { name }).closest(".crewlet-choice-list__item");
  expect(
    item(new RegExp(CATEGORY_WORDS[CATEGORY_ORDER[0]!].label))?.classList.contains("is-separated"),
  ).toBe(true);
  expect(item(/Not yet/)?.classList.contains("is-separated")).toBe(true);
  expect(item(/Everything/)?.classList.contains("is-separated")).toBe(false);
});

test("the changes list names each seat that moves, where from and where to, and nothing else", () => {
  const today = usesOf(split.models.find((m) => m.key === "default")!.entry.used_by);
  // NOTHING MOVED, nothing listed.
  expect(seatChangesOf(split, "default", today, today)).toEqual([]);
  // The PM's coding added, QA's routines taken off.
  const next: Uses = { ...today, jobs: { ...today.jobs } };
  next.jobs.executor = [...today.jobs.executor, "pm"].sort();
  for (const job of CATEGORY_JOBS.routines)
    next.jobs[job] = today.jobs[job].filter((h) => h !== "qa");
  expect(seatChangesOf(split, "default", today, next)).toEqual([
    {
      key: "qa",
      subject: "Quality Lead",
      rows: [{ kind: "removed", label: "Routines and evaluations", from: "default" }],
    },
    {
      key: "pm",
      subject: "Product Manager",
      rows: [{ kind: "changed", label: "Coding", from: "default-host", to: "default" }],
    },
  ]);
});

// ONE MODEL: the change names the model Ask Crewlet leaves and the one it
// moves to, and nothing else.
test("the changes list says Ask Crewlet's model moving, and only that", () => {
  const ask = usesForIntent("assistant", [], true);
  const helpers = (row: object) => [{ key: "helpers", subject: "Helpers", rows: [row] }];
  expect(seatChangesOf(data, "big", noUses(), ask)).toEqual(
    helpers({ kind: "changed", label: "Ask Crewlet", from: "default", to: "big" }),
  );
  expect(seatChangesOf({ ...data, assistant: { key: null } }, "big", noUses(), ask)).toEqual(
    helpers({ kind: "added", label: "Ask Crewlet", to: "big" }),
  );
});

test("a category the document split by hand reads back half-checked", () => {
  const split = CATEGORY_ORDER.find((c) => CATEGORY_JOBS[c].length > 1)!;
  const uses = noUses();
  uses.jobs[CATEGORY_JOBS[split][0]!] = ["qa"];
  render(<Harness start={{ intent: "custom", uses }} />);
  const box = screen.getByRole("checkbox", {
    name: `Quality Lead ${CATEGORY_WORDS[split].label}`,
  }) as HTMLInputElement;
  expect(box.indeterminate).toBe(true);
  expect(box.checked).toBe(false);
});

test("a harness that cannot code refuses a seat's coding box, says why once, and lets a ticked one off", () => {
  const uses = noUses();
  for (const job of CATEGORY_JOBS.coding) uses.jobs[job] = ["qa"];
  render(<Harness company={split} start={{ harness: "codex", intent: "custom", uses }} />);
  const box = (name: string) => screen.getByRole("checkbox", { name }) as HTMLInputElement;
  const coding = CATEGORY_WORDS.coding.label;
  expect(box(`Product Manager ${coding}`).disabled).toBe(true);
  expect(box(`Quality Lead ${coding}`).disabled).toBe(false);
  expect(box(`Product Manager ${CATEGORY_WORDS.reviews.label}`).disabled).toBe(false);
  // The grid says why under itself, and the section says it once, whatever the number of seats.
  const section = screen.getByRole("region", { name: "Customise per seat" });
  expect(within(section).getAllByText(/but coding|cannot run a seat's executor/)).toHaveLength(1);
  fireEvent.click(box(`Quality Lead ${coding}`));
  for (const job of CATEGORY_JOBS.coding) expect(latest.uses.jobs[job]).toEqual([]);
});

test("the fast-model note shows only where a category carries one, and only for a slow model", () => {
  const noted = CATEGORY_ORDER.filter((c) => FAST_NOTE[c]);
  render(<Harness start={{ model: "opus" }} />);
  for (const c of noted) expect(screen.getByText(FAST_NOTE[c]!)).toBeTruthy();
  cleanup();
  render(<Harness start={{ model: "haiku" }} />);
  for (const c of noted) expect(screen.queryByText(FAST_NOTE[c]!)).toBeNull();
});

// ANY ENTRY OF A HARNESS THE ENGINE CAN RUN WITH NO TOOL OF ITS OWN may run
// Ask Crewlet as it is: choosing it asks nothing of the entry's web tools, and
// a harness that cannot be run that way is refused with its reason.
test("Ask Crewlet takes any model of a harness a helper can run on, with nothing to change on it", () => {
  render(<Harness start={{ key: "chat" }} />);
  fireEvent.click(screen.getByRole("radio", { name: /^Ask Crewlet/ }));
  expect(latest.uses.assistant).toBe(true);
  expect(latest.webDenied).toBe(false);
  expect(screen.queryByRole("button", { name: /Deny web tools/ })).toBeNull();
  cleanup();
  render(<Harness start={{ key: "review", harness: "codex" }} />);
  const refused = screen.getByRole("radio", { name: /^Ask Crewlet/ });
  expect(
    refused.getAttribute("aria-disabled") === "true" || (refused as HTMLInputElement).disabled,
  ).toBe(true);
});

// SAVED WITHOUT SEATS: Everything with no seat picked reads as no seat, never
// as a sentence with nobody in it.
test("the summary of Everything with no seat picked says no seat uses it", () => {
  const draft = { ...emptyDraft(EXECUTORS), key: "haiku", intent: "all" as const, uses: noUses() };
  expect(summaryOf(split, draft)).toEqual([
    "No seat uses haiku yet. Every seat stays on the model it has now.",
  ]);
});

// M-018: PAST A FEW SEATS, a line counts them; the names stay a press away.
test("the summary counts the seats past a few, and names them all when asked", () => {
  const many = {
    ...emptyDraft(EXECUTORS),
    key: "haiku",
    intent: "coding" as const,
    uses: noUses(),
  };
  many.uses.jobs.executor = ["backend-engineer", "designer", "pm"];
  expect(summaryOf(split, many, 2)).toEqual(["3 seats will code with haiku."]);
  expect(summaryOf(split, many)[0]).toBe(
    "Backend Engineer, Product Manager, and Designer will code with haiku.",
  );
});

// EVERYTHING GIVES THE MODEL TO EACH HELPER WITH NONE, in the uses its one
// revision writes, and Review names each helper it gives.
test("Everything gives Ask Crewlet and the Advisor the model where neither has one", () => {
  const bare: ModelsData = { ...split, assistant: { key: null }, advisor: { key: null } };
  render(<Harness company={bare} start={{}} />);
  fireEvent.click(screen.getByRole("radio", { name: /Everything/ }));
  expect(latest.intent).toBe("all");
  expect(latest.uses.assistant).toBe(true);
  expect(latest.uses.advisor).toBe(true);
  expect(screen.getByText("It also answers in Ask Crewlet.")).toBeTruthy();
  expect(screen.getByText("It also explains the Advisor's findings.")).toBeTruthy();
  expect(seatChangesOf(bare, "haiku", noUses(), latest.uses)).toEqual([
    {
      key: "helpers",
      subject: "Helpers",
      rows: [
        { kind: "added", label: "Ask Crewlet", to: "haiku" },
        { kind: "added", label: "Advisor", to: "haiku" },
      ],
    },
  ]);
});

// A HELPER ON A MODEL KEEPS IT, as a seat with one does; one naming a model
// the company no longer has runs on nothing, so Everything takes it.
test("Everything leaves a helper on another model where it is, and takes one whose model is gone", () => {
  const on = { ...split, assistant: { key: "default" }, advisor: { key: "removed-model" } };
  render(<Harness company={on} start={{}} />);
  fireEvent.click(screen.getByRole("radio", { name: /Everything/ }));
  expect(latest.uses.assistant).toBe(false);
  expect(latest.uses.advisor).toBe(true);
});

// THE ADVISOR'S SWITCH IS ITS OWN: turned off in an edit, Everything leaves it off.
test("in an edit, Everything keeps the Advisor off once its switch took it off", () => {
  const today = usesOf(split.models.find((m) => m.key === "default")!.entry.used_by);
  const held = { ...split, assistant: { key: null }, advisor: { key: "default" } };
  render(
    <Harness
      company={held}
      served={{ ...today, advisor: true }}
      start={{ key: "default", intent: "all", uses: { ...today, advisor: true } }}
    />,
  );
  fireEvent.click(screen.getByText("Customise per seat"));
  fireEvent.click(screen.getByRole("switch", { name: /Also explain the Advisor's findings/ }));
  expect(latest.uses.advisor).toBe(false);
  fireEvent.click(screen.getByRole("radio", { name: /^Coding/ }));
  fireEvent.click(screen.getByRole("radio", { name: /^Everything/ }));
  expect(latest.uses.advisor).toBe(false);
  expect(latest.uses.assistant).toBe(true);
});

test("Everything on a harness that runs no helper gives none, and does not say it would", () => {
  const bare: ModelsData = { ...split, assistant: { key: null }, advisor: { key: null } };
  render(<Harness company={bare} start={{ harness: "opencode" }} />);
  expect(screen.queryByText(EVERYTHING.helpers)).toBeNull();
  fireEvent.click(screen.getByRole("radio", { name: /Everything/ }));
  expect(latest.intent).toBe("all");
  expect(latest.uses.assistant).toBe(false);
  expect(latest.uses.advisor).toBe(false);
  cleanup();
  render(<Harness company={bare} start={{}} />);
  expect(screen.getByText(EVERYTHING.helpers)).toBeTruthy();
});

test("everythingHelpers takes a helper with no model or one on a model gone, never one on a model", () => {
  const at = (assistant: string | null, advisor: string | null, harness = "claude-code") =>
    everythingHelpers(
      { ...split, assistant: { key: assistant }, advisor: { key: advisor } },
      harness,
    );
  expect(at(null, null)).toEqual({ assistant: true, advisor: true });
  expect(at("default", "gone")).toEqual({ assistant: false, advisor: true });
  expect(at("gone", "default-host")).toEqual({ assistant: true, advisor: false });
  expect(at(null, null, "opencode")).toEqual({ assistant: false, advisor: false });
});
