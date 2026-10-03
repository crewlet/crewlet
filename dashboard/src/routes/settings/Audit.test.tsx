/**
 * The audit reads six subsystems as one feed, and says what it cannot see.
 *
 * Three claims, and each is the whole reason the screen exists:
 *
 *  1. IT ASKS FOR WHO WAS WRITING. Both history feeds are asked with
 *     `actor_kinds`, not with a set of handles — a person bound to a seat
 *     writes AS the seat, kind `human`, and a credential bound to none writes
 *     under its whole login, kind `operator`, so the name spaces are disjoint,
 *     and the set of people is the roster, which changes. Asked by handle, this
 *     screen would quietly lose every write made by somebody who has since
 *     left, which is the one write an audit is usually opened to find.
 *  2. SIX SOURCES, ONE ORDER. A tracker commit, a page change, a config
 *     revision, a credential write, a runtime call and an identity change are
 *     six honest records in six places; read separately they cannot answer
 *     "what did we change on Tuesday".
 *  3. A PAGE THAT DID NOT REACH THE WINDOW SAYS SO. Three sources are narrowed
 *     here, and every one of the six answers a single page, so a busy
 *     company's window can extend past the oldest row that arrived — and a
 *     screen silent about that is claiming those rows do not exist.
 */

import { Profiler } from "react";
import { act, answered, cleanup, render, screen } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Audit, auditCsv, writerOf } from "./Audit.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import type { QueryName } from "~/protocol/index.ts";
import {
  CLAIMANT,
  CLAIMANT_HREF,
  DUPLICATE,
  DUPLICATE_HREF,
  SHARED_KEY,
} from "~/test/keyCollision.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn(), useOrg: vi.fn() };
});

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });

/**
 * The engine's two REST reads this screen makes — the credential listing and
 * the identity trail — each answered as told, every read recorded by its
 * path.
 */
let restAnswers: {
  secrets: (call: number) => Response | Promise<Response>;
  identity: (call: number) => Response | Promise<Response>;
};
let restReads: { path: string; query: URLSearchParams; at: number }[] = [];

function stubRest() {
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), "http://engine.test");
      restReads.push({ path: url.pathname, query: url.searchParams, at: Date.now() });
      const calls = restReads.filter((r) => r.path === url.pathname).length;
      if (url.pathname === "/secrets") return restAnswers.secrets(calls);
      if (url.pathname === "/iam/audit") return restAnswers.identity(calls);
      return json({});
    }),
  });
}

/** The reads made of one REST path, in order. */
const readsOf = (path: string) => restReads.filter((r) => r.path === path);

// THE CLOCK RUNS FOR REAL. This suite used to fake the interval it ticks on,
// because every tick re-asked the tracker and drew every row again, so a case
// did more work the slower its runner was. A tick reaches only the date cells
// whose words change now — which is what the case below about ticks pins — and
// the cases about time passing move every timer themselves.
beforeEach(() => {
  location.hash = "#/settings/audit";
  restReads = [];
  restAnswers = {
    secrets: () => json({ secrets: [] }),
    identity: () => json({ events: [], next: 0, position: "CREWLET_IAM_LOG@1:1" }),
  };
  stubRest();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
  location.hash = "";
});

/** An instant inside every window this screen offers. */
const RECENTLY = new Date(Date.now() - 60_000).toISOString();

function commit(over: Record<string, unknown> = {}) {
  return {
    id: "h-1",
    log_seq: 1,
    log_stream: "CREWLET_WORK_LOG",
    log_generation: 1,
    at: RECENTLY,
    effective_at: RECENTLY,
    kind: "removed",
    actor: "maya.lee",
    actor_kind: "operator",
    subject_kind: "task",
    subject_id: "t-1",
    subject_key: "ENG-9",
    excerpt: "took it off the board",
    notified: false,
    ...over,
  };
}

function serving(answers: Partial<Record<QueryName, unknown>> = {}) {
  const query = vi.fn(async (what: string) => answers[what as QueryName] ?? {});
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue({
    name: "Acme",
    roles: [
      { name: "Ada Okonkwo", handle: "ada", kind: "agent" },
      { name: "Jane Founder", handle: "jane", kind: "human" },
    ],
  } as never);
  return query;
}

/**
 * The screen. A case lets every answer the stubbed socket and fetch give land,
 * and renders what they change, with [answered].
 *
 * NOT A POLL. A `waitFor` gave the screen a second of real time to show a
 * row, re-reading the whole document each time it looked — a hundred rows of
 * it, in the case whose page is full — and on a loaded machine the second ran
 * out while the grid was still rendering. The answers are promises, so `act`
 * runs them, and the renders they cause, to the end, however long that takes.
 * And the flush is refused once the case that asks has ended, so a case still
 * running after its time ran out cannot open an `act` beside the next one.
 */
function mount() {
  return render(
    <Router>
      <Audit />
    </Router>,
  );
}

/** The parameters one question was actually asked with. */
function asked(query: ReturnType<typeof serving>, what: QueryName): Record<string, unknown> {
  const calls = query.mock.calls as unknown as [string, Record<string, unknown>?][];
  return calls.findLast(([name]) => name === what)?.[1] ?? {};
}

/** The notice above the grid that says how the rows fall short, as one text. */
function shortfall(): string {
  return document.querySelector(".crewlet-callout")?.textContent ?? "";
}

/** The claim the card's header makes when no source fell short. */
const CLAIM = /Every write a person or a credential made/;

// IT ASKS BY KIND, ON BOTH FEEDS.
test("both history feeds are asked for who was writing, not for a list of people", async () => {
  const query = serving({ work_activity: { records: [], complete: true } });
  mount();
  await answered();
  expect(asked(query, "work_activity").actor_kinds).toBeTruthy();

  for (const what of ["work_activity", "page_activity"] as const) {
    const kinds = String(asked(query, what).actor_kinds ?? "");
    expect(kinds, what).toContain("operator");
    // A PERSON AT THE DASHBOARD AND A TOKEN ARE DIFFERENT KINDS, and both are
    // an operator's write: the tracker refuses to record a token under a seat
    // handle precisely so the distinction survives, and an audit asking for
    // one of them is an audit missing half of what it is for.
    expect(kinds, what).toContain("human");
    expect(asked(query, what).actor, what).toBeUndefined();
  }
  // AND THE ONE FEED THAT TAKES A CLOCK IS GIVEN ONE. `from`/`to` bound the
  // AUTHORED instants, which is what a person typing "last week" means.
  expect(asked(query, "work_activity").from).toBeTruthy();
  expect(asked(query, "work_activity").to).toBeTruthy();
});

// THE TRACKER IS ASKED ON THE POLL, NOT ON THE CLOCK.
//
// The window's edges were read off the one-second clock at render and written
// into the question, so every tick was a new question: the feed was asked one,
// two, three, four times over three ticks where the poll says once a minute,
// and a minute moved in one `act` re-keyed it sixty times in a row — which
// React reports as "Maximum update depth exceeded". Keyed on the window, with
// its edges computed by the ask, a tick asks nothing and the poll asks over
// the window ending when it asks.
test("the tracker is asked once a minute however often the clock ticks", async () => {
  vi.useFakeTimers();
  const errors = vi.spyOn(console, "error");
  const query = serving({ work_activity: { records: [], complete: true } });
  const asks = () =>
    (query.mock.calls as unknown as [string, Record<string, unknown>][])
      .filter(([what]) => what === "work_activity")
      .map(([, params]) => params);
  mount();
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  expect(asks()).toHaveLength(1);

  // FIFTY-NINE TICKS of the clock, one at a time, as a tab open on the screen
  // sees them.
  for (let tick = 1; tick < 60; tick++) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
  }
  expect(asks()).toHaveLength(1);

  // AND THE POLL, which asks over the window as of ITS instant: a minute on
  // from the first ask, still seven days wide.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(1_000);
  });
  expect(asks()).toHaveLength(2);
  const [first, second] = asks();
  const at = (params: Record<string, unknown> | undefined, edge: "from" | "to") =>
    Date.parse(String(params?.[edge]));
  expect(at(second, "to") - at(first, "to")).toBe(60_000);
  expect(at(second, "to") - at(second, "from")).toBe(7 * 24 * 60 * 60_000);

  // A MINUTE MOVED IN ONE GO is one more ask, not sixty.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(60_000);
  });
  expect(asks()).toHaveLength(3);
  expect(
    errors.mock.calls.filter(([message]) => /Maximum update depth/.test(String(message))),
  ).toEqual([]);
});

// AND A TICK DRAWS NOTHING. The window was the clock and the columns closed
// over it, so every second re-asked the feed and drew every row again — a
// probe counted 131 to 248 ms of render a tick for a hundred rows under the
// development build. A row aged a minute reads "1m ago" for the next minute,
// so ten ticks inside it are ten ticks in which nothing on this screen moves.
test("a tick of the clock draws nothing on the audit", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
  // THE ROWS' AGE IS SET, not inherited from when this case happens to run.
  // They were written at RECENTLY, fixed when the file loaded; on the real
  // clock their age was a minute plus however long the cases before this one
  // took, and fifty seconds of that on a loaded runner put the ten ticks
  // across "2m ago" — a render this case would then count against the screen.
  // Sixty-five seconds old, they read "1m ago" for the next fifty-five.
  vi.setSystemTime(Date.parse(RECENTLY) + 65_000);
  const records = Array.from({ length: 20 }, (_, i) =>
    commit({ id: `h-${i}`, subject_key: `ENG-${i}`, excerpt: `edit ${i}` }),
  );
  serving({ work_activity: { records, complete: true } });
  let commits = 0;
  render(
    <Profiler id="audit" onRender={() => (commits += 1)}>
      <Router>
        <Audit />
      </Router>
    </Profiler>,
  );
  await answered();
  expect(screen.getByText("edit 19")).toBeTruthy();
  const settled = commits;

  for (let tick = 0; tick < 10; tick++) {
    act(() => {
      vi.advanceTimersByTime(1_000);
    });
  }
  expect(commits).toBe(settled);
});

// FOUR SOURCES, ONE FEED, NEWEST FIRST.
test("a tracker commit, a page change and a config revision land in one list", async () => {
  serving({
    work_activity: { records: [commit()], complete: true },
    page_activity: {
      changes: [
        {
          id: "p-1",
          page_id: "pg-1",
          kind: "saved",
          actor: "ada",
          actor_kind: "human",
          operator_id: "session:0192f00e",
          at: RECENTLY,
          log_seq: 2,
          title: "Deploy runbook",
          container: "ENG",
          excerpt: "rewrote the rollback step",
        },
      ],
      complete: true,
    },
    config_audit: [
      {
        revision_id: "rev-abcdef12",
        summary: "turn on the Slack integration",
        source: "dashboard",
        created_by: "founder",
        created_by_kind: "human",
        operator_id: "pat:0192f00d",
        created_at: RECENTLY,
      },
    ],
  });
  mount();

  await answered();

  expect(screen.getByText("took it off the board")).toBeTruthy();
  expect(screen.getByText("rewrote the rollback step")).toBeTruthy();
  expect(screen.getByText("turn on the Slack integration")).toBeTruthy();
  // AND EVERY ROW SAYS WHICH KIND OF WRITER MADE IT, which is the fact that
  // separates "a person did this" from "a token did" from "the engine did".
  expect(screen.getAllByText("operator").length).toBeGreaterThan(0);
  expect(screen.getAllByText("human").length).toBeGreaterThan(0);
  // AND THE CREDENTIAL BESIDE THE WRITER, on every source that records one:
  // a revision a person's token wrote is theirs, and saying which token is
  // what tells it from one they wrote by hand.
  expect(screen.getByText("pat:0192f00d")).toBeTruthy();
  expect(screen.getByText("session:0192f00e")).toBeTruthy();
});

// A PURGED TASK IS NOT A LINK.
//
// Its rows are destroyed and this entry is the only evidence it existed, so an
// anchor here is a NotFound on the one row a reader most wants to follow.
test("a purge names its key and does not link to a page that is gone", async () => {
  serving({
    work_activity: {
      records: [commit({ kind: "purged", excerpt: "duplicate of ENG-4" })],
      complete: true,
    },
  });
  mount();
  await answered();
  expect(screen.getByText("duplicate of ENG-4")).toBeTruthy();
  // BY ROLE, not by walking up from a span: a grid wraps each cell, so the
  // wrapper's own `closest("a")` is null whether or not the anchor is INSIDE
  // it — an assertion that passes either way.
  expect(screen.getByText("ENG-9")).toBeTruthy();
  expect(screen.queryByRole("link", { name: "ENG-9" })).toBeNull();
  // A removal of a task that still exists IS a link, which is what makes the
  // case above about the purge rather than about the column.
  cleanup();
  serving({ work_activity: { records: [commit()], complete: true } });
  mount();
  await answered();
  expect(screen.getByText("took it off the board")).toBeTruthy();
  expect(screen.getByRole("link", { name: "ENG-9" })).toBeTruthy();
});

// TWO TASKS UNDER ONE KEY ARE TWO LINKS.
//
// A commit is named by its task's key, and a key two tasks hold opens the one
// that claimed it first — so the audit row for a change to the duplicate led
// to its claimant, which is the one place a reader goes to check what was done
// to which task. The engine says beside the key when it opens another task,
// and that row goes by the task's id.
test("changes to two tasks under one key link to their own two tasks", async () => {
  serving({
    work_activity: {
      records: [
        commit({
          id: "h-2",
          subject_id: DUPLICATE,
          subject_key: SHARED_KEY,
          subject_key_collision: true,
          excerpt: "edited the duplicate",
        }),
        commit({
          id: "h-1",
          subject_id: CLAIMANT,
          subject_key: SHARED_KEY,
          excerpt: "edited the claimant",
        }),
      ],
      complete: true,
    },
  });
  mount();
  await answered();
  const hrefs = screen
    .getAllByRole("link", { name: SHARED_KEY })
    .map((a) => a.getAttribute("href"));
  expect(new Set(hrefs)).toEqual(new Set([CLAIMANT_HREF, DUPLICATE_HREF]));
});

// A SHORT PAGE IS REPORTED, NOT SWALLOWED.
//
// The three sources with no server-side window are asked for their newest page
// and narrowed here. A window reaching further back than that page does is a
// window this screen cannot honestly claim to have covered.
test("a source whose page stops inside the window says so", async () => {
  const old = new Date(Date.now() - 60_000).toISOString();
  // THE PAGE ANSWERS LAST, once the other three sources have, so its hundred
  // rows are rendered twice — for the answer, and again as the grid takes the
  // keyboard — rather than three times: answered beside the others, the
  // credentials' answer, a REST read landing a few turns after the socket's,
  // rendered every row once more. On an idle machine the case went from
  // 700 ms to 575.
  let answerThePage: (page: unknown) => void = () => {};
  serving({
    work_activity: { records: [], complete: true },
    page_activity: new Promise((resolve) => {
      answerThePage = resolve;
    }),
  });
  mount();
  await answered();
  // A FULL PAGE whose oldest row is still inside the window: the page filled
  // up before it reached the start, so there are older rows the screen never
  // saw.
  answerThePage({
    changes: Array.from({ length: 100 }, (_, i) => ({
      id: `p-${i}`,
      page_id: "pg-1",
      kind: "saved",
      actor: "ada",
      actor_kind: "human",
      at: old,
      log_seq: 100 - i,
      title: "Deploy runbook",
      container: "ENG",
      excerpt: `edit ${i}`,
    })),
    complete: true,
  });
  await answered();
  // Found once: a text query walks every element of the page, and this page
  // is a hundred rows.
  const caption = screen.getByText(/does not reach the start of this window/);
  // AND IT NAMES WHICH SOURCE. "Some of this may be missing" is a caption
  // nobody can act on; "Knowledge answered one page" says where to look.
  expect(caption.textContent).toContain("Knowledge");
});

// AND THE TRACKER'S PAGE IS A PAGE TOO. The engine windows its feed, but it
// answers one page of two hundred, and a busy week fills it before reaching
// the window's start — while the header claimed every write across the
// tracker. A cursor comes back exactly when more rows match.
test("a tracker page that stops inside the window says so, and one that covers it does not", async () => {
  serving({
    work_activity: { records: [commit()], next_cursor: "CREWLET_WORK_LOG@1:1", complete: true },
  });
  mount();
  await answered();
  expect(
    screen.getByText(/Work answered one page, which does not reach the start of this window/),
  ).toBeTruthy();
  cleanup();

  // THE CONTROL: the same page with nothing after it is the whole window.
  serving({ work_activity: { records: [commit()], complete: true } });
  mount();
  await answered();
  expect(screen.queryByText(/does not reach the start of this window/)).toBeNull();
  expect(screen.getByText(CLAIM)).toBeTruthy();
});

// A SOURCE WHOSE ANSWER COULD NOT ACCOUNT FOR EVERYTHING SAYS SO, AND SAYS
// WHICH. The tracker's feed carries its own coverage — complete or not, the
// records this build cannot read, the applied prefix against the position —
// and this screen read none of it: a node holding tracker records it could not
// decode served an audit missing their writes under a header claiming every
// write across the tracker. It is said in History's words and named by its
// source, because the knowledge base's log, the configuration and the
// credentials are not behind with it.
test("a tracker answer that is incomplete or behind is named as the tracker's", async () => {
  serving({
    work_activity: {
      records: [commit()],
      complete: false,
      incomplete: {
        records: 2,
        from: { stream: "CREWLET_WORK_LOG", generation: 1, seq: 40 },
        scope: ["project:ENG"],
        version: 9,
      },
      log_seq: 52,
      applied_through: 40,
    },
    page_activity: { changes: [], complete: true, log_seq: 7, applied_through: 7 },
  });
  mount();
  await answered();
  await answered();
  const header = shortfall();
  expect(header).toContain(
    "Work: This answer is incomplete — 2 record(s) this build cannot read. Rows may be missing",
  );
  // AND WHOLE, as History says it: which objects the unread records are about,
  // and that a build which reads them — not a refresh — is what resolves it.
  expect(header).toContain(
    "the counts were computed over what is shown. Affected: project:ENG. Record version 9, from sequence 40 — a build that can read it is what resolves this, not a refresh.",
  );
  expect(header).toContain(
    "Work: This node holds records it has not applied yet (applied through 40 of 52).",
  );
  // ONE SOURCE'S SHORTFALL IS NOT THE PAGE'S: the knowledge base answered
  // whole and is not named, and the claim that every source is covered goes.
  expect(header).not.toContain("Knowledge:");
  expect(screen.queryByText(CLAIM)).toBeNull();
});

test("a knowledge base answer that is behind is named as the knowledge base's", async () => {
  serving({
    work_activity: { records: [commit()], complete: true, log_seq: 5, applied_through: 5 },
    page_activity: { changes: [], complete: false, log_seq: 30, applied_through: 12 },
  });
  mount();
  await answered();
  const header = shortfall();
  expect(header).toContain("Knowledge: This answer is incomplete.");
  expect(header).toContain(
    "Knowledge: This node holds records it has not applied yet (applied through 12 of 30).",
  );
  expect(header).not.toContain("Work:");
});

// AN ANSWER OF NO KNOWN AGE IS ITS SOURCE'S TOO. A node that could not measure
// its own distance from a log serves a coherent point in that log's order and
// says nothing about how old it is — the chip History draws — and that is a
// fact about the one log, not about the page.
test("a source served at a level of unknown age is named as that source's", async () => {
  serving({
    work_activity: { records: [commit()], complete: true, read_level: "stale" },
    page_activity: { changes: [], complete: true, read_level: "consistent_prefix" },
  });
  mount();
  await answered();
  const header = shortfall();
  expect(header).toContain(
    "Knowledge: This node could not measure its own distance from the log, so this answer is a coherent point in its order with no statement about age (read level consistent_prefix).",
  );
  expect(header).not.toContain("Work:");
  expect(screen.queryByText(CLAIM)).toBeNull();
});

// THE CONTROL: answers that covered everything say nothing of the kind, and the
// header still claims every source.
test("sources that answered whole leave the header's claim standing", async () => {
  serving({
    work_activity: { records: [commit()], complete: true, log_seq: 5, applied_through: 5 },
    page_activity: { changes: [], complete: true, log_seq: 7, applied_through: 7 },
  });
  mount();
  await answered();
  expect(screen.queryByText(/This answer is incomplete|has not applied yet/)).toBeNull();
  expect(screen.getByText(CLAIM)).toBeTruthy();
});

// THE EXPORT IS WHAT IS ON SCREEN.
//
// A file that carried more than the grid shows cannot be reconciled with the
// page it was exported from; one that claimed to be complete would be a claim
// about rows this screen never read. And a comma or a quote in a summary must
// not shift every later column — which is the failure that makes a CSV export
// worse than none, because a spreadsheet opens it without complaint.
test("the export escapes what a spreadsheet would otherwise split", () => {
  const csv = auditCsv([
    {
      id: "config:rev-1",
      at: "2031-04-16T00:00:00Z",
      source: "config",
      kind: "dashboard",
      actor: "founder",
      actorKind: "human",
      through: "pat:0192f00d",
      subject: "rev-1",
      detail: 'turn on Slack, and say "done"',
    },
  ]);
  const [header, row] = csv.split("\r\n");
  expect(header).toBe(
    '"at","where","who","who_kind","through","what","to","detail","node","failed"',
  );
  expect(row).toContain('"pat:0192f00d"');
  expect(row).toContain('"turn on Slack, and say ""done"""');
  // Ten columns, whatever the detail held.
  expect(row?.match(/","/g)?.length).toBe(9);
});

// A CELL SOMEBODY ELSE WROTE IS NEVER A FORMULA. A summary is text a model or a
// person typed, and a spreadsheet evaluates a leading `=` in it when the file
// opens — so the export marks it as text, the spreadsheet's own way.
test("the export opens a formula-looking cell as text", () => {
  const csv = auditCsv([
    {
      id: "config:rev-2",
      at: "2031-04-16T00:00:00Z",
      source: "config",
      kind: "dashboard",
      actor: "founder",
      actorKind: "human",
      through: "",
      subject: "rev-2",
      detail: '=HYPERLINK("https://example.com","click")',
    },
  ]);
  expect(csv.split("\r\n")[1]).toContain(`"'=HYPERLINK(""https://example.com"",""click"")"`);
});

// A PERSON BOUND TO A SEAT WRITES AS THE SEAT, AND A LOGIN IS NOT A SEAT.
//
// A bound person's write names their seat, kind `human`, and draws that seat
// as a person, linked. A credential bound to none writes under its whole login,
// kind `operator`, which no chart holds — so the cell fell to the chart's
// default and drew it as an agent's squircle, linking to a seat page that does
// not exist.
test("a bound person is drawn as their seat, and a login as plain text", async () => {
  serving({
    work_activity: {
      records: [
        commit({ id: "h-1", actor: "maya.lee", actor_kind: "operator", excerpt: "unbound write" }),
        commit({
          id: "h-2",
          actor: "token:deploy",
          actor_kind: "operator",
          excerpt: "a Tier A token's write",
        }),
        commit({
          id: "h-3",
          actor: "jane",
          actor_kind: "human",
          operator_id: "pat:0192f00d",
          excerpt: "bound write",
        }),
      ],
      complete: true,
    },
  });
  const { container } = mount();
  await answered();
  expect(screen.getByText("unbound write")).toBeTruthy();

  // UNBOUND: the human circle and the login, with nothing to follow.
  expect(screen.getByText("maya.lee")).toBeTruthy();
  expect(screen.queryByRole("link", { name: /maya/ })).toBeNull();
  expect(screen.getByText("token:deploy")).toBeTruthy();
  expect(screen.queryByRole("link", { name: /token:deploy/ })).toBeNull();
  // BOUND: the seat, by its name, linked — and the credential it came through
  // in its own column.
  const bound = screen.getByRole("link", { name: /Jane Founder/ });
  expect(bound.getAttribute("href")).toBe("#/agents/seats/jane");
  expect(screen.getByText("pat:0192f00d")).toBeTruthy();
  // ALL THREE ARE CIRCLES: the kit's outline for a person.
  expect(container.querySelectorAll(".crewlet-avatar--human").length).toBe(3);
  expect(container.querySelectorAll(".crewlet-avatar--agent").length).toBe(0);
  // No row links to a seat named after a login.
  for (const link of container.querySelectorAll("a[href^='#/agents/seats/']")) {
    expect(link.getAttribute("href")).not.toMatch(/maya|token/);
  }
});

test("writerOf: a login is a person's, a seat is the chart's, and the engine is named", () => {
  const seatOf = (address: string) =>
    address === "cto"
      ? { handle: "cto", name: "CTO", kind: "agent" as const }
      : address === "old-cto"
        ? { handle: "cto", name: "CTO", kind: "agent" as const }
        : null;
  expect(writerOf({ actor: "maya.lee", actorKind: "operator" }, seatOf)).toEqual({
    as: "login",
    name: "maya.lee",
  });
  // A SEAT THE CHART HOLDS keeps the chart's word, under the handle it answers
  // to NOW — a write recorded before a rename links to where the seat lives.
  expect(writerOf({ actor: "cto", actorKind: "human" }, seatOf)).toMatchObject({
    as: "seat",
    kind: "agent",
  });
  expect(writerOf({ actor: "old-cto", actorKind: "agent" }, seatOf)).toMatchObject({
    as: "seat",
    handle: "cto",
  });
  // …and one it no longer holds takes the kind its write was recorded under.
  expect(writerOf({ actor: "gone", actorKind: "human" }, seatOf)).toMatchObject({
    as: "seat",
    kind: "human",
  });
  expect(writerOf({ actor: "trim", actorKind: "system" }, seatOf)).toEqual({
    as: "system",
    name: "trim",
  });
  expect(writerOf({ actor: "", actorKind: "" }, seatOf)).toEqual({ as: "engine" });
  // THE IDENTITY TRAIL'S PRINCIPAL KINDS read the same way: a machine is a
  // login, the engine the system, and a person their seat only where the
  // chart holds one by that address.
  expect(writerOf({ actor: "ci:release", actorKind: "machine" }, seatOf)).toEqual({
    as: "login",
    name: "ci:release",
  });
  expect(writerOf({ actor: "sweep", actorKind: "engine" }, seatOf)).toEqual({
    as: "system",
    name: "sweep",
  });
  expect(writerOf({ actor: "jane.doe", actorKind: "person" }, seatOf)).toEqual({
    as: "login",
    name: "jane.doe",
  });
  expect(writerOf({ actor: "cto", actorKind: "person" }, seatOf)).toMatchObject({ as: "seat" });
  // AND A RECORD THAT SAYS NOTHING IS NOT THE ENGINE.
  expect(writerOf({ actor: "", actorKind: "", unrecorded: true }, seatOf)).toEqual({
    as: "unrecorded",
  });
});

// A CONFIG REVISION SAYS WHAT WROTE IT, AND THE ROW BELIEVES IT.
//
// The row used to be labelled `operator` whatever wrote the revision — so a
// node's boot seed and the reconcile loop's reload after sealing a credential
// read as a person's writes. The kind is the revision's own now, and one with
// no recorded author says so rather than being drawn as the engine.
test("a config revision is labelled with the kind it recorded, not with operator", async () => {
  serving({
    config_audit: [
      {
        revision_id: "rev-seed0001",
        summary: "seeded from company.yaml",
        source: "file",
        created_by: "node-a",
        created_by_kind: "system",
        created_at: RECENTLY,
      },
      {
        revision_id: "rev-loop0001",
        summary: "reload after provisioning sealed a credential",
        source: "api",
        created_by: "reconcile loop",
        created_by_kind: "system",
        created_at: RECENTLY,
      },
      {
        revision_id: "rev-nobody01",
        summary: "nobody recorded the author",
        source: "fleet",
        created_by: "",
        created_by_kind: "",
        created_at: RECENTLY,
      },
    ],
  });
  mount();
  await answered();
  expect(screen.getByText("seeded from company.yaml")).toBeTruthy();
  expect(screen.getByText("node-a")).toBeTruthy();
  expect(screen.getByText("reconcile loop")).toBeTruthy();
  expect(screen.getAllByText("system")).toHaveLength(2);
  // NO ROW IS AN OPERATOR'S: none of these was a person's write.
  expect(screen.queryByText("operator")).toBeNull();
  expect(screen.getByText("Not recorded")).toBeTruthy();
  expect(screen.queryByText("the engine")).toBeNull();
});

/** One runtime audit row, as the event log lists it: payload-free, with the
 *  dimensions the store promoted into its tags. */
function runtimeEvent(over: Record<string, unknown> = {}) {
  return {
    id: "ev-1",
    type: "operator_acted",
    timestamp: RECENTLY,
    source: "operator",
    actor: "jane",
    summary: "jane ran update_work_item: refused (conflict)",
    category: "lifecycle",
    trace_id: "",
    span_id: "",
    parent_span_id: "",
    topic: "",
    failed: true,
    tags: {
      node: "node-b",
      actor_kind: "human",
      operator_id: "session:0192f00e",
      tool: "update_work_item",
    },
    ...over,
  };
}

// THE FIFTH SOURCE: EVERY CALL, WHATEVER BECAME OF IT.
//
// A refused tool call and a backup change no tracker or wiki record, so the
// four histories hold nothing about either. The runtime audit does, and it is
// asked for by the source the engine stamps on it, over the same window.
test("runtime calls are merged in, drawn as their author, and narrowed by Where", async () => {
  const query = serving({
    work_activity: { records: [commit()], complete: true },
    events: {
      events: [
        runtimeEvent(),
        runtimeEvent({
          id: "ev-2",
          type: "backup_requested",
          failed: false,
          summary: "jane backed up to /var/backups/one (12 streams)",
          tags: {
            node: "node-a",
            actor_kind: "human",
            operator_id: "session:0192f00e",
            dir: "/var/backups/one",
          },
        }),
      ],
      next: null,
      exhausted: true,
      coverage: { nodes: [{ id: "node-a", answered: true, error: "" }], complete: true },
    },
  });
  mount();
  await answered();
  expect(screen.getByText(/ran update_work_item/)).toBeTruthy();
  expect(asked(query, "events").source).toBe("operator");
  expect(asked(query, "events").since).toBeTruthy();
  expect(asked(query, "events").until).toBeTruthy();

  // WHAT WAS DONE: the tool a call ran, and a backup as a backup.
  expect(screen.getByText("update work item")).toBeTruthy();
  expect(screen.getByText("backup")).toBeTruthy();
  // TO WHAT: a backup's directory, linked to the backups it is one of; a tool
  // call's arguments are never recorded, and the cell says so.
  const dirLink = screen.getByRole("link", { name: "/var/backups/one" });
  expect(dirLink.getAttribute("href")).toBe("#/settings/backups");
  // CUT WITH AN ELLIPSIS AT THE COLUMN, the whole directory on the title.
  expect(dirLink.getAttribute("title")).toBe("/var/backups/one");
  expect(dirLink.classList.contains("truncate")).toBe(true);
  expect(screen.getByText("Not recorded")).toBeTruthy();
  // WHO: the seat the person wrote as, with the KIND and the CREDENTIAL the
  // engine recorded on the row — never "operator" for a bound person's call.
  expect(screen.getAllByRole("link", { name: /Jane Founder/ }).length).toBe(2);
  expect(screen.getAllByText("session:0192f00e").length).toBe(2);
  expect(screen.getByText("took it off the board")).toBeTruthy();

  // AND `kind=runtime` IS THE RUNTIME ROWS ALONE.
  cleanup();
  location.hash = "#/settings/audit?kind=runtime";
  serving({
    work_activity: { records: [commit()], complete: true },
    events: { events: [runtimeEvent()], next: null, exhausted: true },
  });
  mount();
  await answered();
  expect(screen.getByText(/ran update_work_item/)).toBeTruthy();
  expect(screen.queryByText("took it off the board")).toBeNull();
});

// A WINDOWED PAGE THAT FILLED IS REPORTED TOO. The runtime audit and the
// tracker's feed are windowed by the engine, but a page is still a page: a busy
// week reaches further back than one, and the screen must not claim it saw the
// whole window.
test("a windowed source whose page filled says so", async () => {
  serving({
    events: {
      events: Array.from({ length: 200 }, (_, i) => runtimeEvent({ id: `ev-${i}` })),
      next: null,
      exhausted: false,
    },
  });
  mount();
  await answered();
  const note = screen.getByText(/does not reach the start of this window/);
  expect(note.textContent).toContain("Runtime");
  // A NOTICE OF ITS OWN, never the card header's one line, which cut the
  // sentence before "not all of them" at every width.
  expect(note.textContent).toMatch(/not all of them\.$/);
  expect(note.closest(".crewlet-card")).toBeNull();
});

// AN EMPTY "TO" SAYS WHY, IN THE ROW'S OWN TERMS. "Not recorded" is the
// runtime audit's policy about a tool call's ARGUMENTS; a backup row that
// carries no directory tag is a different fact and must not borrow it.
test("an empty To cell says why in the row's own terms", async () => {
  serving({
    events: {
      events: [
        runtimeEvent({
          id: "ev-nodir",
          type: "backup_requested",
          failed: false,
          summary: "jane backed up to /var/backups/two (12 streams)",
          tags: { node: "node-a", actor_kind: "human" },
        }),
      ],
      next: null,
      exhausted: true,
    },
  });
  mount();
  await answered();
  expect(screen.getByText(/backed up to \/var\/backups\/two/)).toBeTruthy();
  expect(screen.getByText("No directory recorded")).toBeTruthy();
  expect(screen.queryByText("Not recorded")).toBeNull();
});

/** One entry of the identity trail, as `GET /iam/audit` answers it. */
function identityEntry(over: Record<string, unknown> = {}) {
  return {
    id: "ih-1",
    class: "change",
    object_kind: "login",
    object_id: "sam.okafor",
    person: "0198f0a0-0000-7000-8000-0000000000e5",
    op: "invite",
    actor: "jane",
    actor_kind: "person",
    operator_id: "session:0192f00e",
    summary: "invited sam.okafor",
    at: RECENTLY,
    position: 41,
    ...over,
  };
}

// THE SIXTH SOURCE: WHO CAN REACH THE COMPANY, AND WHO CHANGED IT.
//
// An invitation, a grant edit and a revoked token change no tracker or wiki
// record either. The identity estate keeps its own trail, which the route
// pages by POSITION — so the window's start is handed over as an instant for
// the route to resolve, computed when the trail is asked.
test("the identity trail is merged in, asked from the window's start", async () => {
  restAnswers.identity = () =>
    json({ events: [identityEntry()], next: 0, position: "CREWLET_IAM_LOG@1:41" });
  serving({ work_activity: { records: [commit()], complete: true } });
  mount();
  await answered();
  expect(screen.getByText("invited sam.okafor")).toBeTruthy();
  const read = readsOf("/iam/audit")[0]!;
  expect(Date.parse(read.query.get("at") ?? "")).toBeLessThan(Date.now() - 6 * 86_400_000);
  expect(read.query.get("limit")).toBe("200");
  // THE RECORD'S OWN KIND, the principal's, and its author drawn as the seat
  // the chart holds by that address.
  expect(screen.getByText("person")).toBeTruthy();
  expect(screen.getByRole("link", { name: /Jane Founder/ })).toBeTruthy();
  // ITS SUBJECT a name a person reads, opening the screen that shows it.
  expect(screen.getByRole("link", { name: "sam.okafor" }).getAttribute("href")).toBe(
    "#/settings/people",
  );
});

// A TRAIL PAGE WHOSE ENGINE SAID MORE MATCH, stopping inside the window, is a
// page short of it — and one that says nothing more is the whole window.
test("an identity page that stops inside the window says so", async () => {
  restAnswers.identity = () =>
    json({ events: [identityEntry()], next: 40, position: "CREWLET_IAM_LOG@1:41" });
  serving({ work_activity: { records: [], complete: true } });
  mount();
  await answered();
  expect(shortfall()).toContain(
    "Identity answered one page, which does not reach the start of this window",
  );
});

// A REFUSED TRAIL IS SAID, AND THE REST STAND: the directory's trail is the
// audit grant's, and a reader refused it keeps every other source.
test("a refused identity trail names the grant, and the other sources stand", async () => {
  restAnswers.identity = () =>
    json({ error: "unauthorized", reason: "no_grant", grants: ["audit:read"] }, 403);
  serving({ work_activity: { records: [commit()], complete: true } });
  mount();
  await answered();
  expect(shortfall()).toContain("Reading the identity trail needs audit:read");
  expect(screen.getByText("took it off the board")).toBeTruthy();
  expect(screen.queryByText(CLAIM)).toBeNull();
});

const SECRET = {
  name: "GITHUB_TOKEN",
  key_id: "k1",
  updated_at: RECENTLY,
  updated_by: "founder",
  updated_by_kind: "human",
  source: "dashboard",
};

// THE CREDENTIALS MISSING IS SAID, AND THE REST STAND. This read failed in
// silence: the credentials dropped out of an audit whose header said it covered
// them, so a reader refused them, or one whose request nothing answered, was
// told every write was here.
test("a refused credential listing names the grant, and the other sources stand", async () => {
  restAnswers.secrets = () =>
    json({ error: "unauthorized", reason: "no_grant", grants: ["config:read"] }, 403);
  serving({ work_activity: { records: [commit()], complete: true } });
  mount();
  await answered();
  expect(shortfall()).toContain("Listing the credentials' writes needs config:read");
  expect(screen.getByText("took it off the board")).toBeTruthy();
  expect(screen.queryByText(CLAIM)).toBeNull();
});

// AND A READ NOBODY ANSWERED IS ASKED AGAIN ON ITS OWN — it was read once, at
// mount, and never again while the sources beside it polled — and then on the
// cadence those keep, so a credential stored after the screen opened arrives
// with the rest.
test("a credential listing nothing answered is said, asked again, and then polled", async () => {
  vi.useFakeTimers();
  try {
    restAnswers.secrets = (call) => {
      if (call === 1) throw new TypeError("Failed to fetch");
      return json({ secrets: [SECRET] });
    };
    serving({ work_activity: { records: [], complete: true } });
    mount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(shortfall()).toContain("none of their writes are listed here");

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    expect(readsOf("/secrets")).toHaveLength(2);
    expect(screen.getByText("GITHUB_TOKEN")).toBeTruthy();
    expect(shortfall()).not.toContain("none of their writes are listed here");

    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(readsOf("/secrets")).toHaveLength(3);
  } finally {
    vi.useRealTimers();
  }
});

// A NAMED RANGE ENDS NOW, NOT AT THE TRACKER'S LAST ASK.
//
// The sources narrowed here were cut at both edges the tracker was last asked
// over, and the top one is only the instant of that ask: "the last seven days"
// ends now. So a credential, a revision or a page change written after it was
// hidden until the tracker was asked again — a minute on every poll, and for
// ever once a refusal no wait clears stopped the tracker's poll while the
// others went on answering. Here the credentials' read is retried a second
// after the tracker's ask and finds one stored in that second; the tracker is
// not asked again, so nothing but its own source's answer can let it in.
test("a write after the tracker's last ask is listed when its own source answers", async () => {
  vi.useFakeTimers();
  try {
    let stored = 0;
    restAnswers.secrets = (call) => {
      if (call === 1) throw new TypeError("Failed to fetch");
      stored = Date.now();
      return json({ secrets: [{ ...SECRET, updated_at: new Date(stored).toISOString() }] });
    };
    const query = serving({ work_activity: { records: [], complete: true } });
    mount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    const tracked = Date.parse(String(asked(query, "work_activity").to));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
    expect(stored).toBeGreaterThan(tracked);
    expect(
      (query.mock.calls as unknown as [string][]).filter(([what]) => what === "work_activity"),
    ).toHaveLength(1);
    expect(screen.getByText("GITHUB_TOKEN")).toBeTruthy();
  } finally {
    vi.useRealTimers();
  }
});

// AND A READER'S OWN WINDOW STILL ENDS WHERE THEY SAID. Two instants a reader
// named are a bound at both ends, so a revision written after the second one
// is outside it whatever source it came from — which is what makes the case
// above about the named range rather than about dropping the top edge.
test("a reader's own window cuts every source at the end they named", async () => {
  const to = Date.now() - 3_600_000;
  const from = to - 86_400_000;
  const iso = (at: number) => new Date(at).toISOString();
  location.hash = `#/settings/audit?window=${encodeURIComponent(`${iso(from)}/${iso(to)}`)}`;
  const revision = (id: string, at: number, summary: string) => ({
    revision_id: id,
    summary,
    source: "dashboard",
    created_by: "founder",
    created_by_kind: "human",
    created_at: iso(at),
  });
  const query = serving({
    work_activity: { records: [], complete: true },
    config_audit: [
      revision("rev-inside", to - 60_000, "inside the window"),
      revision("rev-after", to + 60_000, "after the window"),
    ],
  });
  mount();
  await answered();
  expect(asked(query, "work_activity").to).toBe(iso(to));
  // AND THE IDENTITY TRAIL IS ASKED FROM THE START THEY NAMED.
  expect(readsOf("/iam/audit")[0]?.query.get("at")).toBe(iso(from));
  expect(screen.getByText("inside the window")).toBeTruthy();
  expect(screen.queryByText("after the window")).toBeNull();
});
