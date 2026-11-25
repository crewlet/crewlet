/**
 * EVERY FIELD AND CONTROL A STEP TAKEN IN CREWLET NAMES IS ON THE FORM.
 *
 * The engine renders each outside-step integration's setup at every point of
 * its steps (`internal/api/setupapi/testdata/setup_steps.json`, `make
 * setup-steps`). This suite draws the form from each point and finds every
 * name a Crewlet step marks: its words on the form the step starts from, its
 * result on the form it leaves. A step naming a button the form does not draw
 * is a step nobody can follow. The Agents tab step is held by the Agents tab's
 * own suite.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { ReactElement } from "react";
import { cleanup, render as rtlRender, screen } from "@testing-library/react";
import { LayerHost } from "@crewlethq/ui";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { SetupForm } from "./SetupForm.tsx";
import { BUCKET_WORDS } from "./integrations/model.ts";
import { rest, type SetupStep, type SetupToolState } from "~/protocol/index.ts";
import type { VendorTextPart } from "~/contract/vendortext.ts";

interface StepsTool {
  kind: string;
  states: { name: string; tool: SetupToolState }[];
}

const FIXTURE = join(
  process.cwd(),
  "..",
  "internal",
  "api",
  "setupapi",
  "testdata",
  "setup_steps.json",
);

const tools = JSON.parse(readFileSync(FIXTURE, "utf8")) as StepsTool[];

/** The UI names a text marks, each a menu path joined as the engine joins it. */
function names(text: VendorTextPart[] | undefined): string[] {
  return (text ?? []).filter((p) => p.ui && p.ui.length > 0).map((p) => p.ui!.join(" > "));
}

function form(tool: SetupToolState, name: string) {
  const ui: ReactElement = (
    <SetupForm sections={[{ name, tool }]} title={name} onClose={() => {}} onDone={() => {}} />
  );
  return rtlRender(ui, { wrapper: LayerHost });
}

/** Whether the form draws name as a field's label or a control's name, the steps' own words aside. */
function drawn(name: string): boolean {
  const own = (el: HTMLElement) => !el.closest('aside, [aria-label="Setup steps"]');
  return (
    screen.queryAllByText(name, { exact: true }).some(own) ||
    screen.queryAllByRole("button", { name }).some(own)
  );
}

/**
 * The point a step starts from, where every field an earlier step yields is
 * held and its own is not, and the point it leaves.
 */
function around(states: StepsTool["states"], steps: SetupStep[], i: number) {
  const held = (state: StepsTool["states"][number], field: string | undefined) =>
    state.tool.requirements.some((r) => r.field === field && r.present);
  const earlier = steps.slice(0, i).flatMap((s) => (s.yields ? [s.yields] : []));
  const at = states.findIndex(
    (state) => earlier.every((f) => held(state, f)) && !held(state, steps[i]!.yields),
  );
  return { before: states[at], after: states[at + 1] };
}

const STATUS = new Set(Object.values(BUCKET_WORDS).map((w) => w.label));

// THE FORM'S OWN REQUESTS NEVER SETTLE: opening it makes the block's key and
// redraws the block, and this suite reads only what the form draws.
beforeEach(() => {
  vi.spyOn(rest, "post").mockReturnValue(new Promise(() => {}));
  vi.spyOn(rest, "get").mockReturnValue(new Promise(() => {}));
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

test("the fixture holds each outside-step integration's steps, placed", () => {
  expect(tools.map((t) => t.kind).sort()).toEqual(["aws", "google_cloud"]);
  for (const tool of tools) {
    const steps = tool.states[0]!.tool.prepare ?? [];
    expect(steps.length, tool.kind).toBeGreaterThan(2);
    for (const step of steps) expect(step.where, `${tool.kind}: ${step.title}`).toBeTruthy();
  }
});

for (const tool of tools) {
  const steps = tool.states[0]!.tool.prepare ?? [];
  steps.forEach((step, i) => {
    if (step.where !== "Crewlet" || !step.yields) return;
    test(`${tool.kind} step ${i + 1} names only what the form draws`, () => {
      const { before, after } = around(tool.states, steps, i);
      expect(before, "no point of the walk starts this step").toBeTruthy();
      expect(after, "no point of the walk follows this step").toBeTruthy();
      const { unmount } = form(before!.tool, tool.kind);
      for (const name of names(step.text)) {
        expect(drawn(name), `"${name}" in step ${i + 1}, on the form at ${before!.name}`).toBe(
          true,
        );
      }
      unmount();
      form(after!.tool, tool.kind);
      for (const name of names(step.result)) {
        if (STATUS.has(name)) continue;
        expect(drawn(name), `"${name}" in step ${i + 1}'s result, at ${after!.name}`).toBe(true);
      }
    });
  });
}
