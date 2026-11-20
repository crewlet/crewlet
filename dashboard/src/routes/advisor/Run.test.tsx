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
