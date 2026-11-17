/**
 * Gitlet › Repositories reads the engine's listing over REST, a page at a time,
 * by last push with every row's facts, and draws each repository as a row.
 *
 * The invariants worth breaking a build over: the listing is asked over REST
 * and never on the socket's slots; a page that holds no row but a cursor is
 * not the end of the list; the filters and the sort are the engine's own; a
 * fact not answered is drawn as nothing; and a refused listing is never asked
 * again without its facts.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactElement } from "react";
import { afterEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { pinZoneForTest } from "~/lib/prefs.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { UNBOUND } from "~/lib/repoWrites.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { afterMicrotasks, settled } from "~/test/settle.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { Repositories } from "./Repositories.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  pinZoneForTest("");
  location.hash = "";
});

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };

const ORG = withDerived({
  name: "Acme",
  roles: [
    { name: "CTO", handle: "cto" },
    { name: "Frontend Engineer", handle: "frontend" },
    { name: "Backend Engineer", handle: "backend" },
  ],
  units: [{ id: "engineering", name: "Engineering", roles: [] }],
  services: { tracker: "worklet", knowledge: "doclet", chat: "chatlet", code: "gitlet" },
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
    search: "indexed",
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

/**
 * The screen in the frame's readings, at `hash`, for `viewer`; the socket's
 * questions recorded.
 */
function mount(ui: ReactElement, hash = "#/gitlet/repos", viewer: unknown = ADMIN_VIEWER) {
  location.hash = hash;
  const store = new Store();
  store.applyOrg(ORG);
  const socket = new LiveSocket(store);
  const onSocket: string[] = [];
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    onSocket.push(what);
    return what === "viewer" ? Promise.resolve(viewer) : new Promise(() => {});
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

/** The names the list draws, in its order, each once. */
const listed = () => [
  ...new Set(
    within(screen.getByRole("list", { name: "Repositories" }))
      .getAllByRole("link")
      .map((a) => a.getAttribute("href")?.split("#/gitlet/repos/")[1]),
  ),
];

// THE ENGINE'S LISTING, OVER REST, by last push with every row's facts on the reader's own days:
// a +5:45 reader's 15 bounds are their midnights. Mutation: ask on the socket, drop `repo_facts`,
// or build the bounds from UTC midnights, and this fails.
test("the repositories are asked by last push with their facts, each row opening its page", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-16T10:00:00Z"));
  pinZoneForTest("Asia/Kathmandu");
  const asked = stubFetch((url) =>
    url.pathname === "/query/code_repos"
      ? json({ repos: [repo("web"), repo("api")], position: POSITION, level: "stale" })
      : null,
  );
  const { onSocket } = mount(<Repositories />);

  await screen.findByRole("list", { name: "Repositories" });
  expect(listed()).toEqual(["web", "api"]);
  const first = firstPages(asked)[0]!;
  expect(first.searchParams.get("sort")).toBe("pushed");
  expect(first.searchParams.get("repo_facts")).toBe("true");
  expect(first.searchParams.has("limit")).toBe(false);
  const bounds = first.searchParams.get("day_bounds")!.split(",");
  expect(bounds).toHaveLength(15);
  expect([bounds[0], bounds[14]]).toEqual(["2026-11-02T18:15:00Z", "2026-11-16T18:15:00Z"]);
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

// THE ENGINE'S OWN FILTER: `status=` is asked as the engine reads it, by name where it lists
// only by name, and an empty answer under it says which state was empty. Mutation: ask
// `sort=pushed` beside `status=deleted`, and the engine refuses the listing.
test("the state filter is the engine's status, said when nothing is in that state", async () => {
  const asked = stubFetch((url) =>
    url.pathname === "/query/code_repos" ? json({ position: POSITION, level: "stale" }) : null,
  );
  mount(<Repositories />, "#/gitlet/repos?status=deleted");

  expect(await screen.findByText("No deleted repositories")).toBeTruthy();
  const listing = asked.find((u) => u.pathname === "/query/code_repos")!;
  expect(listing.searchParams.get("status")).toBe("deleted");
  expect(listing.searchParams.get("sort")).toBe("name");
  const sort = screen.getByRole("combobox", { name: "Sort" });
  expect(sort.textContent).toContain("Sort: Name");
  expect(sort.hasAttribute("disabled") || sort.getAttribute("aria-disabled") === "true").toBe(true);
  expect(document.body.textContent).toContain(
    "Deleted and purging repositories are listed by name only.",
  );
});

// A STATE THIS BUILD DOES NOT KNOW IS NOT ASKED: the live ones are listed instead. Mutation: send
// it as is, and the engine refuses the listing as though the fleet were older.
test("a state this build does not know asks for the live ones", async () => {
  const asked = stubEngine();
  mount(<Repositories />, "#/gitlet/repos?status=bogus");
  await rowOf("web");
  expect(firstPages(asked)[0]!.searchParams.has("status")).toBe(false);
  expect(firstPages(asked)[0]!.searchParams.get("sort")).toBe("pushed");
  expect(screen.queryByText(/unavailable on this fleet/)).toBeNull();
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

// THE FIRST PAGE STAYS LIVE past Load more, and its cursor is the last name
// it scanned: read again under a repository that entered its range —
// declared by an activation, restored, or moved into the filtered state — it
// pushes that name off its foot. The case found in review: a1 and a2, then
// a3; a0 is declared, and a minute on a2 was gone until a reload. The pager
// reads the names between the moved first page and the page after it.
// Mutation: drop the pager's join, and a2 is not listed.
test("a repository declared after Load more pushes none out of the listing", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  let declared = ["a1", "a2", "a3"];
  const asked = stubFetch(byName(() => declared));
  mount(<Repositories />);
  fireEvent.click(await screen.findByRole("button", { name: /load more/i }));
  expect(await screen.findByText("a3")).toBeTruthy();
  expect(listed()).toEqual(["a1", "a2", "a3"]);

  declared = ["a0", "a1", "a2", "a3"];
  await act(async () => void vi.advanceTimersByTime(60_000));
  await settled(() => asked.length);
  expect(firstPages(asked).length).toBeGreaterThan(1);
  expect(listed()).toEqual(["a0", "a1", "a2", "a3"]);
});

// A FILTER LEFT AND COME BACK TO starts the listing again: what the first
// reading loaded is discarded, never hidden under its question, where it came
// back under a first page read since. Mutation: hide the held pages by
// question instead, and the old a3 is drawn under a fresh first page.
test("a filter left and come back to lists from its first page, nothing held", async () => {
  let declared = ["a1", "a2", "a3"];
  const asked = stubFetch(byName(() => declared));
  mount(<Repositories />);
  fireEvent.click(await screen.findByRole("button", { name: /load more/i }));
  expect(await screen.findByText("a3")).toBeTruthy();

  act(() => void (location.hash = "#/gitlet/repos?status=archived"));
  await waitFor(() =>
    expect(asked.some((u) => u.searchParams.get("status") === "archived")).toBe(true),
  );
  declared = ["a0", "a1", "a2", "a3"];
  act(() => void (location.hash = "#/gitlet/repos"));
  expect(await screen.findByText("a0")).toBeTruthy();
  await afterMicrotasks();
  expect(listed()).toEqual(["a0", "a1"]);
  expect(screen.getByRole("button", { name: /load more/i })).toBeTruthy();
});

// THE IMPORT DIALOG'S FIELDS ARE LABELLED where the reader sees them, as
// every other form's: a placeholder is gone once a value is typed. Mutation:
// name a field by aria-label alone, and its visible label is not there.
test("Import a repository labels every field it asks for", async () => {
  stubFetch((url) =>
    url.pathname === "/query/code_repos" ? json({ position: POSITION, level: "stale" }) : null,
  );
  const { store } = mount(<Repositories />, "#/gitlet/repos", {
    ...ADMIN_VIEWER,
    acts: ["code_import_repo"],
  });
  act(() => store.setConnected(true));
  const button = await screen.findByRole("button", { name: "Import a repository" });
  await waitFor(() => expect(button.getAttribute("title")).toBeNull());
  fireEvent.click(button);
  const dialog = await screen.findByRole("dialog");
  const labels = Array.from(dialog.querySelectorAll("label")).map((l) => l.textContent ?? "");
  for (const name of ["Name", "Source URL", "Credential variable", "Owning unit"]) {
    expect(
      labels.some((l) => l.startsWith(name)),
      name,
    ).toBe(true);
  }
});

// IMPORT A REPOSITORY IS DRAWN FOR EVERY READER, held with the reason it
// cannot act, and opens nothing for one who cannot. Mutation: hide it from a
// reader who cannot act, or open its dialog for one, and this fails.
test.each([
  [
    "an anonymous reader",
    { login: "", grants: [], handle: "", owner: "", name: "", acts: [], anonymous: true },
    WRITE_REASONS.anonymous,
  ],
  ["a person the engine does not serve", { ...ADMIN_VIEWER, acts: [] }, WRITE_REASONS.not_served],
])("Import a repository is drawn for %s, held with the reason", async (_who, viewer, reason) => {
  stubFetch((url) =>
    url.pathname === "/query/code_repos" ? json({ position: POSITION, level: "stale" }) : null,
  );
  const { store } = mount(<Repositories />, "#/gitlet/repos", viewer);
  act(() => store.setConnected(true));

  const button = await screen.findByRole("button", { name: "Import a repository" });
  await waitFor(() => expect(button.getAttribute("title")).toBe(reason));
  fireEvent.click(button);
  expect(screen.queryByRole("dialog")).toBeNull();
});

// NEW REPOSITORY IS ONE code_create_repo: the project's name and the unit
// that owns it, and nothing the person left at the engine's default.
// Mutation: send the press before a unit is chosen, send an empty
// description or branch, or name the owner by its name rather than its key,
// and this fails.
test("New repository creates a project a unit owns, sending only what the person said", async () => {
  const acted: { tool: string; args: Record<string, unknown> }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname === "/auth/session") return new Promise<Response>(() => {});
      if (url.pathname.startsWith("/operator/act/")) {
        acted.push({
          tool: url.pathname.split("/operator/act/")[1] ?? "",
          args: (JSON.parse(String(init?.body)) as { args: Record<string, unknown> }).args,
        });
        return json({ outcome: "applied" });
      }
      if (url.pathname === "/query/code_repos") return json({ position: POSITION, level: "stale" });
      return json({}, 404);
    }),
  );
  const { store } = mount(<Repositories />, "#/gitlet/repos", {
    ...ADMIN_VIEWER,
    acts: ["code_create_repo"],
  });
  act(() => store.setConnected(true));

  const button = await screen.findByRole("button", { name: "New repository" });
  await waitFor(() => expect(button.getAttribute("title")).toBeNull());
  fireEvent.click(button);
  const dialog = await screen.findByRole("dialog", { name: "New repository" });
  const create = within(dialog).getByRole("button", { name: "Create repository" });
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "web" } });
  fireEvent.click(create);
  expect(acted).toHaveLength(0);
  fireEvent.click(within(dialog).getByRole("combobox", { name: "Owning unit" }));
  fireEvent.mouseDown(screen.getByRole("option", { name: "Engineering" }));
  fireEvent.click(create);
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_create_repo",
    args: { name: "web", owner: { kind: "unit", ref: "engineering" } },
  });
  // AN APPLIED CREATE LEADS TO ITS PAGE.
  await waitFor(() => expect(location.hash).toBe("#/gitlet/repos/web"));
});

// A CREDENTIAL BOUND TO NOBODY CREATES NOTHING in Gitlet, which names the
// person behind every write: the button is drawn and held with that reason.
test("New repository is held for a credential bound to nobody", async () => {
  stubFetch((url) =>
    url.pathname === "/query/code_repos" ? json({ position: POSITION, level: "stale" }) : null,
  );
  const { store } = mount(<Repositories />, "#/gitlet/repos", {
    ...ADMIN_VIEWER,
    handle: "",
    owner: "",
    kind: "",
    login: "ci:deploy",
    acts: ["code_create_repo"],
  });
  act(() => store.setConnected(true));
  const button = await screen.findByRole("button", { name: "New repository" });
  await waitFor(() => expect(button.getAttribute("title")).toBe(UNBOUND));
});

/** A last push as a card carries it. */
const lastPush = (
  at: string,
  actor: string,
  via = "git",
  outcome: Record<string, number> = { moved: 1 },
) => ({
  push_id: `p-${actor}`,
  at,
  actor,
  actor_kind: "agent",
  actor_seat: actor,
  via,
  outcome,
});

const PASSED = {
  state: "passed",
  counts: { passed: 3 },
  required: 2,
  required_passed: 2,
  last_ended: "2026-11-16T09:40:00Z",
};

/** Fourteen days of pushes, oldest first. */
const SPARK = [9, 14, 6, 1, 3, 12, 17, 11, 8, 15, 19, 7, 13, 8];

/** The board's rows, each with its facts. */
const BOARD = [
  repo("web", {
    description: "The storefront: a React app and the Go server that renders it",
    live_bytes: 18 * 1024 * 1024 + 400_000,
    pushes: 214,
    last_push_at: "2026-11-16T09:35:00Z",
    protections: [{ pattern: "refs/heads/main", direct_push: "persons" }],
    change_counts: { live: 4, open: 3, draft: 1, closed: 0, merged: 9 },
    card: {
      push_mirrored: true,
      last_push: lastPush("2026-11-16T09:35:00Z", "frontend"),
      tip_checks: PASSED,
    },
    spark: SPARK,
  }),
  repo("docs", {
    search: "grep",
    last_push_at: "2026-11-15T08:00:00Z",
    change_counts: { live: 0, open: 0, draft: 0, closed: 0, merged: 0 },
    card: { last_push: lastPush("2026-11-15T08:00:00Z", "cto") },
    spark: Array<number>(14).fill(0),
  }),
  repo("backend.scratch-cache", {
    kind: "scratch",
    owner: { kind: "seat", ref: "backend" },
    head: undefined,
    last_push_at: "2026-11-14T10:00:00Z",
    change_counts: { live: 0, open: 0, draft: 0, closed: 0, merged: 0 },
    card: { last_push: lastPush("2026-11-14T10:00:00Z", "backend", "git", { refused: 1 }) },
    spark: Array<number>(14).fill(0),
  }),
];

/** The engine answering the listing and the kind counts; every request recorded. */
function stubEngine(
  answers: {
    repos?: (url: URL) => unknown;
    counts?: (url: URL) => Response;
  } = {},
) {
  return stubFetch((url) => {
    if (url.pathname === "/query/code_repos") {
      return json(
        answers.repos?.(url) ?? {
          repos: BOARD,
          repos_total: { repos: 3, live_bytes: 68 * 1024 * 1024 },
          position: POSITION,
          level: "stale",
        },
      );
    }
    if (url.pathname === "/query/code_counts") {
      return (
        answers.counts?.(url) ??
        json({
          counts: {
            repos: {
              project: { live: 4, importing: 0, active: 3, archived: 1, deleted: 1, purging: 0 },
              scratch: { live: 1, importing: 0, active: 1, archived: 0, deleted: 0, purging: 0 },
            },
          },
          position: POSITION,
          level: "stale",
        })
      );
    }
    return null;
  });
}

/** A row, by the repository it lists. */
const rowOf = async (name: string) =>
  (await screen.findByRole("link", { name })).closest(".code-list__row") as HTMLElement;

// A ROW SAYS WHAT A CODE HOST'S DOES, IN WORDS: its marks, its owner, its branch with no head id,
// its size, pushes and last push. Mutation: draw the head's id, a vendor mark on Push mirror, or
// the last push as a bare time, and this fails.
test("a row says its marks, owner, branch, size, pushes and last push in words", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-16T10:00:00Z"));
  stubEngine();
  mount(<Repositories />);

  const web = await rowOf("web");
  expect(within(web).getByText("Push mirror")).toBeTruthy();
  expect(web.querySelector("[data-vendor]")).toBeNull();
  expect(within(web).getByText("Engineering")).toBeTruthy();
  expect(within(web).getByText("main")).toBeTruthy();
  expect(within(web).queryByText("0123456")).toBeNull();
  expect(within(web).getByText("18 MiB")).toBeTruthy();
  expect(within(web).getByText("214 pushes")).toBeTruthy();
  const pushed = within(web)
    .getByText(/^pushed/)
    .closest(".code-repo-meta")!;
  expect(pushed.textContent).toBe("pushed 25 minutes ago byFrontend Engineer");
  expect(pushed.querySelector("time")?.getAttribute("datetime")).toBe("2026-11-16T09:35:00Z");
  expect(within(web).queryByText("no protected branch")).toBeNull();

  const docs = await rowOf("docs");
  expect(within(docs).getByText("searched with git grep")).toBeTruthy();
  expect(within(docs).getByText("no protected branch")).toBeTruthy();

  const scratch = await rowOf("backend.scratch-cache");
  expect(within(scratch).getByText("Scratch")).toBeTruthy();
  expect(within(scratch).getByText("No description")).toBeTruthy();
  expect(within(scratch).getAllByText("Backend Engineer").length).toBeGreaterThan(0);
  expect(within(scratch).getByText(", empty")).toBeTruthy();
});

// A ROW'S FACTS ARE THE ENGINE'S: its live changes, its default branch's checks, its pushes by day.
// Mutation: draw a count of zero for a check not kept, or drop the figure's name, and this fails.
test("a row's facts are its live changes, its default branch's checks and its pushes by day", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-16T10:00:00Z"));
  stubEngine();
  mount(<Repositories />);

  const web = await rowOf("web");
  const changes = web.querySelector(".code-repo-facts__count")!;
  expect(changes.getAttribute("title")).toBe("4 live changes: 3 open, 1 draft");
  expect(changes.textContent).toBe("Live changes: 4");
  const checks = within(web).getByText("Passed").closest(".code-check-mark")!;
  expect(checks.getAttribute("title")).toMatch(/^3 checks: 3 passed\. 2 of 2 required passed\./);
  const figure = within(web).getByRole("figure");
  expect(figure.querySelector("figcaption")?.textContent).toBe("web's pushes, last 14 days");
  expect(figure.querySelectorAll(".crewlet-histogram__bar")).toHaveLength(14);
  expect(figure.querySelector(".code-spark__ends")).toBeNull();

  const docs = await rowOf("docs");
  expect(within(docs).getByText("No checks").closest("[title]")?.getAttribute("title")).toBe(
    "No check of main's head is kept: checks are kept 30 days.",
  );
  expect(within(docs).queryByRole("figure")).toBeNull();
  expect(docs.querySelector(".code-repo-facts__quiet")?.getAttribute("title")).toBe(
    "No push in the last 14 days.",
  );
  // AN EMPTY REPOSITORY HAS NO HEAD TO CHECK: no check cell at all.
  const scratch = await rowOf("backend.scratch-cache");
  expect(scratch.querySelector(".code-repo-facts__checks")?.textContent).toBe("");
});

// A FACT NOT ANSWERED IS NOTHING, NEVER ZERO: an older answer's row keeps its cells empty and says
// its last push with no pusher. Mutation: default an absent count to 0, and a "0" is drawn.
test("a fact not answered is drawn as nothing, never as zero", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-16T10:00:00Z"));
  stubEngine({
    repos: () => ({
      repos: [repo("web", { last_push_at: "2026-11-16T08:00:00Z" })],
      position: POSITION,
      level: "stale",
    }),
  });
  mount(<Repositories />);
  const web = await rowOf("web");
  const facts = web.querySelector(".code-repo-facts")!;
  expect(facts.textContent).toBe("");
  expect(within(web).queryByRole("figure")).toBeNull();
  const pushed = within(web)
    .getByText(/^pushed/)
    .closest(".code-repo-meta")!;
  expect(pushed.textContent).toBe("pushed 2 hours ago");
  expect(pushed.getAttribute("title")).toBeNull();
});

// THE LAST PUSHER IS SAID BY THE MOVERS' RULE: Gitlet for a merge, the pusher first when refused,
// and why nobody is named past the history. Mutation: name the merger as the pusher, or guess a
// pusher the card does not hold, and this fails.
test("the last pusher is Gitlet for a merge, named first when refused, and unknown past the history", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-16T10:00:00Z"));
  stubEngine({
    repos: () => ({
      repos: [
        repo("api", { card: { last_push: lastPush("2026-11-16T08:00:00Z", "cto", "merge") } }),
        repo("cache", {
          card: { last_push: lastPush("2026-11-16T08:00:00Z", "backend", "git", { refused: 1 }) },
        }),
        repo("old", { last_push_at: "2026-04-30T10:00:00Z", card: {} }),
        repo("recent", { last_push_at: "2026-11-13T10:00:00Z", card: {} }),
        repo("fresh", { last_push_at: undefined, card: {} }),
      ],
      position: POSITION,
      level: "stale",
    }),
  });
  mount(<Repositories />);
  const api = within(await rowOf("api"))
    .getByText(/^pushed/)
    .closest(".code-repo-meta")!;
  expect(api.textContent).toBe("pushed 2 hours ago byGitlet");
  expect(api.getAttribute("title")).toBe("Gitlet moved main when CTO merged a change");
  const cache = within(await rowOf("cache"))
    .getByText(/^tried to push/)
    .closest(".code-repo-meta")!;
  expect(cache.textContent).toBe("Backend Engineertried to push 2 hours ago");
  expect(cache.getAttribute("title")).toBe("Its last push was refused: nothing moved.");
  const old = within(await rowOf("old"))
    .getByText(/^pushed/)
    .closest(".code-repo-meta")!;
  expect(old.textContent).toBe("pushed on Apr 30");
  expect(old.getAttribute("title")).toBe("Who pushed is no longer kept: pushes are kept 181 days.");
  const recent = within(await rowOf("recent"))
    .getByText(/^pushed/)
    .closest(".code-repo-meta")!;
  expect(recent.getAttribute("title")).toMatch(/the engine's expiries/);
  expect(within(await rowOf("fresh")).getByText("never pushed")).toBeTruthy();
});

// THE KIND IS THE ENGINE'S `kind`, its figures the reader's whole from `code_counts`, never the
// rows loaded. Mutation: count the loaded rows, or draw 0 for a refused count, and this fails.
test("the kind is the engine's kind, with the reader's counts beside it", async () => {
  const asked = stubEngine();
  mount(<Repositories />);
  const kinds = await screen.findByRole("radiogroup", { name: "Which kind of repository" });
  await waitFor(() =>
    expect(within(kinds).getByRole("radio", { name: /Projects/ }).textContent).toBe("Projects4"),
  );
  expect(within(kinds).getByRole("radio", { name: /All/ }).textContent).toBe("All5");
  const counted = asked.find((u) => u.pathname === "/query/code_counts")!;
  expect(counted.searchParams.get("parts")).toBe("repos");

  fireEvent.click(within(kinds).getByRole("radio", { name: /Projects/ }));
  await waitFor(() => expect(location.hash).toBe("#/gitlet/repos?kind=project"));
  await waitFor(() =>
    expect(firstPages(asked).some((u) => u.searchParams.get("kind") === "project")).toBe(true),
  );

  // UNDER A STATE, THAT STATE'S FIGURES.
  act(() => void (location.hash = "#/gitlet/repos?status=archived"));
  await waitFor(() =>
    expect(within(kinds).getByRole("radio", { name: /Projects/ }).textContent).toBe("Projects1"),
  );
});

test("a refused count draws the kind's words without figures, the listing still narrowed", async () => {
  const asked = stubEngine({ counts: () => json({ error: "bad_params" }, 400) });
  mount(<Repositories />, "#/gitlet/repos?kind=scratch");
  const kinds = await screen.findByRole("radiogroup", { name: "Which kind of repository" });
  await settled(() => asked.filter((u) => u.pathname === "/query/code_counts").length);
  expect(within(kinds).getByRole("radio", { name: /Scratch/ }).textContent).toBe("Scratch");
  expect(within(kinds).getByRole("radio", { name: /All/ }).textContent).toBe("All");
  expect(firstPages(asked)[0]!.searchParams.get("kind")).toBe("scratch");
});

// THE SORT IS BY LAST PUSH UNLESS NAME IS CHOSEN, and the address holds only the choice. Mutation:
// write `sort=pushed` into the address, or ask by name by default, and this fails.
test("the sort is by last push unless Name is chosen, and the address keeps only the choice", async () => {
  const asked = stubEngine();
  mount(<Repositories />);
  await rowOf("web");
  fireEvent.click(screen.getByRole("combobox", { name: "Sort" }));
  fireEvent.mouseDown(await screen.findByRole("option", { name: "Sort: Name" }));
  await waitFor(() => expect(location.hash).toBe("#/gitlet/repos?sort=name"));
  await waitFor(() => expect(firstPages(asked).at(-1)!.searchParams.get("sort")).toBe("name"));
  const byName = firstPages(asked).length;

  fireEvent.click(screen.getByRole("combobox", { name: "Sort" }));
  fireEvent.mouseDown(await screen.findByRole("option", { name: "Sort: Last push" }));
  await waitFor(() => expect(location.hash).toBe("#/gitlet/repos"));
  await waitFor(() => expect(firstPages(asked).at(-1)!.searchParams.get("sort")).toBe("pushed"));
  expect(firstPages(asked).length).toBe(byName + 1);
});

// THE FOOTER COUNTS THE WHOLE LISTING, never the rows loaded, and sums its size across pages.
// Mutation: count and sum the loaded rows, and this fails.
test.each([
  [
    "whole",
    { repos: BOARD, repos_total: { repos: 3, live_bytes: 68 * 1024 * 1024 } },
    "3 repositories · 68 MiB",
    null,
  ],
  [
    "paged",
    { repos: BOARD, next: "x", repos_total: { repos: 230, live_bytes: 1.4 * 1024 ** 3 } },
    "3 of 230 repositories loaded, newest push first · 1.4 GiB in all",
    null,
  ],
  [
    "unsized",
    { repos: BOARD, repos_total: { repos: 12_000 } },
    "12,000 repositories",
    "Their size is not summed past 10,000 repositories.",
  ],
  ["older", { repos: BOARD, next: "x" }, "3 repositories loaded", null],
])("the footer counts the whole listing: %s", async (_case, answer, words, title) => {
  stubEngine({ repos: () => ({ ...answer, position: POSITION, level: "stale" }) });
  mount(<Repositories />);
  await rowOf("web");
  const note = document.querySelector(".code-list__note")!;
  expect(note.textContent).toBe(words);
  expect(note.querySelector("[title]")?.getAttribute("title") ?? null).toBe(title);
});

// LOAD MORE KEEPS THE FIRST PAGE'S TOTALS, the only page that carries them. Mutation: read the
// totals from the last page loaded, and the footer falls back to the rows loaded.
test("Load more keeps the footer's whole count and size", async () => {
  stubEngine({
    repos: (url) =>
      url.searchParams.get("cursor") === "x"
        ? { repos: [repo("yak"), repo("zeta")], next: "y", position: POSITION, level: "stale" }
        : {
            repos: BOARD,
            next: "x",
            repos_total: { repos: 230, live_bytes: 1.4 * 1024 ** 3 },
            position: POSITION,
            level: "stale",
          },
  });
  mount(<Repositories />);
  await rowOf("web");
  fireEvent.click(screen.getByRole("button", { name: "Load more repositories" }));
  await rowOf("zeta");
  expect(document.querySelector(".code-list__note")!.textContent).toBe(
    "5 of 230 repositories loaded, newest push first · 1.4 GiB in all",
  );
});

// A FLEET THAT REFUSES THE FACTS SAYS SO IN THE ENGINE'S WORDS, and the listing is never asked
// again without them. Mutation: ask again without `day_bounds`, and a second read is made.
test("a fleet that refuses the listing's facts says so, and asks nothing without them", async () => {
  const asked = stubFetch((url) =>
    url.pathname === "/query/code_repos"
      ? json({ error: "bad_params", detail: "day_bounds is not a key this node reads" }, 400)
      : null,
  );
  mount(<Repositories />);
  expect(
    await screen.findByText(
      "Listing repositories with their activity is unavailable on this fleet yet: day_bounds is not a key this node reads.",
    ),
  ).toBeTruthy();
  await afterMicrotasks();
  const listings = asked.filter((u) => u.pathname === "/query/code_repos");
  expect(listings).toHaveLength(1);
  expect(listings[0]!.searchParams.has("day_bounds")).toBe(true);
  expect(screen.getByRole("radiogroup", { name: "Which kind of repository" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "New repository" })).toBeTruthy();
});

// J AND K WALK THE REPOSITORIES AND ENTER OPENS ONE, the list's own keys. Mutation: hand the rows
// no link, and Enter opens nothing.
test("j and k walk the repositories and Enter opens one", async () => {
  stubEngine();
  mount(<Repositories />);
  await rowOf("web");
  const opened: (string | null)[] = [];
  const watch = (e: MouseEvent) => {
    opened.push((e.target as Element).closest("a")?.getAttribute("href") ?? null);
    e.preventDefault();
  };
  document.addEventListener("click", watch);
  const press = (key: string) => fireEvent.keyDown(window, { key });
  press("j");
  press("j");
  press("j");
  press("k");
  (document.activeElement as HTMLElement | null)?.blur();
  press("Enter");
  document.removeEventListener("click", watch);
  expect(opened).toEqual(["#/gitlet/repos/docs"]);
});

// NEW AND IMPORT ARE IN THE PAGE BAR, New the view's one primary. Mutation: draw Import as a
// primary too, and two primaries compete.
test("New repository and Import a repository are in the page bar, New the one primary", async () => {
  stubEngine();
  mount(<Repositories />);
  const bar = (await screen.findByRole("button", { name: "New repository" })).closest(
    ".page-actions",
  ) as HTMLElement;
  expect(bar).toBeTruthy();
  const buttons = within(bar).getAllByRole("button");
  expect(buttons.map((b) => b.textContent)).toEqual(["Import a repository", "New repository"]);
  expect(bar.querySelectorAll(".crewlet-btn--primary")).toHaveLength(1);
  expect(within(bar).getByRole("button", { name: "New repository" }).classList).toContain(
    "crewlet-btn--primary",
  );
});

// THE SPARK'S DAYS MOVE WHEN THE READER'S DAY TURNS, and not before: one clock, one new ask.
// Mutation: key the listing on the minute or the hour, and it is asked again within the day.
test("the spark's days move when the reader's day turns, and not before", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-16T12:00:00Z"));
  pinZoneForTest("UTC");
  const asked = stubEngine();
  mount(<Repositories />);
  await rowOf("web");
  const days = () =>
    [...new Set(firstPages(asked).map((u) => u.searchParams.get("day_bounds")))].length;
  expect(days()).toBe(1);
  const before = firstPages(asked).length;
  act(() => {
    vi.setSystemTime(new Date("2026-11-16T23:59:00Z"));
    document.dispatchEvent(new Event("visibilitychange"));
  });
  await afterMicrotasks();
  expect(days()).toBe(1);
  expect(firstPages(asked).length).toBe(before);
  act(() => {
    vi.setSystemTime(new Date("2026-11-17T00:01:00Z"));
    document.dispatchEvent(new Event("visibilitychange"));
  });
  await waitFor(() => expect(days()).toBe(2));
  const last = firstPages(asked).at(-1)!.searchParams.get("day_bounds")!.split(",");
  expect(last[14]).toBe("2026-11-18T00:00:00Z");
});
