/**
 * The Tools tab and the Timeline say the same thing about the same call.
 *
 * THE CASE IS A STRUCTURED SUBMISSION: stamped, and finished inside a
 * millisecond, so its `duration_ms` is a MEASURED 0. The Timeline drew it with
 * a bar (it is stamped) while this tab, reading the duration alone, listed it
 * as never timed. Both now ask the waterfall's one answer (`callMeasured`).
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { buildWaterfall } from "~/lib/waterfall.ts";
import { phaseRecord, toolCall } from "~/test/phaseRecord.ts";
import { ToolsTab } from "./Tools.tsx";

afterEach(cleanup);

const PHASE = phaseRecord({
  key: "turn-1|execute|1",
  startedAt: "2026-09-28T10:00:00Z",
  durationMs: 4_000,
  tools: [
    toolCall({ name: "submit_work", startedAt: "2026-09-28T10:00:03Z", durationMs: 0 }),
    toolCall({ name: "bridged_call", startedAt: "", durationMs: 0 }),
  ],
});

function mount(onOpen = vi.fn()) {
  const model = buildWaterfall({
    events: [],
    phases: [PHASE],
    now: Date.parse("2026-09-28T10:01:00Z"),
    running: false,
    parked: false,
  });
  render(
    <Router>
      <ToolsTab phases={[PHASE]} model={model} onOpen={onOpen} />
    </Router>,
  );
  return { model, onOpen };
}

const row = (name: string) =>
  screen.getByText(name).closest(".crewlet-data-grid__row") as HTMLElement;

test("a stamped call that took under a millisecond is timed on both tabs", () => {
  const { model } = mount();
  const drawn = model.spans.find((s) => s.label === "submit_work");
  expect(drawn, "the Timeline draws the stamped call").toBeTruthy();
  expect(within(row("submit_work")).queryByText("not timed")).toBeNull();
  expect(within(row("submit_work")).getByText("0ms")).toBeTruthy();
});

test("a call nothing measured is not timed on both tabs", () => {
  const { model } = mount();
  expect(model.spans.find((s) => s.label === "bridged_call")).toBeUndefined();
  expect(model.untimed.map((u) => u.label)).toContain("bridged_call");
  expect(within(row("bridged_call")).getByText("not timed")).toBeTruthy();
});

// A ROW OPENS WHAT HOLDS IT: its own span where the Timeline drew one, and the
// Timeline alone — whose list names it — where it did not, never a span id
// the waterfall has no row for.
test("a row opens its span, or the Timeline when it has none", () => {
  const { model, onOpen } = mount();
  fireEvent.click(row("submit_work").querySelector(".crewlet-data-grid__row-link")!);
  const id = model.spans.find((s) => s.label === "submit_work")!.id;
  expect(onOpen).toHaveBeenLastCalledWith(id);
  fireEvent.click(row("bridged_call").querySelector(".crewlet-data-grid__row-link")!);
  expect(onOpen).toHaveBeenLastCalledWith("");
});

/**
 * THE CALLS NO RECORD LISTS ARE SAID. A collected executor's record keeps its
 * run's head and tail and counts the middle it dropped, by tool; the tab lists
 * the calls the records kept and says how many more there were, and what, so
 * two hundred rows of a run that made more never read as the whole run.
 *
 * Mutation: list the rows alone, and the dropped calls are nowhere on the tab.
 */
test("the calls no record lists are said above the ones it does", () => {
  const phase = {
    ...PHASE,
    callsElided: 63,
    elidedCalls: [{ name: "read_file", calls: 63, failed: 2 }],
  };
  const model = buildWaterfall({
    events: [],
    phases: [phase],
    now: Date.parse("2026-09-28T10:01:00Z"),
    running: false,
    parked: false,
  });
  render(
    <Router>
      <ToolsTab phases={[phase]} model={model} onOpen={vi.fn()} />
    </Router>,
  );
  expect(screen.getByText(/63 more calls aren’t listed/)).toBeTruthy();
  expect(screen.getByText(/read_file 63 times \(2 failed\)/)).toBeTruthy();
});

/**
 * A HELD CALL IS NEITHER A FAILURE NOR A CALL THAT RETURNED. An executor's call
 * held for a person's approval is recorded failed only because nothing it
 * asked for happened: it is the run's question for a person, and the tab says
 * so, as the execute card and the executors board do. The call they approved
 * returned.
 *
 * Mutation: draw a held call as returned, and the run's question reads as a
 * call that ran.
 */
test("a held call reads as held for approval", () => {
  const phase = phaseRecord({
    key: "turn-1|execute|1",
    startedAt: "2026-09-28T10:00:00Z",
    durationMs: 4_000,
    tools: [
      toolCall({ name: "gh_delete_repo", held: true, result: "held for a person" }),
      toolCall({ name: "gh_merge", approved: true }),
    ],
  });
  const model = buildWaterfall({
    events: [],
    phases: [phase],
    now: Date.parse("2026-09-28T10:01:00Z"),
    running: false,
    parked: false,
  });
  render(
    <Router>
      <ToolsTab phases={[phase]} model={model} onOpen={vi.fn()} />
    </Router>,
  );
  expect(within(row("gh_delete_repo")).getByText("held for approval")).toBeTruthy();
  expect(within(row("gh_delete_repo")).queryByText("failed")).toBeNull();
  expect(within(row("gh_merge")).getByText("returned")).toBeTruthy();
});
