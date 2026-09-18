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

import { Audit, auditCsv } from "./Audit.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import type { QueryName } from "~/protocol/index.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn(), useOrg: vi.fn() };
});

beforeEach(() => {
  location.hash = "#/admin/audit";
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
    roles: [{ name: "Ada Okonkwo", handle: "ada", kind: "agent" }],
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
  const [header, row] = csv.split("\n");
  expect(header).toBe("at,where,who,who_kind,what,to,detail");
  expect(row).toContain('"turn on Slack, and say ""done"""');
  // Seven columns, whatever the detail held.
  expect(row?.match(/","/g)?.length).toBe(6);
});
