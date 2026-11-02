/**
 * Deep work and the files it delivers, as the drawer draws them from the
 * engine's own golden stream (`internal/assistant/wire/testdata/frames.sse`):
 * a deep run is one card redrawn in place as the engine re-sends its step, the
 * steps it made are nested under it rather than listed in the run chip, the
 * chip says the assistant switched to deep work, and a delivered file is
 * previewed only through an `<img>`, never as the markup it carries.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { repoRoot } from "~/test/engineFiles.ts";
import type { AssistantStepEvent } from "~/contract/assistant.ts";
import {
  createAssistantStreamParser,
  type AssistantNarrowFrame,
} from "~/protocol/assistantWire.ts";
import { Router } from "~/app/router.tsx";
import { deepMeasures, deepNote, runChipOf, type CardHandlers } from "./blocks.tsx";
import { Thread } from "./Thread.tsx";
import { EMPTY, reduce, type ConversationView } from "./turn.ts";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const golden = readFileSync(
  join(repoRoot(), "internal/assistant/wire/testdata/frames.sse"),
  "utf8",
);
const frames: AssistantNarrowFrame[] = [];
createAssistantStreamParser({ frame: (f) => frames.push(f), fault: () => {} }).push(golden);
const first = frames[0]!.turn_id;
const own = frames.filter((f) => f.turn_id === first);
const runSeq = own.find((f) => f.kind === "step" && f.step.deep?.state === "running")!.seq;

/** The first golden turn, reduced through the frame at `upTo`. */
function replay(upTo = Infinity): ConversationView {
  let view = reduce(EMPTY, {
    type: "send",
    key: "g",
    text: "Redesign the brand assets",
    page: null,
    attachments: [],
    mode: "ask",
    at: "2026-10-24T09:41:00Z",
  });
  view = reduce(view, { type: "accepted", key: "g", turnId: first, afterSeq: 0 });
  for (const frame of own.filter((f) => f.seq <= upTo))
    view = reduce(view, { type: "frame", frame });
  return view;
}

const on: CardHandlers = {
  approve: () => {},
  decline: () => {},
  prepareAgain: () => {},
  openHere: () => {},
  ask: () => {},
  undo: () => {},
};

const NOW = Date.parse("2026-10-24T09:45:00Z");

function draw(view: ConversationView) {
  return render(
    <Router>
      <Thread
        view={view}
        now={NOW}
        on={on}
        opened={new Set()}
        onRetry={() => {}}
        onReconnect={() => {}}
      />
    </Router>,
  );
}

/** Answer every file read with an SVG carrying a script, and record the object URLs made. */
function stubFiles() {
  const fetched: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      fetched.push(String(input instanceof Request ? input.url : input));
      return new Response(
        '<svg xmlns="http://www.w3.org/2000/svg" id="planted"><script>alert(1)</script></svg>',
      );
    }),
  );
  const made: Blob[] = [];
  vi.stubGlobal(
    "URL",
    Object.assign(URL, {
      createObjectURL: (blob: Blob) => {
        made.push(blob);
        return `blob:test/${made.length}`;
      },
      revokeObjectURL: () => {},
    }),
  );
  return { fetched, made };
}

test("a running deep run draws its goal, limits and activity, and the chip says deep work", () => {
  const view = replay(runSeq);
  const turn = view.turns[0]!;
  expect(runChipOf(turn, NOW).title).toBe("Deep work");
  const { container } = draw(view);
  const card = screen.getByRole("region", { name: /Deep work Working/ });
  expect(within(card).getByText("Redesign the brand assets")).toBeTruthy();
  const meters = within(card).getAllByRole("meter");
  expect(meters.map((m) => m.getAttribute("aria-valuetext"))).toEqual([
    "4m of 20m",
    "1 of 150",
    "12 percent of the run's budget",
  ]);
  const activity = within(card).getByRole("list", { name: "Latest activity" });
  expect(activity.querySelectorAll("code")[1]!.textContent).toBe("Ran a command");
  expect(container.querySelector(".crewlet-run-chip")!.textContent).toContain("Deep work");
});

test("the re-sent run is one card at its latest state, with its nested step under it", () => {
  const view = replay();
  const turn = view.turns[0]!;
  expect(turn.steps.filter((s) => s.deep).length).toBe(1);
  const { container } = draw(view);
  const cards = screen.getAllByRole("region", { name: /^Deep work/ });
  expect(cards).toHaveLength(1);
  expect(cards[0]!.getAttribute("aria-labelledby")).toBeTruthy();
  expect(within(cards[0]!).getByText("Finished")).toBeTruthy();
  // THE NESTED STEP IS THE CARD'S, never the run chip's.
  expect(within(cards[0]!).getByText("Read GET /brand")).toBeTruthy();
  const chip = container.querySelector(".crewlet-run-chip")!;
  expect(chip.textContent).not.toContain("Read GET /brand");
  expect(chip.textContent).not.toContain("Deep work: redesign");
});

test("a nested step whose run was not heard stays in the run chip", () => {
  const view = replay();
  const orphan: AssistantStepEvent = { ...view.turns[0]!.steps.find((s) => s.parent)!, parent: 9 };
  const turn = { ...view.turns[0]!, steps: [orphan] };
  const { container } = draw({ ...view, turns: [turn] });
  expect(container.querySelector(".crewlet-run-chip")!.textContent).toBeTruthy();
  expect(screen.queryByRole("region", { name: /^Deep work/ })).toBeNull();
});

test("many nested steps fold behind their count", () => {
  const view = replay();
  const turn = view.turns[0]!;
  const child = turn.steps.find((s) => s.parent !== undefined)!;
  const many = [1, 2, 3, 4, 5].map((n) => ({ ...child, step_id: `n${n}`, index: 10 + n }));
  draw({ ...view, turns: [{ ...turn, steps: [...turn.steps, ...many] }] });
  const card = screen.getByRole("region", { name: /^Deep work/ });
  expect(
    within(card)
      .getByRole("button", { name: /6 steps/ })
      .getAttribute("aria-expanded"),
  ).toBe("false");
});

test("a stopped or failed run says why; a waiting one says what it waits on", () => {
  const deep = replay(runSeq).turns[0]!.steps.find((s) => s.deep)!.deep!;
  expect(deepNote({ ...deep, state: "stopped", reason: "You stopped it." })).toBe(
    "You stopped it.",
  );
  expect(deepNote({ ...deep, state: "failed" })).toBe("It could not finish.");
  expect(deepNote({ ...deep, state: "waiting" })).toMatch(/approval of the change below/);
  expect(deepNote(deep)).toBeUndefined();
});

test("a settled run's time is how long it ran, not the clock since", () => {
  const run = replay().turns[0]!.steps.find((s) => s.deep)!;
  const [time] = deepMeasures({ step: run, deep: run.deep!, children: [] }, NOW + 3_600_000);
  expect(time!.hint).toBe("6m 52s of 20m");
});

test("a delivered SVG is previewed through an img from its download route, never inlined", async () => {
  const { fetched, made } = stubFiles();
  const { container } = draw(replay());
  const files = screen.getByRole("list", { name: "Delivered files" });
  expect(within(files).getAllByRole("listitem")).toHaveLength(2);
  expect(screen.getByRole("button", { name: "Download logo.svg" })).toBeTruthy();
  await waitFor(() => expect(files.querySelector("img")?.getAttribute("src")).toBe("blob:test/1"));
  // ONE PREVIEW, of the image only, typed from the reference.
  expect(files.querySelectorAll("img")).toHaveLength(1);
  expect(made.map((b) => b.type)).toEqual(["image/svg+xml"]);
  expect(fetched).toHaveLength(1);
  expect(fetched[0]).toMatch(/\/assistant\/conversations\/[0-9a-f-]+\/files\/[0-9a-f-]+$/);
  // NOTHING THE FILE CARRIED IS IN THE PAGE AS MARKUP.
  expect(container.querySelector("#planted, script, iframe, object, embed")).toBeNull();
  expect(container.innerHTML).not.toContain("alert(1)");
});
