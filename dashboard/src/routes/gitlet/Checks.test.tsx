/**
 * Gitlet › Checks, and the pane every checks listing opens an attempt in.
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

import {
  CODE_POSITION,
  jsonAnswer,
  mountCode,
  stubCodeEngine,
  onTheRight,
} from "~/test/codeScreens.tsx";
import { Checks, checkBars } from "./Checks.tsx";
import { failureHref } from "./checks/CheckList.tsx";
import { RepoChecks } from "./checks/RepoChecks.tsx";
import { commitHref } from "./paths.ts";

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
  mountCode(`#/gitlet/checks?window=${from}/${to}`, <Checks />);
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
  mountCode("#/gitlet/checks", <Checks />);
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

// THE WINDOW'S STATE CHIPS ARE THE ENDINGS ALONE: the window lists the
// attempts that ended, and the engine refuses it narrowed to `queued` or
// `running`, so no chip asks it either.
// Mutation: offer every state (`CHECK_STATES`) → red.
test("the window offers a chip for each ending and none for a state that never ends in it", async () => {
  stubCodeEngine({ "/query/code_checks": LISTED });
  mountCode("#/gitlet/checks", <Checks />);
  await screen.findByRole("table", { name: "Attempts of checks" });
  const chips = within(screen.getByRole("group", { name: "State" }));
  for (const ended of ["passed", "failed", "errored", "cancelled", "skipped"]) {
    expect(chips.getByRole("button", { name: new RegExp(ended) })).toBeTruthy();
  }
  for (const live of ["queued", "running"]) {
    expect(chips.queryByRole("button", { name: new RegExp(live) })).toBeNull();
  }
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
// checked on — the comparison of its base and its commit, on the file at its
// line — and the tail; its log is asked only when the reader asks, by its
// object. Mutation: link a failure to the commit alone, drop its line from
// the link, or read the log on open, and this fails.
test("an attempt opens whole, its failures linked into its diff and its log read on demand", async () => {
  const asked = stubCodeEngine({
    "/query/code_checks": LISTED,
    "/query/code_check": WHOLE,
    "/query/code_check_log": () =>
      jsonAnswer({ object: "objects/log-1", size: 2048, offset: 0, text: "=== RUN TestRetry\n" }),
  });
  mountCode("#/gitlet/checks", <Checks />);
  const grid = await screen.findByRole("table", { name: "Attempts of checks" });
  // A ROW OPENS by its own control, which the grid lays over the row.
  const row = within(grid).getByText("unit").closest<HTMLElement>('[role="row"]')!;
  fireEvent.click(within(row).getByRole("button"));
  const pane = await screen.findByLabelText("Check unit, attempt 1");
  const failure = await within(pane).findByRole("link", { name: "app/retry.go:12" });
  const target = new URL(failure.getAttribute("href")!, "http://x");
  expect(target.hash).toContain("/gitlet/repos/web/compare");
  expect(target.hash).toContain(`base=${BASE}`);
  expect(target.hash).toContain(`head=${OID}`);
  expect(target.hash).toContain("file=app%2Fretry.go");
  expect(target.hash).toContain("line=12");
  expect(within(pane).getByText("--- FAIL: TestRetry")).toBeTruthy();
  expect(asked.some((u) => u.pathname === "/query/code_check_log")).toBe(false);
  // THE BRANCH CHECKED IS ITS CHIP, in the row ("for main", a space kept) and
  // in the pane's For. Mutation: drop the space after "for", and this fails.
  const chip = row.querySelector(".code-branch-chip");
  expect(chip?.textContent).toBe("main");
  expect(chip?.parentElement?.textContent).toContain("for main");
  expect(
    within(pane)
      .getAllByText("main")
      .some((el) => el.closest(".code-branch-chip")),
  ).toBe(true);

  fireEvent.click(within(pane).getByRole("button", { name: /Read its log/ }));
  expect(await within(pane).findByText(/=== RUN TestRetry/)).toBeTruthy();
  const log = asked.find((u) => u.pathname === "/query/code_check_log")!;
  expect(log.searchParams.get("object")).toBe("objects/log-1");
  expect(log.searchParams.get("offset")).toBe("0");
  expect(within(pane).getByText("That is the whole log.")).toBeTruthy();
});

// AN ATTEMPT WITH NO BASE has no diff to point into: a failure naming a line
// opens the file at the attempt's own commit on that line, and one naming no
// line the commit's page. Mutation: open the file at no line, at another
// commit, or the commit's page for a failure with a line, and this fails.
test("a failure of an attempt with no base opens the file at its commit on its line", () => {
  const row = attempt("unit", "failed", { base: "" });
  const at = new URL(
    failureHref("web", row, { path: "app/retry.go", line: 12, message: "never stopped" }),
    "http://x",
  );
  expect(at.hash).toContain("/gitlet/repos/web?");
  // THE CODE TAB IS THE REPOSITORY'S LANDING, so the address names no tab.
  expect(at.hash).not.toContain("tab=");
  expect(at.hash).toContain(`ref=${OID}`);
  expect(at.hash).toContain("path=app%2Fretry.go");
  expect(at.hash).toContain("lines=12");
  expect(failureHref("web", row, { path: "app/retry.go", message: "never stopped" })).toBe(
    commitHref("web", OID),
  );
});

// A REPOSITORY'S TAB is its own listing, which takes no window.
// Mutation: send the section's `since` beside `repo=` (a bad_params), and
// this fails.
test("a repository's Checks tab lists its own attempts, with no window", async () => {
  const asked = stubCodeEngine({ "/query/code_checks": LISTED });
  mountCode("#/gitlet/repos/web?tab=checks", <RepoChecks repo="web" />);
  const grid = await screen.findByRole("table", { name: "Checks of web" });
  expect(within(grid).getByText("lint")).toBeTruthy();
  expect(onTheRight(grid, "Ended")).toBe(true);
  const first = asked.find((u) => u.pathname === "/query/code_checks")!;
  expect(first.searchParams.get("repo")).toBe("web");
  expect(first.searchParams.has("since")).toBe(false);
});

// ---- the pane every checks view opens a run in ----------------------------

const API_OID = "c17a9e2".padEnd(40, "0");
const API_HEAD = "9b2e4d7".padEnd(40, "0");
const API_BASE = "5c3e8b1".padEnd(40, "0");
const BOARD_TAIL = [
  "=== RUN   TestKeysetPagesInOrder",
  "    orders_test.go:71: seeded 120 orders",
  "    orders_test.go:88: got 49 rows, want 50",
  "    orders_test.go:91: ord_8f21 seen twice",
  "--- FAIL: TestKeysetPagesInOrder (0.42s)",
  "=== RUN   TestKeysetEmptyPage",
  "--- PASS: TestKeysetEmptyPage (0.03s)",
  "=== RUN   TestKeysetForeignCursor",
  "--- PASS: TestKeysetForeignCursor (0.01s)",
  "FAIL",
  "FAIL    nimbus/api/internal/http      1.874s",
  "ok      nimbus/api/internal/orders    0.912s",
  "ok      nimbus/api/internal/store     2.301s",
  "ok      nimbus/api/internal/billing   0.655s",
  "tearing down the postgres container (12s)",
  "FAIL",
  "make: *** [Makefile:41: e2e] Error 1",
  "exit status 1",
];

/** api's e2e, failed for api!17 in the queue, as the standing listing answers it. */
function apiE2E(extra: Record<string, unknown> = {}) {
  return {
    repo_id: "id-api",
    repo: "api",
    oid: API_OID,
    target: "refs/heads/main",
    name: "e2e",
    attempt: 1,
    state: "failed",
    purpose: "candidate",
    required: true,
    change: {
      repo: "api",
      ref: "api!17",
      title: "api: page the orders listing by keyset",
      state: "open",
    },
    source_ref: "refs/heads/backend/orders-keyset",
    change_head: API_HEAD,
    base: API_BASE,
    duration_ms: 112_000,
    started_at: new Date(Date.now() - 22 * 60_000).toISOString(),
    finished_at: new Date(Date.now() - 20 * 60_000).toISOString(),
    ...extra,
  };
}

/** The same run whole, as `code_check` answers it: with no change, which only a standing row names. */
function apiE2EWhole(extra: Record<string, unknown> = {}) {
  const run: Record<string, unknown> = apiE2E();
  delete run.change;
  delete run.source_ref;
  delete run.change_head;
  return {
    ...run,
    def: { name: "e2e", run: "make e2e", timeout_seconds: 900 },
    tests: { total: 214, failed: 1 },
    exit_code: 1,
    summary: "1 test failed",
    log_lines: 1201,
    tail_first_line: 1184,
    tail: `${BOARD_TAIL.join("\n")}\n`,
    failures: [
      {
        path: "internal/http/orders_test.go",
        line: 88,
        test_id: "TestKeysetPagesInOrder",
        message: "got 49 rows, want 50",
      },
    ],
    log: { object: "objects/log-api", hash: "h", size: 88_064 },
    url: "https://ci.example.com/runs/8812",
    ...extra,
  };
}

/** The words of the pane's fact under `label`. */
function fact(pane: HTMLElement, label: string): string | null | undefined {
  const dt = [...pane.querySelectorAll("dt")].find((d) => d.textContent === label);
  return dt?.nextElementSibling?.textContent;
}

/** Open api's e2e from api's Checks tab, its whole read answered by `whole`. */
async function openApiPane(whole: Record<string, unknown> = apiE2EWhole()) {
  stubCodeEngine({
    "/query/code_checks": () => jsonAnswer({ checks: [apiE2E()], ...page }),
    "/query/code_check": () => jsonAnswer({ check: whole, ...page }),
  });
  mountCode("#/gitlet/repos/api?tab=checks", <RepoChecks repo="api" />);
  const grid = await screen.findByRole("table", { name: "Checks of api" });
  const row = within(grid).getByText("e2e").closest<HTMLElement>('[role="row"]')!;
  fireEvent.click(within(row).getByRole("button"));
  const pane = await screen.findByRole("region", { name: "Check e2e, attempt 1" });
  await within(pane).findByRole("list", { name: "Failures" });
  return pane;
}

// Mutation: draw an absent count as 0, or repeat the summary under the tests line.
test("the pane says how many tests failed of how many ran", async () => {
  const pane = await openApiPane();
  expect(within(pane).getByRole("heading", { name: "e2e failed for api!17" })).toBeTruthy();
  const tests = pane.querySelector(".code-check-pane__tests")!;
  expect(tests.textContent).toBe("1 of 214 tests failed · exit code 1");
  expect(tests.getAttribute("data-tone")).toBe("danger");
  cleanup();
  const passed = await openApiPane(
    apiE2EWhole({
      state: "passed",
      tests: { total: 214 },
      exit_code: 0,
      summary: "214 tests passed · exit code 0",
    }),
  );
  expect(passed.querySelector(".code-check-pane__tests")?.textContent).toBe(
    "214 tests passed · exit code 0",
  );
  expect(within(passed).getAllByText("214 tests passed · exit code 0")).toHaveLength(1);
  cleanup();
  const uncounted = await openApiPane(apiE2EWhole({ tests: undefined }));
  expect(uncounted.querySelector(".code-check-pane__tests")?.textContent).toBe("exit code 1");
  expect(uncounted.textContent).not.toMatch(/\b0 tests\b/);
});

// Mutation: say the candidate's own commit, or drop the train's base.
test("the pane's facts say what was checked", async () => {
  const pane = await openApiPane();
  expect(fact(pane, "Checked")).toBe("merged onto main at 5c3e8b1 in the queue");
  const commit = within(pane).getByRole("link", { name: "9b2e4d7" });
  expect(commit.getAttribute("href")).toBe(commitHref("api", API_HEAD));
  expect(commit.parentElement?.textContent).toBe("9b2e4d7 on backend/orders-keyset");
  expect(
    within(pane)
      .getByRole("link", { name: /api!17/ })
      .getAttribute("href"),
  ).toContain("/gitlet/repos/api/changes/17");
});

// Mutation: number from 1, or tint a line that says no failure.
test("the end of the output is numbered from its first line and its failing lines are marked", async () => {
  const pane = await openApiPane();
  const tail = within(pane).getByRole("region", { name: "The end of its output" });
  const lines = [...tail.querySelectorAll<HTMLElement>(".code-check-tail__line")];
  const numbers = lines.map((l) => Number(l.querySelector(".code-check-tail__n")!.textContent));
  expect(numbers[0]).toBe(1184);
  expect(numbers.at(-1)).toBe(1201);
  expect(within(pane).getByText("lines 1,184 to 1,201 of 1,201")).toBeTruthy();
  expect(
    lines
      .filter((l) => l.hasAttribute("data-fail"))
      .map((l, _, all) => numbers[lines.indexOf(l)] ?? all),
  ).toEqual([1186, 1188, 1193, 1194, 1199]);
  cleanup();
  const cut = await openApiPane(apiE2EWhole({ tail_cut: true }));
  expect(cut.querySelector(".code-check-tail__n")?.textContent).toBe("1184…");
  cleanup();
  const old = await openApiPane(apiE2EWhole({ log_lines: undefined, tail_first_line: undefined }));
  expect(old.querySelector(".code-check-tail")).toBeNull();
  expect(old.querySelector(".code-check-tail__n")).toBeNull();
  expect(old.textContent).toContain("=== RUN   TestKeysetPagesInOrder");
});

// Mutation: draw Open in CI for a run that names no CI.
test("Open in CI is drawn only for a run that names its CI", async () => {
  const pane = await openApiPane();
  const ci = within(pane).getByRole("link", { name: /Open in CI/ });
  expect(ci.getAttribute("href")).toBe("https://ci.example.com/runs/8812");
  expect(ci.getAttribute("target")).toBe("_blank");
  cleanup();
  const local = await openApiPane(apiE2EWhole({ url: undefined }));
  expect(within(local).queryByRole("link", { name: /Open in CI/ })).toBeNull();
});
