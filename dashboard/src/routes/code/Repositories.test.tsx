/**
 * Code › Repositories reads the engine's listing over REST, a page at a time,
 * and draws each repository as the listing knows it.
 *
 * The invariants worth breaking a build over: the listing is asked over REST
 * and never on the socket's slots; a page that holds no row but a cursor is
 * not the end of the list; the state filter is the engine's own `status`; and
 * an empty company is told where a repository is declared.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactElement } from "react";
import { afterEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { Repositories } from "./Repositories.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
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
    kind: "project",
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
  return { onSocket, store };
}

/**
 * The engine's listing over `declared()`, by name, two to a page: a page's
 * cursor is the last name it scanned, as `code_repos` mints one, so a page
 * asked after a repository lands continues from that name.
 */
function byName(declared: () => readonly string[]) {
  return (url: URL) => {
    if (url.pathname !== "/query/code_repos") return null;
    const after = url.searchParams.get("cursor") ?? "";
    const names = declared().filter((name) => name > after);
    const page = names.slice(0, 2);
    return json({
      repos: page.map((name) => repo(name)),
      ...(names.length > page.length ? { next: page.at(-1) } : {}),
      position: POSITION,
      level: "stale",
    });
  };
}

/** The listing's first pages asked: no cursor named. */
const firstPages = (asked: URL[]) =>
  asked.filter((u) => u.pathname === "/query/code_repos" && !u.searchParams.has("cursor"));

/** The names the grid lists, in its order, each once. */
const listed = () => [
  ...new Set(
    within(screen.getByRole("table", { name: "Repositories" }))
      .getAllByRole("link")
      .map((a) => a.getAttribute("href")?.split("repo=")[1]),
  ),
];

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

// A PAGE THAT FAILS KEEPS LOAD MORE, which asks it again, the failure said
// beside it rather than as a banner promising a retry nothing makes.
// Mutation: answer the failure with QueryState's banner again, and the
// caption is not there.
test("a page that fails keeps Load more, which asks it again", async () => {
  let failures = 1;
  stubFetch((url) => {
    if (url.pathname !== "/query/code_repos") return null;
    if (url.searchParams.get("cursor") !== "m") {
      return json({ repos: [repo("alpha")], next: "m", position: POSITION, level: "stale" });
    }
    return failures-- > 0
      ? json({ error: "query_failed" }, 500)
      : json({ repos: [repo("zeta")], position: POSITION, level: "stale" });
  });
  mount(<Repositories />);
  fireEvent.click(await screen.findByRole("button", { name: /load more/i }));
  expect(
    await screen.findByText("Could not read the next page (query_failed). Try again."),
  ).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: /load more/i }));
  expect(await screen.findByText("zeta")).toBeTruthy();
  expect(screen.queryByText(/Could not read the next page/)).toBeNull();
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

// THE FIRST PAGE IS HELD from the press of Load more. Its cursor is the last
// name it scanned, and read again under a repository that entered its range —
// declared by an activation, restored, or moved into the filtered state — it
// pushes that name out of both pages. The case found in review: a1 and a2,
// then a3; a0 is declared, and a minute on a2 was gone until a reload.
// Mutation: ask the listing without the pager's hold, and a2 is not listed.
test("a repository declared after Load more pushes none out of the listing", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  let declared = ["a1", "a2", "a3"];
  const asked = stubFetch(byName(() => declared));
  const { store } = mount(<Repositories />);
  fireEvent.click(await screen.findByRole("button", { name: /load more/i }));
  expect(await screen.findByText("a3")).toBeTruthy();
  expect(listed()).toEqual(["a1", "a2", "a3"]);

  declared = ["a0", "a1", "a2", "a3"];
  await act(async () => void vi.advanceTimersByTime(3 * 60_000));
  act(() => store.setConnected(true));
  act(() => store.setConnected(false));
  act(() => store.setConnected(true));
  await afterMicrotasks();
  expect(firstPages(asked)).toHaveLength(1);
  expect(listed()).toEqual(["a1", "a2", "a3"]);
});

// A FILTER LEFT AND COME BACK TO starts the listing again: what the first
// reading held is discarded, never hidden under its question, where it came
// back under a first page read since. Mutation: hide the held pages by
// question instead, and the old a3 is drawn under a fresh first page.
test("a filter left and come back to lists from its first page, nothing held", async () => {
  let declared = ["a1", "a2", "a3"];
  const asked = stubFetch(byName(() => declared));
  mount(<Repositories />);
  fireEvent.click(await screen.findByRole("button", { name: /load more/i }));
  expect(await screen.findByText("a3")).toBeTruthy();

  act(() => void (location.hash = "#/code/repos?status=archived"));
  await waitFor(() =>
    expect(asked.some((u) => u.searchParams.get("status") === "archived")).toBe(true),
  );
  declared = ["a0", "a1", "a2", "a3"];
  act(() => void (location.hash = "#/code/repos"));
  expect(await screen.findByText("a0")).toBeTruthy();
  await afterMicrotasks();
  expect(listed()).toEqual(["a0", "a1"]);
  expect(screen.getByRole("button", { name: /load more/i })).toBeTruthy();
});
