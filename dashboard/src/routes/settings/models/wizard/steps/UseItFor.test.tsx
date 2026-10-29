/**
 * Use it for: what a model does, and for which seats.
 *
 * The invariants: a category writes exactly the jobs `CATEGORY_JOBS` gives
 * it; what a choice replaces is said on a line of its own; seats are named,
 * never keyed; a document that split a category reads back half-checked; the
 * fast-model note is quiet and only where a category carries one; and the
 * web tools rule is said wherever a choice meets it, with its one-click fix.
 */

import { useState } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { CATEGORY_JOBS, CATEGORY_ORDER, SEAT_JOBS, noUses } from "~/lib/modelUses.ts";
import { EVERY_JOB, modelEntry, modelsData, runs } from "~/routes/settings/models/fixtures.ts";
import { CATEGORY_WORDS, FAST_NOTE } from "../../words.ts";
import { emptyDraft, type Draft } from "../draft.ts";
import { UseItFor } from "./UseItFor.tsx";

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
function Harness({ start }: { start: Partial<Draft> }) {
  const [draft, setDraft] = useState<Draft>({
    ...emptyDraft(),
    key: "haiku",
    model: "haiku",
    ...start,
  });
  latest = draft;
  return <UseItFor draft={draft} data={data} onChange={(p) => setDraft((d) => ({ ...d, ...p }))} />;
}

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

test("what a choice replaces is a line of its own, and seats are named rather than keyed", () => {
  render(<Harness start={{ intent: "all", uses: { ...noUses(), jobs: { ...noUses().jobs } } }} />);
  const replaced = screen.getAllByText("Replaces default for 2 seats");
  expect(replaced.length).toBeGreaterThan(0);
  for (const line of replaced) expect(line.closest(".models-choice-detail")).not.toBeNull();
  fireEvent.click(screen.getByRole("radio", { name: /Everything/ }));
  expect(screen.getAllByText("Backend Engineer").length).toBeGreaterThan(0);
  expect(screen.queryByText("backend-engineer")).toBeNull();
});

test("a category the document split by hand reads back half-checked", () => {
  const split = CATEGORY_ORDER.find((c) => CATEGORY_JOBS[c].length > 1)!;
  const uses = noUses();
  uses.jobs[CATEGORY_JOBS[split][0]!] = ["qa"];
  render(<Harness start={{ intent: "custom", uses }} />);
  const box = screen.getByRole("checkbox", {
    name: `Quality Lead: ${CATEGORY_WORDS[split].label} on haiku`,
  }) as HTMLInputElement;
  expect(box.indeterminate).toBe(true);
  expect(box.checked).toBe(false);
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
