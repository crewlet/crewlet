/**
 * A repository's page reads the repository's row and its content over REST,
 * each at the revision and the path its address names.
 *
 * The invariants worth breaking a build over: the content is read at the
 * address's `ref=` and `path=` and nothing else; a Permalink names the commit
 * the ref resolved to, whole; the lines an address picks are the lines marked,
 * and pressing a line's number writes them to the address; the blame is read
 * at the commit the file was; a branch opens its files; the history is read
 * at the revision and the path; a declared repository's settings are shown,
 * never edited, with the way to Settings › Configuration; a tool-made
 * repository's search mode is one settings change; a purge names who keeps
 * copies before it is asked and only its confirmation sends it; a reader
 * holding no human seat is held from a mirror and a purge; and the Overview
 * says how far an import went and where the mirrors stand.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { onTheRight } from "~/test/codeScreens.tsx";
import { afterEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { DECLARED, PERSON_ONLY, PURGE_KEEPERS } from "~/lib/repoWrites.ts";
import { RepositoryPage } from "./RepositoryPage.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "";
});

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };
const HEAD = "0123456789abcdef0123456789abcdef01234567";
const OTHER = "89abcdef0123456789abcdef0123456789abcdef";

const ORG = withDerived({
  name: "Acme",
  roles: [{ name: "CTO", handle: "cto" }],
  units: [{ id: "engineering", name: "Engineering", roles: [] }],
  services: { tracker: "worklet", knowledge: "doclet", chat: "chatlet", code: "gitlet" },
}) as unknown as OrgProjection;

/** The repository as `code_repo` answers it, with what a case overrides. */
function detail(extra: Record<string, unknown> = {}) {
  return {
    id: "id-web",
    name: "web",
    status: "active",
    managed_by: "config",
    kind: "project",
    owner: { kind: "unit", ref: "engineering" },
    visibility: "company",
    search: "indexed",
    default_branch: "main",
    description: "The storefront",
    refs: 3,
    live_packs: 2,
    live_bytes: 2048,
    live_objects: 40,
    position: POSITION,
    access: "maintain",
    head: HEAD,
    pushes: 5,
    last_push_at: "2026-10-27T10:00:00Z",
    branches: 2,
    tags: 1,
    tombstones: 0,
    created_at: "2026-10-01T10:00:00Z",
    protections: [{ pattern: "refs/heads/main", direct_push: "persons" }],
    gates: [
      {
        pattern: "refs/heads/main",
        gate: {
          merge_by: "writers",
          approvals: "one",
          checks: ["unit"],
          landing: { depth: 1, batch: 1 },
          delete_source: "seats",
        },
      },
    ],
    rule_sources: [
      { pattern: "refs/heads/main", set_by_kind: "config", set_at: "2026-10-01T10:00:00Z" },
    ],
    path_owners: [{ owner: { kind: "unit", ref: "engineering" }, paths: ["src/**"] }],
    ...extra,
  };
}

const json = (payload: unknown, status = 200) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });

/** A read of content by a ref, as the engine answers one: bare, with the ref it resolved. */
const byRef = (url: URL, part: Record<string, unknown>) => ({
  repo: "web",
  ...part,
  ...(url.searchParams.get("ref")?.length === 40
    ? {}
    : {
        resolved: {
          ref: { ref: `refs/heads/${url.searchParams.get("ref") ?? "main"}`, oid: HEAD },
        },
        position: POSITION,
      }),
});

/** A file longer than a page, its lines each `line <n>`. */
const BIG_FILE = "src/big.go";
const BIG_LINES = 3000;
/** The lines one page of the big file holds, as about 56 KiB of ordinary source does. */
const BIG_PAGE_LINES = 1500;
/** The lines one page of its blame holds: two pages a blame's range of 500. */
const BIG_BLAME_PAGE_LINES = 250;

const bigText = (from: number, to: number) =>
  Array.from({ length: to - from + 1 }, (_, i) => `line ${from + i}\n`).join("");

/**
 * The range a read of the big file asks, as the engine holds it: `start_line`
 * and `end_line` (each from 1), and a cursor `<letter><line>` that must name a
 * line inside them — refused otherwise, as the engine refuses one.
 */
function bigRange(url: URL, defaultEnd: (start: number) => number, letter: string) {
  const p = url.searchParams;
  const start = Number(p.get("start_line") ?? 1);
  const end = Number(p.get("end_line") ?? defaultEnd(start));
  const cursor = p.get("cursor");
  const from = cursor ? Number(cursor.slice(1)) : start;
  const ok =
    start <= BIG_LINES && from >= start && from <= end && (!cursor || cursor[0] === letter);
  return { start, end, from, ok };
}

/** A page of the big file: from the range's first line or the cursor's, at most BIG_PAGE_LINES. */
function bigFileRead(url: URL) {
  const { end, from, ok } = bigRange(url, () => BIG_LINES, "l");
  if (!ok) return json({ error: "bad_params" }, 400);
  const to = Math.min(from + BIG_PAGE_LINES - 1, end);
  return json(
    byRef(url, {
      kind: "blob",
      ...(to < end ? { next: `l${to + 1}` } : {}),
      blob: {
        path: BIG_FILE,
        oid: OTHER,
        size: bigText(1, BIG_LINES).length,
        lines: BIG_LINES,
        start_line: from,
        end_line: to,
        text: bigText(from, to),
      },
    }),
  );
}

/**
 * A page of the big file's blame: a range of at most 500 lines (the engine's
 * gitlet.MaxBlameLines), the default 500 from its first line, paged within it
 * BIG_BLAME_PAGE_LINES at a time — a range any wider, or a cursor outside it,
 * refused as the engine refuses them.
 */
function bigBlameRead(url: URL) {
  const { start, end, from, ok } = bigRange(url, (s) => Math.min(s + 499, BIG_LINES), "b");
  if (!ok || end - start + 1 > 500) return json({ error: "bad_params" }, 400);
  const to = Math.min(from + BIG_BLAME_PAGE_LINES - 1, end);
  return json(
    byRef(url, {
      kind: "blame",
      ...(to < end ? { next: `b${to + 1}` } : {}),
      blame: {
        path: BIG_FILE,
        start_line: from,
        end_line: to,
        lines: Array.from({ length: to - from + 1 }, (_, i) => ({
          line: from + i,
          commit: HEAD,
          orig_line: from + i,
          text: `line ${from + i}`,
        })),
        commits: { [HEAD]: { author: "Ada", authored_at: "2026-10-20T10:00:00Z" } },
      },
    }),
  );
}

/**
 * The engine: the repository, its refs, a root holding `src/` and a README, a
 * file of three lines and one of BIG_LINES, their blames and a history — and
 * every request made.
 */
function stubEngine(repo: Record<string, unknown> = detail(), acted: Acted[] = []) {
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      asked.push(url);
      const p = url.searchParams;
      if (url.pathname.startsWith("/operator/act/")) {
        acted.push({
          tool: decodeURIComponent(url.pathname.split("/operator/act/")[1] ?? ""),
          args: (JSON.parse(String(init?.body)) as { args: Record<string, unknown> }).args,
        });
        return json({ outcome: "applied" });
      }
      switch (url.pathname) {
        case "/auth/session":
          return new Promise<Response>(() => {});
        case "/query/code_repo": {
          // THE PATH OWNERS AND THE GATES ONLY TO A QUESTION THAT ASKS, as
          // the engine answers them, who set each rule beside the gates.
          const { gates, rule_sources, path_owners, ...row } = repo;
          return json({
            repo: {
              ...row,
              ...(p.get("gates") === "true" ? { gates, rule_sources } : {}),
              ...(p.get("path_owners") === "true" ? { path_owners } : {}),
            },
            position: POSITION,
            level: "session",
          });
        }
        case "/query/code_refs":
          return json({
            refs:
              p.get("prefix") === "refs/tags/"
                ? [{ ref: "refs/tags/v1", oid: OTHER, position: POSITION }]
                : [
                    { ref: "refs/heads/feature", oid: OTHER, position: POSITION },
                    { ref: "refs/heads/main", oid: HEAD, position: POSITION },
                  ],
            position: POSITION,
            level: "session",
          });
        case "/query/code_tree":
          return json(
            byRef(url, {
              kind: "tree",
              tree: {
                tree: OTHER,
                entries: [
                  { mode: "100644", type: "blob", oid: OTHER, path: "README.md", size: 20 },
                  { mode: "040000", type: "tree", oid: OTHER, path: "src" },
                ],
                total: 2,
              },
            }),
          );
        case "/query/code_blob":
          if (p.get("path") === "README.md") {
            return json(
              byRef(url, {
                kind: "blob",
                blob: {
                  path: "README.md",
                  oid: OTHER,
                  size: 20,
                  lines: 1,
                  text: "# Hello storefront\n",
                },
              }),
            );
          }
          if (p.get("path") === "src") {
            return json(
              byRef(url, {
                kind: "tree",
                tree: {
                  tree: OTHER,
                  path: "src",
                  entries: [
                    { mode: "100644", type: "blob", oid: OTHER, path: "src/app.go", size: 30 },
                  ],
                  total: 1,
                },
              }),
            );
          }
          if (p.get("path") === BIG_FILE) return bigFileRead(url);
          return json(
            byRef(url, {
              kind: "blob",
              blob: {
                path: "src/app.go",
                oid: OTHER,
                size: 30,
                lines: 3,
                start_line: 1,
                end_line: 3,
                text: "package app\n\nfunc Run() {}\n",
              },
            }),
          );
        case "/query/code_blame":
          if (p.get("path") === BIG_FILE) return bigBlameRead(url);
          return json(
            byRef(url, {
              kind: "blame",
              blame: {
                path: "src/app.go",
                start_line: 1,
                end_line: 3,
                lines: [
                  { line: 1, commit: HEAD, orig_line: 1, text: "package app" },
                  { line: 2, commit: HEAD, orig_line: 2, text: "" },
                  { line: 3, commit: OTHER, orig_line: 3, text: "func Run() {}" },
                ],
                commits: {
                  [HEAD]: { author: "Ada", authored_at: "2026-10-20T10:00:00Z", subject: "Start" },
                  [OTHER]: { author: "Grace", authored_at: "2026-10-21T10:00:00Z", subject: "Run" },
                },
              },
            }),
          );
        case "/query/code_log":
          return json(
            byRef(url, {
              kind: "log",
              ...(p.get("cursor") ? {} : { next: "c1" }),
              log: {
                commits: p.get("cursor")
                  ? [
                      {
                        oid: OTHER,
                        author: "Grace",
                        authored_at: "2026-10-21T10:00:00Z",
                        committed_at: "2026-10-21T10:00:00Z",
                        subject: "Older work",
                      },
                    ]
                  : [
                      {
                        oid: HEAD,
                        author: "Ada",
                        authored_at: "2026-10-22T10:00:00Z",
                        committed_at: "2026-10-22T10:00:00Z",
                        subject: "Start the storefront",
                      },
                    ],
                scanned: 1,
              },
              pushed_by: p.get("cursor")
                ? {}
                : {
                    [HEAD]: {
                      push_id: "p1",
                      position: POSITION,
                      at: "2026-10-22T10:00:00Z",
                      principal: { kind: "agent", seat: "cto" },
                      via: "git",
                    },
                  },
            }),
          );
        case "/query/code_pushes":
          return json({ position: POSITION, level: "session" });
      }
      return json({}, 404);
    }),
  );
  return asked;
}

/** One write the page sent, as `POST /operator/act/{tool}` carried it. */
interface Acted {
  tool: string;
  args: Record<string, unknown>;
}

/** A person the engine serves the repository writes for: Jane, an admin on her own human seat. */
const WRITER = {
  ...ADMIN_VIEWER,
  acts: ["code_update_repo", "code_purge_unreachable", "code_import_repo"],
};

function mount(hash: string, repo = "web", viewer: unknown = ADMIN_VIEWER) {
  location.hash = hash;
  const store = new Store();
  store.applyOrg(ORG);
  store.setConnected(true);
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" ? Promise.resolve(viewer) : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <RepositoryPage repo={repo} />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

const reads = (asked: URL[], what: string) => asked.filter((u) => u.pathname === `/query/${what}`);

// THE OVERVIEW IS THE REPOSITORY'S ROW AND ITS README: the header names the
// repository and its owner off the chart, and the README at the default
// branch's root is read at the commit that listing resolved, rendered.
// Mutation: read the README at the branch rather than the commit the root was
// listed at, and the two could be two revisions — this fails.
test("the overview is the repository's row and the README at the commit its root was listed at", async () => {
  const asked = stubEngine();
  mount("#/gitlet/repos/web");

  expect(await screen.findByRole("heading", { name: "Hello storefront" })).toBeTruthy();
  // THE FILE IS NAMED AS IT IS: never in the label register, whose capitals
  // would print README.MD.
  const name = screen.getByRole("link", { name: "README.md" });
  expect(name.closest(".t-label")).toBeNull();
  expect(screen.getByText("Engineering")).toBeTruthy();
  expect(screen.getByText("The storefront")).toBeTruthy();
  const readme = reads(asked, "code_blob").find((u) => u.searchParams.get("path") === "README.md");
  expect(readme?.searchParams.get("ref")).toBe(HEAD);
  expect(reads(asked, "code_repo")[0]?.searchParams.get("name")).toBe("web");
});

// A PATH IS ONE READ, AT THE ADDRESS'S REVISION: a directory under the root
// is asked as a file and drawn as the listing it is, each entry opening its
// own path at the same revision. Mutation: drop `ref` from the read, or link
// an entry without it, and this fails.
test("a directory is read at the address's revision and its entries open at it", async () => {
  const asked = stubEngine();
  mount("#/gitlet/repos/web?tab=files&ref=feature&path=src");

  const table = await screen.findByRole("table", { name: "Files" });
  const link = within(table).getByRole("link", { name: "app.go" });
  expect(link.getAttribute("href")).toBe(
    "#/gitlet/repos/web?tab=files&ref=feature&path=src%2Fapp.go",
  );
  const read = reads(asked, "code_blob").find((u) => u.searchParams.get("path") === "src");
  expect(read?.searchParams.get("ref")).toBe("feature");
});

// A DIRECTORY IS A LIST LIKE EVERY OTHER: the data grid, its sizes on the
// right edge, and its heads sorting the entries once the whole directory is
// here. Mutation: draw the hand-built table again, or leave Size at the
// start, and this fails.
test("a directory's entries are drawn on the data grid, sortable once the whole directory is here", async () => {
  stubEngine();
  mount("#/gitlet/repos/web?tab=files");

  const table = await screen.findByRole("table", { name: "Files" });
  expect(table.classList.contains("crewlet-data-grid")).toBe(true);
  expect(onTheRight(table, "Size")).toBe(true);
  const name = within(table).getByRole("columnheader", { name: /Name/ });
  expect(within(name).getByRole("button")).toBeTruthy();
});

// THE BRANCHES AND TAGS ARE READ ONCE FOR THE PAGE, never once a picker: a
// move from the root to a path draws another picker, and so does the
// Commits tab, and none of them asks either listing again — each is up to a
// thousand refs. Mutation: let each picker read its own listings, and this
// fails.
test("the revision picker's branches and tags are read once while the reader moves", async () => {
  const asked = stubEngine();
  mount("#/gitlet/repos/web?tab=files");

  await screen.findByRole("link", { name: "src/" });
  await waitFor(() => expect(reads(asked, "code_refs")).toHaveLength(2));
  act(() => {
    location.hash = "#/gitlet/repos/web?tab=files&path=src";
  });
  expect(await screen.findByRole("link", { name: "app.go" })).toBeTruthy();
  act(() => {
    location.hash = "#/gitlet/repos/web?tab=commits";
  });
  const history = await screen.findByRole("table", { name: "History of main" });
  expect(onTheRight(history, "Authored")).toBe(true);

  const listings = reads(asked, "code_refs").map((u) => u.searchParams.get("prefix"));
  expect(listings.sort()).toEqual([null, "refs/tags/"]);
});

// THE LINES AN ADDRESS PICKS ARE MARKED, a press on a number writes its line
// to the address, Shift extends the run, and the Permalink names the commit
// the branch resolved to, whole, with the lines. Mutation: mark by index
// rather than line number, or build the Permalink from the branch, and this
// fails.
test("a file's picked lines are its address, and its Permalink names the commit", async () => {
  stubEngine();
  mount("#/gitlet/repos/web?tab=files&path=src%2Fapp.go&lines=2");

  const line2 = await screen.findByRole("button", { name: "Line 2" });
  expect(line2.getAttribute("aria-pressed")).toBe("true");
  expect(screen.getByRole("button", { name: "Line 1" }).getAttribute("aria-pressed")).toBe("false");
  expect(screen.getByText("func Run() {}")).toBeTruthy();

  const permalink = screen.getByRole("link", { name: "Permalink" });
  expect(permalink.getAttribute("href")).toBe(
    `#/gitlet/repos/web?tab=files&ref=${HEAD}&path=src%2Fapp.go&lines=2`,
  );

  fireEvent.click(screen.getByRole("button", { name: "Line 3" }), { shiftKey: true });
  await waitFor(() => expect(location.hash).toContain("lines=2-3"));
  fireEvent.click(screen.getByRole("button", { name: "Line 1" }));
  await waitFor(() => expect(location.hash).toContain("lines=1"));
  expect(location.hash).not.toContain("lines=1-");
});

// A FILE'S TEXT IS SPLIT INTO LINES ONCE A PAGE, never on a render that
// brought no page: a press on a line writes the address and draws the screen
// again, and a file read to its end is a mebibyte split on every such press.
// Mutation: build the file's pages afresh on every render, or split them
// without keeping the lines, and this fails.
test("a press on a line does not split the file's text again", async () => {
  stubEngine();
  mount("#/gitlet/repos/web?tab=files&path=src%2Fapp.go");

  const line1 = await screen.findByRole("button", { name: "Line 1" });
  const split = vi.spyOn(String.prototype, "split");
  try {
    fireEvent.click(line1);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Line 1" }).getAttribute("aria-pressed")).toBe(
        "true",
      ),
    );
    const file = "package app\n\nfunc Run() {}\n";
    expect(split.mock.contexts.filter((self) => String(self) === file)).toHaveLength(0);
  } finally {
    split.mockRestore();
  }
});

// THE BLAME IS READ AT THE COMMIT THE FILE WAS, and each run of lines one
// commit changed last is headed by that commit and its author once.
// Mutation: blame the branch, or head every line, and this fails.
test("the blame is read at the file's commit and heads each run of lines once", async () => {
  const asked = stubEngine();
  mount("#/gitlet/repos/web?tab=files&path=src%2Fapp.go&lens=blame");

  const table = await screen.findByRole("table", { name: /Who last changed each line/ });
  expect(within(table).getAllByText("Ada")).toHaveLength(1);
  expect(within(table).getAllByText("Grace")).toHaveLength(1);
  await waitFor(() => expect(reads(asked, "code_blame").length).toBeGreaterThan(0));
  expect(reads(asked, "code_blame")[0]?.searchParams.get("ref")).toBe(HEAD);
});

/** Watch what is scrolled into view: jsdom lays nothing out, and has no scrollIntoView. */
function watchScrolls(): Element[] {
  const scrolled: Element[] = [];
  Element.prototype.scrollIntoView = function (this: Element) {
    scrolled.push(this);
  };
  return scrolled;
}

afterEach(() => {
  delete (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView;
});

/** The `start_line` of every read of the big file a kind of read asked, in order: null where it named none. */
const bigStarts = (asked: URL[], what: string) =>
  reads(asked, what)
    .filter((u) => u.searchParams.get("path") === BIG_FILE)
    .map((u) => u.searchParams.get("start_line"));

// A LINK TO A LINE PAST A FILE'S FIRST PAGE OPENS AT IT — a search hit, ⌘K, a
// Permalink — in ONE read: the path's own read asks from PICK_CONTEXT lines
// before the line (`start_line`), since every read of a file is a stream of
// the whole of it from git, the line is drawn picked and scrolled into view,
// and the lines before are read back a press at a time, each step a range
// that meets the lines after it. Mutation: read the path from line 1 first
// (no `start_line`), or leave the earlier step's end short of the lines
// drawn, and this fails.
test("a file opens at a picked line past its first page, and reads back from there", async () => {
  const scrolled = watchScrolls();
  const asked = stubEngine();
  mount(`#/gitlet/repos/web?tab=files&path=src%2Fbig.go&lines=2400`);

  const line = await screen.findByRole("button", { name: "Line 2400" });
  expect(line.getAttribute("aria-pressed")).toBe("true");
  expect(screen.getByText("line 2400")).toBeTruthy();
  await waitFor(() => expect(scrolled.map((e) => e.id)).toContain("L2400"));
  expect(bigStarts(asked, "code_blob")).toEqual(["2350"]);
  expect(screen.queryByRole("button", { name: "Line 1" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Line 2349" })).toBeNull();

  // A STEP BACK IS A PAGE'S WORTH OF LINES, EARLIER_FILE_LINES: each step
  // costs the engine one whole-file stream whatever its width.
  fireEvent.click(screen.getByRole("button", { name: "Load earlier lines" }));
  expect(await screen.findByRole("button", { name: "Line 850" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Line 2349" })).toBeTruthy();
  const back = reads(asked, "code_blob").filter((u) => u.searchParams.has("end_line"));
  expect(
    back.map((u) => [u.searchParams.get("start_line"), u.searchParams.get("end_line")]),
  ).toEqual([["850", "2349"]]);
  // A PRESS ON A LINE DRAWN MOVES NOTHING: no read, no scroll.
  const before = asked.length;
  fireEvent.click(screen.getByRole("button", { name: "Line 1900" }));
  await waitFor(() => expect(location.hash).toContain("lines=1900"));
  expect(asked.length).toBe(before);
  expect(scrolled.map((e) => e.id)).toEqual(["L2400"]);
});

// A FILE'S BLAME OPENS AT THE PICKED LINE TOO, its FIRST range the one that
// holds it — a blame walks the file's history for every line it names, so a
// range from line 1 that only said where to ask next is a walk nothing draws
// — and reads range after range: a blame names at most CODE_MAX_BLAME_LINES
// lines, so its lines past a range are read from the line after it, and a
// page inside a range is asked with the range's own first line. Mutation: page
// the blame by the engine's cursor alone, or open it from line 1 first, and
// this fails.
test("a blame opens at a picked line past its first range and reads on across ranges", async () => {
  const scrolled = watchScrolls();
  const asked = stubEngine();
  mount(`#/gitlet/repos/web?tab=files&path=src%2Fbig.go&lens=blame&lines=2400`);

  const line = await screen.findByRole("button", { name: "Line 2400" });
  expect(line.getAttribute("aria-pressed")).toBe("true");
  await waitFor(() => expect(scrolled.map((e) => e.id)).toContain("L2400"));
  const ranges = () =>
    reads(asked, "code_blame").map((u) => [
      u.searchParams.get("start_line"),
      u.searchParams.get("cursor"),
    ]);
  expect(ranges()).toEqual([["2350", null]]);
  expect(bigStarts(asked, "code_blob")).toEqual(["2350"]);
  expect(screen.queryByRole("button", { name: "Line 2600" })).toBeNull();

  // ON PAST THE RANGE'S FIRST PAGE, then into the next range.
  fireEvent.click(screen.getByRole("button", { name: "Load more lines" }));
  expect(await screen.findByRole("button", { name: "Line 2600" })).toBeTruthy();
  expect(ranges()).toContainEqual(["2350", "b2600"]);
  fireEvent.click(screen.getByRole("button", { name: "Load more lines" }));
  expect(await screen.findByRole("button", { name: "Line 2850" })).toBeTruthy();
  expect(ranges()).toContainEqual(["2850", null]);

  fireEvent.click(screen.getByRole("button", { name: "Load earlier lines" }));
  expect(await screen.findByRole("button", { name: "Line 1850" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Line 2349" })).toBeTruthy();
  // NO BLAME FROM LINE 1 was ever asked: every range named its first line.
  expect(bigStarts(asked, "code_blame")).not.toContain(null);
});

// A LINK TO A LINE THE FILE DOES NOT HAVE — a stale one — is refused by the
// engine as a `start_line` past the file's end, and the file is read again
// from line 1, its caption saying the line is not there rather than the
// refusal standing in for the file. Mutation: keep the refused line, or read
// the file again from it, and this fails.
test("a link past a file's end opens the file from its first line, saying so", async () => {
  const asked = stubEngine();
  mount(`#/gitlet/repos/web?tab=files&path=src%2Fbig.go&lines=3100`);

  expect(await screen.findByRole("button", { name: "Line 1" })).toBeTruthy();
  expect(screen.getByText(/it has no line 3100/)).toBeTruthy();
  expect(bigStarts(asked, "code_blob")).toEqual(["3050", null]);
});

// A BRANCH OPENS ITS FILES: the default branch at the bare Files address, any
// other by its name. Mutation: link the default branch by name, or every
// branch to the default, and this fails.
test("the branches are the engine's refs, each opening its files", async () => {
  stubEngine();
  mount("#/gitlet/repos/web?tab=branches");

  const grid = await screen.findByRole("table", { name: "Branches of web" });
  expect(within(grid).getByText("default")).toBeTruthy();
  const links = within(grid)
    .getAllByRole("link")
    .map((a) => a.getAttribute("href"));
  expect(links).toContain("#/gitlet/repos/web?tab=files&ref=feature");
  expect(links).toContain("#/gitlet/repos/web?tab=files");
});

// THE HISTORY IS READ AT THE REVISION AND THE PATH, with who pushed each
// commit from the page that named it, and Load older asks the cursor.
// Mutation: drop the path, read who pushed off the commit's author, or link a
// commit anywhere but its own page, and this fails.
test("the history is read at the revision and the path, with who pushed each commit", async () => {
  const asked = stubEngine();
  mount("#/gitlet/repos/web?tab=commits&ref=feature&path=src");

  const grid = await screen.findByRole("table", { name: "History of feature" });
  expect(within(grid).getByText("Start the storefront")).toBeTruthy();
  expect(within(grid).getByText("CTO")).toBeTruthy();
  // EACH COMMIT OPENS ITS OWN PAGE, by its whole id.
  expect(
    within(grid)
      .getAllByRole("link")
      .map((a) => a.getAttribute("href")),
  ).toContain(`#/gitlet/repos/web/commits/${HEAD}`);
  const first = reads(asked, "code_log")[0];
  expect(first?.searchParams.get("ref")).toBe("feature");
  expect(first?.searchParams.get("path")).toBe("src");

  fireEvent.click(await screen.findByRole("button", { name: /load more/i }));
  expect(await within(grid).findByText("Older work")).toBeTruthy();
  expect(reads(asked, "code_log").some((u) => u.searchParams.get("cursor") === "c1")).toBe(true);
});

// A DECLARED REPOSITORY'S SETTINGS ARE SHOWN, NEVER EDITED: the rules and
// their gates as the detail carries them — a gate requiring a check with how
// its queue lands a change — with who set each rule (a declared one, the
// company document), and the way to Settings › Configuration, where they are
// changed. Mutation: offer an edit, drop the link, leave the queue's landing
// out of the gate, or drop who set a rule, and this fails.
test("a declared repository's settings are shown and lead to the configuration", async () => {
  const asked = stubEngine();
  mount("#/gitlet/repos/web?tab=settings");

  expect(await screen.findByText("refs/heads/main")).toBeTruthy();
  expect(await screen.findByText(/one approval/)).toBeTruthy();
  expect(screen.getByText(/^Set by the company document, /)).toBeTruthy();
  expect(screen.getByText("src/**")).toBeTruthy();
  // THE GATES AND THE PATH OWNERS ARE THE TAB'S OWN ASK: the page's polled
  // read of the row asks for neither (at their bound, three quarters of a
  // mebibyte of JSON a minute).
  const asks = reads(asked, "code_repo").map(
    (u) => `${u.searchParams.get("path_owners")}/${u.searchParams.get("gates")}`,
  );
  expect(new Set(asks)).toEqual(new Set(["null/null", "true/true"]));
  expect(screen.getByText(/landed by its queue, each change tested merged/)).toBeTruthy();
  expect(screen.getByRole("link", { name: "Open the configuration" }).getAttribute("href")).toBe(
    "#/settings/config",
  );
  expect(screen.queryByRole("textbox")).toBeNull();
});

// A NAME NO REPOSITORY CAN HAVE IS SAID IN PLACE and never asked about.
// Mutation: ask the engine anyway, and this fails.
test("a name no repository can have is said in place and never asked", async () => {
  const asked = stubEngine();
  mount("#/gitlet/repos/Not_A_Repo", "Not_A_Repo");
  expect(await screen.findByText("Not a repository")).toBeTruthy();
  expect(reads(asked, "code_repo")).toHaveLength(0);
});

// A DECLARED REPOSITORY'S SEARCH MODE AND MIRRORS ARE HELD WITH THE
// DOCUMENT'S SENTENCE, drawn and never pressable, while its purge — which
// changes none of its settings — is offered. Mutation: let a declared
// repository's mode or mirror through, hold its purge, or hide a held control,
// and this fails.
test("a declared repository's search mode and mirrors are held, and its purge is offered", async () => {
  const acted: Acted[] = [];
  stubEngine(detail({ search: "grep" }), acted);
  mount("#/gitlet/repos/web?tab=settings", "web", WRITER);

  const index = await screen.findByRole("button", { name: "Index it" });
  await waitFor(() => expect(index.getAttribute("title")).toBe(DECLARED));
  expect(screen.getByRole("button", { name: "Make it a pull mirror" }).getAttribute("title")).toBe(
    DECLARED,
  );
  expect(screen.getByRole("button", { name: "Add a push mirror" }).getAttribute("title")).toBe(
    DECLARED,
  );
  fireEvent.click(index);
  expect(acted).toHaveLength(0);
  expect(
    screen.getByRole("button", { name: "Purge unreachable history" }).getAttribute("title"),
  ).toBeNull();
});

// A TOOL-MADE REPOSITORY'S SEARCH MODE IS ONE code_update_repo NAMING IT AND
// THE MODE, and the mode it has is held as no change. Mutation: send another
// argument, the mode it has, or let the held mode through, and this fails.
test("a tool-made repository's search mode is one settings change naming the mode", async () => {
  const acted: Acted[] = [];
  stubEngine(detail({ managed_by: "tools", search: "grep" }), acted);
  mount("#/gitlet/repos/web?tab=settings", "web", WRITER);

  const grep = await screen.findByRole("button", { name: "Search it with grep" });
  await waitFor(() => expect(grep.getAttribute("title")).toMatch(/searched this way already/));
  fireEvent.click(grep);
  const index = screen.getByRole("button", { name: "Index it" });
  await waitFor(() => expect(index.getAttribute("title")).toBeNull());
  fireEvent.click(index);
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({ tool: "code_update_repo", args: { repo: "web", search: "indexed" } });
});

// THE PURGE NAMES WHO KEEPS COPIES BEFORE IT IS ASKED, and only its Confirm
// sends it: ONE code_purge_unreachable naming the repository and nothing else,
// never on the first press and never on Keep it. Mutation: send on the first
// press, drop a keeper from the confirmation, or send more than the repository,
// and this fails.
test("a purge names every keeper of a copy before it is asked, and only Purge sends it", async () => {
  const acted: Acted[] = [];
  stubEngine(detail({ managed_by: "tools" }), acted);
  mount("#/gitlet/repos/web?tab=settings", "web", WRITER);

  const purge = await screen.findByRole("button", { name: "Purge unreachable history" });
  await waitFor(() => expect(purge.getAttribute("title")).toBeNull());
  fireEvent.click(purge);
  const dialog = await screen.findByRole("alertdialog");
  for (const keeper of PURGE_KEEPERS) expect(dialog.textContent).toContain(keeper);
  expect(acted).toHaveLength(0);
  fireEvent.click(within(dialog).getByRole("button", { name: "Keep it" }));
  await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  expect(acted).toHaveLength(0);

  fireEvent.click(purge);
  fireEvent.click(
    within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Purge" }),
  );
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({ tool: "code_purge_unreachable", args: { repo: "web" } });
});

// A READER HOLDING NO HUMAN SEAT IS HELD FROM A MIRROR AND A PURGE with the
// person-only sentence, the controls still drawn. Mutation: hide them, or let
// an agent-bound reader through, and this fails.
test("a reader holding no human seat is held from a mirror and a purge, with the reason", async () => {
  stubEngine(detail({ managed_by: "tools" }));
  mount("#/gitlet/repos/web?tab=settings", "web", { ...WRITER, kind: "agent", handle: "cto" });

  const purge = await screen.findByRole("button", { name: "Purge unreachable history" });
  await waitFor(() => expect(purge.getAttribute("title")).toBe(PERSON_ONLY));
  expect(screen.getByRole("button", { name: "Add a push mirror" }).getAttribute("title")).toBe(
    PERSON_ONLY,
  );
});

// A REPOSITORY'S SETTINGS WRITES ARE DRAWN FOR EVERY READER, held with the
// reason each cannot act — the search mode, both mirrors and the purge — for a
// reader signed in as nobody and for a person the engine does not make them
// for, and none sends anything. Mutation: hide one from a reader who cannot
// act, or let a press through, and this fails.
test.each([
  [
    "an anonymous reader",
    { login: "", grants: [], handle: "", owner: "", name: "", acts: [], anonymous: true },
    WRITE_REASONS.anonymous,
  ],
  ["a person the engine does not serve", { ...WRITER, acts: [] }, WRITE_REASONS.not_served],
])(
  "a repository's settings writes are drawn for %s, held with the reason",
  async (_who, viewer, reason) => {
    const acted: Acted[] = [];
    stubEngine(detail({ managed_by: "tools", search: "grep" }), acted);
    mount("#/gitlet/repos/web?tab=settings", "web", viewer);

    const index = await screen.findByRole("button", { name: "Index it" });
    await waitFor(() => expect(index.getAttribute("title")).toBe(reason));
    for (const name of [
      "Make it a pull mirror",
      "Add a push mirror",
      "Purge unreachable history",
    ]) {
      expect(screen.getByRole("button", { name }).getAttribute("title"), name).toBe(reason);
    }
    fireEvent.click(index);
    fireEvent.click(screen.getByRole("button", { name: "Purge unreachable history" }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(acted).toHaveLength(0);
  },
);

// AN IMPORTING REPOSITORY SAYS HOW FAR ITS IMPORT WENT, and where its mirrors
// stand, on the Overview's rail. Mutation: drop the import's progress or the
// pull mirror's line from the rail, and this fails.
test("the overview's rail says how far an import went and where the mirrors stand", async () => {
  const source = { url: "https://forge.example.com/acme/web.git" };
  stubEngine(
    detail({
      managed_by: "tools",
      status: "importing",
      remotes: { mirror_from: source, import: { source, chunks: 3 } },
    }),
  );
  mount("#/gitlet/repos/web");

  expect(
    await screen.findByText(
      "Importing from https://forge.example.com/acme/web.git: 3 chunks landed so far.",
    ),
  ).toBeTruthy();
  expect(
    screen.getByText("Follows https://forge.example.com/acme/web.git; not synced yet."),
  ).toBeTruthy();
});
