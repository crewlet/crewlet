/**
 * Gitlet › Checks, and the pane every checks listing opens a run in.
 *
 * The invariants: the section asks where each check stands on the engine, counted and narrowed
 * there; a run opens beside the list and in the address; and the pane reads a run whole, its
 * failures linked into their diff, its output numbered and its log read only on demand.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeAll, expect, test, vi } from "vitest";

import {
  CODE_POSITION,
  jsonAnswer,
  mountCode,
  stubCodeEngine,
  onTheRight,
} from "~/test/codeScreens.tsx";
import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import { withDerived } from "~/test/org.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { Checks } from "./Checks.tsx";
import { failureHref } from "./checks/CheckList.tsx";
import { RepoChecks } from "./checks/RepoChecks.tsx";
import { commitHref } from "./paths.ts";

beforeAll(() => {
  // jsdom implements no scrolling, and opening a run scrolls its heading into view.
  Element.prototype.scrollIntoView = () => {};
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  location.hash = "";
});

const OID = "d".repeat(40);
const BASE = "e".repeat(40);
const page = { position: CODE_POSITION, level: "stale" };
const sha = (short: string) => short.padEnd(40, "0");
const minutesAgo = (m: number) => new Date(Date.now() - m * 60_000).toISOString();

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

// ---- where every check stands -----------------------------------------------------------------

interface Judged {
  purpose: "head" | "candidate" | "tip";
  head: string;
  oid?: string;
  change?: string;
  source?: string;
  attempt?: number;
  required?: boolean;
}

/** One standing row: `minutes` since it ended (or started, or was queued), `ms` its duration. */
function stands(
  repo: string,
  name: string,
  state: string,
  minutes: number,
  ms: number,
  how: Judged,
) {
  const ended = state !== "running" && state !== "queued";
  return {
    repo_id: `id-${repo}`,
    repo,
    oid: sha(how.oid ?? how.head),
    target: "refs/heads/main",
    name,
    attempt: how.attempt ?? 1,
    state,
    purpose: how.purpose,
    required: how.required ?? false,
    ...(how.change
      ? {
          change: { repo, ref: how.change, title: `${how.change}'s title`, state: "open" },
          source_ref: `refs/heads/${how.source ?? "topic"}`,
          change_head: sha(how.head),
        }
      : {}),
    ...(how.purpose === "candidate" ? { base: sha("5c3e8b1") } : {}),
    queued_at: minutesAgo(minutes + 1),
    ...(state === "queued" ? {} : { started_at: minutesAgo(ended ? minutes + 2 : minutes) }),
    ...(ended ? { finished_at: minutesAgo(minutes), duration_ms: ms } : {}),
  };
}

const API17: Judged = {
  purpose: "candidate",
  head: "9b2e4d7",
  oid: "c17a9e2",
  change: "api!17",
  source: "backend/orders-keyset",
  required: true,
};
const WEB43: Judged = { purpose: "head", head: "d2a8f4b", change: "web!43", source: "fix/token" };
const WEB44: Judged = {
  purpose: "candidate",
  head: "a03b5c7",
  oid: "c44b0d1",
  change: "web!44",
  source: "sre/drain",
  required: true,
};
const WEB42: Judged = { purpose: "head", head: "f18e2a0", change: "web!42", source: "ui/rail" };
const TIP: Judged = { purpose: "tip", head: "c71d0e9" };

/** The board's twelve: two failed, two running, eight passed, three of them tips of main. */
function board() {
  return [
    stands("api", "e2e", "failed", 20, 112_000, API17),
    stands("web", "lint", "failed", 180, 21_000, WEB43),
    stands("web", "e2e", "running", 4, 0, WEB44),
    stands("web", "e2e", "running", 2, 0, WEB42),
    stands("web", "unit", "passed", 3, 41_000, WEB44),
    stands("api", "unit", "passed", 21, 64_000, API17),
    stands("web", "unit", "passed", 23, 95_000, { ...WEB42, attempt: 2 }),
    stands("web", "e2e", "passed", 180, 141_000, WEB43),
    stands("web", "unit", "passed", 180, 44_000, WEB43),
    stands("web", "e2e", "passed", 300, 128_000, TIP),
    stands("web", "unit", "passed", 300, 47_000, TIP),
    stands("infra", "unit", "passed", 300, 29_000, { purpose: "tip", head: "3f9a1c2" }),
  ];
}

const API_E2E_KEY = `api:e2e:1:${sha("c17a9e2")}:refs/heads/main`;

/** `code_checks` as the engine answers a standing listing: narrowed, counted but for the state. */
function standing(url: URL, extra: Record<string, unknown> = {}) {
  const p = url.searchParams;
  const counted = board().filter(
    (r) =>
      (!p.get("name") || r.name === p.get("name")) && (!p.get("repo") || r.repo === p.get("repo")),
  );
  const counts: Record<string, number> = {};
  for (const r of counted) counts[r.state] = (counts[r.state] ?? 0) + 1;
  const checks = counted.filter((r) => !p.get("state") || r.state === p.get("state"));
  return jsonAnswer({ checks, standing_counts: { counts }, ...page, ...extra });
}

const listed = (asked: URL[]) =>
  asked.filter((u) => u.pathname === "/query/code_checks" && !u.searchParams.has("cursor"));

async function mountBoard(
  hash = "#/gitlet/checks",
  answers: Record<string, (url: URL) => Response> = {},
) {
  const asked = stubCodeEngine({
    "/query/code_checks": (url) => standing(url),
    "/query/code_check": () => jsonAnswer({ check: apiE2EWhole(), ...page }),
    ...answers,
  });
  mountCode(hash, <Checks />);
  const list = await screen.findByRole("list", { name: "Where checks stand" });
  return { asked, list };
}

const rowOf = (list: HTMLElement, key: string) =>
  list.querySelector<HTMLElement>(`[data-key="${CSS.escape(key)}"]`)!;
const linkOf = (list: HTMLElement, key: string) =>
  rowOf(list, key).querySelector<HTMLAnchorElement>("a[data-list-link]")!;
const tabs = () =>
  within(screen.getByRole("radiogroup", { name: "State" }))
    .getAllByRole("radio")
    .map((r) => r.textContent);

// Mutation: send `until`, ask the window's ended attempts with no `standing`, or round the
// window's edge up with `Math.ceil`. The board's clock is floored to the hour, so from 00:00 to
// 00:59 UTC it sits on a day's start; rounded up, the week's edge stayed at that midnight and
// the window reached back seven days and the minutes past it.
test("the section asks where each check stands since the window's start, failing first, with no end", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  // A day ends with the hour in progress, and a week with the day in progress, at either hour.
  const sinces = {
    "2026-11-18T00:30:00Z": { day: "2026-11-17T01:00:00.000Z", week: "2026-11-12T00:00:00.000Z" },
    "2026-11-18T12:30:00Z": { day: "2026-11-17T13:00:00.000Z", week: "2026-11-12T00:00:00.000Z" },
  };
  for (const [clock, since] of Object.entries(sinces)) {
    vi.setSystemTime(new Date(clock));
    const { asked } = await mountBoard();
    const first = listed(asked)[0]!;
    expect(first.searchParams.get("standing")).toBe("failing");
    expect(first.searchParams.has("until")).toBe(false);
    expect(first.searchParams.get("since"), `a day at ${clock}`).toBe(since.day);
    cleanup();
    const week = await mountBoard("#/gitlet/checks?window=7d");
    expect(listed(week.asked)[0]!.searchParams.get("since"), `a week at ${clock}`).toBe(since.week);
    cleanup();
  }
  const recent = await mountBoard("#/gitlet/checks?sort=recent");
  expect(listed(recent.asked)[0]!.searchParams.get("standing")).toBe("recent");
});

// Mutation: count the rows loaded, or narrow the counts by the state chosen.
test("the state tabs carry the engine's counts and narrow on the engine, their counts unmoved", async () => {
  const { asked } = await mountBoard();
  expect(tabs()).toEqual(["All12", "Failed2", "Running2", "Passed8"]);
  fireEvent.click(
    within(screen.getByRole("radiogroup", { name: "State" })).getByRole("radio", {
      name: /Failed/,
    }),
  );
  await screen.findByText(/failed of the 12 latest runs/);
  expect(listed(asked).at(-1)!.searchParams.get("state")).toBe("failed");
  expect(tabs()).toEqual(["All12", "Failed2", "Running2", "Passed8"]);
  cleanup();
  await mountBoard("#/gitlet/checks", {
    "/query/code_checks": (url) =>
      jsonAnswer({
        checks: board(),
        standing_counts: { counts: { passed: 128, failed: 2, errored: 1 }, at_least: ["passed"] },
        ...page,
      }),
  });
  expect(tabs()).toEqual(["All131+", "Failed2", "Running0", "Passed128+", "Errored1"]);
});

// Mutation: draw the candidate as a head, or drop the tip's branch.
test("each row says what it judges", async () => {
  const { list } = await mountBoard();
  const meta = (key: string) => rowOf(list, key).querySelector(".code-list__meta")!.textContent;
  expect(meta(API_E2E_KEY)).toBe("apibackend/orders-keyset9b2e4d7for api!17 in the queue");
  expect(meta(`web:lint:1:${sha("d2a8f4b")}:refs/heads/main`)).toBe(
    "webfix/tokend2a8f4bfor web!43",
  );
  expect(meta(`web:e2e:1:${sha("c71d0e9")}:refs/heads/main`)).toBe("webmainc71d0e9main's tip");
  expect(within(rowOf(list, API_E2E_KEY)).getByText("Required")).toBeTruthy();
  expect(
    within(rowOf(list, `web:unit:2:${sha("f18e2a0")}:refs/heads/main`)).getByText("attempt 2"),
  ).toBeTruthy();
});

// Mutation: say "0s" for an unended run, or read the clock in the screen.
test("a row's time is said after its state word and its duration in short", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
  const { list } = await mountBoard();
  await afterMicrotasks();
  // ONE TICK FIRST, so the shared clock reads the faked one.
  await act(async () => void vi.advanceTimersByTime(1_000));
  const facts = (key: string) => rowOf(list, key).querySelector(".code-list__facts")!.textContent;
  expect(facts(API_E2E_KEY)).toBe("1m 52sended 20 minutes ago");
  const running = `web:e2e:1:${sha("c44b0d1")}:refs/heads/main`;
  const seconds = () => {
    const m = /^(\d+)m (\d+)sstarted 4 minutes ago$/.exec(facts(running) ?? "");
    expect(m, facts(running) ?? "").not.toBeNull();
    return Number(m![1]) * 60 + Number(m![2]);
  };
  const before = seconds();
  await act(async () => void vi.advanceTimersByTime(2_000));
  expect(seconds()).toBe(before + 2);
  const unended = rowOf(list, `web:e2e:1:${sha("f18e2a0")}:refs/heads/main`);
  expect(unended.querySelector(".code-list__facts")!.textContent).not.toMatch(/\b0s\b/);
});

// Mutation: put the window's start back in the question's key, so the hour turning asks a new
// question whose placeholder takes the rows off screen and stops the running row's clock.
test("the hour turning keeps the rows on screen and ticking, and the next poll reads the moved window", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
  vi.setSystemTime(new Date("2026-11-18T13:59:57.500Z"));
  // ONE BOARD, its instants fixed as the engine's are, so a read again moves no clock.
  const rows = board();
  const { asked, list } = await mountBoard("#/gitlet/checks", {
    "/query/code_checks": () => jsonAnswer({ checks: rows, ...page }),
  });
  await afterMicrotasks();
  // ONE TICK FIRST, so the shared clock reads the faked one.
  await act(async () => void vi.advanceTimersByTime(1_000));
  const running = rowOf(list, `web:e2e:1:${sha("c44b0d1")}:refs/heads/main`);
  const seconds = () => {
    const took = running.querySelector(".code-checks-row__took")!.textContent ?? "";
    const m = /^(\d+)m (\d+)s$/.exec(took);
    expect(m, took).not.toBeNull();
    return Number(m![1]) * 60 + Number(m![2]);
  };
  const before = seconds();
  // From 13:59:58.5, over 14:00, past the first poll 30 s after mounting.
  for (let s = 1; s <= 32; s++) {
    await act(async () => void vi.advanceTimersByTime(1_000));
    expect(list.isConnected).toBe(true);
    expect(running.isConnected).toBe(true);
    expect(list.querySelectorAll("[data-key]")).toHaveLength(rows.length);
    expect(seconds()).toBe(before + s);
  }
  const windows = listed(asked)
    .filter((u) => u.searchParams.has("standing"))
    .map((u) => u.searchParams.get("since"));
  expect(windows).toEqual(["2026-11-17T14:00:00.000Z", "2026-11-17T15:00:00.000Z"]);
});

// Mutation: filter the rows loaded, or offer only the names of the narrowed page.
test("Every repository and Check narrow on the engine", async () => {
  const { asked } = await mountBoard("#/gitlet/checks?repo=api&name=e2e");
  const first = listed(asked)[0]!;
  expect(first.searchParams.get("repo")).toBe("api");
  expect(first.searchParams.get("name")).toBe("e2e");
  expect(
    screen.getByText(/latest run of e2e on live changes and protected branches in api,/),
  ).toBeTruthy();
  fireEvent.click(document.querySelector<HTMLElement>('[aria-label="Check"]')!);
  const options = screen.getAllByRole("option").map((o) => o.textContent);
  expect(options).toEqual(["Every check", "e2e"]);
});

test("the head says what the list is, and the foot where earlier runs are", async () => {
  const { list } = await mountBoard();
  const card = list.closest(".code-list")!;
  expect(card.querySelector(".code-checks-head__words")?.textContent).toBe(
    "12 latest runs on live changes and protected branches, last 24 hours",
  );
  expect(card.querySelector(".code-list__note")?.textContent).toBe(
    "The latest run of each check. Earlier runs are on each change's and repository's Checks tab, kept 30 days.",
  );
  cleanup();
  const failed = await mountBoard("#/gitlet/checks?state=failed&repo=web");
  expect(
    failed.list.closest(".code-list")!.querySelector(".code-checks-head__words")?.textContent,
  ).toBe(
    "1 failed of the 9 latest runs on live changes and protected branches in web, last 24 hours",
  );
  cleanup();
  const e2e = await mountBoard("#/gitlet/checks?name=e2e");
  expect(
    e2e.list.closest(".code-list")!.querySelector(".code-checks-head__words")?.textContent,
  ).toBe("5 latest runs of e2e on live changes and protected branches, last 24 hours");
});

// Mutation: push a history entry per press, or leave focus on the list.
test("pressing a run opens it beside the list and writes it to the address; Escape closes it", async () => {
  const { list } = await mountBoard();
  const before = history.length;
  fireEvent.click(linkOf(list, API_E2E_KEY));
  const heading = await screen.findByRole("heading", { name: "e2e failed for api!17" });
  expect(new URLSearchParams(location.hash.split("?")[1]).get("run")).toBe(API_E2E_KEY);
  expect(history.length).toBe(before);
  expect(document.activeElement).toBe(heading);
  expect(rowOf(list, API_E2E_KEY).hasAttribute("data-current")).toBe(true);
  fireEvent.keyDown(heading, { key: "Escape" });
  expect(location.hash).not.toContain("run=");
  expect(screen.queryByRole("heading", { name: "e2e failed for api!17" })).toBeNull();
  expect(document.activeElement).toBe(linkOf(list, API_E2E_KEY));
  fireEvent.click(linkOf(list, API_E2E_KEY));
  await screen.findByRole("heading", { name: "e2e failed for api!17" });
  fireEvent.click(linkOf(list, API_E2E_KEY));
  expect(location.hash).not.toContain("run=");
  expect(history.length).toBe(before);
});

// Mutation: follow the link on Enter.
test("Enter on the row under j opens the same pane", async () => {
  const { list } = await mountBoard();
  await afterMicrotasks();
  fireEvent.keyDown(window, { key: "j" });
  expect(document.activeElement).toBe(linkOf(list, API_E2E_KEY));
  // ENTER ON THE LINK is the browser's; away from it, the cursor row's.
  act(() => linkOf(list, API_E2E_KEY).blur());
  fireEvent.keyDown(window, { key: "Enter" });
  expect(await screen.findByRole("heading", { name: "e2e failed for api!17" })).toBeTruthy();
  expect(location.hash).toContain(`run=${encodeURIComponent(API_E2E_KEY)}`);
});

// Mutation: draw a pane only for a row the list holds, or drop the change its commit's row judges.
test("a run named by the address that no loaded row holds is read alone and drawn", async () => {
  const key = `api:e2e:3:${sha("c17a9e2")}:refs/heads/main`;
  const { asked } = await mountBoard(`#/gitlet/checks?run=${encodeURIComponent(key)}`, {
    "/query/code_check": () => jsonAnswer({ check: apiE2EWhole({ attempt: 3 }), ...page }),
  });
  expect(await screen.findByRole("heading", { name: "e2e failed for api!17" })).toBeTruthy();
  const read = asked.find((u) => u.pathname === "/query/code_check")!;
  expect(read.searchParams.get("attempt")).toBe("3");
  expect(read.searchParams.get("target")).toBe("refs/heads/main");
});

// Mutation: fall back to the ended window when `standing` is refused, or keep the head's sentence.
test("a fleet that refuses standing says so, with no listing in its place", async () => {
  const refused = () =>
    jsonAnswer({ error: "bad_params", detail: "standing is not a key this node reads" }, 400);
  const asked = stubCodeEngine({ "/query/code_checks": refused });
  mountCode("#/gitlet/checks", <Checks />);
  expect(
    await screen.findByText("Where checks stand is unavailable on this fleet yet"),
  ).toBeTruthy();
  expect(screen.getByText("The engine says: standing is not a key this node reads.")).toBeTruthy();
  expect(document.querySelector(".code-checks-head__words")?.textContent).toBe("");
  await afterMicrotasks();
  const checks = asked.filter((u) => u.pathname === "/query/code_checks");
  expect(checks.length).toBeGreaterThan(0);
  expect(checks.every((u) => u.searchParams.get("standing") === "failing")).toBe(true);
  expect(screen.getByRole("radiogroup", { name: "State" })).toBeTruthy();
  cleanup();
  // A FILTER SENT may be what was refused: the engine's refusal as it said it.
  stubCodeEngine({ "/query/code_checks": refused });
  mountCode("#/gitlet/checks?state=failed", <Checks />);
  expect(
    await screen.findByText(/The engine refused this request: standing is not a key/),
  ).toBeTruthy();
  expect(screen.queryByText("Where checks stand is unavailable on this fleet yet")).toBeNull();
});

// Mutation: read the open run once, or keep reading it once it has ended.
test("an open run that has not ended is read again until it ends", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  const key = `web:e2e:1:${sha("c44b0d1")}:refs/heads/main`;
  let reads = 0;
  const { asked } = await mountBoard(`#/gitlet/checks?run=${encodeURIComponent(key)}`, {
    "/query/code_check": () => {
      reads++;
      const run: Record<string, unknown> = { ...board()[2] };
      delete run.change;
      delete run.source_ref;
      delete run.change_head;
      return jsonAnswer({
        check:
          reads === 1
            ? run
            : {
                ...run,
                state: "failed",
                finished_at: minutesAgo(0),
                duration_ms: 240_000,
                tests: { total: 30, failed: 2 },
                exit_code: 1,
              },
        ...page,
      });
    },
  });
  const pane = await screen.findByRole("region", { name: "Check e2e, attempt 1" });
  await within(pane).findByRole("heading", { name: "e2e running for web!44" });
  expect(pane.querySelector(".code-check-pane__tests")).toBeNull();
  await act(async () => void vi.advanceTimersByTime(30_000));
  await afterMicrotasks();
  expect(within(pane).getByRole("heading", { name: "e2e failed for web!44" })).toBeTruthy();
  expect(pane.querySelector(".code-check-pane__tests")?.textContent).toBe(
    "2 of 30 tests failed · exit code 1",
  );
  const ended = asked.filter((u) => u.pathname === "/query/code_check").length;
  await act(async () => void vi.advanceTimersByTime(60_000));
  await afterMicrotasks();
  expect(asked.filter((u) => u.pathname === "/query/code_check").length).toBe(ended);
});

// A RUN, WHOLE: each failure a link into the diff it was checked on (its base and its commit, on
// the file at its line), and its log asked only when the reader asks, by its object.
// Mutation: link a failure to the commit alone, drop its line, or read the log on open.
test("a run opens whole, its failures linked into its diff and its log read on demand", async () => {
  const { asked, list } = await mountBoard("#/gitlet/checks", {
    "/query/code_check_log": () =>
      jsonAnswer({
        object: "objects/log-api",
        size: 88_064,
        offset: 0,
        text: "=== RUN TestFromTheLog\n",
      }),
  });
  fireEvent.click(linkOf(list, API_E2E_KEY));
  const pane = await screen.findByRole("region", { name: "Check e2e, attempt 1" });
  const failure = await within(pane).findByRole("link", {
    name: "internal/http/orders_test.go:88",
  });
  const target = new URL(failure.getAttribute("href")!, "http://x");
  expect(target.hash).toContain("/gitlet/repos/api/compare");
  expect(target.hash).toContain(`base=${sha("5c3e8b1")}`);
  expect(target.hash).toContain(`head=${sha("c17a9e2")}`);
  expect(target.hash).toContain("file=internal%2Fhttp%2Forders_test.go");
  expect(target.hash).toContain("line=88");
  expect(asked.some((u) => u.pathname === "/query/code_check_log")).toBe(false);
  fireEvent.click(within(pane).getByRole("button", { name: "Read its log, 86 KiB" }));
  expect(await within(pane).findByText(/=== RUN TestFromTheLog/)).toBeTruthy();
  const log = asked.find((u) => u.pathname === "/query/code_check_log")!;
  expect(log.searchParams.get("object")).toBe("objects/log-api");
  expect(log.searchParams.get("offset")).toBe("0");
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

// ---- Re-run one run --------------------------------------------------------------------------

/** A reader who may run checks again. */
const RERUNNER = { ...ADMIN_VIEWER, acts: ["code_rerun_checks"] };

interface Acted {
  tool: string;
  args: Record<string, unknown>;
}

/** The section at `hash` as `viewer`, its acts answered by `answer` and kept. */
function mountActing(
  hash: string,
  viewer: unknown,
  answer: () => Response = () =>
    jsonAnswer({
      outcome: "applied",
      receipt: {
        repo: "api",
        sha: sha("c17a9e2"),
        reruns: [{ name: "e2e", attempt: 2, outcome: "applied", state: "queued" }],
      },
    }),
) {
  const acted: Acted[] = [];
  const asked: URL[] = [];
  let runningReads = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      asked.push(url);
      if (url.pathname === "/auth/session") return new Promise<Response>(() => {});
      if (url.pathname.startsWith("/operator/act/")) {
        acted.push({
          tool: decodeURIComponent(url.pathname.split("/operator/act/")[1] ?? ""),
          args: (JSON.parse(String(init?.body)) as { args: Record<string, unknown> }).args,
        });
        return answer();
      }
      if (url.pathname === "/query/code_checks") return standing(url);
      if (url.pathname === "/query/code_check") {
        if (url.searchParams.get("oid") !== sha("c44b0d1")) {
          return jsonAnswer({ check: apiE2EWhole(), ...page });
        }
        // THE RUNNING RUN, read whole as `code_check` answers it (no change), ends on its second read.
        const run: Record<string, unknown> = { ...board()[2] };
        delete run.change;
        delete run.source_ref;
        delete run.change_head;
        const ended = ++runningReads > 1;
        return jsonAnswer({
          check: {
            ...run,
            def: { name: "e2e", run: "make e2e", timeout_seconds: 900 },
            ...(ended ? { state: "failed", finished_at: minutesAgo(0), duration_ms: 240_000 } : {}),
          },
          ...page,
        });
      }
      return jsonAnswer({}, 404);
    }),
  );
  location.hash = hash;
  const store = new Store();
  store.applyOrg(withDerived({ name: "Acme", roles: [], units: [] }) as never);
  store.setConnected(true);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" ? Promise.resolve(viewer) : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <Checks />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
  return { acted, asked };
}

const AT_API_E2E = `#/gitlet/checks?run=${encodeURIComponent(API_E2E_KEY)}`;

// Mutation: send `all_failed`, or name the run by its change rather than its commit and branch.
test("Re-run runs that one attempt again by its commit and branch", async () => {
  const { acted, asked } = mountActing(AT_API_E2E, RERUNNER);
  const pane = await screen.findByRole("region", { name: "Check e2e, attempt 1" });
  const rerun = await within(pane).findByRole("button", { name: "Re-run" });
  expect(rerun.getAttribute("title")).toBe("Run e2e again as attempt 2");
  const before = listed(asked).length;
  fireEvent.click(rerun);
  expect(await within(pane).findByText(/A later run, attempt 2, is its latest/)).toBeTruthy();
  expect(acted).toEqual([
    {
      tool: "code_rerun_checks",
      args: {
        repo: "api",
        sha: sha("c17a9e2"),
        checked_for: "refs/heads/main",
        checks: [{ name: "e2e", attempt: 1 }],
      },
    },
  ]);
  expect(within(pane).getByRole("button", { name: "Re-run" }).getAttribute("title")).toBe(
    "Attempt 2 is its latest: run that one again.",
  );
  await afterMicrotasks();
  expect(listed(asked).length).toBeGreaterThan(before);
});

// A REFUSED RERUN OF ONE CHECK is a refused act, its sentence drawn under Re-run.
// Mutation: let an unended run be pressed, keep it held once it ends, or draw a refusal as a queued attempt.
test("Re-run is held while a run has not ended, and a refused rerun says why", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  const running = `web:e2e:1:${sha("c44b0d1")}:refs/heads/main`;
  mountActing(`#/gitlet/checks?run=${encodeURIComponent(running)}`, RERUNNER);
  const pane = await screen.findByRole("region", { name: "Check e2e, attempt 1" });
  const held = await within(pane).findByRole("button", { name: "Re-run" });
  expect(held.getAttribute("title")).toBe("It has not ended: an ended run is run again.");
  await act(async () => void vi.advanceTimersByTime(30_000));
  await afterMicrotasks();
  expect(within(pane).getByRole("button", { name: "Re-run" }).getAttribute("title")).toBe(
    "Run e2e again as attempt 2",
  );
  cleanup();
  vi.useRealTimers();
  const said =
    "code_rerun_checks ran nothing again: e2e has run its five attempts on this commit. (attempts_exhausted).";
  const { acted } = mountActing(AT_API_E2E, RERUNNER, () =>
    jsonAnswer({ error: "forbidden", tool: "code_rerun_checks", detail: said }, 403),
  );
  const again = await screen.findByRole("region", { name: "Check e2e, attempt 1" });
  fireEvent.click(await within(again).findByRole("button", { name: "Re-run" }));
  const refusal = await within(again).findByText(/e2e has run its five attempts on this commit/);
  expect(refusal.closest(".crewlet-refusal-strip")).not.toBeNull();
  expect(acted).toHaveLength(1);
  expect(within(again).queryByText(/A later run/)).toBeNull();
});

test.each([
  [
    "an anonymous reader",
    { login: "", grants: [], handle: "", owner: "", name: "", acts: [], anonymous: true },
    WRITE_REASONS.anonymous,
  ],
  ["a person the engine does not serve", { ...RERUNNER, acts: [] }, WRITE_REASONS.not_served],
])("Re-run is drawn for %s, held with the reason", async (_who, viewer, reason) => {
  const { acted } = mountActing(AT_API_E2E, viewer);
  const pane = await screen.findByRole("region", { name: "Check e2e, attempt 1" });
  const rerun = await within(pane).findByRole("button", { name: "Re-run" });
  await afterMicrotasks();
  expect(rerun.getAttribute("title")).toBe(reason);
  fireEvent.click(rerun);
  expect(acted).toEqual([]);
});
