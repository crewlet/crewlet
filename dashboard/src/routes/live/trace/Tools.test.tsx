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
    toolCall({ name: "legacy_call", startedAt: "", durationMs: 0 }),
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

const row = (name: string) => screen.getByText(name).closest(".grid-row") as HTMLElement;

test("a stamped call that took under a millisecond is timed on both tabs", () => {
  const { model } = mount();
  const drawn = model.spans.find((s) => s.label === "submit_work");
  expect(drawn, "the Timeline draws the stamped call").toBeTruthy();
  expect(within(row("submit_work")).queryByText("not timed")).toBeNull();
  expect(within(row("submit_work")).getByText("0ms")).toBeTruthy();
});

test("a call nothing measured is not timed on both tabs", () => {
  const { model } = mount();
  expect(model.spans.find((s) => s.label === "legacy_call")).toBeUndefined();
  expect(model.untimed.map((u) => u.label)).toContain("legacy_call");
  expect(within(row("legacy_call")).getByText("not timed")).toBeTruthy();
});

// A ROW OPENS WHAT HOLDS IT: its own span where the Timeline drew one, and the
// Timeline alone — whose list names it — where it did not, never a span id
// the waterfall has no row for.
test("a row opens its span, or the Timeline when it has none", () => {
  const { model, onOpen } = mount();
  fireEvent.click(row("submit_work"));
  const id = model.spans.find((s) => s.label === "submit_work")!.id;
  expect(onOpen).toHaveBeenLastCalledWith(id);
  fireEvent.click(row("legacy_call"));
  expect(onOpen).toHaveBeenLastCalledWith("");
});
