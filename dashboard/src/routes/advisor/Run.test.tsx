/**
 * The Advisor's run at the head of its page: one line of where it stands,
 * what a running run is doing now and how far through its questions it is,
 * every step folded under Steps, and the sources it could not read.
 */

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { RunLine, runDetail, runState, runView, runWords, type ViewRun } from "./Run.tsx";

afterEach(cleanup);

const NOW = Date.parse("2026-11-20T10:00:00Z");

const running: ViewRun = {
  status: "running",
  startedAt: "2026-11-20T09:58:48Z",
  endedAt: null,
  opened: 0,
  resolved: 0,
  progress: [
    {
      at: "2026-11-20T09:59:00Z",
      text: "Reading flow hours",
      step: "read",
      state: "done",
      detail: "flow_hours days=7",
      why: "",
    },
    {
      at: "2026-11-20T09:59:30Z",
      text: "Testing: releases pile up behind one seat",
      step: "note",
      state: "running",
      detail: "",
      why: "",
    },
  ],
  unread: [],
  silent: [],
  kind: "rules",
  stoppedBy: null,
  budget: null,
  reason: null,
  dropped: 0,
  droppedWhy: {},
  repaired: {},
  deepSkipped: null,
  keptFrom: null,
  agenda: null,
};

// A RUN IS FOLLOWED, NEVER WAITED ON, AND SAID IN TWO LINES: its state and
// how long it has run, then what it is doing now, every step folded under a
// toggle that opens them as a timeline, each with its state and the read
// behind it. Mutation: list the steps open, and the head is a log again.
test("a running run says its state, how long, and what it is doing now, its steps folded", async () => {
  render(<RunLine run={running} now={NOW} />);
  expect(screen.getByText("Running checks")).toBeDefined();
  expect(screen.getByText("1m 12s")).toBeDefined();
  expect(screen.getByText("Testing: releases pile up behind one seat")).toBeDefined();
  expect(screen.queryByRole("list", { name: "What the run did" })).toBeNull();
  const toggle = screen.getByRole("button", { name: "2 steps" });
  expect(toggle.getAttribute("aria-expanded")).toBe("false");
  await act(async () => {
    fireEvent.click(toggle);
  });
  const steps = screen.getByRole("list", { name: "What the run did" });
  expect([...steps.querySelectorAll("li")].map((li) => li.dataset.state)).toEqual([
    "done",
    "running",
  ]);
  expect(steps.textContent).toContain("flow_hours days=7");
});

// A FAILED STEP IS ONE STEP, SAID AS FAILED WITH WHY: the toggle counts it,
// the run's second line names it once the run has ended, and the timeline
// draws its reason under it. Mutation: draw the engine's state as done, and
// the failure reads as a step that went well.
test("a failed read is counted, named, and explained", async () => {
  const ended: ViewRun = {
    ...running,
    status: "done",
    kind: "deep",
    endedAt: "2026-11-20T09:59:50Z",
    progress: [
      { ...running.progress[0]!, state: "done" },
      {
        at: "2026-11-20T09:59:40Z",
        text: "Could not read open work items",
        step: "read",
        state: "failed",
        detail: "work_items status_group=open",
        why: '"open" is not one of the four status groups',
      },
    ],
  };
  render(<RunLine run={ended} now={NOW} />);
  expect(screen.getByText("Could not read open work items")).toBeDefined();
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "2 steps, 1 failed" }));
  });
  const steps = screen.getByRole("list", { name: "What the run did" });
  const failed = [...steps.querySelectorAll("li")].find((li) => li.dataset.state === "failed")!;
  expect(failed.textContent).toContain('"open" is not one of the four status groups');
  expect(failed.textContent).toContain("work_items status_group=open");
});

// A RUN WHOSE OWNER STOPPED IS SAID AS INTERRUPTED, never as running: the
// engine closed it, and the head says it did not finish and why.
test("an interrupted run reads as interrupted, with the engine's reason", () => {
  const stopped: ViewRun = {
    ...running,
    status: "interrupted",
    kind: "deep",
    endedAt: "2026-11-20T09:00:00Z",
    reason: "The engine stopped during the run, so it did not finish.",
  };
  expect(runState(stopped, NOW)).toMatchObject({
    label: "Investigation interrupted",
    tone: "warning",
    pulse: false,
  });
  expect(runWords(stopped, NOW)).toBe(
    "The last deep run stopped 1 hour ago. The engine stopped during the run, so it did not finish.",
  );
});

// UNREAD, THE HEAD HOLDS ITS TWO LINES' ROOM and its controls stay mounted,
// so nothing under it moves and a press is never lost.
test("while the run is read the head holds its room, controls in place", () => {
  const { container } = render(
    <RunLine run={running} now={NOW} loading controls={<button type="button">Run</button>} />,
  );
  expect(container.querySelectorAll(".advisor-head__state, .advisor-head__trail")).toHaveLength(2);
  expect(screen.queryByText("Running checks")).toBeNull();
  expect(screen.getByRole("button", { name: "Run" })).toBeDefined();
});

// HOW FAR A DEEP RUN IS: its questions answered, as a meter under its line.
test("an investigation says how many of its questions it has answered", () => {
  render(
    <RunLine run={{ ...running, kind: "deep", agenda: { items: 9, answered: 3 } }} now={NOW} />,
  );
  expect(screen.getByText("Investigating")).toBeDefined();
  expect(screen.getByText("3 of 9 answered")).toBeDefined();
  const meter = screen.getByRole("meter");
  expect(meter.getAttribute("aria-valuetext")).toBe("3 of 9 questions answered");
});

test("a run that could not read a source says so, in short and in full", () => {
  const unread: ViewRun = {
    ...running,
    status: "done",
    endedAt: "2026-11-20T09:59:50Z",
    progress: [],
    unread: ["flow"],
  };
  render(<RunLine run={unread} now={NOW} />);
  expect(screen.getByText("Checked just now")).toBeDefined();
  expect(runState(unread, NOW)).toMatchObject({
    tone: "warning",
    notes: ["nothing new", "1 source not read"],
  });
  expect(runDetail(unread, NOW)).toMatch(/^All checks ran just now\. Could not read /);
  expect(screen.queryByRole("button", { name: /Steps/ })).toBeNull();
});

// AN ENDED RUN IS ONE LINE: what it moved, the bound it stopped at and what
// it left out, each a word or two; the sentence in full is its tooltip.
test("an ended investigation says what it moved and where it stopped, in a few words", () => {
  const ended: ViewRun = {
    ...running,
    kind: "deep",
    status: "done",
    endedAt: "2026-11-20T09:57:00Z",
    opened: 2,
    resolved: 1,
    dropped: 1,
    stoppedBy: "minutes",
  };
  expect(runState(ended, NOW)).toEqual({
    label: "Investigated 3 minutes ago",
    tone: "warning",
    pulse: false,
    notes: ["2 new", "1 closed", "hit its time limit", "1 left out"],
  });
  expect(runState({ ...ended, stoppedBy: "done", dropped: 0 }, NOW).tone).toBe("success");
  expect(
    runState({ ...ended, deepSkipped: "kept", keptFrom: "2026-11-20T08:00:00Z" }, NOW),
  ).toMatchObject({
    label: "Checked 3 minutes ago",
    notes: ["2 new", "1 closed", "investigation kept"],
  });
  expect(runState(runView(null), NOW).label).toBe("Not run yet");
});

test("no run yet, and a run that did not finish, are said as such", () => {
  expect(runWords(runView(null), NOW)).toBe("The checks have not run yet.");
  expect(runWords({ ...running, status: "failed" }, NOW)).toBe(
    "The last run did not finish. It started 1 minute ago.",
  );
});

// A DEEP RUN SAYS SO, and how it ended: what it spent of its bounds, the bound
// it stopped at (so a short list is read as one), and why one could not run.
test("a deep run says it is one, what it spent, the bound it stopped at, and why it could not run", () => {
  const deep: ViewRun = { ...running, kind: "deep" };
  expect(runWords(deep, NOW)).toBe("Deep run for 1m 12s.");
  const ended: ViewRun = {
    ...deep,
    status: "done",
    endedAt: "2026-11-20T09:57:00Z",
    opened: 2,
    stoppedBy: "done",
    budget: { tokens: 412_000, tokens_ceiling: 1_000_000, minutes: 6, minutes_ceiling: 20 },
  };
  expect(runWords(ended, NOW)).toBe(
    "The deep run ended 3 minutes ago. It found 2 new findings, using 412k of 1M tokens in 6 of 20 minutes.",
  );
  expect(runWords({ ...ended, opened: 0, dropped: 2 }, NOW)).toBe(
    "The deep run ended 3 minutes ago. It used 412k of 1M tokens in 6 of 20 minutes, and left out 2 findings it could not ground in what it read.",
  );
  expect(runWords({ ...ended, opened: 0, stoppedBy: "tokens" }, NOW)).toBe(
    "The deep run ended 3 minutes ago at its token limit, so its list may be short. It used 412k of 1M tokens in 6 of 20 minutes.",
  );
  expect(
    runWords(
      {
        ...deep,
        status: "failed",
        reason: "The Advisor has no model assigned (advisor.model), so it cannot investigate.",
      },
      NOW,
    ),
  ).toBe(
    "The last deep run did not run. The Advisor has no model assigned (advisor.model), so it cannot investigate.",
  );
  expect(
    runView({
      run_id: "r",
      trigger: "person",
      by: "",
      unread: [],
      started_at: "2026-11-20T09:50:00Z",
      ended_at: "2026-11-20T09:57:00Z",
      status: "ceiling",
      classes_run: 0,
      opened: 1,
      resolved: 0,
      model: {
        status: "ceiling",
        tokens: 1,
        listed: 1,
        dropped: { unread_record: 2, unknown_seat: 1 },
        reason: "Bound.",
      },
      kind: "deep",
      progress: [],
      budget: null,
      stopped_by: "minutes",
      classes: [],
    }),
  ).toMatchObject({
    status: "done",
    kind: "deep",
    stoppedBy: "minutes",
    reason: "Bound.",
    dropped: 3,
  });
});

// ONE PRESS, ONE RUN: a press whose deep part was kept says from when, one
// that could not investigate says why, and either way what its checks moved.
test("a run says its deep part was kept, or could not run, and why", () => {
  const done = {
    ...running,
    status: "done" as const,
    kind: "deep" as const,
    endedAt: "2026-11-20T09:59:00Z",
    progress: [],
  };
  expect(
    runWords({ ...done, opened: 2, deepSkipped: "kept", keptFrom: "2026-11-20T08:00:00Z" }, NOW),
  ).toBe(
    "All checks ran 1 minute ago. It found 2 new findings. The deep investigation from 2 hours ago is kept.",
  );
  expect(
    runWords(
      {
        ...done,
        deepSkipped: "no_model",
        reason: "The Advisor has no model assigned (advisor.model).",
      },
      NOW,
    ),
  ).toBe(
    "All checks ran 1 minute ago. The deep investigation could not run. The Advisor has no model assigned (advisor.model).",
  );
});

// WHAT THE MODEL FOUND IS ACCOUNTED FOR: findings listed after a repair are
// counted by the repair, and those left out by why, so an empty list never
// reads as nothing found.
test("a deep run counts what it mended and what it left out, and why", () => {
  const view = runView({
    run_id: "r",
    trigger: "person",
    by: "ada",
    started_at: "2026-11-20T09:40:00Z",
    ended_at: "2026-11-20T09:59:00Z",
    status: "done",
    classes_run: 0,
    unread: [],
    opened: 1,
    resolved: 0,
    model: {
      status: "done",
      tokens: 600_000,
      listed: 3,
      dropped: { wrong_number: 1 },
      repaired: { fix_not_built: 2, reworded: 1 },
      reason: "",
    },
    kind: "deep",
    progress: [],
    budget: { tokens: 600_000, tokens_ceiling: 2_000_000, minutes: 19, minutes_ceiling: 20 },
    stopped_by: "done",
    classes: [],
    deep_skipped: null,
    deep_kept_from: null,
  });
  const words = runWords(view, NOW);
  expect(words).toContain("listed 2 findings with no fix it could build");
  expect(words).toContain("reworded 1 finding the wording rule refused");
  expect(words).toContain(
    "left out 1 finding it could not ground in what it read (1 cited a number not where it said)",
  );
});
