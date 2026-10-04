/**
 * The audit reads four subsystems as one feed, and says what it cannot see.
 *
 * Three claims, and each is the whole reason the screen exists:
 *
 *  1. IT ASKS FOR WHO WAS WRITING. Both history feeds are asked with
 *     `actor_kinds`, not with a set of handles — an `operator` commit carries
 *     a token's own label where an `agent` one carries a seat handle, so the
 *     two name spaces are disjoint, and the set of people is the roster, which
 *     changes. Asked by handle, this screen would quietly lose every write
 *     made by somebody who has since left, which is the one write an audit is
 *     usually opened to find.
 *  2. FOUR SOURCES, ONE ORDER. A tracker commit, a page change, a config
 *     revision and a credential write are four honest records in four places;
 *     read separately they cannot answer "what did we change on Tuesday".
 *  3. A PAGE THAT DID NOT REACH THE WINDOW SAYS SO. Only the tracker's feed
 *     takes a wall-clock window; the other three are narrowed here, so a busy
 *     company's window can extend past the oldest row that arrived — and a
 *     screen silent about that is claiming those rows do not exist.
 */

import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Audit, auditCsv, writerOf } from "./Audit.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import { QueryError, type QueryName } from "~/protocol/index.ts";

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
 * the identity trail — each answered as told, every read recorded by its path.
 */
let restAnswers: {
  secrets: () => Response;
  identity: () => Response | Promise<Response>;
};
let restReads: { path: string; query: URLSearchParams }[] = [];

/** The reads made of one REST path, in order. */
const readsOf = (path: string) => restReads.filter((r) => r.path === path);

beforeEach(() => {
  location.hash = "#/settings/audit";
  restReads = [];
  restAnswers = {
    secrets: () => json({ secrets: [] }),
    identity: () => json({ events: [], next: 0, position: "CREWLET_IAM_LOG@1:1" }),
  };
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), "http://engine.test");
      restReads.push({ path: url.pathname, query: url.searchParams });
      if (url.pathname === "/iam/audit") return restAnswers.identity();
      if (url.pathname === "/secrets") return restAnswers.secrets();
      return json({});
    }),
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.useRealTimers();
  // The tab's visibility goes back to jsdom's own, which stops the clock.
  delete (document as { visibilityState?: unknown }).visibilityState;
  location.hash = "";
});

/**
 * A clock the test moves, started half way through a minute — so a few
 * seconds passing cross no minute, and a poll a minute on comes after one —
 * in a tab that is VISIBLE, which jsdom's is not and the shared clock ticks
 * only in.
 */
function clockMidMinute() {
  Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(Math.ceil(Date.now() / 60_000) * 60_000 + 30_000);
}

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
    actor: "U0FOUNDER",
    actor_kind: "operator",
    subject_kind: "task",
    subject_id: "t-1",
    subject_key: "ENG-9",
    excerpt: "took it off the board",
    notified: false,
    ...over,
  };
}

/** The socket's answers, one per question; an Error is the question refused. */
function serving(answers: Partial<Record<QueryName, unknown>> = {}) {
  const query = vi.fn(async (what: string) => {
    const answer = answers[what as QueryName];
    if (answer instanceof Error) throw answer;
    return answer ?? {};
  });
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

const mount = () =>
  render(
    <Router>
      <Audit />
    </Router>,
  );

/** The parameters one question was actually asked with. */
function asked(query: ReturnType<typeof serving>, what: QueryName): Record<string, unknown> {
  const calls = query.mock.calls as unknown as [string, Record<string, unknown>?][];
  return calls.findLast(([name]) => name === what)?.[1] ?? {};
}

// IT ASKS BY KIND, ON BOTH FEEDS.
test("both history feeds are asked for who was writing, not for a list of people", async () => {
  const query = serving({ work_activity: { records: [], complete: true } });
  mount();
  await waitFor(() => expect(asked(query, "work_activity").actor_kinds).toBeTruthy());

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
        created_by_kind: "operator",
        created_at: RECENTLY,
      },
    ],
  });
  mount();

  await waitFor(() => expect(screen.getByText("took it off the board")).toBeTruthy());
  expect(screen.getByText("rewrote the rollback step")).toBeTruthy();
  expect(screen.getByText("turn on the Slack integration")).toBeTruthy();
  // AND EVERY ROW SAYS WHICH KIND OF WRITER MADE IT, which is the fact that
  // separates "a person did this" from "a token did" from "the engine did".
  expect(screen.getAllByText("operator").length).toBeGreaterThan(0);
  expect(screen.getAllByText("human").length).toBeGreaterThan(0);
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
  await waitFor(() => expect(screen.getByText("duplicate of ENG-4")).toBeTruthy());
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
  await waitFor(() => expect(screen.getByText("took it off the board")).toBeTruthy());
  expect(screen.getByRole("link", { name: "ENG-9" })).toBeTruthy();
});

// A SHORT PAGE IS REPORTED, NOT SWALLOWED.
//
// The three sources with no server-side window are asked for their newest page
// and narrowed here. A window reaching further back than that page does is a
// window this screen cannot honestly claim to have covered.
test("a source whose page stops inside the window says so", async () => {
  const old = new Date(Date.now() - 60_000).toISOString();
  serving({
    work_activity: { records: [], complete: true },
    // A FULL PAGE whose oldest row is still inside the window: the page
    // filled up before it reached the start, so there are older rows the
    // screen never saw.
    page_activity: {
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
    },
  });
  mount();
  await waitFor(() =>
    expect(screen.getByText(/does not reach the start of this window/)).toBeTruthy(),
  );
  // AND IT NAMES WHICH SOURCE. "Some of this may be missing" is a caption
  // nobody can act on; "Knowledge answered one page" says where to look.
  expect(screen.getByText(/does not reach the start of this window/).textContent).toContain(
    "Knowledge",
  );
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
  await waitFor(() => expect(screen.getByText("unbound write")).toBeTruthy());

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
  const seatOf = (handle: string) =>
    handle === "cto" ? { name: "CTO", kind: "agent" as const } : null;
  expect(writerOf({ actor: "maya.lee", actorKind: "operator" }, seatOf)).toEqual({
    as: "login",
    name: "maya.lee",
  });
  // A SEAT THE CHART HOLDS keeps the chart's word; one it no longer holds
  // takes the kind its write was recorded under.
  expect(writerOf({ actor: "cto", actorKind: "human" }, seatOf)).toMatchObject({
    as: "seat",
    kind: "agent",
  });
  expect(writerOf({ actor: "gone", actorKind: "human" }, seatOf)).toMatchObject({
    as: "seat",
    kind: "human",
  });
  // THE ENGINE, NAMED: a node's seed and a loop both write as `system`.
  expect(writerOf({ actor: "node-a", actorKind: "system" }, seatOf)).toEqual({
    as: "system",
    name: "node-a",
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
// no recorded author (adopted from an older engine's pointer) says so rather
// than being drawn as the engine.
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
        revision_id: "rev-oldpeer1",
        summary: "adopted from an older engine",
        source: "fleet",
        created_by: "",
        created_by_kind: "",
        created_at: RECENTLY,
      },
    ],
  });
  mount();
  await waitFor(() => expect(screen.getByText("seeded from company.yaml")).toBeTruthy());
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
  await waitFor(() => expect(screen.getByText(/ran update_work_item/)).toBeTruthy());
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
  await waitFor(() => expect(screen.getByText(/ran update_work_item/)).toBeTruthy());
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
  await waitFor(() =>
    expect(screen.getByText(/does not reach the start of this window/)).toBeTruthy(),
  );
  expect(screen.getByText(/does not reach the start of this window/).textContent).toContain(
    "Runtime",
  );
  // A NOTICE OF ITS OWN, never the card header's one line, which cut the
  // sentence before "not all of them" at every width.
  const note = screen.getByText(/does not reach the start of this window/);
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
          summary: "U0FOUNDER backed up to /var/backups/two (12 streams)",
          tags: { node: "node-a" },
        }),
      ],
      next: null,
      exhausted: true,
    },
  });
  mount();
  await waitFor(() => expect(screen.getByText(/backed up to \/var\/backups\/two/)).toBeTruthy());
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
// the route to resolve.
test("the identity trail is merged in, asked from the window's start", async () => {
  restAnswers.identity = () =>
    json({ events: [identityEntry()], next: 0, position: "CREWLET_IAM_LOG@1:41" });
  serving({ work_activity: { records: [commit()], complete: true } });
  mount();
  await waitFor(() => expect(screen.getByText("invited sam.okafor")).toBeTruthy());
  const read = readsOf("/iam/audit")[0]!;
  expect(Date.parse(read.query.get("at") ?? "")).toBeLessThan(Date.now() - 6 * 86_400_000);
  expect(read.query.get("limit")).toBe("200");
  // ITS AUTHOR drawn as the seat the chart holds by that address, and the
  // credential it came through beside it.
  expect(screen.getAllByRole("link", { name: /Jane Founder/ }).length).toBeGreaterThan(0);
  expect(screen.getAllByText("session:0192f00e").length).toBeGreaterThan(0);
  // AND THE REST STAND beside it.
  expect(screen.getByText("took it off the board")).toBeTruthy();
});

// A REFUSED TRAIL IS SAID, AND THE REST STAND: the directory's trail is the
// audit grant's, and a reader refused it keeps every other source.
test("a refused identity trail names the grant, and the other sources stand", async () => {
  restAnswers.identity = () =>
    json({ error: "unauthorized", reason: "no_grant", grants: ["audit:read"] }, 403);
  serving({ work_activity: { records: [commit()], complete: true } });
  mount();
  await waitFor(() =>
    expect(screen.getByText(/Reading the identity trail needs audit:read/)).toBeTruthy(),
  );
  expect(screen.getByText("took it off the board")).toBeTruthy();
});

// A SOURCE REFUSED ON AUTHORITY IS WITHHELD, AND THE REST STAND.
//
// The section opens on the audit grant, and the configuration's revisions
// answer to another. An auditor without it was shown the refusal banner alone,
// which hid every row of the sources they may read.
test("a source refused on authority names its grant, and the other sources stand", async () => {
  serving({
    work_activity: { records: [commit()], complete: true },
    config_audit: new QueryError("unauthorized", { reason: "no_grant", grants: ["config:read"] }),
  });
  mount();
  await waitFor(() => expect(screen.getByText("took it off the board")).toBeTruthy());
  expect(screen.getByText(/Reading the configuration's revisions needs config:read/)).toBeTruthy();
  expect(screen.queryByText(/You may not read this/)).toBeNull();
});

// AND A REFUSAL REPLACES WHAT IT REFUSED: rows read before a grant was taken
// away are not drawn under the sentence saying they are withheld.
test("a source refused after it answered takes its rows off the grid", async () => {
  clockMidMinute();
  const query = serving();
  let asked = 0;
  query.mockImplementation(async (what: string) => {
    if (what !== "config_audit") return {};
    if (asked++ > 0) {
      throw new QueryError("unauthorized", { reason: "no_grant", grants: ["config:read"] });
    }
    return [
      {
        revision_id: "rev-seed0001",
        summary: "seeded from company.yaml",
        source: "file",
        created_by: "node-a",
        created_by_kind: "system",
        created_at: RECENTLY,
      },
    ];
  });
  mount();
  await waitFor(() => expect(screen.getByText("seeded from company.yaml")).toBeTruthy());
  for (let second = 0; second < 61; second++) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
  }
  expect(screen.getByText(/Reading the configuration's revisions needs config:read/)).toBeTruthy();
  expect(screen.queryByText("seeded from company.yaml")).toBeNull();
});

// THE WINDOW IS NOT THE SECOND HAND.
//
// The windowed questions were keyed on edges read off the one-second clock,
// so each was asked again every tick — the fleet-wide event read among them,
// whose answer can take two seconds and was dropped for the next tick's.
test("a second passing asks the windowed sources nothing again", async () => {
  clockMidMinute();
  const query = serving({ work_activity: { records: [], complete: true } });
  mount();
  const asks = (what: QueryName) => query.mock.calls.filter(([name]) => name === what).length;
  await waitFor(() => expect(asks("events")).toBeGreaterThan(0));
  const before = { work: asks("work_activity"), events: asks("events") };
  await act(async () => {
    await vi.advanceTimersByTimeAsync(3_000);
  });
  expect(asks("work_activity")).toBe(before.work);
  expect(asks("events")).toBe(before.events);
});

// A RE-READ OF THE TRAIL IS QUIET, AND STILL MOVES THE WINDOW.
//
// A REST read whose key changes shows nothing until it answers. Keyed on the
// window's start, the trail's rows left the grid every time the window moved,
// and a read slower than that was aborted by the next and never answered.
test("the identity trail stays on screen while its poll asks from the moved start", async () => {
  clockMidMinute();
  let answered = 0;
  restAnswers.identity = () =>
    answered++ === 0
      ? json({ events: [identityEntry()], next: 0, position: "CREWLET_IAM_LOG@1:41" })
      : new Promise<Response>(() => {});
  serving({ work_activity: { records: [], complete: true } });
  mount();
  await waitFor(() => expect(screen.getByText("invited sam.okafor")).toBeTruthy());

  // A SECOND AT A TIME, as the clock ticks: one jump of a minute would be
  // sixty ticks rendered inside one update.
  for (let second = 0; second < 61; second++) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_000);
    });
  }
  const [first, second] = readsOf("/iam/audit");
  expect(second).toBeTruthy();
  expect(Date.parse(second!.query.get("at")!)).toBeGreaterThan(Date.parse(first!.query.get("at")!));
  // THE SECOND READ HAS NOT ANSWERED, and the first one's rows are still drawn.
  expect(screen.getByText("invited sam.okafor")).toBeTruthy();
});
