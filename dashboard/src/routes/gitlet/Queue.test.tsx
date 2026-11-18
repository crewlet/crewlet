/**
 * Gitlet › Queue draws every repository's merge queues as boxes of trains, as the engine answers
 * them: a box per branch, its train base and its entries in queue order on a spine, the gate's
 * required checks, what each train's checks stand at and how long they have run.
 */

import { act, cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, beforeAll, expect, test, vi } from "vitest";

import { CODE_POSITION, jsonAnswer, mountCode, stubCodeEngine } from "~/test/codeScreens.tsx";
import { afterMicrotasks } from "~/test/settle.ts";
import { Queue } from "./Queue.tsx";

beforeAll(() => {
  // jsdom implements no scrolling, and a step scrolls its row into view.
  Element.prototype.scrollIntoView = () => {};
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  location.hash = "";
});

const oid = (short: string) => short.padEnd(40, "0");
const BASE = oid("c71d0e9");
const T1 = oid("e1e1e1e");
const HEAD = oid("aaaaaaa");
const page = { position: CODE_POSITION, level: "stale" };
const NOW = Date.now();
const ago = (minutes: number) => new Date(NOW - minutes * 60_000).toISOString();
const LISTING = () => jsonAnswer({ repos: [{ name: "web" }, { name: "api" }], ...page });

function entry<M extends object>(repo: string, place: number, number: number, more = {} as M) {
  return {
    repo_id: `id-${repo}`,
    repo,
    entry_id: `${repo}-${place}`,
    target_ref: "refs/heads/main",
    change_id: `c-${repo}-${number}`,
    number,
    place,
    seq: CODE_POSITION,
    head_oid: HEAD,
    method: "merge",
    enqueued_by: { handle: "cto", kind: "agent" },
    state: "waiting",
    enqueued_at: ago(12),
    ...more,
  };
}

const WEB44 = entry("web", 1, 44, {
  title: "server: drain inside the load balancer's deregistration delay",
  state: "testing",
  candidate_id: "cand-1",
  train: "t1",
  enqueued_at: ago(16),
});
const WEB43 = entry("web", 2, 43, {
  title: "checkout: retry a declined card once with the network token",
  enqueued_by: { handle: "jane", kind: "person" },
});
const TRAIN = {
  repo_id: "id-web",
  candidate_id: "cand-1",
  target_ref: "refs/heads/main",
  train: "t1",
  ordinal: 1,
  base_oid: BASE,
  tip_oid: T1,
  entries: [{ entry_id: "web-1", commit: T1 }],
  state: "testing",
  position: CODE_POSITION,
};
const API17 = {
  repo_id: "id-api",
  repo: "api",
  entry_id: "api-gone",
  target_ref: "refs/heads/main",
  change_id: "c-api-17",
  number: 17,
  title: "api: page the orders listing by keyset",
  head_oid: HEAD,
  method: "merge",
  enqueued_by: { handle: "cto", kind: "agent" },
  reason: "check_failed",
  checks: ["e2e"],
  enqueued_at: ago(23),
  ejected_at: ago(20),
  tested_in: { candidate_id: "cand-9", base_oid: oid("5c3e8b1"), commit_oid: oid("c17") },
};

function check(name: string, state: string, more: Record<string, unknown> = {}) {
  return {
    repo_id: "id-web",
    repo: "web",
    oid: T1,
    target: "refs/heads/main",
    name,
    attempt: 1,
    state,
    required: true,
    timeout_seconds: 1800,
    ...more,
  };
}

const T1_CHECKS = [
  check("unit", "passed", { started_at: ago(3) }),
  check("e2e", "running", { started_at: ago(4.5) }),
];

const GATE = {
  pattern: "main",
  gate: {
    merge_by: "writers",
    approvals: "one",
    checks: ["e2e", "unit"],
    landing: { depth: 8, batch: 4 },
    delete_source: "seats",
  },
};

type Answers = Record<string, (url: URL) => Response>;

/** The story's engine: web's train and api's ejection, each answer replaceable. */
function story(over: Answers = {}): URL[] {
  return stubCodeEngine({
    "/query/code_repos": LISTING,
    "/query/code_queue": () =>
      jsonAnswer({
        queue: [WEB43, WEB44],
        trains: [TRAIN],
        ejected: [API17],
        ejected_total: { total: 1 },
        ...page,
      }),
    "/query/code_checks": (url) =>
      jsonAnswer({ checks: url.searchParams.get("oid") === T1 ? T1_CHECKS : [], ...page }),
    "/query/code_repo": (url) =>
      jsonAnswer({ repo: { name: url.searchParams.get("name"), gates: [GATE] }, ...page }),
    "/query/code_refs": () =>
      jsonAnswer({
        refs_detail: [
          {
            ref: "refs/heads/main",
            oid: BASE,
            position: CODE_POSITION,
            moved_at: ago(30),
            protection: { patterns: ["main"], direct_push: "none" },
          },
        ],
        ...page,
      }),
    "/query/code_log": () =>
      jsonAnswer({
        log: {
          commits: [{ oid: BASE, subject: "Merge change web!41: cache the cart badge for 30s" }],
          scanned: 1,
          complete: true,
        },
        ...page,
      }),
    "/query/code_counts": () => jsonAnswer({ counts: { queue: { live: 2 } }, ...page }),
    ...over,
  });
}

const box = (name: string) => screen.getByRole("list", { name: new RegExp(`^${name} into`) });
const headOf = (name: string) =>
  box(name)
    .closest<HTMLElement>(".code-list__group")!
    .querySelector<HTMLElement>(".code-list__band")!;
const asked = (urls: URL[], path: string) => urls.filter((u) => u.pathname === path);
/** An entry's train meter as drawn: each segment's tone (the rest unmarked) and share. */
const segmentsOf = (row: HTMLElement) =>
  [...row.querySelectorAll<HTMLElement>(".crewlet-segmented-meter__segment")].map(
    (s) => `${s.dataset.tone ?? "rest"} ${s.style.flexBasis}`,
  );

// Mutation: ask trains for a named repository only.
test("every scope asks the queues with their trains, on the next page too", async () => {
  const urls = story({
    "/query/code_queue": (url) =>
      url.searchParams.get("cursor") === "p2"
        ? jsonAnswer({ queue: [WEB43], ...page })
        : jsonAnswer({ queue: [WEB44], trains: [TRAIN], next: "p2", ...page }),
  });
  mountCode("#/gitlet/queue", <Queue />);
  fireEvent.click(await screen.findByRole("button", { name: "Load more queued changes" }));
  await screen.findByText(WEB43.title);
  const queues = asked(urls, "/query/code_queue");
  expect(queues.map((u) => u.searchParams.get("trains"))).toEqual(queues.map(() => "true"));
  expect(queues.some((u) => u.searchParams.get("cursor") === "p2")).toBe(true);
  cleanup();
  const named = story();
  mountCode("#/gitlet/queue?repo=web", <Queue />);
  await screen.findByText(WEB44.title);
  const first = asked(named, "/query/code_queue")[0]!;
  expect(first.searchParams.get("repo")).toBe("web");
  expect(first.searchParams.get("trains")).toBe("true");
});

// Mutation: drop "at least" from the page's last box.
test("while more queued changes are to load, the page's last box says at least", async () => {
  story({
    "/query/code_queue": () => jsonAnswer({ queue: [WEB44], trains: [TRAIN], next: "p2", ...page }),
  });
  mountCode("#/gitlet/queue", <Queue />);
  expect(
    await screen.findByRole("list", { name: /^web into main, at least 1 in the queue/ }),
  ).toBeTruthy();
});

// Mutation: draw the rows in the answer's order.
test("a box per queue, its entries in queue order on its spine with their titles", async () => {
  story();
  mountCode("#/gitlet/queue", <Queue />);
  const web = await screen.findByRole("list", { name: /^web into main, 2 in the queue/ });
  const rows = within(web).getAllByRole("listitem");
  expect(rows.map((r) => r.querySelector(".code-list__link")?.textContent)).toEqual([
    WEB44.title,
    WEB43.title,
  ]);
  expect(within(rows[0]!).getByText("Place 1")).toBeTruthy();
  expect(within(rows[0]!).getByText("web!44")).toBeTruthy();
  expect(screen.getByRole("heading", { name: /^api into main, Empty/ })).toBeTruthy();
  expect(screen.getByText("Nothing is queued for main")).toBeTruthy();
});

// Mutation: read the base by the branch.
test("the train base row names the base and its subject", async () => {
  const urls = story();
  mountCode("#/gitlet/queue", <Queue />);
  expect(await screen.findByText("Merge change web!41: cache the cart badge for 30s")).toBeTruthy();
  const base = screen.getByText("Train base").closest("p")!;
  expect(within(base).getByRole("link", { name: "c71d0e9" }).getAttribute("href")).toBe(
    `#/gitlet/repos/web/commits/${BASE}`,
  );
  const logs = asked(urls, "/query/code_log");
  expect(logs).toHaveLength(1);
  expect(logs[0]!.searchParams.get("ref")).toBe(BASE);
  expect(logs[0]!.searchParams.get("limit")).toBe("1");
});

// Mutation: say "next to land" on every row.
test("an entry says who enqueued it, when, and where it stands", async () => {
  story();
  mountCode("#/gitlet/queue", <Queue />);
  await screen.findByText(WEB44.title);
  const [first, second] = within(box("web")).getAllByRole("listitem");
  expect(first!.querySelector(".code-list__meta")?.textContent).toMatch(
    /CTOenqueued it 16 minutes ago· next to land$/,
  );
  expect(second!.querySelector(".code-list__meta")?.textContent).toMatch(
    /Jane Founderenqueued it 12 minutes ago· waits behind web!44$/,
  );
  expect(within(second!).getByRole("link", { name: "web!44" }).getAttribute("href")).toBe(
    "#/gitlet/repos/web/changes/44",
  );
});

// Mutation: total the required rows alone, or drop the candidate's meter.
test("required checks are the gate's of the rules the engine matched, and the meter counts the tip's against them", async () => {
  const urls = story();
  mountCode("#/gitlet/queue", <Queue />);
  await screen.findByText(WEB44.title);
  expect(await within(headOf("web")).findByText("e2e, unit")).toBeTruthy();
  const first = within(box("web")).getAllByRole("listitem")[0]!;
  expect(await within(first).findByText("e2e running · unit passed")).toBeTruthy();
  expect(segmentsOf(first)).toEqual(["success 50%", "info 50%"]);
  const repos = asked(urls, "/query/code_repo");
  expect(repos.map((u) => u.searchParams.get("name")).sort()).toEqual(["api", "web"]);
  expect(repos.every((u) => u.searchParams.get("gates") === "true")).toBe(true);
  const refs = asked(urls, "/query/code_refs").find((u) => u.searchParams.get("name") === "web")!;
  expect(refs.searchParams.get("contains")).toBe("main");
  expect(refs.searchParams.get("detail")).toBe("true");
});

// Mutation: draw a waiting entry's meter from the train ahead.
test("a waiting entry's checks have not started and it says what it starts after", async () => {
  story();
  mountCode("#/gitlet/queue", <Queue />);
  await screen.findByText(WEB43.title);
  const second = within(box("web")).getAllByRole("listitem")[1]!;
  expect(await within(second).findByText("e2e and unit not started")).toBeTruthy();
  expect(second.querySelector(".code-queue__words strong")?.textContent).toBe("web!44");
  expect(second.textContent).toMatch(/starts after web!44/);
  expect(within(second).queryByRole("meter")).toBeNull();
  expect(segmentsOf(second)).toEqual(["rest 100%"]);
});

// Mutation: measure from enqueued_at.
test("the elapsed bar runs from the first required check's start against the slowest timeout", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"], now: NOW });
  story();
  mountCode("#/gitlet/queue", <Queue />);
  await screen.findByText(WEB44.title);
  const first = within(box("web")).getAllByRole("listitem")[0]!;
  const bar = await within(first).findByRole("meter", { name: "Time its checks have run" });
  expect(bar.getAttribute("aria-valuetext")).toBe("4 of 30 minutes");
  expect(within(first).getByText("4 of 30 minutes")).toBeTruthy();
  vi.setSystemTime(NOW + 2 * 60_000);
  await act(async () => void vi.advanceTimersByTime(1_000));
  expect(within(first).getByText("6 of 30 minutes")).toBeTruthy();
});

// Mutation: draw "No required checks".
test("a gate read refused says the required checks are unavailable, and the meter counts the required rows", async () => {
  story({
    "/query/code_repo": () =>
      jsonAnswer({ error: "bad_params", detail: "gates is not a key this node reads" }, 400),
  });
  mountCode("#/gitlet/queue", <Queue />);
  await screen.findByText(WEB44.title);
  const unavailable = await within(headOf("web")).findByText("Required checks are unavailable");
  expect(unavailable.closest("[title]")?.getAttribute("title")).toBe(
    "gates is not a key this node reads",
  );
  expect(within(headOf("web")).queryByText("No required checks")).toBeNull();
  const first = within(box("web")).getAllByRole("listitem")[0]!;
  expect(await within(first).findByText("unit passed · e2e running")).toBeTruthy();
});

// Mutation: one list per box, so j stops at web's last entry.
test("j moves over every box's entries and Enter peeks the change", async () => {
  story({
    "/query/code_queue": () =>
      jsonAnswer({
        queue: [WEB44, entry("api", 1, 18, { title: "api: stream the export" })],
        trains: [TRAIN],
        ...page,
      }),
  });
  mountCode("#/gitlet/queue", <Queue />);
  await screen.findByText("api: stream the export");
  await afterMicrotasks();
  fireEvent.pointerDown(screen.getByRole("group", { name: "Merge queues" }));
  fireEvent.keyDown(window, { key: "j" });
  expect(document.activeElement?.textContent).toBe(WEB44.title);
  fireEvent.keyDown(window, { key: "j" });
  expect(document.activeElement?.textContent).toBe("api: stream the export");
  act(() => (document.activeElement as HTMLElement).blur());
  fireEvent.keyDown(window, { key: "Enter" });
  expect(decodeURIComponent(location.hash)).toMatch(/peek=change:api!18/);
});

// Mutation: fall back to the listing without trains.
test("a fleet that refuses trains says so, with no listing in its place", async () => {
  const urls = story({
    "/query/code_queue": () =>
      jsonAnswer({ error: "bad_params", detail: "trains is not a key this node reads" }, 400),
  });
  mountCode("#/gitlet/queue", <Queue />);
  expect(
    await screen.findByText("Every queue’s trains are unavailable on this fleet yet."),
  ).toBeTruthy();
  expect(screen.getByText(/trains is not a key this node reads/)).toBeTruthy();
  await afterMicrotasks();
  const queues = asked(urls, "/query/code_queue");
  expect(queues.every((u) => u.searchParams.get("trains") === "true")).toBe(true);
  expect(screen.queryByRole("group", { name: "Merge queues" })).toBeNull();
});

// One read for a candidate's entries: its tip, for the box's branch, at each check's latest
// attempt. Mutation: ask the checks per entry.
test("a candidate's tip is read once for every entry it tests", async () => {
  const both = {
    ...TRAIN,
    entries: [
      { entry_id: "web-1", commit: BASE },
      { entry_id: "web-2", commit: T1 },
    ],
  };
  const testing = { state: "testing", candidate_id: "cand-1", train: "t1" };
  const urls = story({
    "/query/code_queue": () =>
      jsonAnswer({ queue: [WEB44, { ...WEB43, ...testing }], trains: [both], ...page }),
  });
  mountCode("#/gitlet/queue?repo=web", <Queue />);
  expect(await screen.findAllByText("e2e running · unit passed")).toHaveLength(2);
  await afterMicrotasks();
  const checks = asked(urls, "/query/code_checks");
  expect(checks.map((u) => u.searchParams.get("oid"))).toEqual([T1]);
  expect(checks[0]!.searchParams.get("target")).toBe("refs/heads/main");
  expect(checks[0]!.searchParams.get("latest")).toBe("true");
});

// Mutation: draw the first page of a tip's checks alone.
test("a tip's checks past one page are all in its words", async () => {
  const urls = story({
    "/query/code_queue": () => jsonAnswer({ queue: [WEB44], trains: [TRAIN], ...page }),
    "/query/code_checks": (url) =>
      url.searchParams.get("cursor") === "unit/1"
        ? jsonAnswer({ checks: [T1_CHECKS[0]], ...page })
        : jsonAnswer({ checks: [T1_CHECKS[1]], next: "unit/1", ...page }),
  });
  mountCode("#/gitlet/queue", <Queue />);
  expect(await screen.findByText("e2e running · unit passed")).toBeTruthy();
  const checks = asked(urls, "/query/code_checks");
  expect(checks.map((u) => u.searchParams.get("cursor"))).toEqual([null, "unit/1"]);
});

test("no queue holding a change is an empty state", async () => {
  story({ "/query/code_queue": () => jsonAnswer(page) });
  mountCode("#/gitlet/queue", <Queue />);
  expect(
    await screen.findByRole("heading", { name: "No merge queue holds a change" }),
  ).toBeTruthy();
});

// Mutation: drop the name check, and the queue is asked.
test("a name no repository can have is said in place and never asked", async () => {
  const urls = story();
  mountCode("#/gitlet/queue?repo=Web_App", <Queue />);
  expect(await screen.findByText(/is not a repository’s name/)).toBeTruthy();
  await afterMicrotasks();
  expect(asked(urls, "/query/code_queue")).toEqual([]);
});
