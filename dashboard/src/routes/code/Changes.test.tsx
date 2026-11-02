/**
 * Code › Changes reads the engine's listing of changes over REST, narrowed by
 * the engine's own filters, and draws each change with the readiness the
 * engine judged on that read.
 *
 * The invariants worth breaking a build over: the listing is asked over REST
 * with a state it can walk, and one it cannot is said in place rather than
 * sent; what a change waits on is the engine's `needs`; the "Needs a person"
 * band is the engine's `needs=person` and nothing where none waits; a row's
 * readiness and blockers are the engine's judgement, never a count of rows;
 * and a row opens its change in the peek, by its reference.
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
import { Changes, changesParams } from "./Changes.tsx";

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
  units: [{ id: "engineering", name: "Engineering", roles: [] }],
  services: { tracker: "worklet", knowledge: "doclet", chat: "chatlet", code: "native" },
}) as unknown as OrgProjection;

/** A readiness of `met` of five, its first unmet precondition the approval. */
function readiness(met: number) {
  const kinds = ["draft", "facts", "approval", "blocker", "mergeable"];
  return {
    met,
    total: kinds.length,
    first: met === kinds.length ? "Ready to merge." : "Waits on an approval from a reviewer.",
    preconditions: kinds.map((kind, i) => ({
      kind,
      state: i < met ? "met" : "unmet",
      detail: `${kind} detail`,
      ...(kind === "blocker" && i >= met
        ? {
            blockers: [
              {
                finding_id: "f1",
                state: "open",
                cleared_by: { raiser: { handle: "cto", kind: "agent" } },
              },
            ],
            blockers_omitted: 2,
          }
        : {}),
    })),
  };
}

/** A change as the listing answers it, with what a case overrides. */
function change(repo: string, number: number, extra: Record<string, unknown> = {}) {
  return {
    id: `id-${repo}-${number}`,
    repo_id: `repo-${repo}`,
    number,
    title: `Change ${number} of ${repo}`,
    state: "open",
    source_ref: "refs/heads/feature",
    target_ref: "refs/heads/main",
    head: "0123456789abcdef0123456789abcdef01234567",
    revision: 6,
    owner: "dev",
    owner_kind: "agent",
    opened_at: "2026-10-27T10:00:00Z",
    opened_position: POSITION,
    version: 4,
    position: POSITION,
    repo,
    ref: `${repo}!${number}`,
    access: "maintain",
    readiness: readiness(2),
    ...extra,
  };
}

const json = (payload: unknown, status = 200) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });

function stubFetch(answer: (url: URL) => Response | null) {
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = new URL(String(input));
      asked.push(url);
      if (url.pathname === "/auth/session") return new Promise<Response>(() => {});
      if (url.pathname === "/query/code_repos") {
        return Promise.resolve(json({ position: POSITION, level: "stale" }));
      }
      return Promise.resolve(answer(url) ?? json({}, 404));
    }),
  );
  return asked;
}

function mount(ui: ReactElement, hash = "#/code") {
  location.hash = hash;
  const store = new Store();
  store.applyOrg(ORG);
  const socket = new LiveSocket(store);
  const onSocket: string[] = [];
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    onSocket.push(what);
    return what === "viewer" ? Promise.resolve(ADMIN_VIEWER) : new Promise(() => {});
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

/** The listing's asks — the grid's, not the band's (`needs=person`, limited). */
const listings = (asked: URL[]) =>
  asked.filter((u) => u.pathname === "/query/code_changes" && !u.searchParams.has("limit"));

// THE ENGINE'S LISTING, OVER REST, asking the OPEN changes until told
// otherwise — a listing names a state, a repository or a work item, and the
// engine refuses one naming none — and each row opens its change in the
// peek, by its reference. Mutation: ask with no state, or link a row
// anywhere but its peek, and this fails.
test("the changes are the engine's listing over REST, open by default, each row peeking its change", async () => {
  const asked = stubFetch((url) =>
    url.pathname === "/query/code_changes" && !url.searchParams.has("limit")
      ? json({
          changes: [change("web", 42), change("api", 7)],
          position: POSITION,
          level: "session",
        })
      : url.pathname === "/query/code_changes"
        ? json({ position: POSITION, level: "session" })
        : null,
  );
  const { onSocket } = mount(<Changes />);

  const grid = await screen.findByRole("table", { name: "Changes" });
  expect(within(grid).getByText("web!42")).toBeTruthy();
  expect(within(grid).getByText("Change 7 of api")).toBeTruthy();
  const links = within(grid)
    .getAllByRole("link")
    .map((a) => a.getAttribute("href"));
  expect(links).toContain("#/code/repos/web/changes/42");
  const listing = listings(asked)[0]!;
  expect(listing.searchParams.get("state")).toBe("open");
  expect(onSocket).not.toContain("code_changes");
});

// A ROW'S READINESS AND BLOCKERS ARE THE ENGINE'S JUDGEMENT: the Meter counts
// the preconditions met of the readiness it was sent, and the blockers are
// the blocker precondition's named and counted beyond — and a change that is
// no longer live has neither. Mutation: count blockers from anything but the
// readiness, and the 3 is not there.
test("a row's readiness and blockers are the engine's, and a closed change has neither", async () => {
  stubFetch((url) =>
    url.pathname === "/query/code_changes" && !url.searchParams.has("limit")
      ? json({
          changes: [change("web", 42), change("web", 9, { state: "merged", readiness: undefined })],
          position: POSITION,
          level: "session",
        })
      : url.pathname === "/query/code_changes"
        ? json({ position: POSITION, level: "session" })
        : null,
  );
  mount(<Changes />);

  const grid = await screen.findByRole("table", { name: "Changes" });
  expect(within(grid).getByText("Merged")).toBeTruthy();
  const meter = within(grid).getByRole("meter");
  expect(meter.getAttribute("aria-valuenow")).toBe("2");
  expect(meter.getAttribute("aria-valuemax")).toBe("5");
  expect(within(grid).getByText("3")).toBeTruthy();
  expect(within(grid).getAllByText("Not live").length).toBeGreaterThanOrEqual(2);
});

// WHAT A CHANGE WAITS ON IS THE ENGINE'S `needs`, sent as it reads it.
test("the waiting-on filter is the engine's needs", async () => {
  const asked = stubFetch((url) =>
    url.pathname === "/query/code_changes" ? json({ position: POSITION, level: "session" }) : null,
  );
  mount(<Changes />, "#/code?needs=me");

  expect(await screen.findByText("Nothing waits on you")).toBeTruthy();
  expect(listings(asked)[0]!.searchParams.get("needs")).toBe("me");
});

// EVERY STATE WHERE NOTHING NARROWS THE WALK IS SAID IN PLACE, NEVER SENT:
// the engine keeps no index of every change, and would refuse it. Mutation:
// send it, and the listing is asked with no state.
test("every state with nothing narrowing the walk is said in place and never asked", async () => {
  const asked = stubFetch(() => json({ position: POSITION, level: "session" }));
  mount(<Changes />, "#/code?state=all");

  expect(await screen.findByText(/Every state is listed for one repository/)).toBeTruthy();
  await waitFor(() => expect(asked.some((u) => u.pathname === "/query/code_repos")).toBe(true));
  expect(listings(asked)).toHaveLength(0);
});

// THE BAND IS THE ENGINE'S `needs=person`: the changes waiting on a person,
// above the grid — and nothing at all where none does. Mutation: draw the
// band from the grid's rows, or draw it empty, and this fails.
test("the Needs a person band lists the engine's needs=person, and is absent where none waits", async () => {
  let waiting = [change("web", 5, { title: "Ship the login page" })];
  const asked = stubFetch((url) => {
    if (url.pathname !== "/query/code_changes") return null;
    if (url.searchParams.get("needs") === "person" && url.searchParams.has("limit")) {
      return json({ changes: waiting, position: POSITION, level: "session" });
    }
    return json({ changes: [change("web", 42)], position: POSITION, level: "session" });
  });
  mount(<Changes />);

  const band = await screen.findByRole("region", { name: "Needs a person" });
  expect(within(band).getByText("web!5")).toBeTruthy();
  expect(within(band).getByText(/Ship the login page/)).toBeTruthy();
  const ask = asked.find((u) => u.searchParams.get("needs") === "person")!;
  expect(ask.searchParams.get("limit")).toBe("5");

  cleanup();
  waiting = [];
  mount(<Changes />);
  await screen.findByRole("table", { name: "Changes" });
  expect(screen.queryByRole("region", { name: "Needs a person" })).toBeNull();
});

// THE PARAMETERS ARE THE ENGINE'S FILTERS, and what cannot be asked says why.
test("a listing's parameters are the engine's filters, and a name no repository has is said", () => {
  const none = { state: "open", needs: "", repo: "", owner: "", reviewer: "", work: "" };
  expect(changesParams(none)).toEqual({ params: { state: "open" } });
  expect(changesParams({ ...none, state: "all", repo: "web" })).toEqual({
    params: { repo: "web" },
  });
  expect(changesParams({ ...none, needs: "person", owner: "dev" })).toEqual({
    params: { state: "open", needs: "person", owner: "dev" },
  });
  expect(changesParams({ ...none, repo: "Web" })).toHaveProperty("unasked");
  expect(changesParams({ ...none, needs: "soon" })).toHaveProperty("unasked");
  expect(changesParams({ ...none, state: "all" })).toHaveProperty("unasked");
});
