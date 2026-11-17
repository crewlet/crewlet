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
import { Announcer } from "@crewlethq/ui";
import { onTheRight } from "~/test/codeScreens.tsx";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { engineJson } from "~/test/engineAnswer.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import { modules } from "~/test/source.ts";
import { DECLARED, DECLARED_LIFE, PERSON_ONLY, PURGE_KEEPERS } from "~/lib/repoWrites.ts";
import { SYNTAX_MAX_BYTES, load } from "~/lib/syntax/index.ts";
import { sinceOf } from "./History.tsx";
import { RepositoryPage } from "./RepositoryPage.tsx";

beforeEach(() => {
  // jsdom implements no scrolling, and a history row the keys or Load more reach scrolls into view.
  Element.prototype.scrollIntoView = () => {};
});

// EVERY GRAMMAR FETCH COUNTED, the registry's own load passed through.
vi.mock("~/lib/syntax/index.ts", async (real) => {
  const registry = await real<typeof import("~/lib/syntax/index.ts")>();
  return { ...registry, load: vi.fn(registry.load) };
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
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

/** A revision as the engine resolves it: a whole ref as it is, a bare name as a branch. */
const wholeRef = (rev: string) => (rev.startsWith("refs/") ? rev : `refs/heads/${rev}`);

/** A read of content by a ref, as the engine answers one: bare, with the ref it resolved. */
const byRef = (url: URL, part: Record<string, unknown>) => ({
  repo: "web",
  ...part,
  ...(url.searchParams.get("ref")?.length === 40
    ? {}
    : {
        resolved: {
          ref: { ref: wholeRef(url.searchParams.get("ref") ?? "main"), oid: HEAD },
        },
        position: POSITION,
      }),
});

/** A file longer than a page, its lines each `line <n>`. */
const BIG_FILE = "src/big.go";
/** The big file's blob, which a read by its id alone names. */
const BIG_OID = "fedcba9876543210fedcba9876543210fedcba98";
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
function bigRange(
  url: URL,
  defaultEnd: (start: number) => number,
  letter: string,
  total = BIG_LINES,
) {
  const p = url.searchParams;
  const start = Number(p.get("start_line") ?? 1);
  const end = Math.min(Number(p.get("end_line") ?? defaultEnd(start)), total);
  const cursor = p.get("cursor");
  const from = cursor ? Number(cursor.slice(1)) : start;
  const ok = start <= total && from >= start && from <= end && (!cursor || cursor[0] === letter);
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
        oid: BIG_OID,
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

/** A file a case colours: its blob's id, its text, and what its read says of it. */
interface Coded {
  oid: string;
  text: string;
  /** The blob's size when it is not its text's (a file past the bound). */
  size?: number;
  binary?: boolean;
  /** Held until it settles: a read of the blob by its id alone. */
  held?: Promise<void>;
}

/** A coded file's lines, without their newlines. */
const codedLines = (f: Coded) => f.text.split("\n").slice(0, -1);

/** A page of a coded file, from the range's first line or the cursor's, as the big file's. */
function codedRead(url: URL, path: string, f: Coded) {
  const all = codedLines(f);
  const { end, from, ok } = bigRange(url, () => all.length, "l", all.length);
  if (!ok) return json({ error: "bad_params" }, 400);
  const to = Math.min(from + BIG_PAGE_LINES - 1, end);
  return json(
    byRef(url, {
      kind: "blob",
      ...(to < end ? { next: `l${to + 1}` } : {}),
      blob: {
        path,
        oid: f.oid,
        size: f.size ?? f.text.length,
        lines: all.length,
        ...(f.binary
          ? { binary: true }
          : {
              start_line: from,
              end_line: to,
              text: all.slice(from - 1, to).join("\n") + "\n",
            }),
      },
    }),
  );
}

/** A coded file's blame, ranged and paged as the big file's. */
function codedBlame(url: URL, path: string, f: Coded) {
  const all = codedLines(f);
  const { start, end, from, ok } = bigRange(
    url,
    (s) => Math.min(s + 499, all.length),
    "b",
    all.length,
  );
  if (!ok || end - start + 1 > 500) return json({ error: "bad_params" }, 400);
  const to = Math.min(from + BIG_BLAME_PAGE_LINES - 1, end);
  return json(
    byRef(url, {
      kind: "blame",
      ...(to < end ? { next: `b${to + 1}` } : {}),
      blame: {
        path,
        start_line: from,
        end_line: to,
        lines: all.slice(from - 1, to).map((text, i) => ({
          line: from + i,
          commit: HEAD,
          orig_line: from + i,
          text,
        })),
        commits: { [HEAD]: { author: "Ada", authored_at: "2026-10-20T10:00:00Z" } },
      },
    }),
  );
}

/**
 * The engine: the repository, its refs, a root holding `src/` and a README, a
 * file of three lines and one of BIG_LINES, their blames and a history, and
 * `files` read by path or by their blob's id alone, and every request made.
 */
function stubEngine(
  repo: Record<string, unknown> = detail(),
  acted: Acted[] = [],
  files: Record<string, Coded> = {},
) {
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
        case "/query/code_blob": {
          const path = p.get("path");
          const coded = path
            ? files[path] && ([path, files[path]] as const)
            : Object.entries(files).find(([, f]) => f.oid === p.get("ref"));
          if (coded) {
            if (!path) await coded[1].held;
            return codedRead(url, ...coded);
          }
          if (!path && p.get("ref") === BIG_OID) return bigFileRead(url);
          if (path === "README.md") {
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
        }
        case "/query/code_blame":
          if (files[p.get("path") ?? ""])
            return codedBlame(url, p.get("path")!, files[p.get("path")!]!);
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
        case "/query/code_commit_marks":
          return json({ commit_marks: [], position: POSITION, level: "session" });
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
          {/* A LIST'S ROW REMOVED IS SAID, as the app's own announcer says it. */}
          <Announcer label="Announcements" />
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

const reads = (asked: URL[], what: string) => asked.filter((u) => u.pathname === `/query/${what}`);

// THE CODE TAB IS THE REPOSITORY'S LANDING, as a code host's is: the header
// names the repository under its owner off the chart, with its description;
// the root's files stand under the latest commit, read at the commit the root
// was listed at; and the README at that root is read at the same commit,
// rendered in a card of its own. Mutation: read the README or the latest
// commit at the branch rather than the commit the root was listed at, and the
// three could be three revisions — this fails.
test("the code tab is the root's files, its latest commit and its README, at one commit", async () => {
  const asked = stubEngine();
  mount("#/gitlet/repos/web");

  expect(await screen.findByRole("heading", { name: "Hello storefront" })).toBeTruthy();
  // THE FILE IS NAMED AS IT IS: never in the label register, whose capitals
  // would print README.MD.
  const name = screen.getByRole("heading", { name: "README.md" });
  expect(name.closest(".t-label")).toBeNull();
  expect(screen.getAllByText("Engineering").length).toBeGreaterThan(0);
  expect(screen.getByText("The storefront")).toBeTruthy();
  const readme = reads(asked, "code_blob").find((u) => u.searchParams.get("path") === "README.md");
  expect(readme?.searchParams.get("ref")).toBe(HEAD);
  expect(reads(asked, "code_repo")[0]?.searchParams.get("name")).toBe("web");
  // THE LATEST COMMIT, one commit of the root's history at the listed commit.
  expect(await screen.findByRole("link", { name: "Start the storefront" })).toBeTruthy();
  const latest = reads(asked, "code_log")[0];
  expect(latest?.searchParams.get("ref")).toBe(HEAD);
  expect(latest?.searchParams.get("limit")).toBe("1");
  expect(screen.getByRole("table", { name: "Files" })).toBeTruthy();
});

// The About rail's sentence says its time in words; the latest commit's chip keeps the short form.
test("the About rail says when the repository was pushed in words, the latest commit in short", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-28T12:00:00Z"));
  stubEngine();
  mount("#/gitlet/repos/web");
  expect(await screen.findByText("Pushed yesterday")).toBeTruthy();
  await waitFor(() =>
    expect(document.querySelector(".code-latest__when")?.textContent).toBe("6d ago"),
  );
});

// A PATH IS ONE READ, AT THE ADDRESS'S REVISION: a directory under the root
// is asked as a file and drawn as the listing it is, each entry opening its
// own path at the same revision. Mutation: drop `ref` from the read, or link
// an entry without it, and this fails.
test("a directory is read at the address's revision and its entries open at it", async () => {
  const asked = stubEngine();
  mount("#/gitlet/repos/web?ref=feature&path=src");

  const table = await screen.findByRole("table", { name: "Files" });
  const link = within(table).getByRole("link", { name: "app.go" });
  expect(link.getAttribute("href")).toBe("#/gitlet/repos/web?ref=feature&path=src%2Fapp.go");
  const read = reads(asked, "code_blob").find((u) => u.searchParams.get("path") === "src");
  expect(read?.searchParams.get("ref")).toBe("feature");
});

// A DIRECTORY IS A LIST LIKE EVERY OTHER: the data grid, its sizes on the
// right edge, and its heads sorting the entries once the whole directory is
// here. Mutation: draw the hand-built table again, or leave Size at the
// start, and this fails.
test("a directory's entries are drawn on the data grid, sortable once the whole directory is here", async () => {
  stubEngine();
  mount("#/gitlet/repos/web");

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
  mount("#/gitlet/repos/web");

  await screen.findByRole("link", { name: "src" });
  await waitFor(() => expect(reads(asked, "code_refs")).toHaveLength(2));
  act(() => {
    location.hash = "#/gitlet/repos/web?path=src";
  });
  expect(await screen.findByRole("link", { name: "app.go" })).toBeTruthy();
  act(() => {
    location.hash = "#/gitlet/repos/web?tab=commits";
  });
  expect(await screen.findByRole("group", { name: "History of main" })).toBeTruthy();

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
  mount("#/gitlet/repos/web?path=src%2Fapp.go&lines=2");

  const line2 = await screen.findByRole("button", { name: "Line 2" });
  expect(line2.getAttribute("aria-pressed")).toBe("true");
  expect(screen.getByRole("button", { name: "Line 1" }).getAttribute("aria-pressed")).toBe("false");
  expect(screen.getByText("func Run() {}")).toBeTruthy();

  const permalink = screen.getByRole("link", { name: "Permalink" });
  expect(permalink.getAttribute("href")).toBe(
    `#/gitlet/repos/web?ref=${HEAD}&path=src%2Fapp.go&lines=2`,
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
  mount("#/gitlet/repos/web?path=src%2Fapp.go");

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
// commit changed last is headed by that commit once — its subject, the link
// to the commit, with its author in the face beside it. Mutation: blame the
// branch, or head every line, and this fails.
test("the blame is read at the file's commit and heads each run of lines once", async () => {
  const asked = stubEngine();
  mount("#/gitlet/repos/web?path=src%2Fapp.go&lens=blame");

  const table = await screen.findByRole("table", { name: /Who last changed each line/ });
  expect(within(table).getAllByRole("link", { name: "Start" })).toHaveLength(1);
  expect(within(table).getAllByRole("link", { name: "Run" })).toHaveLength(1);
  expect(within(table).getByRole("link", { name: "Run" }).getAttribute("href")).toBe(
    `#/gitlet/repos/web/commits/${OTHER}`,
  );
  await waitFor(() => expect(reads(asked, "code_blame").length).toBeGreaterThan(0));
  expect(reads(asked, "code_blame")[0]?.searchParams.get("ref")).toBe(HEAD);
});

// The blame column is a cell, so its times keep the short form.
test("the blame column says each run's time in the short form", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-28T12:00:00Z"));
  stubEngine();
  mount("#/gitlet/repos/web?path=src%2Fapp.go&lens=blame");
  await screen.findByRole("table", { name: /Who last changed each line/ });
  await waitFor(() =>
    expect(
      [...document.querySelectorAll(".code-blame__when")].map((t) => t.textContent).sort(),
    ).toEqual(["7d ago", "8d ago"]),
  );
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
  mount(`#/gitlet/repos/web?path=src%2Fbig.go&lines=2400`);

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
  const back = reads(asked, "code_blob").filter(
    (u) => u.searchParams.has("end_line") && u.searchParams.get("path") === BIG_FILE,
  );
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
  mount(`#/gitlet/repos/web?path=src%2Fbig.go&lens=blame&lines=2400`);

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
  mount(`#/gitlet/repos/web?path=src%2Fbig.go&lines=3100`);

  expect(await screen.findByRole("button", { name: "Line 1" })).toBeTruthy();
  expect(screen.getByText(/it has no line 3100/)).toBeTruthy();
  expect(bigStarts(asked, "code_blob")).toEqual(["3050", null]);
});

// A BRANCH OPENS ITS FILES: the default branch at the repository's own
// address, any other by its name — and the tags under them, each at its whole
// ref. Mutation: link the default branch by name, every branch to the
// default, or a tag by its short name, and this fails.
test("the branches are the engine's refs, each opening its files", async () => {
  stubEngine();
  mount("#/gitlet/repos/web?tab=branches");

  const grid = await screen.findByRole("table", { name: "Branches of web" });
  expect(within(grid).getByText("default")).toBeTruthy();
  const links = within(grid)
    .getAllByRole("link")
    .map((a) => a.getAttribute("href"));
  expect(links).toContain("#/gitlet/repos/web?ref=feature");
  expect(links).toContain("#/gitlet/repos/web");
  // EACH BRANCH IS ITS CHIP beside its marks; a tag is its name, never a chip.
  const chips = [...grid.querySelectorAll(".code-branch-chip")].map((c) => c.textContent);
  expect(chips).toContain("feature");
  expect(
    within(grid).getByText("default").closest(".code-ref")?.querySelector(".code-branch-chip"),
  ).not.toBeNull();
  const tags = await screen.findByRole("table", { name: "Tags of web" });
  expect(
    within(tags)
      .getAllByRole("link")
      .map((a) => a.getAttribute("href")),
  ).toContain("#/gitlet/repos/web?ref=refs%2Ftags%2Fv1");
  expect(tags.querySelector(".code-branch-chip")).toBeNull();
});

// THE HISTORY IS READ AT THE REVISION AND THE PATH, with who pushed each
// commit from the page that named it, and Load older asks the cursor.
// Mutation: drop the path, read who pushed off the commit's author, or link a
// commit anywhere but its own page, and this fails.
test("the history is read at the revision and the path, with who pushed each commit", async () => {
  const asked = stubEngine();
  mount("#/gitlet/repos/web?tab=commits&ref=feature&path=src");

  const grid = await screen.findByRole("group", { name: "History of feature" });
  expect(within(grid).getByText("Start the storefront")).toBeTruthy();
  expect(within(grid).getByText("pushed by CTO")).toBeTruthy();
  // EACH COMMIT OPENS ITS OWN PAGE, by its whole id.
  expect(
    within(grid)
      .getAllByRole("link")
      .map((a) => a.getAttribute("href")),
  ).toContain(`#/gitlet/repos/web/commits/${HEAD}`);
  const first = reads(asked, "code_log")[0];
  expect(first?.searchParams.get("ref")).toBe("feature");
  expect(first?.searchParams.get("path")).toBe("src");

  fireEvent.click(await screen.findByRole("button", { name: "Load older commits" }));
  expect(await within(grid).findByText("Older work")).toBeTruthy();
  expect(reads(asked, "code_log").some((u) => u.searchParams.get("cursor") === "c1")).toBe(true);
});

// A history row is a sentence: who authored the commit and when, in words.
test("a history row says when its commit was authored in words", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-28T12:00:00Z"));
  stubEngine();
  mount("#/gitlet/repos/web?tab=commits");
  const grid = await screen.findByRole("group", { name: /History of/ });
  await waitFor(() =>
    expect(
      within(grid)
        .getByText("Start the storefront")
        .closest(".code-list__row")
        ?.querySelector(".code-list__meta time")?.textContent,
    ).toBe("6 days ago"),
  );
});

/** A commit of a history page, authored at `at`. */
const logged = (oid: string, subject: string, at: string, author = "Ada") => ({
  oid,
  author,
  authored_at: at,
  committed_at: at,
  subject,
});

/** The engine answering one history page of `commits`, with who pushed each. */
function stubHistory(commits: unknown[], pushedBy: Record<string, unknown> = {}) {
  const asked = stubEngine();
  const engine = vi.mocked(fetch).getMockImplementation()!;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname !== "/query/code_log") return engine(input, init);
      asked.push(url);
      return json(
        byRef(url, {
          kind: "log",
          log: { commits, scanned: commits.length, complete: true },
          pushed_by: pushedBy,
        }),
      );
    }),
  );
  return asked;
}

const THIRD = "fedcba9876543210fedcba9876543210fedcba98";

// A HISTORY IS A TIMELINE OF THE READER'S DAYS, in git's order: a day met again
// after another opens its own heading. Mutation: group by day across the page.
test("a history is a timeline of the reader's days, a day met again opening its own heading", async () => {
  stubHistory([
    logged(HEAD, "Newest", "2026-10-22T10:00:00Z"),
    logged(OTHER, "Older", "2026-10-21T10:00:00Z"),
    logged(THIRD, "Merged in", "2026-10-22T09:00:00Z"),
  ]);
  mount("#/gitlet/repos/web?tab=commits");
  const history = await screen.findByRole("group", { name: /History of/ });
  const headings = within(history).getAllByRole("heading", { level: 3 });
  expect(headings.map((h) => h.textContent)).toEqual([
    "Commits on Oct 22, 2026",
    "Commits on Oct 21, 2026",
    "Commits on Oct 22, 2026",
  ]);
  expect(within(history).getByRole("list", { name: "Commits on Oct 21, 2026" })).toBeTruthy();
  expect(history.querySelector(".code-list__note")?.textContent).toBe(
    "3 commits of main loaded, newest first",
  );
});

// EACH COMMIT SAYS HOW IT REACHED ITS BRANCH, from the push the index holds:
// a run for its work item, the author with git, or who pushed it; nothing
// where the index holds no push. Mutation: read the pusher off the author line.
test("each commit says how it reached its branch", async () => {
  const push = (principal: Record<string, unknown>, via = "engine") => ({
    push_id: "p",
    position: POSITION,
    at: "2026-10-22T10:00:00Z",
    principal,
    via,
  });
  const run = { turn_id: "t", work_backend: "worklet", work_key: "ENG-399" };
  const ids = ["1", "2", "3", "4", "5", "6"].map((n) => n.repeat(40));
  stubHistory(
    [
      logged(ids[0]!, "Own run", "2026-10-22T10:00:00Z", "CTO"),
      logged(ids[1]!, "Other run", "2026-10-22T09:00:00Z", "Ada"),
      logged(ids[2]!, "With git", "2026-10-22T08:00:00Z", "CTO"),
      logged(ids[3]!, "Pushed by another", "2026-10-22T07:00:00Z", "Ada"),
      logged(ids[4]!, "Unknown push", "2026-10-22T06:00:00Z", "Ada"),
      logged(ids[5]!, "Own push", "2026-10-22T05:00:00Z", "CTO"),
    ],
    {
      [ids[0]!]: push({ kind: "agent", seat: "cto", run }),
      [ids[1]!]: push({ kind: "agent", seat: "cto", run }),
      [ids[2]!]: push({ kind: "agent", seat: "cto" }, "git"),
      [ids[3]!]: push({ kind: "operator", operator_id: "session:abc", seat: "cto" }, "git"),
      [ids[5]!]: push({ kind: "agent", seat: "cto" }),
    },
  );
  mount("#/gitlet/repos/web?tab=commits");
  const history = await screen.findByRole("group", { name: /History of/ });
  const chip = (subject: string) =>
    within(history)
      .getByText(subject)
      .closest(".code-list__row")
      ?.querySelector(".code-commit-prov");
  await waitFor(() => expect(chip("Own run")?.textContent).toBe("pushed by a run for ENG-399"));
  expect(chip("Other run")?.textContent).toBe("pushed by a run of CTO for ENG-399");
  expect(chip("With git")?.textContent).toBe("pushed with git");
  expect(chip("Pushed by another")?.textContent).toBe("pushed by CTO");
  expect(chip("Pushed by another")?.getAttribute("title")).toBe("through session:abc");
  expect(chip("Unknown push")).toBeNull();
  expect(chip("Own push")?.textContent).toBe("pushed by CTO");
});

/** A rollup of `target`'s checks of one commit, passed unless a case says otherwise. */
const checked = (target: string, extra: Record<string, unknown> = {}) => ({
  target,
  state: "passed",
  counts: { passed: 2 },
  required: 2,
  required_passed: 2,
  ...extra,
});

/** A change a mark names. */
const landing = (n: number, state: string, extra: Record<string, unknown> = {}) => ({
  repo: "web",
  ref: `web!${n}`,
  title: `change ${n}`,
  state,
  ...extra,
});

/**
 * The engine answering one history page of `commits` with who pushed each and their tags,
 * and each commit's marks from `marks` (held until `held` settles); every marks ask kept.
 */
function stubMarked(opts: {
  commits: unknown[];
  pushedBy?: Record<string, unknown>;
  tagged?: Record<string, unknown>;
  marks?: Record<string, Record<string, unknown>>;
  marksAnswer?: () => Response;
  held?: Promise<void>;
}) {
  const asked = stubHistory(opts.commits, opts.pushedBy);
  const marksAsked: URL[] = [];
  const history = vi.mocked(fetch).getMockImplementation()!;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname === "/query/code_log" && opts.tagged) {
        const answer = (await (await history(input, init)).json()) as Record<string, unknown>;
        return json({ ...answer, tagged: opts.tagged });
      }
      if (url.pathname !== "/query/code_commit_marks") return history(input, init);
      marksAsked.push(url);
      await opts.held;
      if (opts.marksAnswer) return opts.marksAnswer();
      const oids = (url.searchParams.get("commits") ?? "").split(",");
      return json({
        commit_marks: oids
          .filter((oid) => opts.marks?.[oid])
          .map((oid) => ({ commit: oid, ...opts.marks![oid] })),
        position: POSITION,
        level: "session",
      });
    }),
  );
  return { asked, marksAsked };
}

/** The history row of the commit whose subject is `subject`. */
const rowOf = async (subject: string) => {
  const history = await screen.findByRole("group", { name: /History of/ });
  return (await within(history).findByText(subject)).closest<HTMLElement>(".code-list__row")!;
};

// EACH COMMIT CARRIES ITS CHECK FOR THE BRANCH READ: the marks are asked once per page, by
// that page's whole ids; a feature branch's history takes its own rollup, the default's
// history the main one. Mutation: pick the first rollup, or ask every page again.
test("each commit carries its check for the branch read", async () => {
  const asked = stubEngine();
  const engine = vi.mocked(fetch).getMockImplementation()!;
  const marks: Record<string, unknown>[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname !== "/query/code_commit_marks") return engine(input, init);
      asked.push(url);
      const failing = checked("refs/heads/main", {
        state: "failing",
        counts: { passed: 1, failed: 1 },
        required_passed: 1,
        failing: [{ name: "e2e", state: "failed", required: true }],
      });
      marks.push({ commits: url.searchParams.get("commits") });
      return json({
        commit_marks: [{ commit: HEAD, checks: [checked("refs/heads/feature"), failing] }],
        position: POSITION,
        level: "session",
      });
    }),
  );
  mount("#/gitlet/repos/web?tab=commits");
  const row = await rowOf("Start the storefront");
  const mark = await within(row).findByText("e2e failed");
  expect(mark.closest(".code-check-mark")?.getAttribute("title")).toBe(
    "2 checks: 1 passed, 1 failed. 1 of 2 required passed.",
  );
  fireEvent.click(await screen.findByRole("button", { name: "Load older commits" }));
  await rowOf("Older work");
  await waitFor(() => expect(marks).toHaveLength(2));
  expect(marks).toEqual([{ commits: HEAD }, { commits: OTHER }]);

  cleanup();
  mount("#/gitlet/repos/web?tab=commits&ref=feature");
  const feature = await rowOf("Start the storefront");
  expect(await within(feature).findByText("Passed")).toBeTruthy();
  expect(within(feature).queryByText("e2e failed")).toBeNull();
});

// THE MARKS DO NOT MOVE A ROW'S ID: the check's slot is drawn, empty, before they answer, and
// holds the mark after; its width is the layout case's. Mutation: draw the slot only once known.
test("the marks do not move a row's id", async () => {
  let release = () => {};
  const held = new Promise<void>((r) => (release = r));
  stubMarked({
    commits: [logged(HEAD, "Newest", "2026-10-22T10:00:00Z")],
    marks: { [HEAD]: { checks: [checked("refs/heads/main")] } },
    held,
  });
  mount("#/gitlet/repos/web?tab=commits");
  const row = await rowOf("Newest");
  const facts = row.querySelector(".code-list__facts")!;
  expect(facts.firstElementChild?.className).toBe("code-commit-check");
  expect(facts.firstElementChild?.textContent).toBe("");
  release();
  await waitFor(() => expect(facts.firstElementChild?.textContent).toBe("Passed"));
  expect(facts.children[1]?.className).toContain("code-sha");
});

/** A push the index holds, by `seat`, `via` how. */
const pushed = (seat: string, via: string, at = "2026-10-22T10:00:00Z") => ({
  push_id: "p",
  position: POSITION,
  at,
  principal: { kind: "agent", seat },
  via,
});

// A MERGE COMMIT GITLET WROTE IS GITLET'S: its mark, "Gitlet committed" at its committer
// date, and the chip naming the merger and the change; a queue landing says the queue; a squash,
// or a merge the engine did not commit, keeps its author. Mutation: decide by `via` alone.
test("a merge commit Gitlet wrote is drawn as Gitlet's, and a squash keeps its author", async () => {
  const MERGE = "c".repeat(40);
  const SQUASH = "d".repeat(40);
  const QUEUED = "e".repeat(40);
  const OWN = "f".repeat(40);
  stubMarked({
    commits: [
      {
        ...logged(MERGE, "Merge change web!41", "2026-10-22T08:00:00Z", "CTO"),
        committed_at: "2026-10-22T10:00:00Z",
        parent_count: 2,
        committer: "Crewlet",
        engine_committed: true,
      },
      { ...logged(SQUASH, "cache the badge", "2026-10-22T09:00:00Z", "Ada"), parent_count: 1 },
      {
        ...logged(QUEUED, "Merge change web!44", "2026-10-22T07:00:00Z", "CTO"),
        parent_count: 2,
        engine_committed: true,
      },
      {
        ...logged(OWN, "Merge branch feature", "2026-10-22T06:00:00Z", "Ada"),
        parent_count: 2,
        engine_committed: false,
      },
    ],
    pushedBy: {
      [MERGE]: pushed("cto", "merge"),
      [SQUASH]: pushed("cto", "merge"),
      [QUEUED]: pushed("cto", "queue"),
      [OWN]: pushed("cto", "merge"),
    },
    marks: {
      [MERGE]: { merged: [landing(41, "merged", { merge_oid: MERGE })] },
      [SQUASH]: { merged: [landing(43, "merged", { merge_oid: SQUASH })] },
      [QUEUED]: { merged: [landing(44, "merged", { merge_oid: QUEUED })] },
    },
  });
  mount("#/gitlet/repos/web?tab=commits");
  const merge = await rowOf("Merge change web!41");
  const meta = merge.querySelector(".code-list__meta")!;
  expect(meta.querySelector(".code-face svg")).toBeTruthy();
  expect(meta.querySelector(".crewlet-avatar")).toBeNull();
  expect(meta.querySelector(".code-commit-meta__who")?.textContent).toBe("Gitlet");
  expect(meta.textContent).toContain("committed");
  expect(meta.querySelector("time")?.getAttribute("dateTime")).toBe("2026-10-22T10:00:00Z");
  const chip = meta.querySelector(".code-commit-prov")!;
  expect(chip.textContent).toBe("merged by Gitlet for CTO");
  await waitFor(() =>
    expect(chip.getAttribute("title")).toBe(
      "Gitlet wrote this merge commit when CTO merged web!41 through its gate",
    ),
  );
  const squash = (await rowOf("cache the badge")).querySelector(".code-list__meta")!;
  expect(squash.querySelector(".code-face")).toBeNull();
  expect(squash.querySelector(".code-commit-meta__who")?.textContent).toBe("Ada");
  expect(squash.querySelector(".code-commit-prov")?.textContent).toBe("merged by Gitlet for CTO");
  const queued = (await rowOf("Merge change web!44")).querySelector(".code-list__meta")!;
  expect(queued.querySelector(".code-commit-meta__who")?.textContent).toBe("Gitlet");
  const landed = queued.querySelector(".code-commit-prov")!;
  expect(landed.textContent).toBe("landed by Gitlet from the queue for CTO");
  await waitFor(() =>
    expect(landed.getAttribute("title")).toBe(
      "Gitlet wrote this commit when web!44 landed from the merge queue, queued by CTO",
    ),
  );
  const own = (await rowOf("Merge branch feature")).querySelector(".code-list__meta")!;
  expect(own.querySelector(".code-face")).toBeNull();
  expect(own.querySelector(".code-commit-meta__who")?.textContent).toBe("Ada");
  expect(own.textContent).toContain("authored");
});

// A MERGE COMMIT WEARS ITS CHANGE'S PILL, a head reads landed with, a live change's head its
// revision, and an inner commit nothing. Mutation: draw "landed with" on the merge commit too.
test("a merge commit wears its change's pill, and a head reads landed with", async () => {
  const [merge, head, live, inner, gone] = ["1", "2", "3", "4", "5"].map((n) => n.repeat(40)) as [
    string,
    string,
    string,
    string,
    string,
  ];
  stubMarked({
    commits: [
      logged(merge, "Merge change web!41", "2026-10-22T10:00:00Z"),
      logged(head, "cache the badge", "2026-10-22T09:00:00Z"),
      logged(live, "stream the shell", "2026-10-22T08:00:00Z"),
      logged(inner, "an inner commit", "2026-10-22T07:00:00Z"),
      logged(gone, "a dropped idea", "2026-10-22T06:00:00Z"),
    ],
    marks: {
      [merge]: {
        merged: [landing(41, "merged", { merge_oid: merge })],
        heads: [{ change: landing(41, "merged"), n: 1 }],
      },
      [head]: { heads: [{ change: landing(41, "merged"), n: 1 }] },
      [live]: { heads: [{ change: landing(42, "open"), n: 3 }] },
      [gone]: { heads: [{ change: landing(40, "closed"), n: 2 }] },
    },
  });
  mount("#/gitlet/repos/web?tab=commits");
  const mergeRow = await rowOf("Merge change web!41");
  const pill = await waitFor(() => {
    const a = mergeRow.querySelector<HTMLElement>("a.code-change-ref");
    expect(a).toBeTruthy();
    return a!;
  });
  expect(pill.getAttribute("href")).toBe("#/gitlet/repos/web/changes/41");
  expect(pill.closest(".code-list__title")).toBeTruthy();
  expect(mergeRow.querySelector(".code-commit-landed")).toBeNull();
  const landed = (await rowOf("cache the badge")).querySelector(".code-commit-landed");
  expect(landed?.textContent).toBe("landed with web!41");
  expect(
    within(landed as HTMLElement)
      .getByRole("link")
      .getAttribute("href"),
  ).toBe("#/gitlet/repos/web/changes/41");
  expect((await rowOf("stream the shell")).querySelector(".code-commit-landed")?.textContent).toBe(
    "revision 3 of web!42",
  );
  expect((await rowOf("a dropped idea")).querySelector(".code-commit-landed")?.textContent).toBe(
    "revision 2 of web!40, closed",
  );
  const innerRow = await rowOf("an inner commit");
  expect(innerRow.querySelector(".code-commit-landed")).toBeNull();
  expect(innerRow.querySelector(".code-change-ref")).toBeNull();
});

// A COMMIT'S TAGS ARE CHIPS from the page's own decoration; a page with none says nothing.
// Mutation: draw "no tags" where the decoration is absent.
test("a commit's tags are chips, and an unknown decoration draws none", async () => {
  stubMarked({
    commits: [logged(HEAD, "Newest", "2026-10-22T10:00:00Z")],
    tagged: { commits: { [HEAD]: { refs: ["refs/tags/v2", "refs/tags/v1"], more: 1 } } },
  });
  mount("#/gitlet/repos/web?tab=commits");
  const meta = (await rowOf("Newest")).querySelector(".code-list__meta")!;
  await waitFor(() => expect(meta.querySelectorAll(".code-tag-chip")).toHaveLength(2));
  expect([...meta.querySelectorAll(".code-tag-chip")].map((a) => a.getAttribute("href"))).toEqual([
    "#/gitlet/repos/web?ref=refs%2Ftags%2Fv2",
    "#/gitlet/repos/web?ref=refs%2Ftags%2Fv1",
  ]);
  expect(meta.textContent).toContain("+1 more tag");

  cleanup();
  stubMarked({ commits: [logged(HEAD, "Untagged", "2026-10-22T10:00:00Z")] });
  mount("#/gitlet/repos/web?tab=commits");
  const bare = (await rowOf("Untagged")).querySelector(".code-list__meta")!;
  expect(bare.querySelector(".code-tag-chip")).toBeNull();
  expect(bare.textContent).not.toContain("tag");
});

// CHECKS OLDER THAN THE CHECKS KEPT SAY SO, never a cross; a recent commit with none says
// nothing. Mutation: draw a cross for an absence.
test("checks older than the checks kept say so", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-30T12:00:00Z"));
  stubMarked({
    commits: [
      logged(HEAD, "Long ago", "2026-10-21T10:00:00Z"),
      logged(OTHER, "Lately", "2026-11-20T10:00:00Z"),
    ],
    pushedBy: {
      [HEAD]: pushed("cto", "git", "2026-10-21T10:00:00Z"),
      [OTHER]: pushed("cto", "git", "2026-11-20T10:00:00Z"),
    },
    marks: {},
  });
  mount("#/gitlet/repos/web?tab=commits");
  const old = (await rowOf("Long ago")).querySelector(".code-commit-check")!;
  await waitFor(() => expect(old.textContent).toBe("Checks no longer kept"));
  expect(old.querySelector("svg")).toBeNull();
  expect(old.firstElementChild?.getAttribute("title")).toBe("Checks are kept 30 days.");
  expect((await rowOf("Lately")).querySelector(".code-commit-check")?.textContent).toBe("");
});

// A REFUSED MARKS READ IS SAID ONCE, and the rows still draw. Mutation: a line per row.
test("a refused marks read is said once, and the rows still draw", async () => {
  stubMarked({
    commits: [
      logged(HEAD, "Newest", "2026-10-22T10:00:00Z"),
      logged(OTHER, "Older", "2026-10-21T10:00:00Z"),
    ],
    marksAnswer: () =>
      engineJson({ error: "unavailable", detail: "this node answers no marks yet" }, 503),
  });
  mount("#/gitlet/repos/web?tab=commits");
  const note = await screen.findByText(
    "Checks and the changes each commit landed with are unavailable: this node answers no marks yet.",
  );
  expect(note.getAttribute("role")).toBe("status");
  expect(document.querySelectorAll(".code-commit-note")).toHaveLength(1);
  expect(await rowOf("Older")).toBeTruthy();
  expect(document.querySelector(".code-check-mark")).toBeNull();

  cleanup();
  const { marksAsked } = stubMarked({
    commits: [logged(HEAD, "Newest", "2026-10-22T10:00:00Z")],
    marksAnswer: () => engineJson({ error: "query_failed" }, 500),
  });
  mount("#/gitlet/repos/web?tab=commits");
  const failed = await screen.findByText(
    "Checks and the changes each commit landed with could not be read.",
  );
  expect(marksAsked).toHaveLength(1);
  fireEvent.click(within(failed).getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(marksAsked).toHaveLength(2));
});

/** The engine answering each history page with `page(url)`, every ask kept. */
function stubLog(page: (url: URL) => Record<string, unknown> | Response) {
  const asked = stubEngine();
  const engine = vi.mocked(fetch).getMockImplementation()!;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname !== "/query/code_log") return engine(input, init);
      asked.push(url);
      const answer = page(url);
      return answer instanceof Response ? answer : json(byRef(url, { kind: "log", ...answer }));
    }),
  );
  return asked;
}

/** Choose `option` in the toolbar's searchable picker named `name`, a button opening its list. */
function choose(name: string, option: RegExp) {
  fireEvent.click(screen.getByRole("button", { name }));
  fireEvent.mouseDown(screen.getByRole("option", { name: option }));
}

// A HISTORY IS NARROWED BY WHO PUSHED IT on every page, and what the index cannot judge is
// counted in words, never hidden. Mutation: drop `pushed_by` from the next page.
test("a history is narrowed by who pushed it", async () => {
  const asked = stubLog((url) =>
    url.searchParams.get("cursor")
      ? {
          log: {
            commits: [logged(OTHER, "Older", "2026-10-21T10:00:00Z")],
            scanned: 4,
            unjudged: 5,
            complete: true,
          },
        }
      : {
          next: "c1",
          log: {
            commits: [logged(HEAD, "Newest", "2026-10-22T10:00:00Z")],
            scanned: 9,
            ...(url.searchParams.get("pushed_by") ? { unjudged: 7 } : {}),
          },
        },
  );
  mount("#/gitlet/repos/web?tab=commits");
  await rowOf("Newest");
  choose("Pushed by", /CTO/);
  await waitFor(() => expect(location.hash).toContain("by=cto"));
  await waitFor(() =>
    expect(reads(asked, "code_log").some((u) => u.searchParams.get("pushed_by") === "cto")).toBe(
      true,
    ),
  );
  fireEvent.click(await screen.findByRole("button", { name: "Load older commits" }));
  await rowOf("Older");
  const next = reads(asked, "code_log").find((u) => u.searchParams.get("cursor") === "c1");
  expect(next?.searchParams.get("pushed_by")).toBe("cto");
  const note = document.querySelector(".code-list__note")!;
  await waitFor(() =>
    expect(note.textContent).toBe(
      "2 commits of main pushed by CTO loaded, newest first. 12 commits are not listed: who pushed them is unknown to this node’s index of pushes.",
    ),
  );
});

// A HISTORY IS NARROWED BY WHEN IT WAS COMMITTED, from the first instant of the reader's day
// six days back for the last seven, and that start holds all day. Mutation: start at now - 7d.
test("a history is narrowed by when it was committed", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-16T10:00:00Z"));
  const asked = stubLog(() => ({
    log: { commits: [logged(HEAD, "Newest", "2026-11-15T10:00:00Z")], scanned: 1, complete: true },
  }));
  mount("#/gitlet/repos/web?tab=commits&when=7d");
  await rowOf("Newest");
  // THE SUITE'S ZONE IS BERLIN: Nov 10 begins at 23:00 UTC the day before.
  expect(reads(asked, "code_log")[0]?.searchParams.get("since")).toBe("2026-11-09T23:00:00.000Z");
  expect(document.querySelector(".code-list__note")?.textContent).toBe(
    "1 commit of main committed in the last 7 days loaded, newest first",
  );
  expect(sinceOf("today", "2026-11-16")).toBe("2026-11-15T23:00:00.000Z");
  expect(sinceOf("", "2026-11-16")).toBeUndefined();
  vi.setSystemTime(new Date("2026-11-16T20:00:00Z"));
  fireEvent.click(screen.getByRole("combobox", { name: "Committed" }));
  await afterMicrotasks();
  expect(new Set(reads(asked, "code_log").map((u) => u.searchParams.get("since")))).toEqual(
    new Set(["2026-11-09T23:00:00.000Z"]),
  );
});

// COMMIT MESSAGES ARE SEARCHED ON ENTER, page by page as the reader asks: a page that matched
// nothing yet has more says how far it searched, and an answer with nothing anywhere is the
// empty state. Mutation: ask on every keystroke, or drop `grep` from the next page.
test("commit messages are searched page by page", async () => {
  const asked = stubLog((url) => {
    const grep = url.searchParams.get("grep");
    if (grep === "zzz") return { log: { commits: [], scanned: 40, complete: true } };
    if (grep !== "cache") {
      return { log: { commits: [logged(HEAD, "Newest", "2026-10-22T10:00:00Z")], scanned: 1 } };
    }
    return url.searchParams.get("cursor")
      ? {
          log: {
            commits: [logged(OTHER, "cache the badge", "2026-10-21T10:00:00Z")],
            scanned: 30,
            complete: true,
          },
        }
      : { next: "c1", log: { commits: [], scanned: 25 } };
  });
  mount("#/gitlet/repos/web?tab=commits");
  await rowOf("Newest");
  const box = screen.getByRole("searchbox", { name: "Search commit messages" });
  fireEvent.change(box, { target: { value: "cache" } });
  await afterMicrotasks();
  expect(reads(asked, "code_log").some((u) => u.searchParams.has("grep"))).toBe(false);
  fireEvent.submit(box.closest("form")!);
  await waitFor(() => expect(location.hash).toContain("q=cache"));
  const more = await screen.findByRole("button", { name: "Keep searching" });
  expect(document.querySelector(".code-list__note")?.textContent).toBe(
    "No message holds “cache” in the 25 commits searched so far",
  );
  fireEvent.click(more);
  await rowOf("cache the badge");
  const next = reads(asked, "code_log").find((u) => u.searchParams.get("cursor") === "c1");
  expect(next?.searchParams.get("grep")).toBe("cache");
  await waitFor(() =>
    expect(document.querySelector(".code-list__note")?.textContent).toBe(
      "1 commit matches “cache”, searched every commit of main (55)",
    ),
  );

  fireEvent.change(box, { target: { value: "zzz" } });
  fireEvent.submit(box.closest("form")!);
  expect(await screen.findByText("No commit message holds “zzz”")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
  await waitFor(() => expect(location.hash).not.toContain("q="));
});

// A REFUSED FILTERED HISTORY KEEPS ITS FILTERS IN VIEW, with the refusal and the way back.
// Mutation: draw the refusal in place of the toolbar.
test("a refused filtered history keeps its filters in view", async () => {
  stubLog((url) =>
    url.searchParams.get("pushed_by")
      ? json({ error: "bad_params", detail: "pushed_by is not a key this node reads" }, 400)
      : { log: { commits: [logged(HEAD, "Newest", "2026-10-22T10:00:00Z")], scanned: 1 } },
  );
  mount("#/gitlet/repos/web?tab=commits&by=cto");
  expect(await screen.findByText(/pushed_by is not a key this node reads/)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Pushed by" }).textContent).toContain("CTO");
  fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
  await waitFor(() => expect(location.hash).not.toContain("by="));
  expect(await rowOf("Newest")).toBeTruthy();
});

// EVERY KEY A LINK MAY WRITE INTO A COMMITS ADDRESS IS ONE COMMITS READS, read off the
// modules' text as the source gates read. Mutation: rename `by` in RepoAt only.
test("every Commits key a repository's link can say is read by the history", () => {
  const text = (path: string) => modules().find((m) => m.path === path)!.text;
  const at = /export interface RepoAt \{([^}]*)\}/.exec(text("routes/gitlet/paths.ts"))![1]!;
  const keys = [...at.matchAll(/^\s*(\w+)\?:/gm)].map((m) => m[1]);
  const history = text("routes/gitlet/repo/History.tsx");
  for (const key of ["ref", "path", "by", "when", "q"]) {
    expect(keys, key).toContain(key);
    expect(history, key).toContain(`useParam("${key}"`);
  }
});

// EACH COMMIT COPIES ITS ID AND BROWSES ITS FILES at it, as a real link a
// modified press opens in a tab. Mutation: browse the branch, not the commit.
test("each commit copies its id and browses its files", async () => {
  stubEngine();
  mount("#/gitlet/repos/web?tab=commits");
  const history = await screen.findByRole("group", { name: /History of/ });
  const row = (await within(history).findByText("Start the storefront")).closest("li")!;
  expect(within(row).getByRole("button", { name: "Copy the commit id 0123456" })).toBeTruthy();
  expect(
    within(row).getByRole("link", { name: "Browse the files at 0123456" }).getAttribute("href"),
  ).toBe(`#/gitlet/repos/web?ref=${HEAD}`);
});

// J OPENS THE HISTORY'S COMMITS FROM THE KEYBOARD, as the grid's did: j focuses
// the first commit's link and Enter elsewhere opens it. Mutation: drop the keys.
test("j opens the history's commits from the keyboard", async () => {
  stubEngine();
  mount("#/gitlet/repos/web?tab=commits");
  const history = await screen.findByRole("group", { name: /History of/ });
  await within(history).findByText("Start the storefront");
  fireEvent.keyDown(window, { key: "j" });
  const link = within(history).getByRole("link", { name: "Start the storefront" });
  expect(document.activeElement).toBe(link);
  link.blur();
  fireEvent.keyDown(window, { key: "Enter" });
  await waitFor(() => expect(location.hash).toBe(`#/gitlet/repos/web/commits/${HEAD}`));
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
// stand, on the About rail beside its code. Mutation: drop the import's
// progress or the pull mirror's line from the rail, and this fails.
test("the About rail says how far an import went and where the mirrors stand", async () => {
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

/** A person the engine serves every repository and branch write for. */
const MAINTAINER = {
  ...ADMIN_VIEWER,
  acts: [
    "code_delete_repo",
    "code_restore_repo",
    "code_protect_branch",
    "code_create_branch",
    "code_delete_branch",
    "code_commit_files",
    "code_update_repo",
    "code_purge_unreachable",
  ],
};

// A DELETE IS ASKED ONLY ONCE ITS NAME IS TYPED, and sends one
// code_delete_repo naming the repository. Mutation: let the press through
// before the name matches, or send on the first press, and this fails.
test("a repository is deleted only once its name is typed back", async () => {
  const acted: Acted[] = [];
  stubEngine(detail({ managed_by: "tools" }), acted);
  mount("#/gitlet/repos/web?tab=settings", "web", MAINTAINER);

  const del = await screen.findByRole("button", { name: "Delete repository" });
  await waitFor(() => expect(del.getAttribute("title")).toBeNull());
  fireEvent.click(del);
  const dialog = await screen.findByRole("dialog", { name: "Delete web?" });
  const confirm = within(dialog).getByRole("button", { name: "Delete repository" });
  fireEvent.click(confirm);
  expect(acted).toHaveLength(0);
  fireEvent.change(within(dialog).getByLabelText(/to confirm/), { target: { value: "web" } });
  fireEvent.click(confirm);
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({ tool: "code_delete_repo", args: { repo: "web" } });
});

// A DECLARED REPOSITORY'S LIFE IS THE COMPANY DOCUMENT'S: its delete and its
// protection are drawn and held with where they are changed. Mutation: hide
// them, or let a declared repository through, and this fails.
test("a declared repository's delete and protection are held with where they are changed", async () => {
  stubEngine(detail(), []);
  mount("#/gitlet/repos/web?tab=settings", "web", MAINTAINER);

  const del = await screen.findByRole("button", { name: "Delete repository" });
  await waitFor(() => expect(del.getAttribute("title")).toBe(DECLARED_LIFE));
  expect(screen.getByRole("button", { name: "Protect a branch" }).getAttribute("title")).toBe(
    DECLARED_LIFE,
  );
});

// A DELETED REPOSITORY SAYS SO ABOVE ITS TABS and offers its restore, one
// code_restore_repo naming it. Mutation: drop the notice or its press and this
// fails.
test("a deleted repository says so and is restored by one request", async () => {
  const acted: Acted[] = [];
  stubEngine(
    detail({ managed_by: "tools", status: "deleted", deleted_at: "2026-10-30T10:00:00Z" }),
    acted,
  );
  mount("#/gitlet/repos/web", "web", MAINTAINER);

  const restore = await screen.findByRole("button", { name: "Restore repository" });
  await waitFor(() => expect(restore.getAttribute("title")).toBeNull());
  // THE NOTICE ABOVE THE TABS, and not the reason each held write gives.
  expect(screen.getByText(/is deleted, since/)).toBeTruthy();
  fireEvent.click(restore);
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({ tool: "code_restore_repo", args: { repo: "web" } });
});

// A BRANCH IS DELETED AT THE HEAD ITS ROW SHOWS, confirmed in the page first:
// a push made since is refused rather than lost. Mutation: send no
// expected_head, or send on the row's press, and this fails.
test("a branch is deleted at the head its row showed, after a confirmation", async () => {
  const acted: Acted[] = [];
  stubEngine(detail({ managed_by: "tools" }), acted);
  mount("#/gitlet/repos/web?tab=branches", "web", MAINTAINER);

  const del = await screen.findByRole("button", { name: "Delete feature" });
  await waitFor(() => expect(del.getAttribute("title")).toBeNull());
  fireEvent.click(del);
  const dialog = await screen.findByRole("alertdialog");
  expect(dialog.textContent).toContain(OTHER.slice(0, 7));
  expect(acted).toHaveLength(0);
  fireEvent.click(within(dialog).getByRole("button", { name: "Delete branch" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_delete_branch",
    args: { repo: "web", branch: "feature", expected_head: OTHER },
  });
});

// THE DEFAULT BRANCH HAS NO DELETE: the engine refuses its deletion whoever
// asks (default_branch), since every clone checks it out, and it is let go by
// making another branch the default first. Mutation: draw Delete on every row,
// and main offers a delete the engine refuses.
test("the default branch offers no delete, and every other branch does", async () => {
  stubEngine(detail({ managed_by: "tools" }), []);
  mount("#/gitlet/repos/web?tab=branches", "web", MAINTAINER);

  const grid = await screen.findByRole("table", { name: "Branches of web" });
  expect(await within(grid).findByRole("button", { name: "Delete feature" })).toBeTruthy();
  expect(within(grid).queryByRole("button", { name: "Delete main" })).toBeNull();
});

// A REPOSITORY WHOSE DEFAULT BRANCH HOLDS NOTHING IS NOT EMPTY while it holds
// other branches: its Files and Commits tabs offer the picker that reaches
// them and say what is so, never "nobody has pushed"; New branch is offered,
// naming where it starts; and Settings changes its default branch to one it
// holds. Mutation: read emptiness off the default branch's head alone, and
// the tabs say nothing was pushed with no way to the branches that were.
test("a repository whose default branch holds nothing reads its other branches", async () => {
  stubEngine(detail({ managed_by: "tools", head: undefined, branches: 1 }), []);
  mount("#/gitlet/repos/web", "web", MAINTAINER);

  expect(
    await screen.findByText(
      "Its default branch, main, holds no commit, though it holds another branch: read one of them, or make one the default on the Settings tab.",
    ),
  ).toBeTruthy();
  expect(screen.queryByText(/Nobody has pushed/)).toBeNull();
  expect(screen.queryByText("This repository is empty")).toBeNull();
  expect(screen.getByLabelText("Revision")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Add a file" })).toBeNull();
});

test("a new branch of a repository whose default branch holds nothing names where it starts", async () => {
  const acted: Acted[] = [];
  stubEngine(detail({ managed_by: "tools", head: undefined, branches: 1 }), acted);
  mount("#/gitlet/repos/web?tab=branches", "web", MAINTAINER);

  const create = await screen.findByRole("button", { name: "New branch" });
  await waitFor(() => expect(create.getAttribute("title")).toBeNull());
  fireEvent.click(create);
  const dialog = await screen.findByRole("dialog", { name: "New branch of web" });
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "fix/login" } });
  const send = within(dialog).getByRole("button", { name: "Create branch" });
  await waitFor(() => expect(send.getAttribute("title")).toMatch(/Name where it starts/));
  fireEvent.change(within(dialog).getByLabelText(/Starts at/), { target: { value: "feature" } });
  await waitFor(() => expect(send.getAttribute("title")).toBeNull());
  fireEvent.click(send);
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_create_branch",
    args: { repo: "web", branch: "fix/login", at: "feature" },
  });
});

// A REPOSITORY'S DETAILS ARE A PERSON'S TO CHANGE HERE: its description,
// default branch and owner, one code_update_repo carrying only what moved, the
// default branch chosen from the branches it holds. Mutation: send every
// field, or offer no form, and this fails.
test("a repository's details are one settings change naming only what moved", async () => {
  const acted: Acted[] = [];
  stubEngine(detail({ managed_by: "tools" }), acted);
  mount("#/gitlet/repos/web?tab=settings", "web", MAINTAINER);

  expect(screen.queryByText(/code_update_repo/)).toBeNull();
  const edit = await screen.findByRole("button", { name: "Edit details" });
  await waitFor(() => expect(edit.getAttribute("title")).toBeNull());
  fireEvent.click(edit);
  const dialog = await screen.findByRole("dialog", { name: "web's details" });
  const save = within(dialog).getByRole("button", { name: "Save details" });
  await waitFor(() => expect(save.getAttribute("title")).toBe("Nothing is changed yet."));
  fireEvent.change(within(dialog).getByLabelText(/^Description/), {
    target: { value: "The storefront and its checkout" },
  });
  await waitFor(() => expect(save.getAttribute("title")).toBeNull());
  fireEvent.click(save);
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_update_repo",
    args: { repo: "web", description: "The storefront and its checkout" },
  });
});

// A DELETED REPOSITORY TAKES NO CHANGE BUT ITS RESTORE: its Settings tab draws
// what it holds and none of the writes a live repository offers, each of which
// the engine would refuse it. Mutation: draw the live tab for a deleted
// repository, and a column of disabled controls stands under Restore again.
test("a deleted repository's settings offer no write but its restore", async () => {
  stubEngine(
    detail({ managed_by: "tools", status: "deleted", deleted_at: "2026-10-30T10:00:00Z" }),
    [],
  );
  mount("#/gitlet/repos/web?tab=settings", "web", MAINTAINER);

  expect(await screen.findByRole("button", { name: "Restore repository" })).toBeTruthy();
  expect(await screen.findByText("refs/heads/main")).toBeTruthy();
  for (const name of [
    "Edit details",
    "Index it",
    "Search it with grep",
    "Make it a pull mirror",
    "Add a push mirror",
    "Purge unreachable history",
    "Protect a branch",
    "Edit the rule on refs/heads/main",
    "Remove the rule on refs/heads/main",
    "Delete repository",
  ]) {
    expect(screen.queryByRole("button", { name }), name).toBeNull();
  }
  expect(screen.getByText(/restore it to change its settings/)).toBeTruthy();
});

// A BRANCH A RULE NAMES ALREADY IS EDITED WHERE ITS RULE IS DRAWN WHOLE: its
// row says it is protected and leads to Settings, while a branch no rule
// names opens a new rule on itself. Mutation: draw Protect on every row, and
// main's new-rule form would start from the engine's defaults; this fails.
test("a protected branch leads to its rule, and another opens a new one", async () => {
  stubEngine(detail({ managed_by: "tools" }), []);
  mount("#/gitlet/repos/web?tab=branches", "web", MAINTAINER);

  const grid = await screen.findByRole("table", { name: "Branches of web" });
  const link = await within(grid).findByRole("link", { name: "Protection →" });
  expect(link.getAttribute("href")).toContain("tab=settings");
  expect(within(grid).getAllByRole("link", { name: "Protection →" })).toHaveLength(1);
  expect(within(grid).getAllByRole("button", { name: "Protect" })).toHaveLength(1);
  expect(within(grid).getByText("protected")).toBeTruthy();
  expect(
    within(grid).getByText("protected").closest(".code-ref")?.querySelector(".code-branch-chip"),
  ).not.toBeNull();
});

// A FILE EDITED IN THE BROWSER IS ONE COMMIT ON THE BRANCH ON SCREEN, at the
// commit the page's read resolved, carrying the whole file. Mutation: commit
// at the address's spelling rather than the resolution, or drop the head, and
// this fails.
test("a file edited here is one commit on the branch read, at the commit it resolved", async () => {
  const acted: Acted[] = [];
  stubEngine(detail({ managed_by: "tools" }), acted);
  mount("#/gitlet/repos/web?path=src%2Fapp.go", "web", MAINTAINER);

  const edit = await screen.findByRole("button", { name: "Edit" });
  await waitFor(() => expect(edit.getAttribute("title")).toBeNull());
  fireEvent.click(edit);
  const dialog = await screen.findByRole("dialog", { name: "Edit src/app.go" });
  const text = (await within(dialog).findByLabelText("File contents")) as HTMLTextAreaElement;
  expect(text.value).toBe("package app\n\nfunc Run() {}\n");
  fireEvent.change(text, { target: { value: "package app\n\nfunc Run() { start() }\n" } });
  fireEvent.change(within(dialog).getByLabelText("Commit message"), {
    target: { value: "Start on run" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Commit to main" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_commit_files",
    args: {
      repo: "web",
      branch: "main",
      expected_head: HEAD,
      message: "Start on run",
      files: [
        { op: "put", path: "src/app.go", content: "package app\n\nfunc Run() { start() }\n" },
      ],
    },
  });
});

// AN EMPTY REPOSITORY IS FILLED HERE: its first file is a root commit that
// makes its default branch, sent with no head, since nothing was read.
// Mutation: hide the control on an empty repository, or send an empty
// expected_head, which the engine refuses, and this fails.
test("an empty repository's first file is a root commit on its default branch", async () => {
  const acted: Acted[] = [];
  stubEngine(detail({ managed_by: "tools", head: undefined, branches: 0, refs: 0 }), acted);
  mount("#/gitlet/repos/web", "web", MAINTAINER);

  const add = await screen.findByRole("button", { name: "Add a file" });
  await waitFor(() => expect(add.getAttribute("title")).toBeNull());
  fireEvent.click(add);
  const dialog = await screen.findByRole("dialog", { name: "New file in web" });
  expect(dialog.textContent).toMatch(/first commit of web/);
  fireEvent.change(within(dialog).getByLabelText("Path"), { target: { value: "README.md" } });
  fireEvent.change(within(dialog).getByLabelText("File contents"), {
    target: { value: "# web\n" },
  });
  fireEvent.change(within(dialog).getByLabelText("Commit message"), {
    target: { value: "Add a README" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Commit to main" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_commit_files",
    args: {
      repo: "web",
      branch: "main",
      message: "Add a README",
      files: [{ op: "put", path: "README.md", content: "# web\n" }],
    },
  });
});

// A TAG TAKES NO COMMIT: the controls are drawn and held, saying so.
test("a file read at a tag is held from a commit, with the reason", async () => {
  stubEngine(detail({ managed_by: "tools" }), []);
  mount("#/gitlet/repos/web?ref=refs%2Ftags%2Fv1&path=src%2Fapp.go", "web", MAINTAINER);

  const edit = await screen.findByRole("button", { name: "Edit" });
  await waitFor(() => expect(edit.getAttribute("title")).toMatch(/Choose a branch/));
});

// WHO READS A REPOSITORY IS ONE OF ITS DETAILS: the form's Read by sends
// `visibility` alone where it moved. Mutation: leave visibility out of the
// form or the call, or send it unmoved, and this fails.
test("who reads a repository is a details change naming only the visibility", async () => {
  const acted: Acted[] = [];
  stubEngine(detail({ managed_by: "tools" }), acted);
  mount("#/gitlet/repos/web?tab=settings", "web", MAINTAINER);

  const edit = await screen.findByRole("button", { name: "Edit details" });
  await waitFor(() => expect(edit.getAttribute("title")).toBeNull());
  fireEvent.click(edit);
  const dialog = await screen.findByRole("dialog", { name: "web's details" });
  fireEvent.click(within(dialog).getByRole("combobox", { name: "Read by" }));
  fireEvent.mouseDown(screen.getByRole("option", { name: "Its owner's seats" }));
  const save = within(dialog).getByRole("button", { name: "Save details" });
  await waitFor(() => expect(save.getAttribute("title")).toBeNull());
  fireEvent.click(save);
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({ tool: "code_update_repo", args: { repo: "web", visibility: "unit" } });
});

// A RENAME IS A RECORD OF ITS OWN: one code_update_repo naming `rename_to`
// alone, held until the name is a project's. Mutation: fold the rename into a
// details change, or let a scratch-shaped name through, and this fails.
test("a repository is renamed by one call naming the new name alone", async () => {
  const acted: Acted[] = [];
  stubEngine(detail({ managed_by: "tools" }), acted);
  mount("#/gitlet/repos/web?tab=settings", "web", MAINTAINER);

  const rename = await screen.findByRole("button", { name: "Rename" });
  await waitFor(() => expect(rename.getAttribute("title")).toBeNull());
  fireEvent.click(rename);
  const dialog = await screen.findByRole("dialog", { name: "Rename web" });
  const press = within(dialog).getByRole("button", { name: "Rename" });
  const field = within(dialog).getByLabelText(/^New name/);
  fireEvent.change(field, { target: { value: "cto.web" } });
  await waitFor(() => expect(press.getAttribute("title")).toMatch(/Name the project/));
  // The reason is said under the field too, which is marked invalid.
  expect(field.getAttribute("aria-invalid")).toBe("true");
  expect(within(dialog).getByRole("alert").textContent).toMatch(/Name the project/);
  fireEvent.click(press);
  expect(acted).toHaveLength(0);
  fireEvent.change(field, { target: { value: "storefront" } });
  await waitFor(() => expect(press.getAttribute("title")).toBeNull());
  expect(field.getAttribute("aria-invalid")).toBeNull();
  fireEvent.click(press);
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_update_repo",
    args: { repo: "web", rename_to: "storefront" },
  });
});

// AN ARCHIVE IS CONFIRMED, then one call naming `archived` alone; an archived
// repository offers its unarchive; a pull mirror's archive is held with why.
// Mutation: archive on the first press, send another field, offer Archive on an
// archived repository, or let a pull mirror's archive through, and this fails.
test("a repository is archived after a confirmation, and unarchived by one press", async () => {
  const acted: Acted[] = [];
  stubEngine(detail({ managed_by: "tools" }), acted);
  mount("#/gitlet/repos/web?tab=settings", "web", MAINTAINER);

  const archive = await screen.findByRole("button", { name: "Archive repository" });
  await waitFor(() => expect(archive.getAttribute("title")).toBeNull());
  fireEvent.click(archive);
  const dialog = await screen.findByRole("dialog", { name: "Archive web?" });
  expect(acted).toHaveLength(0);
  fireEvent.click(within(dialog).getByRole("button", { name: "Archive" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({ tool: "code_update_repo", args: { repo: "web", archived: true } });
  cleanup();

  const again: Acted[] = [];
  stubEngine(detail({ managed_by: "tools", status: "archived" }), again);
  mount("#/gitlet/repos/web?tab=settings", "web", MAINTAINER);
  const unarchive = await screen.findByRole("button", { name: "Unarchive repository" });
  expect(screen.queryByRole("button", { name: "Archive repository" })).toBeNull();
  await waitFor(() => expect(unarchive.getAttribute("title")).toBeNull());
  fireEvent.click(unarchive);
  await waitFor(() => expect(again).toHaveLength(1));
  expect(again[0]).toEqual({ tool: "code_update_repo", args: { repo: "web", archived: false } });
  cleanup();

  stubEngine(detail({ managed_by: "tools", mirrored: true }));
  mount("#/gitlet/repos/web?tab=settings", "web", MAINTAINER);
  const mirror = await screen.findByRole("button", { name: "Archive repository" });
  await waitFor(() => expect(mirror.getAttribute("title")).toMatch(/is a pull mirror/));
});

// A PATH OWNER IS GIVEN GLOBS, ITS GLOBS ARE CHANGED BY NAME, AND IT IS TAKEN
// OFF WITH EVERY GLOB IT HOLDS, each one code_update_repo's `path_owners`.
// Mutation: send an owner's globs whole, or remove fewer than it holds, and
// this fails.
test("a path owner is added, edited by glob, and removed, each one settings change", async () => {
  const acted: Acted[] = [];
  stubEngine(detail({ managed_by: "tools" }), acted);
  mount("#/gitlet/repos/web?tab=settings", "web", MAINTAINER);
  const engineering = { kind: "unit", ref: "engineering" };

  const edit = await screen.findByRole("button", { name: "Edit the paths Engineering owns" });
  await waitFor(() => expect(edit.getAttribute("title")).toBeNull());
  fireEvent.click(edit);
  let dialog = await screen.findByRole("dialog", { name: "Paths Engineering owns" });
  fireEvent.click(within(dialog).getByRole("button", { name: /Remove path 1/ }));
  fireEvent.change(within(dialog).getByRole("textbox", { name: /new path/i }), {
    target: { value: "app/**" },
  });
  const save = within(dialog).getByRole("button", { name: "Save" });
  await waitFor(() => expect(save.getAttribute("title")).toBeNull());
  fireEvent.click(save);
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_update_repo",
    args: {
      repo: "web",
      path_owners: {
        add: [{ owner: engineering, paths: ["app/**"] }],
        remove: [{ owner: engineering, paths: ["src/**"] }],
      },
    },
  });

  fireEvent.click(screen.getByRole("button", { name: "Remove Engineering as a path owner" }));
  fireEvent.click(
    within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Remove owner" }),
  );
  await waitFor(() => expect(acted).toHaveLength(2));
  expect(acted[1]).toEqual({
    tool: "code_update_repo",
    args: { repo: "web", path_owners: { remove: [{ owner: engineering, paths: ["src/**"] }] } },
  });

  fireEvent.click(screen.getByRole("button", { name: "Add a path owner" }));
  dialog = await screen.findByRole("dialog", { name: "Add a path owner to web" });
  fireEvent.click(within(dialog).getByRole("combobox", { name: "Owner" }));
  fireEvent.mouseDown(screen.getByRole("option", { name: "CTO" }));
  fireEvent.change(within(dialog).getByRole("textbox", { name: /new path/i }), {
    target: { value: "docs/**" },
  });
  const add = within(dialog).getByRole("button", { name: "Add owner" });
  await waitFor(() => expect(add.getAttribute("title")).toBeNull());
  fireEvent.click(add);
  await waitFor(() => expect(acted).toHaveLength(3));
  expect(acted[2]).toEqual({
    tool: "code_update_repo",
    args: {
      repo: "web",
      path_owners: { add: [{ owner: { kind: "seat", ref: "cto" }, paths: ["docs/**"] }] },
    },
  });
});

// A DECLARED REPOSITORY'S NAME, ARCHIVE AND PATH OWNERS ARE THE DOCUMENT'S:
// drawn and held with where they are changed. Mutation: hide one, or let a
// declared repository through, and this fails.
test("a declared repository's rename, archive and path owners are held with the document's sentence", async () => {
  const acted: Acted[] = [];
  stubEngine(detail(), acted);
  mount("#/gitlet/repos/web?tab=settings", "web", MAINTAINER);

  const rename = await screen.findByRole("button", { name: "Rename" });
  await waitFor(() => expect(rename.getAttribute("title")).toBe(DECLARED));
  for (const name of [
    "Archive repository",
    "Add a path owner",
    "Edit the paths Engineering owns",
    "Remove Engineering as a path owner",
  ]) {
    expect(screen.getByRole("button", { name }).getAttribute("title"), name).toBe(DECLARED);
  }
  fireEvent.click(rename);
  fireEvent.click(screen.getByRole("button", { name: "Archive repository" }));
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(screen.queryByRole("alertdialog")).toBeNull();
  expect(acted).toHaveLength(0);
});

/** A Go file the file view colours. */
const MAIN_GO = [
  "package main",
  "",
  "import (",
  '\t"fmt"',
  '\t"net/http"',
  ")",
  "",
  "// Main serves the storefront.",
  "func main() {",
  '\tfmt.Println("serving")',
  '\thttp.ListenAndServe(":8080", nil)',
  "}",
];
const MAIN_OID = "1111111111111111111111111111111111111111";

/** A long Go file whose lines 2300 to 2400 are one block comment. */
const DEEP_LINES = 3000;
const DEEP_OID = "2222222222222222222222222222222222222222";
const deepText = () =>
  Array.from({ length: DEEP_LINES }, (_, i) => {
    const n = i + 1;
    if (n === 2300) return "/* the renderer's notes";
    if (n === 2400) return "the notes end */";
    return n > 2300 && n < 2400 ? `note ${n}` : `var x${n} = ${n}`;
  }).join("\n") + "\n";

/** The lines of a file's view, by number, once drawn. */
const textOf = (n: number) => document.querySelector(`#L${n} .code-source__text`);
/** The coloured spans under `root`. */
const painted = (root: ParentNode = document) => root.querySelectorAll('[class^="code-syn-"]');
/** The reads of a blob by its id alone, as `[start_line, end_line, cursor]`. */
const byId = (asked: URL[]) =>
  reads(asked, "code_blob")
    .filter((u) => !u.searchParams.has("path"))
    .map((u) => ["start_line", "end_line", "cursor"].map((k) => u.searchParams.get(k)));

// A FILE'S CODE IS COLOURED BY KIND, each token a span in its kind's class,
// and the line reads the same text it did plain. Mutation: draw the kind's
// class on the whole line, or drop a token's text, and this fails.
test("a file's code is coloured by kind, and reads the same text", async () => {
  stubEngine(detail(), [], {
    "cmd/web/main.go": { oid: MAIN_OID, text: MAIN_GO.join("\n") + "\n" },
  });
  mount("#/gitlet/repos/web?path=cmd%2Fweb%2Fmain.go");

  await waitFor(() => expect(textOf(1)?.querySelector(".code-syn-keyword")).toBeTruthy());
  expect(textOf(1)!.querySelector(".code-syn-keyword")!.textContent).toBe("package");
  expect(textOf(5)!.querySelector(".code-syn-string")!.textContent).toBe('"net/http"');
  expect(textOf(8)!.querySelector(".code-syn-comment")!.textContent).toBe(
    "// Main serves the storefront.",
  );
  MAIN_GO.forEach((text, i) => expect(textOf(i + 1)!.textContent, `line ${i + 1}`).toBe(text));
});

// A FILE OPENED PAST LINE 1 TAKES ITS STATE FROM THE LINES ABOVE IT, read once
// by the blob's id, plain until they arrive. Mutation: walk from the first drawn
// line with no state, or read the prefix again on Load earlier lines, and this fails.
test("a file opened past line 1 reads the lines above it by the blob's id, once, and colours them from there", async () => {
  let release = () => {};
  const held = new Promise<void>((r) => (release = r));
  const asked = stubEngine(detail(), [], {
    "internal/render/page.go": { oid: DEEP_OID, text: deepText(), held },
  });
  mount("#/gitlet/repos/web?path=internal%2Frender%2Fpage.go&lines=2380");

  await screen.findByRole("button", { name: "Line 2380" });
  await waitFor(() => expect(byId(asked)).toEqual([["1", "2329", null]]));
  expect(painted()).toHaveLength(0);
  release();
  await waitFor(() => expect(textOf(2380)?.querySelector(".code-syn-comment")).toBeTruthy());
  expect(textOf(2380)!.querySelector(".code-syn-comment")!.textContent).toBe("note 2380");
  expect(textOf(2401)!.querySelector(".code-syn-keyword")!.textContent).toBe("var");
  expect(byId(asked)).toEqual([
    ["1", "2329", null],
    ["1", "2329", "l1501"],
  ]);

  fireEvent.click(screen.getByRole("button", { name: "Load earlier lines" }));
  expect(await screen.findByRole("button", { name: "Line 830" })).toBeTruthy();
  await waitFor(() => expect(textOf(830)?.querySelector(".code-syn-keyword")).toBeTruthy());
  expect(textOf(2350)!.querySelector(".code-syn-comment")).toBeTruthy();
  expect(byId(asked)).toHaveLength(2);
});

// A RAW STRING OPENED ON ONE PAGE IS CARRIED ONTO THE NEXT: the pages are one
// file's lines. Mutation: walk each page from no state, and this fails.
test("a raw string opened on one page is carried onto the next", async () => {
  const lines = Array.from({ length: 1600 }, (_, i) => `var x${i + 1} = ${i + 1}`);
  lines[1499] = "var s = `raw text";
  lines[1500] = "still raw`";
  stubEngine(detail(), [], {
    "src/raw.go": { oid: DEEP_OID, text: lines.join("\n") + "\n" },
  });
  mount("#/gitlet/repos/web?path=src%2Fraw.go");

  await screen.findByRole("button", { name: "Line 1500" });
  fireEvent.click(await screen.findByRole("button", { name: "Load more lines" }));
  await screen.findByRole("button", { name: "Line 1501" });
  await waitFor(() => expect(textOf(1501)?.querySelector(".code-syn-string")).toBeTruthy());
  expect(textOf(1501)!.querySelector(".code-syn-string")!.textContent).toBe("still raw`");
});

// A FILE AT OR PAST THE BOUND, A BINARY ONE AND A PLAIN-TEXT ONE are drawn as
// before: no span, no read of the lines above, no grammar for plain text.
// Mutation: colour by the drawn text's length, not the blob's size, and this fails.
test("a file at or past 256 KiB, a binary one and a plain-text one draw no span and read nothing more", async () => {
  for (const [path, file, at] of [
    ["src/edge.go", { oid: DEEP_OID, text: deepText(), size: SYNTAX_MAX_BYTES }, 2380],
    ["src/big.js", { oid: DEEP_OID, text: deepText(), size: 300 * 1024 }, 2380],
    ["src/logo.go", { oid: DEEP_OID, text: "x\n", binary: true }, 0],
    ["notes.txt", { oid: DEEP_OID, text: "func main() {}\n" }, 0],
  ] as const) {
    vi.mocked(load).mockClear();
    const asked = stubEngine(detail(), [], { [path]: file });
    mount(`#/gitlet/repos/web?path=${encodeURIComponent(path)}${at ? `&lines=${at}` : ""}`);
    await screen.findByText(path === "src/logo.go" ? /no lines of a binary file/ : /./, {
      selector: path === "src/logo.go" ? "p" : `#L${at || 1} .code-source__text`,
    });
    if (path === "notes.txt") expect(load).not.toHaveBeenCalled();
    // A TICK FOR A GRAMMAR AND A WALK THAT MUST NOT COME.
    await load("go");
    await act(async () => {});
    expect(painted(), path).toHaveLength(0);
    expect(byId(asked), path).toEqual([]);
    cleanup();
  }
});

// THE BLAME IS COLOURED AS THE FILE IS: from line 1, or opened past its first
// range, after the lines above it read by the blob's id. Mutation: walk the
// blame from its first drawn line with no state, and this fails.
test("the blame is coloured from line 1, and opened past its first range reads its prefix", async () => {
  const cell = (n: number) =>
    screen
      .getByRole("button", { name: `Line ${n}` })
      .closest("tr")!
      .querySelector(".code-blame__text");
  let asked = stubEngine(detail(), [], {
    "cmd/web/main.go": { oid: MAIN_OID, text: MAIN_GO.join("\n") + "\n" },
  });
  mount("#/gitlet/repos/web?path=cmd%2Fweb%2Fmain.go&lens=blame");
  await screen.findByRole("button", { name: "Line 8" });
  await waitFor(() => expect(cell(8)?.querySelector(".code-syn-comment")).toBeTruthy());
  expect(cell(1)!.querySelector(".code-syn-keyword")!.textContent).toBe("package");
  expect(cell(5)!.textContent).toBe('\t"net/http"');
  expect(byId(asked)).toEqual([]);
  cleanup();

  asked = stubEngine(detail(), [], {
    "internal/render/page.go": { oid: DEEP_OID, text: deepText() },
  });
  mount("#/gitlet/repos/web?path=internal%2Frender%2Fpage.go&lens=blame&lines=2380");
  await screen.findByRole("button", { name: "Line 2380" });
  await waitFor(() => expect(cell(2380)?.querySelector(".code-syn-comment")).toBeTruthy());
  expect(byId(asked)[0]).toEqual(["1", "2329", null]);
});

// A PRESS ON A LINE NUMBER TOKENIZES NOTHING: a line's tokens follow its text
// and state, never its pick. Mutation: drop the memo on SourceLine's tokens,
// or pass a fresh tokens array per render, and this fails.
test("a press on a line number redraws the lines whose pick moved, and tokenizes nothing else", async () => {
  stubEngine(detail(), [], {
    "cmd/web/main.go": { oid: MAIN_OID, text: MAIN_GO.join("\n") + "\n" },
  });
  mount("#/gitlet/repos/web?path=cmd%2Fweb%2Fmain.go&lines=2");
  await waitFor(() => expect(textOf(9)?.querySelector(".code-syn-keyword")).toBeTruthy());
  const grammar = await load("go");
  const line = vi.spyOn(grammar, "line");
  try {
    fireEvent.click(screen.getByRole("button", { name: "Line 9" }));
    await waitFor(() => expect(location.hash).toContain("lines=9"));
    await act(async () => {});
    expect(screen.getByRole("button", { name: "Line 9" }).getAttribute("aria-pressed")).toBe(
      "true",
    );
    expect(line).not.toHaveBeenCalled();
  } finally {
    line.mockRestore();
  }
});

// A PRESS DEEP IN A FILE OR ITS BLAME WALKS NOTHING AGAIN: the prefix and the
// rows keep their arrays across a render. Mutation: a fresh prefix slice per
// render, or inline readers for the blame's pages, and this fails.
test.each([
  ["file", ""],
  ["blame", "&lens=blame"],
])("a press in a %s opened past line 1 tokenizes nothing", async (_, lens) => {
  stubEngine(detail(), [], {
    "internal/render/page.go": { oid: DEEP_OID, text: deepText() },
  });
  mount(`#/gitlet/repos/web?path=internal%2Frender%2Fpage.go${lens}&lines=2380`);
  await screen.findByRole("button", { name: "Line 2385" });
  await waitFor(() => expect(document.querySelector("#L2401 .code-syn-keyword")).toBeTruthy());
  const grammar = await load("go");
  const line = vi.spyOn(grammar, "line");
  try {
    fireEvent.click(screen.getByRole("button", { name: "Line 2385" }));
    await waitFor(() => expect(location.hash).toContain("lines=2385"));
    await act(async () => {});
    expect(screen.getByRole("button", { name: "Line 2385" }).getAttribute("aria-pressed")).toBe(
      "true",
    );
    expect(line).not.toHaveBeenCalled();
  } finally {
    line.mockRestore();
  }
});

// A LINE OF MARKUP IS TEXT, NEVER ELEMENTS: a repository's bytes are drawn
// as strings in spans. Mutation: draw a token's text as markup, and this fails.
test("a line of markup is text, never elements", async () => {
  const text = "<img src=x onerror=alert(1)>";
  stubEngine(detail(), [], { "web/index.html": { oid: MAIN_OID, text: `${text}\n` } });
  mount("#/gitlet/repos/web?path=web%2Findex.html");
  await waitFor(() => expect(painted(textOf(1)!).length).toBeGreaterThan(0));
  expect(document.querySelector("img")).toBeNull();
  expect(textOf(1)!.textContent).toBe(text);
});

// A LINE PAST SYNTAX_MAX_LINE DRAWS PLAIN, its state still carried, so the
// line after it is coloured right. Mutation: drop the long line's state, and
// this fails.
test("a line past 4,000 code units draws plain and the next line is still coloured right", async () => {
  const long = `/* ${"a".repeat(4100)}`;
  stubEngine(detail(), [], {
    "src/long.go": { oid: MAIN_OID, text: `${long}\nstill a note */ var x\n` },
  });
  mount("#/gitlet/repos/web?path=src%2Flong.go");
  await waitFor(() => expect(textOf(2)?.querySelector(".code-syn-comment")).toBeTruthy());
  expect(textOf(2)!.querySelector(".code-syn-comment")!.textContent).toBe("still a note */");
  expect(painted(textOf(1)!)).toHaveLength(0);
  expect(textOf(1)!.textContent).toBe(long);
});
