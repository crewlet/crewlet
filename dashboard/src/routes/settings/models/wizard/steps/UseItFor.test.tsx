/**
 * Use it for: what a model does, and for which seats.
 *
 * The invariants: a category writes exactly the jobs `CATEGORY_JOBS` gives
 * it; an edit's choice keeps the seats the model serves, never every seat;
 * no choice row carries a "Replaces" line, Review lists the changes; seats
 * are named, never keyed; a document that split a category reads back half-checked;
 * a CLI that cannot code refuses the coding box of a seat without it; the
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
  usesOf,
  type Uses,
} from "~/lib/modelUses.ts";
import {
  EVERY_JOB,
  modelEntry,
  modelsData,
  runs,
  seat,
} from "~/routes/settings/models/fixtures.ts";
import type { ModelsData } from "../../data.ts";
import { CATEGORY_WORDS, FAST_NOTE } from "../../words.ts";
import { emptyDraft, type Draft } from "../draft.ts";
import { UseItFor, seatChangesOf } from "./UseItFor.tsx";

const data = modelsData({
  entries: [
    modelEntry({
      key: "default",
      used_by: [...runs("backend-engineer", ...EVERY_JOB), ...runs("pm", ...EVERY_JOB)],
    }),
  ],
  assistant: { enabled: true, key: "default" },
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
    ...emptyDraft(),
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
 * GoPhrase's split: `default` runs every job for the engineers and QA,
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
  // AN ADD still starts from every seat, where no model serves them yet.
  cleanup();
  render(<Harness company={split} start={{ key: "new" }} />);
  fireEvent.click(screen.getByRole("radio", { name: new RegExp(CATEGORY_WORDS.coding.label) }));
  expect(latest.uses.jobs.executor).toEqual(["backend-engineer", "designer", "pm", "qa"]);
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
    "Quality Lead, routines and evaluations: no longer on default",
    "Product Manager, coding: default-host → default",
  ]);
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

test("a CLI that cannot code refuses a seat's coding box, says why once, and lets a ticked one off", () => {
  const uses = noUses();
  for (const job of CATEGORY_JOBS.coding) uses.jobs[job] = ["qa"];
  render(<Harness company={split} start={{ agent: "codex", intent: "custom", uses }} />);
  const box = (name: string) => screen.getByRole("checkbox", { name }) as HTMLInputElement;
  const coding = CATEGORY_WORDS.coding.label;
  expect(box(`Product Manager ${coding}`).disabled).toBe(true);
  expect(box(`Quality Lead ${coding}`).disabled).toBe(false);
  expect(box(`Product Manager ${CATEGORY_WORDS.reviews.label}`).disabled).toBe(false);
  // The grid says why under itself, and the section says it once, whatever the number of seats.
  const section = screen.getByRole("region", { name: "Customise per seat" });
  expect(within(section).getAllByText(/but coding|cannot run coding/)).toHaveLength(1);
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

test("the assistant asks for web tools denied, and one click denies them", () => {
  render(<Harness start={{ key: "chat" }} />);
  fireEvent.click(screen.getByRole("radio", { name: /Assistant/ }));
  expect(latest.uses.assistant).toBe(true);
  fireEvent.click(screen.getByRole("button", { name: "Deny web tools for this model" }));
  expect(latest.webDenied).toBe(true);
  expect(screen.queryByRole("button", { name: "Deny web tools for this model" })).toBeNull();
});

test("Everything on a model that allows web tools says the assistant cannot use it", () => {
  const warning = "The Dashboard Assistant can't use this model while web tools are allowed.";
  render(<Harness start={{}} />);
  fireEvent.click(screen.getByRole("radio", { name: /Everything/ }));
  expect(screen.getByText(warning)).toBeTruthy();
  cleanup();
  render(<Harness start={{ webDenied: true }} />);
  fireEvent.click(screen.getByRole("radio", { name: /Everything/ }));
  expect(screen.queryByText(warning)).toBeNull();
});
