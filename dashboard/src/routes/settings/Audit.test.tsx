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

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Audit, auditCsv, writerOf } from "./Audit.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import type { QueryName } from "~/protocol/index.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn(), useOrg: vi.fn() };
});

beforeEach(() => {
  location.hash = "#/settings/audit";
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(() =>
      Promise.resolve(
        new Response(JSON.stringify({ secrets: [] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      ),
    ),
  });
});

afterEach(() => {
  cleanup();
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
      actorKind: "operator",
      subject: "rev-1",
      detail: 'turn on Slack, and say "done"',
    },
  ]);
  const [header, row] = csv.split("\r\n");
  expect(header).toBe('"at","where","who","who_kind","who_seat","what","to","detail"');
  expect(row).toContain('"turn on Slack, and say ""done"""');
  // Eight columns, whatever the detail held.
  expect(row?.match(/","/g)?.length).toBe(7);
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
      actorKind: "operator",
      subject: "rev-2",
      detail: '=HYPERLINK("https://example.com","click")',
    },
  ]);
  expect(csv.split("\r\n")[1]).toContain(`"'=HYPERLINK(""https://example.com"",""click"")"`);
});

// AN OPERATOR IS A PERSON, AND A TOKEN IS NOT A SEAT.
//
// An `operator` write carries the token's name, which no chart holds — so the
// cell fell to the chart's default and drew every operator as an agent's
// squircle, linking "maya" to a seat page that does not exist.
test("an operator is drawn as a person: their seat when the token is bound, plain text when not", async () => {
  serving({
    work_activity: {
      records: [
        commit({ id: "h-1", actor: "maya", actor_kind: "operator", excerpt: "unbound write" }),
        commit({
          id: "h-2",
          actor: "U0FOUNDER",
          actor_kind: "operator",
          actor_seat: "jane",
          excerpt: "bound write",
        }),
      ],
      complete: true,
    },
  });
  const { container } = mount();
  await waitFor(() => expect(screen.getByText("unbound write")).toBeTruthy());

  // UNBOUND: the human circle and the token's name, with nothing to follow.
  expect(screen.getByText("maya")).toBeTruthy();
  expect(screen.queryByRole("link", { name: /maya/ })).toBeNull();
  // BOUND: the person's seat, by its name, linked — the token stays in the
  // tooltip, since it is the audit trail.
  const bound = screen.getByRole("link", { name: /Jane Founder/ });
  expect(bound.getAttribute("href")).toBe("#/agents/seats/jane");
  expect(bound.getAttribute("title")).toContain("U0FOUNDER");
  // BOTH ARE CIRCLES: the kit's outline for a person.
  const circles = container.querySelectorAll(".crewlet-avatar--human");
  expect(circles.length).toBe(2);
  expect(container.querySelectorAll(".crewlet-avatar--agent").length).toBe(0);
  // No row links to a seat named after a token.
  for (const link of container.querySelectorAll("a[href^='#/agents/seats/']")) {
    expect(link.getAttribute("href")).not.toMatch(/maya|U0FOUNDER/);
  }
});

test("writerOf: the kind an operator is drawn with is a person's", () => {
  const who = (handle: string) =>
    handle === "cto" ? { name: "CTO", kind: "agent" as const } : { name: handle };
  expect(writerOf({ actor: "maya", actorKind: "operator" }, who)).toEqual({
    as: "token",
    name: "maya",
  });
  expect(writerOf({ actor: "tok", actorKind: "operator", actorSeat: "jane" }, who)).toEqual({
    as: "seat",
    handle: "jane",
    name: "jane",
    kind: "human",
    token: "tok",
  });
  // A seat the chart holds keeps the chart's word; one it no longer holds
  // takes the kind its write was recorded under.
  expect(writerOf({ actor: "cto", actorKind: "human" }, who)).toMatchObject({ kind: "agent" });
  expect(writerOf({ actor: "gone", actorKind: "human" }, who)).toMatchObject({ kind: "human" });
  expect(writerOf({ actor: "trim", actorKind: "system" }, who)).toEqual({
    as: "system",
    name: "trim",
  });
  expect(writerOf({ actor: "", actorKind: "" }, who)).toEqual({ as: "engine" });
  // A NODE IS THE ENGINE, NAMED — never a seat to look up.
  expect(writerOf({ actor: "node-a", actorKind: "node" }, who)).toEqual({
    as: "system",
    name: "node-a",
  });
  // AND A RECORD THAT SAYS NOTHING IS NOT THE ENGINE.
  expect(writerOf({ actor: "", actorKind: "", unrecorded: true }, who)).toEqual({
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
        created_by_kind: "node",
        created_at: RECENTLY,
      },
      {
        revision_id: "rev-loop0001",
        summary: "reload after provisioning sealed a credential",
        source: "api",
        created_by: "reconcile loop",
        created_by_kind: "node",
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
  expect(screen.getAllByText("node")).toHaveLength(2);
  // NO ROW IS AN OPERATOR'S: none of these was a person's write.
  expect(screen.queryByText("operator")).toBeNull();
  expect(screen.getByText("Not recorded")).toBeTruthy();
  expect(screen.queryByText("the engine")).toBeNull();
});
