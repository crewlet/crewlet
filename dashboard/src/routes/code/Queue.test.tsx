/**
 * Code › Queue draws the merge queues of gated branches as the engine
 * answers them.
 *
 * The invariants: a band per repository and branch, its entries in queue
 * order with the change, who enqueued it and where it stands; once a
 * repository is named, each entry's train base and a meter of the required
 * checks of what it is tested in, and what the queue put out lately in the
 * danger tone with the reason and the checks or paths the engine found;
 * nothing queued is an empty state, never an empty band; and a name no
 * repository can have is said in place and never asked.
 */

import { cleanup, fireEvent, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { CODE_POSITION, jsonAnswer, mountCode, stubCodeEngine } from "~/test/codeScreens.tsx";
import { afterMicrotasks } from "~/test/settle.ts";
import { Queue } from "./Queue.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

const A = "a".repeat(40);
const BASE = "b".repeat(40);
const TIP = "c".repeat(40);
const page = { position: CODE_POSITION, level: "stale" };
const LISTING = () => jsonAnswer({ repos: [{ name: "web" }, { name: "api" }], ...page });

function entry(repo: string, place: number, extra: Record<string, unknown> = {}) {
  return {
    repo_id: `id-${repo}`,
    repo,
    entry_id: `${repo}-${place}`,
    target_ref: "refs/heads/main",
    change_id: `c-${repo}-${place}`,
    number: 40 + place,
    place,
    seq: CODE_POSITION,
    head_oid: A,
    method: "merge",
    enqueued_by: { handle: "cto", kind: "agent" },
    state: "waiting",
    enqueued_at: "2026-11-01T10:00:00Z",
    ...extra,
  };
}

// EVERY REPOSITORY'S QUEUES, a band each, in queue order: the change, who
// enqueued it and where it stands. Mutation: draw one band for every
// repository, or the rows in the order the answer listed them, and this
// fails.
test("every repository's queues are a band per branch, in queue order", async () => {
  stubCodeEngine({
    "/query/code_repos": LISTING,
    "/query/code_queue": () =>
      jsonAnswer({
        queue: [entry("web", 2), entry("api", 1), entry("web", 1, { state: "testing" })],
        ...page,
      }),
  });
  mountCode("#/code/queue", <Queue />);
  const web = await screen.findByRole("table", { name: "Queued into web · main" });
  const rows = within(web).getAllByRole("row").slice(1);
  expect(rows.map((r) => within(r).getByRole("link").textContent)).toEqual(["web!41", "web!42"]);
  expect(within(rows[0]!).getByText("testing")).toBeTruthy();
  expect(within(rows[1]!).getByText("CTO")).toBeTruthy();
  expect(screen.getByRole("table", { name: "Queued into api · main" })).toBeTruthy();
});

// A NAMED REPOSITORY'S DETAIL: the train base an entry is tested on, the
// meter of its candidate's required checks, and what the queue put out —
// danger, with why and the checks that failed. The candidate tests a batch of
// two, and its tip's checks are asked ONCE for both. Mutation: drop the
// ejections, meter every check rather than the required ones, or ask the
// checks per entry, and this fails.
test("a named repository's queue shows its train base, its required checks and its ejections", async () => {
  const asked = stubCodeEngine({
    "/query/code_repos": LISTING,
    "/query/code_queue": () =>
      jsonAnswer({
        queue: [
          entry("web", 1, { state: "testing", candidate_id: "cand-1", train: "t1" }),
          entry("web", 2, { state: "testing", candidate_id: "cand-1", train: "t1" }),
        ],
        trains: [
          {
            repo_id: "id-web",
            candidate_id: "cand-1",
            target_ref: "refs/heads/main",
            train: "t1",
            ordinal: 1,
            base_oid: BASE,
            tip_oid: TIP,
            entries: [
              { entry_id: "web-1", commit: BASE },
              { entry_id: "web-2", commit: TIP },
            ],
            state: "testing",
            position: CODE_POSITION,
          },
        ],
        ejected: [
          {
            repo_id: "id-web",
            repo: "web",
            entry_id: "web-gone",
            target_ref: "refs/heads/main",
            change_id: "c-gone",
            number: 39,
            head_oid: A,
            enqueued_by: { handle: "cto", kind: "agent" },
            reason: "check_failed",
            checks: ["unit"],
            enqueued_at: "2026-11-01T09:00:00Z",
            ejected_at: "2026-11-01T09:20:00Z",
          },
        ],
        ...page,
      }),
    "/query/code_checks": () =>
      jsonAnswer({
        checks: [
          {
            repo_id: "id-web",
            repo: "web",
            oid: TIP,
            target: "refs/heads/main",
            name: "unit",
            attempt: 1,
            state: "passed",
            required: true,
            timeout_seconds: 1800,
          },
          {
            repo_id: "id-web",
            repo: "web",
            oid: TIP,
            target: "refs/heads/main",
            name: "e2e",
            attempt: 1,
            state: "running",
            required: true,
            timeout_seconds: 600,
          },
          {
            repo_id: "id-web",
            repo: "web",
            oid: TIP,
            target: "refs/heads/main",
            name: "lint",
            attempt: 1,
            state: "failed",
          },
        ],
        ...page,
      }),
  });
  mountCode("#/code/queue?repo=web", <Queue />);
  const web = await screen.findByRole("table", { name: "Queued into web · main" });
  expect(within(web).getAllByText(BASE.slice(0, 7))).toHaveLength(2);
  // OF TWO: unit and e2e are required, lint is not — on both entries.
  expect(await within(web).findAllByRole("img", { name: / of 2$/ })).toHaveLength(2);
  expect(within(web).getAllByText(/of 30m/)).toHaveLength(2);
  const out = screen.getByLabelText("Put out lately");
  expect(within(out).getByText("ejected")).toBeTruthy();
  expect(within(out).getByText("a required check failed")).toBeTruthy();
  expect(within(out).getByText("unit")).toBeTruthy();
  expect(asked.find((u) => u.pathname === "/query/code_queue")?.searchParams.get("repo")).toBe(
    "web",
  );
  // ONE READ for the candidate both entries are tested in.
  await afterMicrotasks();
  const checks = asked.filter((u) => u.pathname === "/query/code_checks");
  expect(checks.map((u) => u.searchParams.get("oid"))).toEqual([TIP]);
});

// NOTHING QUEUED is an empty state that says how a change joins a queue.
test("no queue holding a change is an empty state", async () => {
  stubCodeEngine({ "/query/code_repos": LISTING, "/query/code_queue": () => jsonAnswer(page) });
  mountCode("#/code/queue", <Queue />);
  expect(
    await screen.findByRole("heading", { name: "No merge queue holds a change" }),
  ).toBeTruthy();
});

// A NAME NO REPOSITORY CAN HAVE is said in place, by the engine's grammar,
// and never sent. Mutation: drop the check, and the queue is asked.
test("a name no repository can have is said in place and never asked", async () => {
  const asked = stubCodeEngine({
    "/query/code_repos": LISTING,
    "/query/code_queue": () => jsonAnswer(page),
  });
  mountCode("#/code/queue?repo=Web_App", <Queue />);
  expect(await screen.findByText(/is not a repository’s name/)).toBeTruthy();
  await afterMicrotasks();
  expect(asked.filter((u) => u.pathname === "/query/code_queue")).toEqual([]);
});

// A QUEUE PAST ONE PAGE reads on: Load more asks the page after with the
// engine's cursor, its entries join their band in queue order, and the
// candidate both pages name is asked for its checks once. Mutation: draw the
// first page alone (no pager), and web!43 is never listed.
test("a queue past one page reads its next with Load more", async () => {
  const train = {
    repo_id: "id-web",
    candidate_id: "cand-1",
    target_ref: "refs/heads/main",
    train: "t1",
    ordinal: 1,
    base_oid: BASE,
    tip_oid: TIP,
    entries: [
      { entry_id: "web-1", commit: BASE },
      { entry_id: "web-3", commit: TIP },
    ],
    state: "testing",
    position: CODE_POSITION,
  };
  const testing = { state: "testing", candidate_id: "cand-1", train: "t1" };
  const asked = stubCodeEngine({
    "/query/code_repos": LISTING,
    "/query/code_queue": (url) =>
      url.searchParams.get("cursor") === "p2"
        ? jsonAnswer({ queue: [entry("web", 3, testing)], trains: [train], ...page })
        : jsonAnswer({
            queue: [entry("web", 1, testing), entry("web", 2)],
            trains: [train],
            next: "p2",
            ...page,
          }),
    "/query/code_checks": () => jsonAnswer({ checks: [], ...page }),
  });
  mountCode("#/code/queue?repo=web", <Queue />);
  fireEvent.click(await screen.findByRole("button", { name: "Load more" }));
  const web = screen.getByRole("table", { name: "Queued into web · main" });
  await within(web).findByText("web!43");
  const rows = within(web).getAllByRole("row").slice(1);
  expect(rows.map((r) => within(r).getAllByRole("link")[0]!.textContent)).toEqual([
    "web!41",
    "web!42",
    "web!43",
  ]);
  const pages = asked.filter((u) => u.pathname === "/query/code_queue");
  expect(pages.some((u) => u.searchParams.get("cursor") === "p2")).toBe(true);
  await afterMicrotasks();
  const checks = asked.filter((u) => u.pathname === "/query/code_checks");
  expect(checks.map((u) => u.searchParams.get("oid"))).toEqual([TIP]);
});
