/**
 * Code › Checks, and the pane every checks listing opens an attempt in.
 *
 * The invariants: the section asks the engine's window by the window's own
 * start and end, narrowed by state and by a check's name as the ENGINE
 * narrows them;
 * it charts and counts the attempts it holds, and says so; an attempt opens
 * in a pane that reads it whole — each failure a link into the diff it was
 * checked on, the tail of its output, and its log a page at a time by its
 * object, never asked until the reader asks; and a repository's tab lists its
 * own attempts with no window.
 */

import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { CODE_POSITION, jsonAnswer, mountCode, stubCodeEngine } from "~/test/codeScreens.tsx";
import { Checks, checkBars } from "./Checks.tsx";
import { RepoChecks } from "./checks/RepoChecks.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

const OID = "d".repeat(40);
const BASE = "e".repeat(40);
const page = { position: CODE_POSITION, level: "stale" };

function attempt(name: string, state: string, extra: Record<string, unknown> = {}) {
  return {
    repo_id: "id-web",
    repo: "web",
    oid: OID,
    target: "refs/heads/main",
    name,
    attempt: 1,
    state,
    purpose: "head",
    base: BASE,
    duration_ms: 90_000,
    finished_at: new Date(Date.now() - 60_000).toISOString(),
    ...extra,
  };
}

const LISTED = () =>
  jsonAnswer({ checks: [attempt("unit", "failed"), attempt("lint", "passed")], ...page });

const WHOLE = () =>
  jsonAnswer({
    check: {
      ...attempt("unit", "failed"),
      summary: "1 test failed",
      exit_code: 1,
      tail: "--- FAIL: TestRetry",
      failures: [
        { path: "app/retry.go", line: 12, test_id: "TestRetry", message: "never stopped" },
      ],
      log: { object: "objects/log-1", hash: "h", size: 2048 },
    },
    ...page,
  });

// A CUSTOM INTERVAL IS THE ENGINE'S TO BOUND, at both ends: the screen asks
// `since` and `until` as the interval names them, so the engine's first page
// is the interval's own newest attempts, and draws every row it is sent.
// Mutation: ask with no `until` (and trim on the client) → red.
test("a custom interval asks the engine for its start and its end", async () => {
  const from = "2026-10-10T00:00:00.000Z";
  const to = "2026-10-11T00:00:00.000Z";
  const asked = stubCodeEngine({
    "/query/code_checks": () =>
      jsonAnswer({
        checks: [attempt("unit", "failed", { finished_at: "2026-10-10T12:00:00Z" })],
        ...page,
      }),
  });
  mountCode(`#/code/checks?window=${from}/${to}`, <Checks />);
  const grid = await screen.findByRole("table", { name: "Attempts of checks" });
  expect(within(grid).getByText("unit")).toBeTruthy();
  const first = asked.find((u) => u.pathname === "/query/code_checks")!;
  expect(first.searchParams.get("since")).toBe(from);
  expect(first.searchParams.get("until")).toBe(to);
});

// THE WINDOW IS THE ENGINE'S `since`, and a narrowing is the engine's: a
// state chip asks `state=`. The rows are named by repository and commit.
// Mutation: narrow on the client, or ask with no `since`, and this fails.
test("the section asks the window from its start and narrows by state as the engine does", async () => {
  const asked = stubCodeEngine({ "/query/code_checks": LISTED });
  mountCode("#/code/checks", <Checks />);
  const grid = await screen.findByRole("table", { name: "Attempts of checks" });
  expect(within(grid).getByText("unit")).toBeTruthy();
  expect(within(grid).getAllByText("web").length).toBeGreaterThan(0);
  const first = asked.find((u) => u.pathname === "/query/code_checks")!;
  expect(Date.parse(first.searchParams.get("since") ?? "")).toBeGreaterThan(
    Date.now() - 2 * 86_400_000,
  );
  expect(first.searchParams.has("repo")).toBe(false);
  // BOTH RAILS say their counts are the loaded rows', never the window's.
  expect(screen.getAllByText(/counts over the rows loaded/)).toHaveLength(2);

  fireEvent.click(
    within(screen.getByRole("group", { name: "State" })).getByRole("button", { name: /failed/ }),
  );
  await waitFor(() =>
    expect(
      asked.some(
        (u) => u.pathname === "/query/code_checks" && u.searchParams.get("state") === "failed",
      ),
    ).toBe(true),
  );
});

// THE BARS ARE THE ATTEMPTS THAT ENDED, by when, the failed ones their foot.
test("the chart buckets ended attempts by when they ended, failures apart", () => {
  const since = "2026-11-01T00:00:00Z";
  const until = "2026-11-01T03:00:00Z";
  const bars = checkBars(
    [
      attempt("unit", "failed", { finished_at: "2026-11-01T00:10:00Z" }),
      attempt("lint", "passed", { finished_at: "2026-11-01T00:20:00Z" }),
      attempt("vet", "passed", { finished_at: "2026-11-01T02:20:00Z" }),
      attempt("e2e", "running", { finished_at: undefined }),
    ] as never,
    since,
    until,
    "hour",
  );
  expect(bars.map((b) => [b.count, b.failed])).toEqual([
    [2, 1],
    [0, 0],
    [1, 0],
  ]);
});

// ONE ATTEMPT, WHOLE: what it said, each failure a link into the diff it was
// checked on — the comparison of its base and its commit, on the file — and
// the tail; its log is asked only when the reader asks, by its object.
// Mutation: link a failure to the commit alone, or read the log on open, and
// this fails.
test("an attempt opens whole, its failures linked into its diff and its log read on demand", async () => {
  const asked = stubCodeEngine({
    "/query/code_checks": LISTED,
    "/query/code_check": WHOLE,
    "/query/code_check_log": () =>
      jsonAnswer({ object: "objects/log-1", size: 2048, offset: 0, text: "=== RUN TestRetry\n" }),
  });
  mountCode("#/code/checks", <Checks />);
  const grid = await screen.findByRole("table", { name: "Attempts of checks" });
  // A ROW OPENS by its own control, which the grid lays over the row.
  const row = within(grid).getByText("unit").closest<HTMLElement>('[role="row"]')!;
  fireEvent.click(within(row).getByRole("button"));
  const pane = await screen.findByLabelText("Check unit, attempt 1");
  const failure = await within(pane).findByRole("link", { name: "app/retry.go:12" });
  const target = new URL(failure.getAttribute("href")!, "http://x");
  expect(target.hash).toContain("/code/repos/web/compare");
  expect(target.hash).toContain(`base=${BASE}`);
  expect(target.hash).toContain(`head=${OID}`);
  expect(target.hash).toContain("file=app%2Fretry.go");
  expect(within(pane).getByText("--- FAIL: TestRetry")).toBeTruthy();
  expect(asked.some((u) => u.pathname === "/query/code_check_log")).toBe(false);

  fireEvent.click(within(pane).getByRole("button", { name: /Read its log/ }));
  expect(await within(pane).findByText(/=== RUN TestRetry/)).toBeTruthy();
  const log = asked.find((u) => u.pathname === "/query/code_check_log")!;
  expect(log.searchParams.get("object")).toBe("objects/log-1");
  expect(log.searchParams.get("offset")).toBe("0");
  expect(within(pane).getByText("That is the whole log.")).toBeTruthy();
});

// A REPOSITORY'S TAB is its own listing, which takes no window.
// Mutation: send the section's `since` beside `repo=` (a bad_params), and
// this fails.
test("a repository's Checks tab lists its own attempts, with no window", async () => {
  const asked = stubCodeEngine({ "/query/code_checks": LISTED });
  mountCode("#/code/repos/web?tab=checks", <RepoChecks repo="web" />);
  const grid = await screen.findByRole("table", { name: "Checks of web" });
  expect(within(grid).getByText("lint")).toBeTruthy();
  const first = asked.find((u) => u.pathname === "/query/code_checks")!;
  expect(first.searchParams.get("repo")).toBe("web");
  expect(first.searchParams.has("since")).toBe(false);
});
