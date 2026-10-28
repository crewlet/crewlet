/**
 * Code › Pushes reads one repository's history over REST and says what each
 * push did, as the engine recorded it.
 *
 * The invariants worth breaking a build over: no history is asked until a
 * repository is named, and never for a name no repository can have; a row says
 * who pushed and what became of each ref, a refusal in the danger tone; a
 * repository nobody declared is said with the engine's own sentence; and the
 * log reaches its older pushes by fetching them, holding its first page still
 * while it does.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { Pushes } from "./Pushes.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  location.hash = "";
});

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 30 };
const ZERO = "0".repeat(40);
const A = "a".repeat(40);
const B = "b".repeat(40);
const C = "c".repeat(40);

const ORG = withDerived({
  name: "Acme",
  roles: [
    { name: "CTO", handle: "cto" },
    { name: "Jane Founder", handle: "jane", kind: "human" },
  ],
  services: { tracker: "native", knowledge: "native", code: "native" },
}) as unknown as OrgProjection;

/** A push as the history answers it, with what a case overrides. */
function push(id: string, extra: Record<string, unknown> = {}) {
  return {
    push_id: id,
    repo_id: "id-web",
    position: POSITION,
    at: "2026-10-27T10:00:00Z",
    actor: "cto",
    actor_kind: "agent",
    actor_seat: "cto",
    via: "git",
    commands: [{ ref: "refs/heads/main", old: A, new: B, result: "ok" }],
    received_at: "2026-10-27T10:00:00Z",
    ...extra,
  };
}

const json = (payload: unknown, status = 200) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });

const LISTING = {
  repos: [{ name: "web" }, { name: "api" }],
  position: POSITION,
  level: "stale",
};

/** The engine's REST answers, by path; every request made. */
function stubFetch(history: (url: URL) => Response) {
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = new URL(String(input));
      asked.push(url);
      if (url.pathname === "/auth/session") return new Promise<Response>(() => {});
      if (url.pathname === "/query/code_repos") return Promise.resolve(json(LISTING));
      if (url.pathname === "/query/code_pushes") return Promise.resolve(history(url));
      return Promise.resolve(json({}, 404));
    }),
  );
  return asked;
}

const histories = (asked: URL[]) => asked.filter((u) => u.pathname === "/query/code_pushes");

function mount(hash: string) {
  location.hash = hash;
  const store = new Store();
  store.applyOrg(ORG);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" ? Promise.resolve(ADMIN_VIEWER) : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <Pushes />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
  return { store };
}

/** The tab's socket comes back after a drop, as the loader hears it. */
function reconnect(store: Store) {
  act(() => store.setConnected(true));
  act(() => store.setConnected(false));
  act(() => store.setConnected(true));
}

/** A newest page of the history: no cursor named. */
const newest = (asked: URL[]) => histories(asked).filter((u) => !u.searchParams.has("cursor"));

// NO HISTORY UNTIL A REPOSITORY IS NAMED: the engine keeps one per
// repository, so the section asks for one and offers the repositories.
test("without a repository the section asks for one, and asks no history", async () => {
  const asked = stubFetch(() => json({ position: POSITION, level: "stale" }));
  mount("#/code/pushes");
  expect(await screen.findByRole("heading", { name: "Choose a repository" })).toBeTruthy();
  await afterMicrotasks();
  expect(histories(asked)).toEqual([]);
  expect(asked.some((u) => u.pathname === "/query/code_repos")).toBe(true);
});

// A NAME NO REPOSITORY CAN HAVE is said in place, by the engine's grammar,
// and never sent. Mutation: drop the check, and the history is asked.
test("a name no repository can have is said in place and never asked", async () => {
  const asked = stubFetch(() => json({ position: POSITION, level: "stale" }));
  mount("#/code/pushes?repo=Web_App");
  expect(await screen.findByText(/is not a repository’s name/)).toBeTruthy();
  await afterMicrotasks();
  expect(histories(asked)).toEqual([]);
});

// WHAT EACH PUSH DID: who — a seat, or the credential a person pushed
// through and the seat it acts as — what became of each ref, the commits it
// brought, and the turn behind a seat's push.
test("a repository's history says who pushed and what became of each ref", async () => {
  const asked = stubFetch(() =>
    json({
      pushes: [
        push("p3", {
          commands: [
            {
              ref: "refs/heads/main",
              old: B,
              new: C,
              result: "ng",
              reason: "non_fast_forward",
            },
          ],
        }),
        push("p2", {
          actor: "pat:abc",
          actor_kind: "operator",
          actor_seat: "jane",
          commands: [{ ref: "refs/tags/v1", old: ZERO, new: C, result: "ok" }],
        }),
        push("p1", {
          run: { turn_id: "turn-1", work_backend: "native", work_key: "ENG-7", model: "m" },
          introduced_count: 3,
        }),
      ],
      position: POSITION,
      level: "stale",
    }),
  );
  mount("#/code/pushes?repo=web");

  const grid = await screen.findByRole("table", { name: "Pushes to web" });
  expect(histories(asked)[0]!.searchParams.get("name")).toBe("web");
  // A SEAT'S PUSH: the ref moved, and the turn and work item behind it.
  // The ids short, as git abbreviates them, and whole on the title.
  expect(within(grid).getByText("aaaaaaa..bbbbbbb").getAttribute("title")).toBe(`${A}..${B}`);
  expect(within(grid).getByText("moved")).toBeTruthy();
  expect(within(grid).getByText("ENG-7")).toBeTruthy();
  expect(within(grid).getByRole("link", { name: "turn →" }).getAttribute("href")).toBe(
    "#/live/turns/turn-1",
  );
  // A PERSON'S PUSH, through their token: a tag created.
  expect(within(grid).getByText("tag v1")).toBeTruthy();
  expect(within(grid).getByText("created")).toBeTruthy();
  expect(within(grid).getByText("Jane Founder")).toBeTruthy();
  // A REFUSED COMMAND: the engine's reason, as a failure is marked.
  expect(within(grid).getByText("refused: non_fast_forward")).toBeTruthy();
});

// A REPOSITORY NOBODY DECLARED is said with the engine's own sentence, which
// names the setting that declares one.
test("a repository nobody declared is said with the engine's sentence", async () => {
  stubFetch(() =>
    json(
      {
        error: "not_found",
        reason: "not_declared",
        detail: 'no repository named "gone" is declared in code.repos',
      },
      404,
    ),
  );
  mount("#/code/pushes?repo=gone");
  expect(await screen.findByText("No repository named “gone”")).toBeTruthy();
  expect(screen.getByText('no repository named "gone" is declared in code.repos')).toBeTruthy();
});

// A LOG REACHES ITS OLDER ROWS BY FETCHING THEM, and while it holds an older
// page its first page is held still: re-read under newer pushes, the pushes
// between the two pages would belong to neither — so neither a poll tick nor
// a reconnect asks it. Mutation: ask the history without the pager's hold, and
// the newest page is asked again below.
test("Load older asks the next page by its cursor, and the first page is then held still", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  const asked = stubFetch((url) =>
    url.searchParams.get("cursor") === "c1"
      ? json({ pushes: [push("p1")], position: POSITION, level: "stale" })
      : json({ pushes: [push("p2")], next: "c1", position: POSITION, level: "stale" }),
  );
  const { store } = mount("#/code/pushes?repo=web");
  await screen.findByRole("table", { name: "Pushes to web" });
  // POLLED WHILE NOTHING OLDER IS HELD: a minute on, the newest page again.
  await act(async () => void vi.advanceTimersByTime(60_000));
  await afterMicrotasks();
  expect(histories(asked)).toHaveLength(2);

  fireEvent.click(screen.getByRole("button", { name: "Load older" }));
  await screen.findByText("That is every push the repository keeps.");
  expect(histories(asked).at(-1)!.searchParams.get("cursor")).toBe("c1");
  expect(screen.getByText("Paused while you page back.")).toBeTruthy();
  const before = histories(asked).length;
  await act(async () => void vi.advanceTimersByTime(5 * 60_000));
  reconnect(store);
  await afterMicrotasks();
  expect(histories(asked)).toHaveLength(before);
});

// HELD FROM THE PRESS, not from the answer: a tick landing while the older
// page is on its way would read the first page again under the cursor that
// page is asked from. Mutation: hold only once an older page is held, and the
// tick asks the newest page.
test("a poll tick while the older page is on its way asks nothing of the first", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  const asked = stubFetch((url) =>
    url.searchParams.get("cursor") === "c1"
      ? (new Promise<Response>(() => {}) as unknown as Response)
      : json({ pushes: [push("p2")], next: "c1", position: POSITION, level: "stale" }),
  );
  mount("#/code/pushes?repo=web");
  await screen.findByRole("table", { name: "Pushes to web" });
  fireEvent.click(screen.getByRole("button", { name: "Load older" }));
  await afterMicrotasks();
  expect(histories(asked).at(-1)!.searchParams.get("cursor")).toBe("c1");
  await act(async () => void vi.advanceTimersByTime(3 * 60_000));
  await afterMicrotasks();
  expect(newest(asked)).toHaveLength(1);
});

// ANOTHER REPOSITORY AND BACK starts the history again: what was held for the
// first is discarded, never hidden under its name, where it came back under a
// first page read since and the pushes between the two vanished. Mutation:
// keep the pages and hide them by repository, and p1 is drawn again.
test("leaving a repository's history and coming back starts it from its newest page", async () => {
  let landed = 4;
  const asked = stubFetch((url) => {
    if (url.searchParams.get("name") === "api") {
      return json({ pushes: [push("q1")], position: POSITION, level: "stale" });
    }
    if (url.searchParams.get("cursor") === "c3") {
      return json({ pushes: [push("p2"), push("p1")], position: POSITION, level: "stale" });
    }
    const top = Array.from({ length: 2 }, (_, i) => push(`p${landed - i}`));
    return json({ pushes: top, next: `c${landed - 1}`, position: POSITION, level: "stale" });
  });
  mount("#/code/pushes?repo=web");
  await screen.findByRole("table", { name: "Pushes to web" });
  fireEvent.click(screen.getByRole("button", { name: "Load older" }));
  await screen.findByText("That is every push the repository keeps.");
  expect(screen.getByText("4 pushes listed")).toBeTruthy();

  act(() => void (location.hash = "#/code/pushes?repo=api"));
  await screen.findByRole("table", { name: "Pushes to api" });
  landed = 6;
  act(() => void (location.hash = "#/code/pushes?repo=web"));
  await screen.findByRole("table", { name: "Pushes to web" });
  await afterMicrotasks();
  expect(screen.getByText("2 pushes listed")).toBeTruthy();
  expect(screen.queryByText("Paused while you page back.")).toBeNull();
  expect(screen.getByRole("button", { name: "Load older" })).toBeTruthy();
  expect(histories(asked).filter((u) => u.searchParams.get("cursor") === "c3")).toHaveLength(1);
});
