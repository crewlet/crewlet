/**
 * Code › Pushes reads one repository's history over REST and says what each
 * push did, as the engine recorded it.
 *
 * The invariants worth breaking a build over: no history is asked until a
 * repository is named, and never for a name no repository can have; a row says
 * who pushed and what became of its refs, a refusal in the danger tone; the
 * history is asked in brief, a push of many refs drawn five at most with the
 * rest counted, and read whole only when the reader asks for every ref; the
 * clock re-renders no ref; a repository nobody declared is said with the
 * engine's own sentence; and the log reaches its older pushes by fetching
 * them, holding its first page still while it does.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { commandWord } from "~/lib/code.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { Pushes } from "./Pushes.tsx";

// EVERY REF A ROW DRAWS, WATCHED: a pass-through, so every case draws the
// real words, and the clock's case can count which refs a tick drew again.
vi.mock("~/lib/code.ts", async (importOriginal) => {
  const real = await importOriginal<typeof import("~/lib/code.ts")>();
  return { ...real, commandWord: vi.fn(real.commandWord) };
});

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
  services: { tracker: "worklet", knowledge: "doclet", chat: "chatlet", code: "native" },
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

/** A push's id, as its receiving node mints one: a UUIDv7. */
const IMPORT = "019a2f4e-0000-7000-8000-000000000001";

/** A tag a push created. */
const tagged = (name: string) => ({ ref: `refs/tags/${name}`, old: ZERO, new: A, result: "ok" });

/** The tags t00, t01, … an import pushed, `n` of them. */
const tags = (n: number) =>
  Array.from({ length: n }, (_, i) => tagged(`t${String(i).padStart(2, "0")}`));

/** A history page holding one push. */
const page = (row: Record<string, unknown>, level = "stale") =>
  json({ pushes: [row], position: POSITION, level });

// THE HISTORY IS ASKED IN BRIEF, and a push of more refs than a row gives it
// is drawn five at most — its refusals first, as the engine cut it — with
// what became of the rest in one line, a refusal in the danger tone, and
// every ref one press away. Mutation: ask the history whole, and `brief` is
// not on the request; draw the counts in the engine's order, and the line
// reads refused last.
test("a push of many refs is drawn in brief: five, its refusals first, and the rest counted", async () => {
  const asked = stubFetch(() =>
    page(
      push(IMPORT, {
        commands: [
          { ref: "refs/heads/main", old: B, new: C, result: "ng", reason: "protected_force" },
          { ref: "refs/tags/v0", old: A, new: B, result: "ng", reason: "tag_moved" },
          ...tags(3),
        ],
        omitted: { created: 990, deleted: 3, refused: 2 },
        introduced_count: 40,
      }),
    ),
  );
  mount("#/code/pushes?repo=web");
  const grid = await screen.findByRole("table", { name: "Pushes to web" });
  expect(histories(asked)[0]!.searchParams.get("brief")).toBe("true");
  expect(within(grid).getByText("refused: protected_force")).toBeTruthy();
  expect(within(grid).getByText("refused: tag_moved")).toBeTruthy();
  expect(within(grid).getByText("tag t02")).toBeTruthy();
  expect(within(grid).getByText(/and 995 more:/).textContent).toBe(
    "and 995 more: 2 refused, 990 created, 3 deleted",
  );
  expect(within(grid).getByText("2 refused").closest(".crewlet-tag--danger")).toBeTruthy();
  expect(within(grid).getByRole("button", { name: "Show all 1,000 refs" })).toBeTruthy();
  expect(within(grid).getByText("40")).toBeTruthy();
});

// EVERY REF IS ONE READ OF THE PUSH AWAY, made when the reader asks: that
// push alone, whole, at its own position — a copy behind the page it came
// from waits for it — and read once, however often it is folded and opened.
// Mutation: ask the push without its position, or read it again on every
// opening, and the requests below differ.
test("Show all reads that one push whole at its own position, once, and Show fewer folds it", async () => {
  const asked = stubFetch((url) =>
    url.searchParams.has("push")
      ? page(push(IMPORT, { commands: tags(17), introduced: [C], introduced_count: 1 }), "session")
      : page(push(IMPORT, { commands: tags(5), omitted: { created: 12 } })),
  );
  mount("#/code/pushes?repo=web");
  const grid = await screen.findByRole("table", { name: "Pushes to web" });
  expect(within(grid).queryByText("tag t16")).toBeNull();

  fireEvent.click(within(grid).getByRole("button", { name: "Show all 17 refs" }));
  expect(await within(grid).findByText("tag t16")).toBeTruthy();
  const reads = () => histories(asked).filter((u) => u.searchParams.has("push"));
  expect(reads()).toHaveLength(1);
  expect(Object.fromEntries(reads()[0]!.searchParams)).toEqual({
    name: "web",
    push: IMPORT,
    read_level: "session",
    min_position: "CREWLET_CODE_LOG@1:30",
  });
  expect(within(grid).queryByText(/more:/)).toBeNull();

  const fewer = within(grid).getByRole("button", { name: "Show fewer" });
  expect(fewer.getAttribute("aria-expanded")).toBe("true");
  fireEvent.click(fewer);
  expect(within(grid).queryByText("tag t16")).toBeNull();
  expect(within(grid).getByText(/and 12 more:/)).toBeTruthy();
  fireEvent.click(within(grid).getByRole("button", { name: "Show all 17 refs" }));
  expect(within(grid).getByText("tag t16")).toBeTruthy();
  expect(reads()).toHaveLength(1);
});

// A PUSH THE HISTORY NO LONGER HOLDS — past its days, or its repository
// purged since the page was read — is said, never drawn as a push with no
// refs.
test("a push the history no longer holds says so", async () => {
  stubFetch((url) =>
    url.searchParams.has("push")
      ? json({ position: POSITION, level: "session" })
      : page(push(IMPORT, { commands: tags(5), omitted: { created: 4 } })),
  );
  mount("#/code/pushes?repo=web");
  const grid = await screen.findByRole("table", { name: "Pushes to web" });
  fireEvent.click(within(grid).getByRole("button", { name: "Show all 9 refs" }));
  expect(await within(grid).findByText("The history no longer holds this push.")).toBeTruthy();
  expect(within(grid).queryByRole("button", { name: /Show all/ })).toBeNull();
});

// A PUSH THAT COULD NOT BE READ is said beside Show all, which asks it again:
// it is the reader's to ask again, so nothing promises a retry on its own.
// Mutation: draw the failure in the control's place, and there is nothing to
// press.
test("a push that could not be read says so beside Show all, which asks it again", async () => {
  let failures = 1;
  const asked = stubFetch((url) => {
    if (!url.searchParams.has("push")) {
      return page(push(IMPORT, { commands: tags(5), omitted: { created: 4 } }));
    }
    return failures-- > 0
      ? json({ error: "query_failed" }, 500)
      : page(push(IMPORT, { commands: tags(9) }), "session");
  });
  mount("#/code/pushes?repo=web");
  const grid = await screen.findByRole("table", { name: "Pushes to web" });
  fireEvent.click(within(grid).getByRole("button", { name: "Show all 9 refs" }));
  expect(
    await within(grid).findByText("Could not read this push (query_failed). Try again."),
  ).toBeTruthy();
  fireEvent.click(within(grid).getByRole("button", { name: "Show all 9 refs" }));
  expect(await within(grid).findByText("tag t08")).toBeTruthy();
  expect(within(grid).queryByText(/Could not read this push/)).toBeNull();
  expect(histories(asked).filter((u) => u.searchParams.has("push"))).toHaveLength(2);
});

// THE CLOCK RE-RENDERS THE TIME A PUSH LANDED AND NO REF: the refs are the
// tallest thing a row draws, and a grid re-rendered every second drew every
// one of them again. Mutation: read the clock in the screen again and drop
// the refs' memo, and every tick draws every ref.
test("the clock's tick re-renders the time a push landed and no ref", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
  stubFetch(() =>
    json({
      pushes: [push("p2", { commands: tags(4) }), push("p1")],
      position: POSITION,
      level: "stale",
    }),
  );
  mount("#/code/pushes?repo=web");
  await screen.findByRole("table", { name: "Pushes to web" });
  await afterMicrotasks();
  const drawn = vi.mocked(commandWord).mock.calls.length;
  expect(drawn).toBeGreaterThan(0);
  await act(async () => void vi.advanceTimersByTime(3_000));
  expect(vi.mocked(commandWord).mock.calls.length).toBe(drawn);
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

// A LOG REACHES ITS OLDER ROWS BY FETCHING THEM, by the cursor its newest
// page carried, and the newest page stays polled while the reader pages back:
// another person's push reaches the open history by that read or not at all.
// Mutation: stop polling the newest page once an older one is loaded, and the
// tick asks nothing.
test("Load older asks the next page by its cursor, and the newest page stays polled", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  const asked = stubFetch((url) =>
    url.searchParams.get("cursor") === "c1"
      ? json({ pushes: [push("p1")], position: POSITION, level: "stale" })
      : json({ pushes: [push("p2")], next: "c1", position: POSITION, level: "stale" }),
  );
  mount("#/code/pushes?repo=web");
  await screen.findByRole("table", { name: "Pushes to web" });
  // POLLED: a minute on, the newest page again.
  await act(async () => void vi.advanceTimersByTime(60_000));
  await afterMicrotasks();
  expect(histories(asked)).toHaveLength(2);

  fireEvent.click(screen.getByRole("button", { name: "Load older" }));
  await screen.findByText("That is every push the repository keeps.");
  expect(histories(asked).at(-1)!.searchParams.get("cursor")).toBe("c1");
  // AN OLDER PAGE IS ASKED IN BRIEF as the first is.
  expect(histories(asked).at(-1)!.searchParams.get("brief")).toBe("true");
  const before = newest(asked).length;
  await act(async () => void vi.advanceTimersByTime(60_000));
  await afterMicrotasks();
  expect(newest(asked).length).toBeGreaterThan(before);
  expect(screen.getByText("2 pushes listed")).toBeTruthy();
});

// AN OLDER PAGE THAT FAILS LEAVES THE CONTROL THAT ASKS IT AGAIN, its failure
// said beside it: the pager keeps the cursor the page was asked from, and a
// banner in the button's place left a reader nothing to press, and nothing to
// do but reload. Mutation: draw the
// failure in the control's place again, and there is no Load older to press.
test("an older page that fails keeps Load older, which asks it again", async () => {
  let failures = 1;
  stubFetch((url) => {
    if (url.searchParams.get("cursor") !== "c1") {
      return json({ pushes: [push("p2")], next: "c1", position: POSITION, level: "stale" });
    }
    return failures-- > 0
      ? json({ error: "query_failed" }, 500)
      : json({ pushes: [push("p1")], position: POSITION, level: "stale" });
  });
  mount("#/code/pushes?repo=web");
  await screen.findByRole("table", { name: "Pushes to web" });
  fireEvent.click(screen.getByRole("button", { name: "Load older" }));
  expect(
    await screen.findByText("Could not read the next page (query_failed). Try again."),
  ).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Load older" }));
  await screen.findByText("That is every push the repository keeps.");
  expect(screen.queryByText(/Could not read the next page/)).toBeNull();
  expect(screen.getByText("2 pushes listed")).toBeTruthy();
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
  expect(screen.getByRole("button", { name: "Load older" })).toBeTruthy();
  expect(histories(asked).filter((u) => u.searchParams.get("cursor") === "c3")).toHaveLength(1);
});
