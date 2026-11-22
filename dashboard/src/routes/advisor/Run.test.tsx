/**
 * The Advisor's run at the head of its page: how long a running run has run,
 * the steps it reported with the newest on its line, and the sources it could
 * not read.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { RunLine, runView, runWords, type ViewRun } from "./Run.tsx";

afterEach(cleanup);

const NOW = Date.parse("2026-11-20T10:00:00Z");

const running: ViewRun = {
  status: "running",
  startedAt: "2026-11-20T09:58:48Z",
  endedAt: null,
  opened: 0,
  resolved: 0,
  progress: [
    { at: "2026-11-20T09:59:00Z", text: "Reading flow hours." },
    { at: "2026-11-20T09:59:30Z", text: "Testing: releases pile up behind one seat." },
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
};

// A RUN IS FOLLOWED, NEVER WAITED ON: its elapsed time and what it is doing
// now are on its line, the steps before under it.
test("a running run says how long it has run and the lead it is testing, its steps under it", () => {
  render(<RunLine run={running} now={NOW} />);
  expect(
    screen.getByText("Running for 1m 12s. Testing: releases pile up behind one seat."),
  ).toBeDefined();
  const steps = screen.getByRole("list", { name: "What the run did so far" });
  expect(steps.textContent).toBe("Reading flow hours.");
});

test("a run that could not read a source says so", () => {
  render(
    <RunLine
      run={{
        ...running,
        status: "done",
        endedAt: "2026-11-20T09:59:50Z",
        progress: [],
        unread: ["flow"],
      }}
      now={NOW}
    />,
  );
  expect(screen.getByText(/^All checks ran just now\. Could not read /)).toBeDefined();
  expect(screen.queryByRole("list")).toBeNull();
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
