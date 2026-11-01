/**
 * The listings of changes and reviews a screen outside Code mounts: a task's
 * changes, a seat's Code tab, My work's Reviews and Home's "Code waiting on a
 * person". Each is the engine's own listing over REST with the filter the
 * screen fixes, drawn in Code › Changes' own grid.
 *
 * The invariants worth breaking a build over: a work item's listing names no
 * state (every state of the item, which the work item lets the engine walk);
 * a review row names the change it judged and opens that change's page; the
 * Home card asks the reader's own review only of a reader bound to a seat;
 * and a seat's Code tab draws the usage counters for an agent and none for a
 * person.
 */

import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactElement } from "react";
import { afterEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { SeatCode } from "~/routes/agents/seat/Code.tsx";
import { CodeWaiting } from "~/routes/home/CodeWaiting.tsx";
import type { Seat } from "~/lib/seats.ts";
import { ChangeList, ReviewList } from "./ChangeList.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };

const ORG = withDerived({
  name: "Acme",
  roles: [
    { name: "CTO", handle: "cto" },
    { name: "Engineer", handle: "dev" },
  ],
  units: [],
  services: { tracker: "native", knowledge: "native", code: "native" },
}) as unknown as OrgProjection;

function change(repo: string, number: number) {
  return {
    id: `id-${repo}-${number}`,
    repo_id: `repo-${repo}`,
    number,
    title: `Change ${number} of ${repo}`,
    state: "open",
    source_ref: "refs/heads/feature",
    target_ref: "refs/heads/main",
    head: "0123456789abcdef0123456789abcdef01234567",
    revision: 2,
    owner: "dev",
    owner_kind: "agent",
    opened_at: "2026-10-27T10:00:00Z",
    opened_position: POSITION,
    version: 1,
    position: POSITION,
    repo,
    ref: `${repo}!${number}`,
    access: "maintain",
  };
}

const json = (payload: unknown, status = 200) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });

/** Answers every code question with `answer`'s reply, recording each ask. */
function stubFetch(answer: (url: URL) => unknown) {
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = new URL(String(input));
      asked.push(url);
      if (url.pathname === "/auth/session") return new Promise<Response>(() => {});
      const body = answer(url);
      return Promise.resolve(body === undefined ? json({}, 404) : json(body));
    }),
  );
  return asked;
}

function mount(
  ui: ReactElement,
  viewer: unknown = ADMIN_VIEWER,
  socketAnswers: Record<string, unknown> = {},
) {
  const store = new Store();
  store.applyOrg(ORG);
  const socket = new LiveSocket(store);
  const onSocket: { what: string; params: unknown }[] = [];
  (socket as unknown as { query: (what: string, params?: unknown) => Promise<unknown> }).query = (
    what,
    params,
  ) => {
    onSocket.push({ what, params });
    if (what === "viewer") return Promise.resolve(viewer);
    if (what in socketAnswers) return Promise.resolve(socketAnswers[what]);
    return new Promise(() => {});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>{ui}</Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
  return { onSocket };
}

const codeAsks = (asked: URL[], what: string) =>
  asked.filter((u) => u.pathname === `/query/${what}`);

// A WORK ITEM'S CHANGES ARE ASKED WITH NO STATE — the engine lists every state
// of one work item, and refuses `state=all` — in Code › Changes' own grid.
// Mutation: send a state with the work item, or draw rows in another grid,
// and this fails.
test("a work item's listing names the item and no state, drawn in the Changes grid", async () => {
  const asked = stubFetch((url) =>
    url.pathname === "/query/code_changes"
      ? { changes: [change("web", 42)], position: POSITION, level: "session" }
      : undefined,
  );
  mount(<ChangeList params={{ work: "native:item-1" }} empty={{ title: "None", hint: "" }} />);

  const grid = await screen.findByRole("table", { name: "Changes" });
  expect(within(grid).getByText("web!42")).toBeTruthy();
  const ask = codeAsks(asked, "code_changes")[0]!;
  expect(ask.searchParams.get("work")).toBe("native:item-1");
  expect(ask.searchParams.has("state")).toBe(false);
});

// A REVIEW ROW NAMES THE CHANGE IT JUDGED AND OPENS ITS PAGE, the verdict in
// words. Mutation: drop the change's reference, or link the row anywhere but
// the change's page, and this fails.
test("a reviewer's reviews each name the change they judged and open its page", async () => {
  const asked = stubFetch((url) =>
    url.pathname === "/query/code_reviews"
      ? {
          reviews: [
            {
              id: "rv1",
              revision: 3,
              reviewer: "cto",
              reviewer_kind: "agent",
              verdict: "approve",
              findings: 2,
              position: POSITION,
              at: "2026-10-30T10:00:00Z",
              change: { ref: "web!42", title: "Login form", repo: "web", state: "open" },
            },
          ],
          position: POSITION,
          level: "session",
        }
      : undefined,
  );
  mount(<ReviewList reviewer="cto" empty={{ title: "None", hint: "" }} />);

  const grid = await screen.findByRole("table", { name: "Reviews" });
  expect(within(grid).getByText("web!42")).toBeTruthy();
  expect(within(grid).getByText("Login form")).toBeTruthy();
  const hrefs = within(grid)
    .getAllByRole("link")
    .map((a) => a.getAttribute("href"));
  expect(hrefs).toContain("#/code/repos/web/changes/42");
  expect(codeAsks(asked, "code_reviews")[0]!.searchParams.get("reviewer")).toBe("cto");
});

// HOME'S CARD ASKS THE READER'S OWN REVIEW ONLY OF A READER BOUND TO A SEAT,
// and what waits on any person of everybody. Mutation: ask needs=review of a
// reader bound to no seat (which the engine refuses), or stop asking
// needs=person, and this fails.
test("Home's card asks a seated reader's reviews and what waits on a person", async () => {
  const asked = stubFetch((url) =>
    url.pathname === "/query/code_changes"
      ? url.searchParams.get("needs") === "review"
        ? { changes: [change("web", 42)], position: POSITION, level: "session" }
        : { changes: [change("api", 7)], position: POSITION, level: "session" }
      : undefined,
  );
  mount(<CodeWaiting />);

  expect(await screen.findByText("web!42")).toBeTruthy();
  expect(await screen.findByText("api!7")).toBeTruthy();
  const needs = codeAsks(asked, "code_changes").map((u) => u.searchParams.get("needs"));
  expect(needs).toContain("review");
  expect(needs).toContain("person");
  for (const u of codeAsks(asked, "code_changes")) {
    expect(u.searchParams.get("limit")).toBe("3");
    expect(u.searchParams.has("reviewer")).toBe(false);
  }
});

test("Home's card asks a reader bound to no seat no review of its own, and says why", async () => {
  const asked = stubFetch((url) =>
    url.pathname === "/query/code_changes"
      ? { changes: [change("api", 7)], position: POSITION, level: "session" }
      : undefined,
  );
  mount(<CodeWaiting />, {
    ...ADMIN_VIEWER,
    login: "token:ci",
    owner: "token:ci",
    handle: "",
    kind: "",
  });

  expect(await screen.findByText("api!7")).toBeTruthy();
  expect(screen.getByText(/A review is asked of a seat/)).toBeTruthy();
  const needs = codeAsks(asked, "code_changes").map((u) => u.searchParams.get("needs"));
  expect(needs).not.toContain("review");
});

const seat = (handle: string) => ({ handle, name: handle }) as unknown as Seat;

const ACTIVITY = {
  seats: [
    {
      handle: "dev",
      changes_opened: 4,
      reviews_given: 9,
      findings_received: 2,
    },
  ],
};

// A SEAT'S CODE TAB lists its open changes and the reviews it gave, and for
// an AGENT the three counters its usage days carry; a person's openings and
// reviews count on no seat's day, so a human seat's tab asks none. Mutation:
// ask the owner's changes in no state (refused), draw the counters for a
// person, or read them from anything but `seat_activity`, and this fails.
test("an agent seat's Code tab draws its open changes, its reviews and its code counters", async () => {
  const asked = stubFetch((url) =>
    url.pathname === "/query/code_changes"
      ? { changes: [change("web", 42)], position: POSITION, level: "session" }
      : url.pathname === "/query/code_reviews"
        ? { position: POSITION, level: "session" }
        : undefined,
  );
  const { onSocket } = mount(<SeatCode seat={seat("dev")} human={false} />, ADMIN_VIEWER, {
    seat_activity: ACTIVITY,
  });

  expect(await screen.findByText("web!42")).toBeTruthy();
  await waitFor(() => expect(screen.getAllByText("9").length).toBeGreaterThan(0));
  expect(screen.getAllByText("4").length).toBeGreaterThan(0);
  expect(screen.getByText("Findings received")).toBeTruthy();
  const owned = codeAsks(asked, "code_changes")[0]!;
  expect(owned.searchParams.get("owner")).toBe("dev");
  expect(owned.searchParams.get("state")).toBe("open");
  expect(codeAsks(asked, "code_reviews")[0]!.searchParams.get("reviewer")).toBe("dev");
  expect(onSocket.some((q) => q.what === "seat_activity")).toBe(true);
});

test("a person's Code tab lists their changes and reviews and draws no counters", async () => {
  stubFetch((url) =>
    url.pathname.startsWith("/query/code_") ? { position: POSITION, level: "session" } : undefined,
  );
  const { onSocket } = mount(<SeatCode seat={seat("jane")} human />, ADMIN_VIEWER, {
    seat_activity: ACTIVITY,
  });

  expect(await screen.findByText("No open change")).toBeTruthy();
  expect(screen.getByRole("heading", { name: "Reviews given" })).toBeTruthy();
  expect(screen.queryByText("Findings received")).toBeNull();
  expect(onSocket.some((q) => q.what === "seat_activity")).toBe(false);
});
