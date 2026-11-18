/**
 * Gitlet › Pushes reads one repository's history over REST as a feed of sentences on the reader's
 * days, each day counted by the engine, every push said as the engine recorded it.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { commandWord } from "~/lib/code.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { pinZoneForTest } from "~/lib/prefs.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { Pushes } from "./Pushes.tsx";

// EVERY REF A ROW DRAWS, WATCHED: a pass-through, so the clock's case can count what a tick drew.
vi.mock("~/lib/code.ts", async (importOriginal) => {
  const real = await importOriginal<typeof import("~/lib/code.ts")>();
  return { ...real, commandWord: vi.fn(real.commandWord) };
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  pinZoneForTest("");
  location.hash = "";
});

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 30 };
const ZERO = "0".repeat(40);
const A = "a".repeat(40);
const B = "b".repeat(40);
const C = "c".repeat(40);
const D = "d".repeat(40);
/** The story's afternoon: Today holds eight pushes, Yesterday thirteen. */
const NOW = Date.parse("2026-11-16T14:00:00Z");

const ORG = withDerived({
  name: "Acme",
  roles: [
    { name: "CTO", handle: "cto" },
    { name: "Frontend Engineer", handle: "frontend" },
    { name: "Backend Engineer", handle: "backend" },
    { name: "Site Reliability Engineer", handle: "sre" },
    { name: "Jane Founder", handle: "jane", kind: "human" },
  ],
  services: { tracker: "worklet", knowledge: "doclet", chat: "chatlet", code: "gitlet" },
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

const minutesAgo = (m: number) => new Date(NOW - m * 60_000).toISOString();
const run = (key: string) => ({ turn_id: `turn-${key}`, work_backend: "worklet", work_key: key });
const moved = (ref: string) => ({ ref: `refs/heads/${ref}`, old: A, new: B, result: "ok" });
const created = (ref: string) => ({ ref, old: ZERO, new: C, result: "ok" });
const CHANGE = {
  repo: "web",
  ref: "web!41",
  title: "cache the cart badge for 30s",
  state: "merged",
};

/** The story's ten pushes to web: eight today, two yesterday. */
const STORY = [
  push("p10", {
    at: minutesAgo(25),
    actor: "frontend",
    actor_seat: "frontend",
    via: "engine",
    run: run("ENG-415"),
    commands: [moved("frontend/ENG-415-stream-shell")],
    introduced_count: 1,
    pack_bytes: 8_400,
  }),
  push("p9", {
    at: minutesAgo(120),
    actor: "frontend",
    actor_seat: "frontend",
    via: "engine",
    run: run("ENG-415"),
    commands: [moved("frontend/ENG-415-stream-shell")],
    introduced_count: 1,
  }),
  push("p8", {
    at: minutesAgo(180),
    actor: "backend",
    actor_seat: "backend",
    run: run("ENG-412"),
    commands: [{ ...moved("backend/ENG-412-network-token"), forced: true }],
    introduced_count: 2,
  }),
  push("p7", {
    at: minutesAgo(190),
    actor: "frontend",
    actor_seat: "frontend",
    via: "engine",
    run: run("ENG-415"),
    commands: [created("refs/heads/frontend/ENG-415-stream-shell")],
    introduced_count: 1,
  }),
  push("p6", {
    at: minutesAgo(300),
    via: "engine",
    run: run("ENG-417"),
    commands: [created("refs/tags/v2026.11.2")],
  }),
  push("p5", {
    at: minutesAgo(305),
    via: "merge",
    run: { turn_id: "turn-merge" },
    commands: [{ ref: "refs/heads/main", old: D, new: C, result: "ok" }],
    change: CHANGE,
    introduced_count: 1,
  }),
  push("p4", {
    at: minutesAgo(360),
    actor: "pat:jane-laptop",
    actor_kind: "operator",
    actor_seat: "jane",
    commands: [
      {
        ref: "refs/heads/main",
        old: D,
        new: B,
        result: "ng",
        reason: "protected_direct",
        detail: "main takes no direct push: a change is the only way into it.",
      },
    ],
    offered: 1,
  }),
  push("p3", {
    at: minutesAgo(420),
    actor: "frontend",
    actor_seat: "frontend",
    via: "engine",
    run: run("ENG-399"),
    commands: [created("refs/heads/frontend/ENG-399-badge-cache")],
    introduced_count: 1,
  }),
  push("p2", {
    at: "2026-11-15T17:40:00Z",
    via: "engine",
    run: run("ENG-417"),
    commands: [created("refs/tags/v2026.11.1")],
  }),
  push("p1", {
    at: "2026-11-15T17:05:00Z",
    actor: "sre",
    actor_seat: "sre",
    run: run("ENG-414"),
    commands: [created("refs/heads/sre/drain")],
    introduced_count: 1,
  }),
];

const STORY_SENTENCES = [
  "Frontend Engineer pushed 1 commit to frontend/ENG-415-stream-shell",
  "Frontend Engineer pushed 1 commit to frontend/ENG-415-stream-shell",
  "Backend Engineer force-pushed 2 commits to backend/ENG-412-network-token",
  "Frontend Engineer pushed 1 commit to a new branch frontend/ENG-415-stream-shell",
  "CTO created the tag v2026.11.2",
  "Gitlet merged web!41 into main",
  "Jane Founder tried to push 1 commit to main",
  "Frontend Engineer pushed 1 commit to a new branch frontend/ENG-399-badge-cache",
  "CTO created the tag v2026.11.1",
  "Site Reliability Engineer pushed 1 commit to a new branch sre/drain",
];

/** The day counts for the bounds asked: today 8, yesterday 13, three on each earlier day. */
function pushDays(url: URL): number[] | undefined {
  const bounds = url.searchParams.get("day_bounds")?.split(",");
  if (!bounds) return undefined;
  const days = bounds.length - 1;
  return Array.from({ length: days }, (_, i) => (i === days - 1 ? 8 : i === days - 2 ? 13 : 3));
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

/** Who pushed to web over 30 UTC days, most first, and four who pushed less. */
const PUSHERS = {
  from: "2026-10-17T00:00:00Z",
  until: "2026-11-16T00:00:00Z",
  top: [
    { kind: "agent", handle: "frontend", pushes: 88 },
    { kind: "agent", handle: "backend", pushes: 49 },
    { kind: "agent", handle: "sre", pushes: 29 },
    { kind: "gitlet", pushes: 23 },
    { kind: "agent", handle: "cto", pushes: 14 },
    { kind: "person", handle: "jane", pushes: 6 },
  ],
  omitted: 4,
  omitted_pushes: 5,
};

/** The facts read (pushers and a 30-day spark), refused for anything else. */
const FACTS = (url: URL) =>
  url.searchParams.get("repo_facts") === "true"
    ? json({
        repo: { name: "web", spark: Array<number>(30).fill(2), pushers: PUSHERS },
        position: POSITION,
        level: "stale",
      })
    : json({}, 404);

/** The engine's REST answers, by path; every request made. */
function stubFetch(history: (url: URL) => Response, repo: (url: URL) => Response = FACTS) {
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: RequestInfo | URL) => {
      const url = new URL(String(input));
      asked.push(url);
      if (url.pathname === "/auth/session") return new Promise<Response>(() => {});
      if (url.pathname === "/query/code_repos") return Promise.resolve(json(LISTING));
      if (url.pathname === "/query/code_pushes") return Promise.resolve(history(url));
      if (url.pathname === "/query/code_repo") return Promise.resolve(repo(url));
      return Promise.resolve(json({}, 404));
    }),
  );
  return asked;
}

const histories = (asked: URL[]) => asked.filter((u) => u.pathname === "/query/code_pushes");

/** A history page holding these pushes, its days counted where asked. */
const pageOf = (url: URL, pushes: unknown[], extra: Record<string, unknown> = {}) =>
  json({ pushes, push_days: pushDays(url), position: POSITION, level: "stale", ...extra });

/** The story at its afternoon, in UTC. */
function atTheStory(zone = "UTC") {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(NOW);
  pinZoneForTest(zone);
}

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

/** The feed, once its rows have drawn. */
const feedOf = (repo = "web") => screen.findByRole("group", { name: `Pushes to ${repo}` });
const sentences = (feed: HTMLElement) =>
  [...feed.querySelectorAll(".code-push-sentence")].map((s) => s.textContent);
/** The row whose sentence holds these words. */
const rowOf = (feed: HTMLElement, words: string) =>
  [...feed.querySelectorAll<HTMLElement>(".code-list__row")].find((r) =>
    r.querySelector(".code-push-sentence")?.textContent?.includes(words),
  )!;
/** Each day's heading with its count. */
const headings = (feed: HTMLElement) =>
  [...feed.querySelectorAll(".code-list__heading")].map((h) => h.textContent);

/** A newest page of the history: no cursor named. */
const newest = (asked: URL[]) => histories(asked).filter((u) => !u.searchParams.has("cursor"));

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

// NO HISTORY UNTIL A REPOSITORY IS NAMED: the engine keeps one per repository.
test("without a repository the section asks for one, and asks no history", async () => {
  const asked = stubFetch(() => json({ position: POSITION, level: "stale" }));
  mount("#/gitlet/pushes");
  expect(await screen.findByRole("heading", { name: "Choose a repository" })).toBeTruthy();
  await afterMicrotasks();
  expect(histories(asked)).toEqual([]);
  expect(asked.some((u) => u.pathname === "/query/code_repos")).toBe(true);
});

// A NAME NO REPOSITORY CAN HAVE is said in place and never sent. Mutation: drop the check.
test("a name no repository can have is said in place and never asked", async () => {
  const asked = stubFetch(() => json({ position: POSITION, level: "stale" }));
  mount("#/gitlet/pushes?repo=Web_App");
  expect(await screen.findByText(/is not a repository’s name/)).toBeTruthy();
  await afterMicrotasks();
  expect(histories(asked)).toEqual([]);
});

// Mutation: count the refs as commits, and every "1 commit" reads its ref count.
test("each push is one sentence under its subject's face", async () => {
  atTheStory();
  stubFetch((url) => pageOf(url, STORY));
  mount("#/gitlet/pushes?repo=web");
  const feed = await feedOf();
  expect(sentences(feed)).toEqual(STORY_SENTENCES);
  const row = rowOf(feed, "force-pushed");
  expect(row.querySelector(".code-list__lead")?.firstElementChild).toBeTruthy();
  expect(within(row).getByText("Backend Engineer").tagName).toBe("STRONG");
});

// Mutation: make the merger the subject, and the row reads as the CTO's push.
test("the merge push names its change, its title, Gitlet as its subject and who asked", async () => {
  atTheStory();
  stubFetch((url) => pageOf(url, STORY));
  mount("#/gitlet/pushes?repo=web");
  const row = rowOf(await feedOf(), "merged");
  expect(row.querySelector(".code-list__lead .code-face")).toBeTruthy();
  expect(within(row).getByRole("link", { name: "web!41" }).getAttribute("href")).toBe(
    "#/gitlet/repos/web/changes/41",
  );
  expect(row.querySelector(".code-push-change")?.textContent).toBe(
    "· cache the cart badge for 30s",
  );
  const meta = row.querySelector(".code-list__meta")?.textContent;
  expect(meta).toBe("Asked by CTO · turna change merged");
});

test("a queue landing says it landed from its queue and who queued it", async () => {
  atTheStory();
  stubFetch((url) =>
    pageOf(url, [
      push("q1", { at: minutesAgo(5), via: "queue", change: { ...CHANGE, ref: "web!44" } }),
    ]),
  );
  mount("#/gitlet/pushes?repo=web");
  const feed = await feedOf();
  expect(sentences(feed)).toEqual(["Gitlet landed web!44 on main from its queue"]);
  expect(feed.querySelector(".code-list__meta")?.textContent).toBe(
    "Queued by CTOits queue landed it",
  );
});

// Mutation: read an absent `offered` as 0, and the sentence says "0 commits".
test("a refused push says what it offered, never zero, and that nothing was stored", async () => {
  atTheStory();
  const jane = STORY[6]!;
  stubFetch((url) => pageOf(url, [jane, { ...jane, push_id: "p0", offered: undefined }]));
  mount("#/gitlet/pushes?repo=web");
  const feed = await feedOf();
  expect(sentences(feed)).toEqual([
    "Jane Founder tried to push 1 commit to main",
    "Jane Founder tried to push to main",
  ]);
  const stored = feed.querySelector<HTMLElement>(".code-push-stored")!;
  expect(stored.textContent).toBe(
    "Nothing was stored: main takes no direct push: a change is the only way into it. See its rules",
  );
  expect(within(stored).getByRole("link", { name: "See its rules" }).getAttribute("href")).toBe(
    "#/gitlet/repos/web?tab=settings",
  );
  expect(
    within(feed).getAllByText("refused: protected direct")[0]!.closest(".crewlet-tag--danger"),
  ).toBeTruthy();
});

// Mutation: link the work key, and the run chip holds two links.
test("a person's push names its credential, a seat's its run and turn, each how it was built", async () => {
  atTheStory();
  stubFetch((url) => pageOf(url, STORY));
  mount("#/gitlet/pushes?repo=web");
  const feed = await feedOf();
  const seat = rowOf(feed, "force-pushed").querySelector<HTMLElement>(".code-list__meta")!;
  expect(seat.textContent).toBe("Run for ENG-412 · turnwith git");
  expect(
    within(seat)
      .getAllByRole("link")
      .map((a) => a.getAttribute("href")),
  ).toEqual(["#/live/turns/turn-ENG-412"]);
  const person = rowOf(feed, "tried").querySelector(".code-list__meta")!;
  expect(person.textContent).toBe("Personal access token pat:jane-laptopwith git");
});

// Mutation: say the loaded count alone, and Yesterday reads "2".
test("each day's heading counts its pushes, and a day past the counts says what is loaded", async () => {
  atTheStory();
  const older = push("p0", { at: "2026-09-30T10:00:00Z" });
  stubFetch((url) =>
    url.searchParams.get("cursor") === "c1"
      ? pageOf(url, [older])
      : pageOf(url, STORY, { next: "c1" }),
  );
  mount("#/gitlet/pushes?repo=web");
  const feed = await feedOf();
  expect(headings(feed)).toEqual(["Today8", "Yesterday2 of 13"]);
  expect(feed.querySelectorAll(".code-list__aside [title]")[1]?.getAttribute("title")).toBe(
    "13 pushes on Nov 15, 2026, 2 of them loaded",
  );
  fireEvent.click(screen.getByRole("button", { name: "Load older pushes" }));
  await screen.findByText(/That is every push the history keeps/);
  expect(headings(feed).at(-1)).toBe("Sep 30, 20261 loaded");
});

// A DAY SPLIT BY OUT-OF-ORDER INSTANTS counts every run of it together.
// Mutation: count a run's rows alone, and each Today run reads "1 of 8".
test("a day split into two runs by an out-of-order push counts both runs in each heading", async () => {
  atTheStory();
  const split = [STORY[0]!, STORY[8]!, STORY[1]!];
  stubFetch((url) => pageOf(url, split));
  mount("#/gitlet/pushes?repo=web");
  const feed = await feedOf();
  expect(headings(feed)).toEqual(["Today2 of 8", "Yesterday1 of 13", "Today2 of 8"]);
});

// Mutation: send UTC midnights, and the first bound is the UTC day's.
test("the first page asks the reader's own midnights and older pages ask none", async () => {
  atTheStory("Asia/Kathmandu");
  const asked = stubFetch((url) =>
    url.searchParams.get("cursor") === "c1"
      ? pageOf(url, [push("p0")])
      : pageOf(url, STORY, { next: "c1" }),
  );
  mount("#/gitlet/pushes?repo=web");
  await feedOf();
  const bounds = newest(asked)[0]!.searchParams.get("day_bounds")!.split(",");
  expect(bounds).toHaveLength(32);
  expect([bounds[0], bounds.at(-1)]).toEqual(["2026-10-16T18:15:00Z", "2026-11-16T18:15:00Z"]);
  fireEvent.click(screen.getByRole("button", { name: "Load older pushes" }));
  await screen.findByText(/That is every push the history keeps/);
  const older = histories(asked).find((u) => u.searchParams.get("cursor") === "c1")!;
  expect(older.searchParams.has("day_bounds")).toBe(false);
  expect(older.searchParams.get("brief")).toBe("true");
});

// AN OLDER FLEET REFUSES THE DAY COUNTS, and the history says so: nothing asks again without them.
// Mutation: retry without day_bounds, and a second request is made.
test("a fleet that refuses the day counts refuses the history, and nothing asks again", async () => {
  const asked = stubFetch(() =>
    json({ error: "bad_params", detail: "day_bounds is not a key this node reads" }, 400),
  );
  mount("#/gitlet/pushes?repo=web");
  expect(
    await screen.findByText(
      "Pushes are unavailable on this fleet yet: day_bounds is not a key this node reads.",
    ),
  ).toBeTruthy();
  await afterMicrotasks();
  expect(histories(asked)).toHaveLength(1);
  expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
});

// THE HISTORY IS ASKED IN BRIEF: five refs at most, refusals first, the rest counted.
// Mutation: ask the history whole, and `brief` is not on the request.
test("a push of many refs is drawn in brief: five, its refusals first, and the rest counted", async () => {
  const asked = stubFetch((url) =>
    pageOf(url, [
      push(IMPORT, {
        commands: [
          { ref: "refs/heads/main", old: B, new: C, result: "ng", reason: "protected_force" },
          { ref: "refs/tags/v0", old: A, new: B, result: "ng", reason: "tag_moved" },
          ...tags(3),
        ],
        omitted: { created: 990, deleted: 3, refused: 2 },
        introduced_count: 40,
      }),
    ]),
  );
  mount("#/gitlet/pushes?repo=web");
  const feed = await feedOf();
  expect(histories(asked)[0]!.searchParams.get("brief")).toBe("true");
  expect(sentences(feed)).toEqual(["CTO pushed 40 commits to 1,000 refs 4 refused"]);
  expect(within(feed).getByText("refused: protected force")).toBeTruthy();
  expect(within(feed).getByText("refused: tag moved")).toBeTruthy();
  expect(within(feed).getByText("t02")).toBeTruthy();
  expect(within(feed).getByText(/and 995 more:/).textContent).toBe(
    "and 995 more: 2 refused, 990 created, 3 deleted",
  );
  expect(within(feed).getByText("2 refused").closest(".crewlet-tag--danger")).toBeTruthy();
  expect(within(feed).getByRole("button", { name: "Show all 1,000 refs" })).toBeTruthy();
});

// EVERY REF IS ONE READ OF THE PUSH AWAY: that push alone, at its own position, read once.
// Mutation: ask the push without its position, or read it again on every opening.
test("Show all reads that one push whole at its own position, once, and Show fewer folds it", async () => {
  const asked = stubFetch((url) =>
    url.searchParams.has("push")
      ? page(push(IMPORT, { commands: tags(17), introduced: [C], introduced_count: 1 }), "session")
      : page(push(IMPORT, { commands: tags(5), omitted: { created: 12 } })),
  );
  mount("#/gitlet/pushes?repo=web");
  const feed = await feedOf();
  expect(within(feed).queryByText("t16")).toBeNull();

  fireEvent.click(within(feed).getByRole("button", { name: "Show all 17 refs" }));
  expect(await within(feed).findByText("t16")).toBeTruthy();
  const reads = () => histories(asked).filter((u) => u.searchParams.has("push"));
  expect(reads()).toHaveLength(1);
  expect(Object.fromEntries(reads()[0]!.searchParams)).toEqual({
    name: "web",
    push: IMPORT,
    read_level: "session",
    min_position: "CREWLET_CODE_LOG@1:30",
  });
  expect(within(feed).queryByText(/more:/)).toBeNull();

  const fewer = within(feed).getByRole("button", { name: "Show fewer" });
  expect(fewer.getAttribute("aria-expanded")).toBe("true");
  fireEvent.click(fewer);
  expect(within(feed).queryByText("t16")).toBeNull();
  expect(within(feed).getByText(/and 12 more:/)).toBeTruthy();
  fireEvent.click(within(feed).getByRole("button", { name: "Show all 17 refs" }));
  expect(within(feed).getByText("t16")).toBeTruthy();
  expect(reads()).toHaveLength(1);
});

// A PUSH THE HISTORY NO LONGER HOLDS is said, never drawn as a push with no refs.
test("a push the history no longer holds says so", async () => {
  stubFetch((url) =>
    url.searchParams.has("push")
      ? json({ position: POSITION, level: "session" })
      : page(push(IMPORT, { commands: tags(5), omitted: { created: 4 } })),
  );
  mount("#/gitlet/pushes?repo=web");
  const feed = await feedOf();
  fireEvent.click(within(feed).getByRole("button", { name: "Show all 9 refs" }));
  expect(await within(feed).findByText("The history no longer holds this push.")).toBeTruthy();
  expect(within(feed).queryByRole("button", { name: /Show all/ })).toBeNull();
});

// A PUSH THAT COULD NOT BE READ is said beside Show all, which asks it again.
// Mutation: draw the failure in the control's place, and there is nothing to press.
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
  mount("#/gitlet/pushes?repo=web");
  const feed = await feedOf();
  fireEvent.click(within(feed).getByRole("button", { name: "Show all 9 refs" }));
  expect(
    await within(feed).findByText("Could not read this push (query_failed). Try again."),
  ).toBeTruthy();
  fireEvent.click(within(feed).getByRole("button", { name: "Show all 9 refs" }));
  expect(await within(feed).findByText("t08")).toBeTruthy();
  expect(within(feed).queryByText(/Could not read this push/)).toBeNull();
  expect(histories(asked).filter((u) => u.searchParams.has("push"))).toHaveLength(2);
});

// THE CLOCK RE-RENDERS THE TIME A PUSH LANDED AND NO REF.
// Mutation: read the clock in the history and drop the refs' memo, and every tick draws every ref.
test("the clock's tick re-renders the time a push landed and no ref", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
  stubFetch(() =>
    json({
      pushes: [push("p2", { commands: tags(4) }), push("p1")],
      position: POSITION,
      level: "stale",
    }),
  );
  mount("#/gitlet/pushes?repo=web");
  await feedOf();
  await afterMicrotasks();
  const drawn = vi.mocked(commandWord).mock.calls.length;
  expect(drawn).toBeGreaterThan(0);
  await act(async () => void vi.advanceTimersByTime(3_000));
  expect(vi.mocked(commandWord).mock.calls.length).toBe(drawn);
});

// A REPOSITORY NOBODY DECLARED is said with the engine's own sentence.
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
  mount("#/gitlet/pushes?repo=gone");
  expect(await screen.findByText("No repository named “gone”")).toBeTruthy();
  expect(screen.getByText('no repository named "gone" is declared in code.repos')).toBeTruthy();
});

// A LOG REACHES ITS OLDER ROWS BY FETCHING THEM, and the newest page stays polled meanwhile.
// Mutation: stop polling the newest page once an older one is loaded, and the tick asks nothing.
test("Load older asks the next page by its cursor, and the newest page stays polled", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  const asked = stubFetch((url) =>
    url.searchParams.get("cursor") === "c1"
      ? json({ pushes: [push("p1")], position: POSITION, level: "stale" })
      : json({ pushes: [push("p2")], next: "c1", position: POSITION, level: "stale" }),
  );
  mount("#/gitlet/pushes?repo=web");
  await feedOf();
  await act(async () => void vi.advanceTimersByTime(60_000));
  await afterMicrotasks();
  expect(histories(asked)).toHaveLength(2);

  fireEvent.click(screen.getByRole("button", { name: "Load older pushes" }));
  await screen.findByText(/That is every push the history keeps/);
  expect(histories(asked).at(-1)!.searchParams.get("cursor")).toBe("c1");
  const before = newest(asked).length;
  await act(async () => void vi.advanceTimersByTime(60_000));
  await afterMicrotasks();
  expect(newest(asked).length).toBeGreaterThan(before);
});

// AN OLDER PAGE THAT FAILS LEAVES THE PRESS THAT ASKS IT AGAIN, its failure beside it.
// Mutation: draw the failure in the press's place, and there is no Load older to press.
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
  mount("#/gitlet/pushes?repo=web");
  await feedOf();
  fireEvent.click(screen.getByRole("button", { name: "Load older pushes" }));
  expect(
    await screen.findByText("Could not read the next page (query_failed). Try again."),
  ).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Load older pushes" }));
  await screen.findByText(/That is every push the history keeps/);
  expect(screen.queryByText(/Could not read the next page/)).toBeNull();
});

// Mutation: drop the retention sentence, and the end of the history reads as a page's end.
test("the foot says what is loaded and, at the end, how long the history keeps", async () => {
  stubFetch((url) =>
    url.searchParams.get("cursor") === "c1"
      ? json({ pushes: [push("p1")], position: POSITION, level: "stale" })
      : json({ pushes: [push("p2")], next: "c1", position: POSITION, level: "stale" }),
  );
  mount("#/gitlet/pushes?repo=web");
  await feedOf();
  const note = () => document.querySelector(".code-list__note")?.textContent;
  expect(note()).toBe("1 push loaded, newest first");
  fireEvent.click(screen.getByRole("button", { name: "Load older pushes" }));
  await screen.findByText(/That is every push/);
  expect(note()).toBe(
    "2 pushes loaded, newest first. That is every push the history keeps: the last 181 days.",
  );
});

// ANOTHER REPOSITORY AND BACK starts the history again from its newest page.
// Mutation: keep the pages and hide them by repository, and p1 is drawn again.
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
  mount("#/gitlet/pushes?repo=web");
  await feedOf();
  fireEvent.click(screen.getByRole("button", { name: "Load older pushes" }));
  await screen.findByText(/That is every push the history keeps/);
  expect(screen.getByText(/^4 pushes loaded/)).toBeTruthy();

  act(() => void (location.hash = "#/gitlet/pushes?repo=api"));
  await feedOf("api");
  landed = 6;
  act(() => void (location.hash = "#/gitlet/pushes?repo=web"));
  await feedOf();
  await afterMicrotasks();
  expect(screen.getByText("2 pushes loaded, newest first")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Load older pushes" })).toBeTruthy();
  expect(histories(asked).filter((u) => u.searchParams.get("cursor") === "c3")).toHaveLength(1);
});

/** The seat filter's options, as the list draws them. */
function seatOptions() {
  fireEvent.click(screen.getByRole("button", { name: "Pushed by" }));
  return screen.getAllByRole("option").map((o) => o.textContent);
}

// A SEAT IS NARROWED ON THE ENGINE, by its kind and handle, and asks no day counts: they count
// every pusher. Mutation: send day_bounds beside a pusher, and the first request carries them.
test("by= narrows on the engine and asks no day counts", async () => {
  atTheStory();
  const four = STORY.filter((p) => p.actor === "frontend");
  const asked = stubFetch((url) => pageOf(url, four));
  mount("#/gitlet/pushes?repo=web&by=frontend");
  const feed = await feedOf();
  const first = newest(asked)[0]!.searchParams;
  expect([first.get("pusher_kind"), first.get("pusher"), first.has("day_bounds")]).toEqual([
    "agent",
    "frontend",
    false,
  ]);
  expect(headings(feed)).toEqual(["Today4 loaded"]);
  expect(screen.getByText("4 pushes by Frontend Engineer loaded, newest first")).toBeTruthy();

  cleanup();
  const jane = stubFetch((url) => pageOf(url, [STORY[6]!]));
  mount("#/gitlet/pushes?repo=web&by=jane");
  await feedOf();
  expect(newest(jane)[0]!.searchParams.get("pusher_kind")).toBe("person");

  cleanup();
  const gitlet = stubFetch((url) => pageOf(url, [STORY[5]!]));
  mount("#/gitlet/pushes?repo=web&by=gitlet");
  await feedOf();
  const asks = newest(gitlet)[0]!.searchParams;
  expect([asks.get("pusher_kind"), asks.has("pusher")]).toEqual(["gitlet", false]);
});

// A WINDOW STARTS AT THE READER'S MIDNIGHT and its day counts lie inside it.
// Mutation: start the window at now less seven days, and since is 14:00 on Nov 9.
test("when= narrows from the reader's midnight and every bound lies inside it", async () => {
  atTheStory();
  const asked = stubFetch((url) => pageOf(url, STORY));
  mount("#/gitlet/pushes?repo=web&when=7d");
  const feed = await feedOf();
  const first = newest(asked)[0]!.searchParams;
  const bounds = first.get("day_bounds")!.split(",");
  expect(first.get("since")).toBe("2026-11-10T00:00:00.000Z");
  expect(bounds).toHaveLength(8);
  expect([bounds[0], bounds.at(-1)]).toEqual(["2026-11-10T00:00:00Z", "2026-11-17T00:00:00Z"]);
  expect(headings(feed)).toEqual(["Today8", "Yesterday2 of 13"]);
  expect(
    screen.getByText("10 pushes in the last 7 days loaded, newest first", { exact: false }),
  ).toBeTruthy();
});

// A NARROWED PAGE READS A BOUNDED STRETCH: one that read short says how far back it looked.
// Mutation: draw the empty state while `next` is set, and "No push" is drawn.
test("a narrowed page that read short says how far back it looked, and Load older looks on", async () => {
  atTheStory();
  const twoDaysAgo = minutesAgo(2 * 24 * 60);
  const asked = stubFetch((url) =>
    url.searchParams.get("cursor") === "c1"
      ? pageOf(url, [STORY[9]!])
      : pageOf(url, [], { next: "c1", push_scan: { to: twoDaysAgo } }),
  );
  mount("#/gitlet/pushes?repo=web&by=sre");
  const more = await screen.findByRole("button", { name: "Load older pushes" });
  const note = () => document.querySelector(".code-list__note")?.textContent;
  expect(note()).toBe("Looked back to 2 days ago; Load older looks further.");
  expect(screen.queryByText(/No push/)).toBeNull();
  fireEvent.click(more);
  await screen.findByText("1 push by Site Reliability Engineer loaded, newest first");
  const older = histories(asked).find((u) => u.searchParams.get("cursor") === "c1")!;
  expect([older.searchParams.get("pusher"), older.searchParams.has("day_bounds")]).toEqual([
    "sre",
    false,
  ]);
});

// WHOM THE FILTER OFFERS is who pushed here, most first, then every other seat by name.
// Mutation: sort the pushers by name, and Backend Engineer leads.
test("the seat options are who pushed, most first, then any seat by name", async () => {
  atTheStory();
  stubFetch((url) => pageOf(url, STORY));
  mount("#/gitlet/pushes?repo=web&by=reviewer");
  await feedOf();
  await afterMicrotasks();
  expect(seatOptions()).toEqual([
    "All seats",
    "reviewer",
    "Frontend Engineer 88",
    "Backend Engineer 49",
    "Site Reliability Engineer 29",
    "Gitlet 23",
    "CTO 14",
    "JFJane Founder 6",
    "and 4 more who pushed lessType a name to choose one",
  ]);

  cleanup();
  stubFetch(
    (url) => pageOf(url, STORY),
    () => json({ error: "unavailable" }, 503),
  );
  mount("#/gitlet/pushes?repo=web");
  await feedOf();
  await afterMicrotasks();
  expect(seatOptions()).toEqual([
    "All seats",
    "Gitlet",
    "Backend Engineer",
    "CTO",
    "Frontend Engineer",
    "JFJane Founder",
    "Site Reliability Engineer",
  ]);
});

// Mutation: clear only by=, and the window stays in the address.
test("a narrowed history with nothing says so, and Clear filters clears who and when", async () => {
  atTheStory();
  stubFetch((url) => pageOf(url, []));
  mount("#/gitlet/pushes?repo=web&by=frontend&when=7d");
  expect(
    await screen.findByRole("heading", {
      name: "No push by Frontend Engineer in the last 7 days",
    }),
  ).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
  await afterMicrotasks();
  expect(location.hash).toBe("#/gitlet/pushes?repo=web");
});

// AN OLDER FLEET REFUSES A FILTER, and Clear filters beside the refusal is the way back.
// Mutation: drop Clear filters beside the refusal, and a narrowed reader has no way back.
test("a narrowed history the fleet refuses offers Clear filters beside the refusal", async () => {
  atTheStory();
  stubFetch(() =>
    json({ error: "bad_params", detail: "pusher_kind is not a key this node reads" }, 400),
  );
  mount("#/gitlet/pushes?repo=web&by=frontend&when=7d");
  expect(
    await screen.findByText(
      "Pushes are unavailable on this fleet yet: pusher_kind is not a key this node reads.",
    ),
  ).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
  await afterMicrotasks();
  expect(location.hash).toBe("#/gitlet/pushes?repo=web");
});
