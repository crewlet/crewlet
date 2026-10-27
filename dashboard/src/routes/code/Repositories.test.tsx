/**
 * Code › Repositories reads the engine's listing over REST, a page at a time,
 * and draws each repository as the listing knows it.
 *
 * The invariants worth breaking a build over: the listing is asked over REST
 * and never on the socket's slots; a page that holds no row but a cursor is
 * not the end of the list; the state filter is the engine's own `status`; and
 * an empty company is told where a repository is declared.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactElement } from "react";
import { afterEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { Repositories } from "./Repositories.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };

const ORG = withDerived({
  name: "Acme",
  roles: [{ name: "CTO", handle: "cto" }],
  units: [{ id: "engineering", name: "Engineering", roles: [] }],
  services: { tracker: "native", knowledge: "native", code: "native" },
}) as unknown as OrgProjection;

/** A repository as the listing answers it, with what a case overrides. */
function repo(name: string, extra: Record<string, unknown> = {}) {
  return {
    id: `id-${name}`,
    name,
    status: "active",
    managed_by: "config",
    owner: { kind: "unit", ref: "engineering" },
    visibility: "company",
    default_branch: "main",
    refs: 3,
    live_packs: 2,
    live_bytes: 2048,
    live_objects: 40,
    position: POSITION,
    access: "maintain",
    head: "0123456789abcdef0123456789abcdef01234567",
    pushes: 5,
    last_push_at: "2026-10-27T10:00:00Z",
    ...extra,
  };
}

const json = (payload: unknown, status = 200) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });

/**
 * The engine's REST answers, by path, and every request made. The frame's own
 * read of who is signed in is left unanswered: the viewer answers on the
 * socket, which is the reading the locks are drawn by.
 */
function stubFetch(answer: (url: URL) => Response | null) {
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = new URL(String(input));
      asked.push(url);
      if (url.pathname === "/auth/session") return new Promise<Response>(() => {});
      return Promise.resolve(answer(url) ?? json({}, 404));
    }),
  );
  return asked;
}

/** The screen in the frame's readings, at `hash`; the socket's questions recorded. */
function mount(ui: ReactElement, hash = "#/code/repos") {
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

// THE ENGINE'S LISTING, OVER REST, never one of the socket's slots — and each
// row says what the listing knows and opens the repository's pushes.
// Mutation: ask `code_repos` on the socket, or link a row anywhere else, and
// this fails.
test("the repositories are the engine's listing over REST, each row opening its pushes", async () => {
  const asked = stubFetch((url) =>
    url.pathname === "/query/code_repos"
      ? json({
          repos: [
            repo("api", {
              status: "archived",
              owner: { kind: "seat", ref: "cto" },
              visibility: "unit",
              head: undefined,
              last_push_at: undefined,
              protections: [{ pattern: "refs/heads/main", direct_push: "persons" }],
            }),
            repo("web"),
          ],
          position: POSITION,
          level: "stale",
        })
      : null,
  );
  const { onSocket } = mount(<Repositories />);

  const grid = await screen.findByRole("table", { name: "Repositories" });
  const rows = within(grid).getAllByRole("row");
  expect(within(grid).getByText("web")).toBeTruthy();
  expect(within(grid).getByText("api")).toBeTruthy();
  // WHAT A ROW KNOWS: the state beyond active, the owner by name — a unit's
  // from the chart, a seat's badge — which seats read it, the head, the rules.
  expect(within(grid).getByText("Archived")).toBeTruthy();
  expect(within(grid).getByText("Engineering")).toBeTruthy();
  expect(within(grid).getByText("CTO")).toBeTruthy();
  expect(within(grid).getByText("Its owner's seats")).toBeTruthy();
  expect(within(grid).getByText("0123456")).toBeTruthy();
  expect(within(grid).getByText("empty")).toBeTruthy();
  expect(within(grid).getByText("1 rule")).toBeTruthy();
  expect(rows.length).toBeGreaterThanOrEqual(2);
  const links = within(grid)
    .getAllByRole("link")
    .map((a) => a.getAttribute("href"));
  expect(links).toContain("#/code/pushes?repo=web");
  expect(links).toContain("#/code/pushes?repo=api");

  expect(asked.some((u) => u.pathname === "/query/code_repos")).toBe(true);
  expect(onSocket).not.toContain("code_repos");
});

// A PAGE IS WHAT THE ENGINE SCANNED: one that holds no row the reader may
// read can still carry a cursor, and that is not the end of the listing.
// Mutation: read an empty page as the end, and "No repositories yet" is drawn
// over a company that has some.
test("a page holding no row but a cursor offers the next, asked with that cursor", async () => {
  const asked = stubFetch((url) => {
    if (url.pathname !== "/query/code_repos") return null;
    return url.searchParams.get("cursor") === "m"
      ? json({ repos: [repo("zeta")], position: POSITION, level: "stale" })
      : json({ next: "m", position: POSITION, level: "stale" });
  });
  mount(<Repositories />);

  const more = await screen.findByRole("button", { name: /load more/i });
  expect(screen.queryByText("No repositories yet")).toBeNull();
  fireEvent.click(more);
  expect(await screen.findByText("zeta")).toBeTruthy();
  expect(asked.some((u) => u.searchParams.get("cursor") === "m")).toBe(true);
});

// THE ENGINE'S OWN FILTER: `status=` is asked as the engine reads it, and an
// empty answer under it says which state was empty.
test("the state filter is the engine's status, said when nothing is in that state", async () => {
  const asked = stubFetch((url) =>
    url.pathname === "/query/code_repos" ? json({ position: POSITION, level: "stale" }) : null,
  );
  mount(<Repositories />, "#/code/repos?status=deleted");

  expect(await screen.findByText("No deleted repositories")).toBeTruthy();
  const listing = asked.find((u) => u.pathname === "/query/code_repos")!;
  expect(listing.searchParams.get("status")).toBe("deleted");
});

// AN EMPTY COMPANY IS TOLD WHERE A REPOSITORY IS DECLARED, and offered the
// configuration where the reader may open it.
test("a company with no repository is told where one is declared", async () => {
  stubFetch((url) =>
    url.pathname === "/query/code_repos" ? json({ position: POSITION, level: "stale" }) : null,
  );
  mount(<Repositories />);

  expect(await screen.findByText("No repositories yet")).toBeTruthy();
  expect(screen.getByText(/code\.repos/)).toBeTruthy();
  const open = await screen.findByRole("link", { name: "Open the configuration" });
  expect(open.getAttribute("href")).toBe("#/settings/config");
  await waitFor(() => expect(screen.queryByRole("table")).toBeNull());
});
