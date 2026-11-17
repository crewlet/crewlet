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
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { pinZoneForTest } from "~/lib/prefs.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { engineJson } from "~/test/engineAnswer.ts";
import { afterMicrotasks } from "~/test/settle.ts";
import { modules } from "~/test/source.ts";
import { DECLARED, DECLARED_LIFE, PERSON_ONLY, PURGE_KEEPERS } from "~/lib/repoWrites.ts";
import { SYNTAX_MAX_BYTES, load } from "~/lib/syntax/index.ts";
import { forgetDownloadProbes } from "./downloads.ts";
import { forgetTreeChoice } from "./FileView.tsx";
import { forgetTrees } from "./tree.ts";
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
  forgetDownloadProbes();
  forgetTrees();
  forgetTreeChoice();
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

/** The address the engine answers a repository is cloned from, on the deployment's own address. */
const CLONE_URL = "https://crewlet.example.com/git/web.git";

const json = (payload: unknown, status = 200) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });

/** A revision as the engine resolves it: a whole ref as it is, a bare name as a branch. */
const wholeRef = (rev: string) => (rev.startsWith("refs/") ? rev : `refs/heads/${rev}`);

/** Where a commit's checks stand for `target`, as `code_commit_marks` answers it. */
const rollup = (target: string, state: string) => ({
  target,
  state,
  counts: state === "passed" ? { passed: 2 } : { passed: 1, running: 1 },
  required: 2,
  required_passed: state === "passed" ? 2 : 1,
});

/** The tags newest first, as `code_tags` answers them: one dated, one past 9999, one of a tree. */
const TAG_ROWS = [
  {
    ref: "refs/tags/v3",
    oid: HEAD,
    type: "commit",
    peeled: HEAD,
    peeled_type: "commit",
    date: "2026-10-28T07:00:00Z",
  },
  {
    ref: "refs/tags/v2",
    oid: OTHER,
    type: "tag",
    peeled: OTHER,
    peeled_type: "commit",
    date_unix: 253402300801,
  },
  {
    ref: "refs/tags/v1",
    oid: "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c",
    type: "tree",
    peeled: "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c",
    peeled_type: "tree",
  },
];

/** One head of a `code_tips` page, behind and ahead of its base. */
const tipOf = (oid: string, behind?: number, ahead?: number) => ({
  oid,
  type: "commit",
  peeled: oid,
  peeled_type: "commit",
  ...(behind === undefined ? { counts_out_of_time: true } : { behind, ahead }),
});

/** Who moved a branch, as the push history names the push. */
const mover = (seat: string, via: string) => ({
  push_id: `p-${seat}-${via}`,
  actor: seat,
  actor_kind: "agent",
  actor_seat: seat,
  via,
});

/** The branches by when each moved, as `code_refs` with `detail=true` answers them. */
const REF_ROWS = [
  {
    ref: "refs/heads/feature",
    oid: OTHER,
    position: POSITION,
    moved_at: "2026-10-27T11:00:00Z",
    moved_by: mover("cto", "git"),
    change: { number: 42, title: "Stream the shell", state: "open" },
    may_delete: true,
  },
  {
    ref: "refs/heads/main",
    oid: HEAD,
    position: POSITION,
    moved_at: "2026-10-27T10:00:00Z",
    moved_by: mover("cto", "merge"),
    protection: { patterns: ["refs/heads/main"], direct_push: "persons" },
    delete_refusal: "default_branch",
    delete_detail: "main is web's default branch: it is never deleted",
  },
];

/** A listing in detail of `rows`, found by any part of a name, its active count where asked. */
function refsListing(p: URLSearchParams, rows: readonly Record<string, unknown>[] = REF_ROWS) {
  const has = (p.get("contains") ?? "").toLowerCase();
  return {
    refs_detail: rows.filter((r) =>
      String(r.ref).slice("refs/heads/".length).toLowerCase().includes(has),
    ),
    ...(p.get("active_since") ? { active_branches: { count: rows.length - 1 } } : {}),
    position: POSITION,
    level: "session",
  };
}

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
          const { gates, rule_sources, path_owners, clone_url = CLONE_URL, ...row } = repo;
          return json({
            ...(clone_url ? { clone_url } : {}),
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
          if (p.get("detail") === "true") return json(refsListing(p));
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
        case "/query/code_tree": {
          // THE ROOT, `src` (the file view's tree reads it), and any other directory empty.
          const dir = p.get("path") ?? "";
          const entries =
            dir === ""
              ? [
                  { mode: "100644", type: "blob", oid: OTHER, path: "README.md", size: 20 },
                  { mode: "040000", type: "tree", oid: OTHER, path: "src" },
                ]
              : dir === "src"
                ? [{ mode: "100644", type: "blob", oid: OTHER, path: "src/app.go", size: 30 }]
                : [];
          return json(
            byRef(url, {
              kind: "tree",
              tree: { tree: OTHER, ...(dir ? { path: dir } : {}), entries, total: entries.length },
            }),
          );
        }
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
          return json({
            commit_marks: (p.get("commits") ?? "")
              .split(",")
              .filter((c) => c === HEAD)
              .map((commit) => ({ commit, checks: [rollup("refs/heads/main", "passed")] })),
            position: POSITION,
            level: "session",
          });
        case "/query/code_tags":
          return json({
            repo: "web",
            kind: "tags",
            tags: { tags: TAG_ROWS.slice(0, Number(p.get("limit") ?? 50)), total: TAG_ROWS.length },
          });
        case "/query/code_tips":
          return json({
            repo: "web",
            kind: "tips",
            tips: {
              base: p.get("base"),
              tips: (p.get("oids") ?? "").split(",").map((oid) => tipOf(oid, 0, 3)),
            },
          });
        case "/query/code_tree_commits":
          return json({
            repo: "web",
            kind: "tree_commits",
            tree_commits: {
              tree: OTHER,
              entries:
                p.get("path") === "src"
                  ? [{ path: "src/app.go", commit: HEAD }]
                  : [
                      { path: "README.md", commit: HEAD },
                      { path: "src", commit: OTHER },
                    ],
              commits: [
                {
                  oid: HEAD,
                  author: "Ada",
                  authored_at: "2026-10-22T10:00:00Z",
                  committed_at: "2026-10-22T10:00:00Z",
                  subject: "Start the storefront",
                },
                {
                  oid: OTHER,
                  author: "Grace",
                  authored_at: "2026-10-21T10:00:00Z",
                  committed_at: "2026-10-21T10:00:00Z",
                  subject: "Older work",
                },
              ],
              total: p.get("path") === "src" ? 1 : 2,
            },
          });
        case "/query/code_find":
          return json({
            repo: "web",
            kind: "find",
            find: {
              tree: OTHER,
              matches: p.get("q") === "zzz" ? [] : FOUND,
              total: p.get("q") === "zzz" ? 0 : 43,
              files: 1204,
            },
          });
      }
      return json({}, 404);
    }),
  );
  return asked;
}

/** The engine, with `what` answered by `answer`; every request kept. */
function stubAnswering(
  what: string,
  answer: (url: URL) => Response,
  repo: Record<string, unknown> = detail(),
) {
  const asked = stubEngine(repo);
  const engine = vi.mocked(fetch).getMockImplementation()!;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname !== `/query/${what}`) return engine(input, init);
      asked.push(url);
      return answer(url);
    }),
  );
  return asked;
}

/** What Go to file finds for any query but `zzz`: the best two of 43. */
const FOUND = [
  { path: "cmd/web/main.go", oid: OTHER, mode: "100644", spans: [[8, 12]] },
  { path: "src/main_test.go", oid: OTHER, mode: "100644", spans: [[4, 8]] },
];

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

/** The head row's subject link, once it says `subject`. */
const headRow = (subject: string) =>
  waitFor(() => {
    const link = document.querySelector<HTMLAnchorElement>(".code-latest__subject");
    expect(link?.textContent).toBe(subject);
    return link!;
  });

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
  // THE CARD IS NAMED BY THE FILE AS IT IS, its tab README the one chosen.
  const card = screen.getByRole("region", { name: "README.md" });
  expect(within(card).getByRole("tab", { name: "README" }).getAttribute("aria-selected")).toBe(
    "true",
  );
  expect(screen.getAllByText("Engineering").length).toBeGreaterThan(0);
  expect(screen.getByText("The storefront")).toBeTruthy();
  const readme = reads(asked, "code_blob").find((u) => u.searchParams.get("path") === "README.md");
  expect(readme?.searchParams.get("ref")).toBe(HEAD);
  expect(reads(asked, "code_repo")[0]?.searchParams.get("name")).toBe("web");
  // THE LATEST COMMIT, one commit of the root's history at the listed commit.
  expect(await headRow("Start the storefront")).toBeTruthy();
  const latest = reads(asked, "code_log")[0];
  expect(latest?.searchParams.get("ref")).toBe(HEAD);
  expect(latest?.searchParams.get("limit")).toBe("1");
  expect(screen.getByRole("table", { name: "Files in main" })).toBeTruthy();
  // EACH ENTRY'S LAST COMMIT AND THE HEAD ROW'S MARKS, at the same commit.
  await waitFor(() => expect(reads(asked, "code_tree_commits")).toHaveLength(1));
  expect(reads(asked, "code_tree_commits")[0]!.searchParams.get("ref")).toBe(HEAD);
  await waitFor(() =>
    expect(
      reads(asked, "code_commit_marks").some((u) => u.searchParams.get("commits") === HEAD),
    ).toBe(true),
  );
});

// The About rail's sentence says its time in words; the latest commit's chip keeps the short form.
test("the About rail says when the repository was pushed in words, the latest commit in short", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-28T12:00:00Z"));
  stubEngine();
  mount("#/gitlet/repos/web");
  expect((await screen.findByText(/^Last pushed/)).textContent).toBe("Last pushed yesterday");
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

  const table = await screen.findByRole("table", { name: "Files in src at feature" });
  const link = within(table).getByRole("link", { name: "app.go" });
  expect(link.getAttribute("href")).toBe("#/gitlet/repos/web?ref=feature&path=src%2Fapp.go");
  const read = reads(asked, "code_blob").find((u) => u.searchParams.get("path") === "src");
  expect(read?.searchParams.get("ref")).toBe("feature");
});

/** The rows of the files table, each as its cells' words. */
const fileRows = (table: HTMLElement) =>
  [...table.querySelectorAll("tbody tr")].map((tr) =>
    [...tr.querySelectorAll("td")].map((td) => td.textContent),
  );

// EACH ENTRY NAMES ITS LAST COMMIT AND WHEN, directories first once the whole directory
// is here, git's order while it is paged; the head cells are a screen reader's alone.
// Mutation: sort a paged directory, or link an entry's subject to the branch, and this fails.
test("a directory's entries carry their last commit and age, directories first once the directory is whole", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-28T12:00:00Z"));
  stubEngine();
  mount("#/gitlet/repos/web");
  const table = await screen.findByRole("table", { name: "Files in main" });
  await waitFor(() =>
    expect(within(table).getAllByRole("link", { name: "Older work" })).toHaveLength(1),
  );
  expect(fileRows(table)).toEqual([
    ["srcDirectory", "Older work", "7d ago"],
    ["README.mdFile", "Start the storefront", "6d ago"],
  ]);
  expect(within(table).getByRole("link", { name: "Older work" }).getAttribute("href")).toBe(
    `#/gitlet/repos/web/commits/${OTHER}`,
  );
  expect(within(table).getByRole("link", { name: "Older work" }).getAttribute("title")).toBe(
    "Older work by Grace",
  );
  expect(
    within(table)
      .getAllByRole("columnheader")
      .map((h) => h.textContent),
  ).toEqual(["Name", "Last commit", "Updated"]);
  expect(table.querySelector("thead")!.classList.contains("sr-only")).toBe(true);

  cleanup();
  stubAnswering("code_tree", (url) =>
    json(
      byRef(url, {
        kind: "tree",
        next: "e:2",
        tree: {
          tree: OTHER,
          entries: [
            { mode: "100644", type: "blob", oid: OTHER, path: "README.md", size: 20 },
            { mode: "040000", type: "tree", oid: OTHER, path: "src" },
          ],
          total: 3,
        },
      }),
    ),
  );
  mount("#/gitlet/repos/web");
  const paged = await screen.findByRole("table", { name: "Files in main" });
  expect(fileRows(paged).map((r) => r[0])).toEqual(["README.mdFile", "srcDirectory"]);
  expect(screen.getByText("2 of 3 entries loaded")).toBeTruthy();
});

// J AND K WALK A DIRECTORY'S ENTRIES, each focusing its name's link, and Enter elsewhere
// opens the one the cursor is on. Mutation: drop useListKeys from Entries, and this fails.
test("j and k walk a directory's entries and Enter opens one", async () => {
  stubEngine();
  mount("#/gitlet/repos/web");
  const table = await screen.findByRole("table", { name: "Files in main" });
  const src = await within(table).findByRole("link", { name: "src" });
  const readme = within(table).getByRole("link", { name: "README.md" });
  fireEvent.keyDown(window, { key: "j" });
  expect(document.activeElement).toBe(src);
  fireEvent.keyDown(window, { key: "j" });
  expect(document.activeElement).toBe(readme);
  fireEvent.keyDown(window, { key: "k" });
  expect(document.activeElement).toBe(src);
  fireEvent.keyDown(window, { key: "j" });
  readme.blur();
  fireEvent.keyDown(window, { key: "Enter" });
  await waitFor(() => expect(location.hash).toBe("#/gitlet/repos/web?path=README.md"));
});

/** A tree-commits page naming `entries`, each commit a log row named by its subject. */
function treeCommits(entries: Record<string, unknown>[], next?: string, extra = {}) {
  return json({
    repo: "web",
    kind: "tree_commits",
    ...(next ? { next } : {}),
    tree_commits: {
      tree: OTHER,
      entries,
      commits: entries
        .filter((e) => e.commit)
        .map((e) => ({
          oid: e.commit,
          author: "Ada",
          authored_at: "2026-10-22T10:00:00Z",
          committed_at: "2026-10-22T10:00:00Z",
          subject: `Last of ${String(e.path)}`,
        })),
      total: 3,
      ...extra,
    },
  });
}

// THE LAST COMMITS ARE READ PAGE BY PAGE until every loaded entry is answered, each page
// asked with the one before's cursor. Mutation: stop after the first page, and this fails.
test("an entry's last commit is read page by page until every loaded entry is answered", async () => {
  const asked = stubAnswering("code_tree_commits", (url) =>
    url.searchParams.get("cursor") === "e:1"
      ? treeCommits([{ path: "src" }, { path: "README.md", out_of_time: true }], undefined, {
          out_of_time: true,
        })
      : treeCommits([{ path: "LICENSE", commit: HEAD }], "e:1"),
  );
  const tree = vi.mocked(fetch).getMockImplementation()!;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname !== "/query/code_tree") return tree(input, init);
      return json(
        byRef(url, {
          kind: "tree",
          tree: {
            tree: OTHER,
            entries: [
              { mode: "100644", type: "blob", oid: OTHER, path: "LICENSE", size: 20 },
              { mode: "100644", type: "blob", oid: OTHER, path: "README.md", size: 20 },
              { mode: "040000", type: "tree", oid: OTHER, path: "src" },
            ],
            total: 3,
          },
        }),
      );
    }),
  );
  mount("#/gitlet/repos/web");
  expect(await screen.findByText("Not found in one read's time")).toBeTruthy();
  expect(screen.getByText("No file in it was ever committed")).toBeTruthy();
  expect(screen.getByRole("link", { name: "Last of LICENSE" })).toBeTruthy();
  const pages = reads(asked, "code_tree_commits");
  expect(pages.map((u) => u.searchParams.get("cursor"))).toEqual([null, "e:1"]);
  expect(screen.getByText("Not found in one read's time").getAttribute("title")).toBe(
    "Finding its last commit ran past one read's time. Reload to look again.",
  );
});

// A REFUSAL OR A FAILURE IS SAID ONCE UNDER THE ENTRIES, whose names still draw; a failed
// page is asked again by Retry. Mutation: a line per row, or no Retry, and this fails.
test("refused or failed last commits are said once under the entries, and a failed page is asked again by Retry", async () => {
  stubAnswering("code_tree_commits", () =>
    engineJson({ error: "unavailable", detail: "this node answers no last commits yet" }, 503),
  );
  mount("#/gitlet/repos/web");
  const note = await screen.findByText(
    "Each entry's last commit is unavailable: this node answers no last commits yet.",
  );
  expect(note.closest("[role=status]")).toBeTruthy();
  expect(screen.getByRole("link", { name: "README.md" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();

  cleanup();
  let fail = true;
  const asked = stubAnswering("code_tree_commits", () =>
    fail
      ? engineJson({ error: "query_failed" }, 500)
      : treeCommits([{ path: "src", commit: HEAD }]),
  );
  mount("#/gitlet/repos/web");
  await screen.findByText("Each entry's last commit could not be read.", { exact: false });
  fail = false;
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  expect(await screen.findByRole("link", { name: "Last of src" })).toBeTruthy();
  expect(reads(asked, "code_tree_commits")).toHaveLength(2);
});

// THE PICKER READS NOTHING UNTIL IT OPENS: the Code tab, a directory and
// Commits ask no listing; an opening asks one page of the tab it shows.
// Mutation: read the listings on the page again, and this fails.
test("the revision picker reads nothing until it opens, and then one page of the tab it shows", async () => {
  const asked = stubEngine();
  mount("#/gitlet/repos/web");

  // THE RAIL'S OWN READS at the root, the default branch's find and the newest tag, are not the picker's.
  const picker = (query: "code_refs" | "code_tags") =>
    reads(asked, query).filter(
      (u) => !u.searchParams.has("contains") && u.searchParams.get("limit") !== "1",
    );
  await screen.findByRole("link", { name: "src" });
  act(() => {
    location.hash = "#/gitlet/repos/web?path=src";
  });
  expect(await screen.findByRole("link", { name: "app.go" })).toBeTruthy();
  act(() => {
    location.hash = "#/gitlet/repos/web?tab=commits";
  });
  expect(await screen.findByRole("group", { name: "History of main" })).toBeTruthy();
  expect(picker("code_refs")).toHaveLength(0);
  expect(picker("code_tags")).toHaveLength(0);

  fireEvent.click(screen.getByRole("button", { name: "Branch or tag: main" }));
  const panel = await screen.findByRole("dialog", { name: "Switch branches or tags" });
  await within(panel).findByRole("option", { name: /^feature/ });
  const moved = picker("code_refs");
  expect(moved).toHaveLength(1);
  expect(Object.fromEntries(moved[0]!.searchParams)).toMatchObject({
    order: "moved",
    detail: "true",
    limit: "10",
  });
  fireEvent.click(within(panel).getByRole("tab", { name: /Tags/ }));
  await within(panel).findByRole("option", { name: /^v3/ });
  expect(picker("code_tags").map((u) => u.searchParams.get("limit"))).toEqual(["10"]);

  fireEvent.click(within(panel).getByRole("button", { name: "Close" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  fireEvent.click(screen.getByRole("button", { name: "Branch or tag: main" }));
  await screen.findByRole("option", { name: /^feature/ });
  expect(picker("code_refs")).toHaveLength(2);
});

/** Six branches by when each moved, main on the page's second row. */
const MOVED = [
  ["frontend/stream-shell", "2026-10-27T11:00:00Z"],
  ["main", "2026-10-27T10:00:00Z"],
  ["backend/network-token", "2026-10-27T09:00:00Z"],
  ["frontend/badge-cache", "2026-10-27T08:00:00Z"],
  ["release/2026.11", "2026-10-26T08:00:00Z"],
  ["sre/drain", "2026-10-26T07:00:00Z"],
].map(([name, at]) => ({
  ref: `refs/heads/${name}`,
  oid: name === "main" ? HEAD : OTHER,
  position: POSITION,
  moved_at: at,
  ...(name === "main" || name === "release/2026.11"
    ? { protection: { patterns: [`refs/heads/${name}`], direct_push: "none" } }
    : {}),
}));

const openPicker = async (name = "Branch or tag: main") => {
  fireEvent.click(await screen.findByRole("button", { name }));
  return screen.findByRole("dialog", { name: "Switch branches or tags" });
};

const optionNames = (panel: HTMLElement) =>
  within(panel)
    .getAllByRole("option")
    .map((o) => o.querySelector(".code-picker__name")?.textContent ?? o.textContent);

// THE DEFAULT BRANCH IS PINNED FIRST with Default, the rest by when each moved,
// the protected with their shield named by the rules. Mutation: list the page
// in its own order, or drop the shield's words, and this fails.
test("the picker pins the default branch with Default and lists the rest by when they moved, the protected with their shield", async () => {
  stubListing((p) => json(refsListing(p, MOVED)), detail({ branches: 6 }));
  mount("#/gitlet/repos/web");

  const panel = await openPicker();
  await within(panel).findByRole("option", { name: /^sre\/drain/ });
  expect(optionNames(panel)).toEqual([
    "main",
    "frontend/stream-shell",
    "backend/network-token",
    "frontend/badge-cache",
    "release/2026.11",
    "sre/drain",
  ]);
  const main = within(panel).getAllByRole("option")[0]!;
  expect(main.getAttribute("aria-current")).toBe("true");
  expect(main.querySelector(".code-picker__check")).not.toBeNull();
  expect(within(main).getByText("Default")).toBeTruthy();
  expect(within(main).getByRole("img").getAttribute("aria-label")).toBe(
    "Protected by main: no force-push, no delete, pushes through changes only",
  );
  const when = main.querySelector("time")!;
  expect(when.getAttribute("datetime")).toBe("2026-10-27T10:00:00Z");
  expect(when.getAttribute("title")).toBeTruthy();
  const shields = within(panel)
    .getAllByRole("img")
    .map((s) => s.closest("[role=option]")?.querySelector(".code-picker__name")?.textContent);
  expect(shields).toEqual(["main", "release/2026.11"]);
  expect(within(panel).getAllByRole("option")[1]!.getAttribute("aria-current")).toBeNull();
});

// A BRANCH IS CHOSEN FROM THE FIELD: the arrows move the highlight, Enter takes
// it, the panel closes and its button has the focus back. Mutation: move focus
// into the list, or leave the panel open, and this fails.
test("a branch is chosen by the keys, and the panel gives focus back to its button", async () => {
  stubListing((p) => json(refsListing(p, MOVED)), detail({ branches: 6 }));
  mount("#/gitlet/repos/web");

  const panel = await openPicker();
  await within(panel).findByRole("option", { name: /^sre\/drain/ });
  const field = within(panel).getByRole("combobox", { name: "Find a branch" });
  await waitFor(() => expect(document.activeElement).toBe(field));
  fireEvent.keyDown(field, { key: "ArrowDown" });
  fireEvent.keyDown(field, { key: "ArrowDown" });
  expect(field.getAttribute("aria-activedescendant")).toBe(
    within(panel).getAllByRole("option")[2]!.id,
  );
  fireEvent.keyDown(field, { key: "Enter" });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(new URLSearchParams(location.hash.split("?")[1]).get("ref")).toBe("backend/network-token");
  const button = await screen.findByRole("button", {
    name: "Branch or tag: backend/network-token",
  });
  await waitFor(() => expect(document.activeElement).toBe(button));

  const again = await openPicker("Branch or tag: backend/network-token");
  fireEvent.keyDown(within(again).getByRole("combobox"), { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(new URLSearchParams(location.hash.split("?")[1]).get("ref")).toBe("backend/network-token");
});

// A FIND IS THE ENGINE'S, asked once the typing rests, and an empty page that
// the engine stopped short offers to keep looking. Mutation: ask on every key,
// or say "none" of a page the engine cut short, and this fails.
test("a find asks the engine once the typing stops, and says when nothing matches", async () => {
  const asked = stubListing(
    (p) => {
      if (p.get("contains") === "zzz" && !p.get("cursor")) {
        return json({ refs_detail: [], next: "c2", position: POSITION, level: "session" });
      }
      return json(refsListing(p, MOVED));
    },
    detail({ branches: 6 }),
  );
  mount("#/gitlet/repos/web");

  const panel = await openPicker();
  await within(panel).findByRole("option", { name: /^sre\/drain/ });
  const field = within(panel).getByRole("combobox", { name: "Find a branch" });
  fireEvent.change(field, { target: { value: "front" } });
  expect(within(panel).getByText("Looking for “front”…")).toBeTruthy();
  expect(listed(asked).filter((u) => u.searchParams.get("contains"))).toHaveLength(0);
  await within(panel).findByRole("option", { name: /^frontend\/badge-cache/ });
  expect(optionNames(panel)).toEqual(["frontend/stream-shell", "frontend/badge-cache"]);
  expect(listed(asked).map((u) => u.searchParams.get("contains"))).toEqual([null, "front"]);

  fireEvent.change(field, { target: { value: "nothing" } });
  expect(await within(panel).findByText("No branch name holds “nothing”")).toBeTruthy();
  expect(within(panel).getByText("A find matches any part of a name, in any case.")).toBeTruthy();

  fireEvent.change(field, { target: { value: "zzz" } });
  expect(await within(panel).findByText("None among the branches read so far")).toBeTruthy();
  fireEvent.mouseDown(within(panel).getByRole("option", { name: "Keep looking" }));
  await waitFor(() =>
    expect(listed(asked).map((u) => u.searchParams.get("cursor"))).toContain("c2"),
  );
  expect(await within(panel).findByText("No branch name holds “zzz”")).toBeTruthy();
});

// TAGS ARE NEWEST FIRST, Latest on the first only, dated in View all tags' words, chosen
// by whole ref; a tag's find asks the names under refs/tags/ and says it carries no date.
// Mutation: write a tag's short name, or Latest every row, and this fails.
test("tags are newest first with Latest, and a tag is chosen by its whole ref", async () => {
  const asked = stubEngine();
  mount("#/gitlet/repos/web");

  const panel = await openPicker();
  fireEvent.click(within(panel).getByRole("tab", { name: /Tags/ }));
  await within(panel).findByRole("option", { name: /^v1/ });
  expect(optionNames(panel)).toEqual(["v3", "v2", "v1"]);
  expect(within(panel).getAllByText("Latest")).toHaveLength(1);
  const [, late, undated] = within(panel).getAllByRole("option");
  expect(within(late!).getByText("after the year 9999")).toBeTruthy();
  expect(within(undated!).getByText("undated").getAttribute("title")).toMatch(/no date of its own/);

  const field = within(panel).getByRole("combobox", { name: "Find a tag" });
  fireEvent.change(field, { target: { value: "v" } });
  expect(await within(panel).findByText("Found by name; dates are on View all tags.")).toBeTruthy();
  const found = reads(asked, "code_refs").find((u) => u.searchParams.get("contains") === "v")!;
  expect(found.searchParams.get("prefix")).toBe("refs/tags/");
  fireEvent.mouseDown(await within(panel).findByRole("option", { name: /^v1/ }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(new URLSearchParams(location.hash.split("?")[1]).get("ref")).toBe("refs/tags/v1");
  expect(await screen.findByRole("button", { name: "Branch or tag: v1" })).toBeTruthy();
});

// A REFUSED MOVED LISTING SAYS THE ENGINE'S SENTENCE and lists nothing by name
// instead. Mutation: fall back to the listing by name, and this fails.
test("a refused moved listing says the engine's sentence and lists nothing by name", async () => {
  const asked = stubListing(() =>
    json({ error: "bad_params", detail: "order is not a key this build reads" }, 400),
  );
  mount("#/gitlet/repos/web");

  const panel = await openPicker();
  expect(
    await within(panel).findByText(
      "Listing branches by when they moved is unavailable: order is not a key this build reads",
    ),
  ).toBeTruthy();
  expect(within(panel).queryAllByRole("option")).toHaveLength(0);
  expect(
    reads(asked, "code_refs").filter((u) => u.searchParams.get("detail") !== "true"),
  ).toHaveLength(0);
});

const openCode = async () => {
  const button = await waitFor(() => {
    const found = screen
      .getAllByRole("button", { name: "Code" })
      .find((b) => b.getAttribute("aria-haspopup") === "dialog");
    expect(found).toBeTruthy();
    return found!;
  });
  fireEvent.click(button);
  return screen.findByRole("dialog", { name: "Clone web" });
};

// THE ADDRESS IS THE ENGINE'S, never the page's own origin, and the command is
// the whole git clone of it. Mutation: build the address from location.origin,
// and this fails.
test("the Code menu gives the engine's clone address and its command", async () => {
  let written = "";
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: { writeText: (t: string) => ((written = t), Promise.resolve()) },
  });
  stubEngine();
  mount("#/gitlet/repos/web");

  const panel = await openCode();
  expect(within(panel).getByText(CLONE_URL)).toBeTruthy();
  expect(within(panel).getByRole("button", { name: "Copy the clone address" })).toBeTruthy();
  fireEvent.click(within(panel).getByRole("button", { name: "Copy the clone command" }));
  await waitFor(() => expect(written).toBe(`git clone ${CLONE_URL}`));
  expect(
    await within(panel).findByRole("button", { name: "Copied the clone command" }),
  ).toBeTruthy();
});

// A DEPLOYMENT WITH NO ADDRESS OF ITS OWN SAYS SO, never the browser's guess,
// and holds the command with the reason; so does the empty repository's card.
// Mutation: fall back to the page's origin, and this fails.
test("a deployment with no address says so and holds the command", async () => {
  stubEngine(detail({ clone_url: null }));
  mount("#/gitlet/repos/web");

  const panel = await openCode();
  expect(panel.querySelector(".code-clone__missing")?.textContent).toMatch(
    /names no address of its own.*api\.external_url/,
  );
  expect(within(panel).queryByText(/git asks for a password/)).toBeNull();
  const held = within(panel).getByRole("button", { name: "Copy the clone command" });
  expect(held.getAttribute("aria-disabled")).toBe("true");
  const why = document.getElementById(held.getAttribute("aria-describedby") ?? "");
  expect(why?.textContent).toMatch(/names no address of its own/);
  cleanup();

  stubEngine(detail({ clone_url: null, head: undefined, branches: 0 }));
  mount("#/gitlet/repos/web");
  expect(await screen.findByText("This repository is empty")).toBeTruthy();
  expect(screen.getAllByText(/names no address of its own/).length).toBeGreaterThan(0);
  expect(screen.queryByText(/git remote add origin/)).toBeNull();
});

/** The engine as stubEngine answers it, the archive's probe answered `status`; every probe kept. */
function stubArchive(status: number, repo: Record<string, unknown> = detail()) {
  const asked = stubEngine(repo);
  const engine = vi.mocked(fetch).getMockImplementation()!;
  const probes: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname !== "/git/web/-/archive.zip") return engine(input, init);
      probes.push(init?.method ?? "GET");
      return new Response(null, { status });
    }),
  );
  return { asked, probes };
}

const zipOf = (panel: HTMLElement) =>
  within(panel)
    .getAllByText("Download ZIP")
    .map((t) => t.closest("a, button") as HTMLElement)[0]!;

// DOWNLOAD ZIP IS THE LISTING'S COMMIT, whole, never the branch's name, which
// the route refuses. Mutation: link `rev=` to the address's ref, and this fails.
test("Download ZIP downloads the commit the listing resolved", async () => {
  stubArchive(405);
  mount("#/gitlet/repos/web?ref=feature");

  const panel = await openCode();
  await waitFor(() => expect(zipOf(panel).tagName).toBe("A"));
  const link = zipOf(panel) as HTMLAnchorElement;
  expect(link.getAttribute("href")).toBe(`/git/web/-/archive.zip?rev=${HEAD}`);
  expect(link.hasAttribute("download")).toBe(true);
  expect(link.getAttribute("title")).toBe("Every file of 0123456, as a ZIP");
});

// AN OLDER FLEET HOLDS THE DOWNLOAD WITH ITS REASON, asked once per name, on the
// menu's first opening. Mutation: draw the link on a 404, or probe on the page's
// mount or on every opening, and this fails.
test("Download ZIP is held on a fleet that does not serve it, and the probe is asked once", async () => {
  const { probes } = stubArchive(404);
  mount("#/gitlet/repos/web");
  await screen.findByRole("link", { name: "src" });
  expect(probes).toEqual([]);

  let panel = await openCode();
  const held = await waitFor(() => {
    const zip = zipOf(panel);
    expect(zip.getAttribute("aria-disabled")).toBe("true");
    const why = document.getElementById(zip.getAttribute("aria-describedby") ?? "");
    expect(why?.textContent).toBe("Downloading a ZIP is not available on this fleet yet.");
    return zip;
  });
  expect(held.tagName).toBe("BUTTON");
  fireEvent.keyDown(panel, { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  panel = await openCode();
  await waitFor(() => expect(zipOf(panel).getAttribute("aria-disabled")).toBe("true"));
  expect(probes).toEqual(["HEAD"]);
});

// A COMMIT NOT YET KNOWN IS NO DOWNLOAD: held, saying what it waits for.
test("Download ZIP is held while the listing's commit is unknown", async () => {
  const { asked } = stubArchive(405);
  let release = () => {};
  const engine = vi.mocked(fetch).getMockImplementation()!;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname === "/query/code_tree") {
        asked.push(url);
        await new Promise<void>((done) => (release = done));
      }
      return engine(input, init);
    }),
  );
  mount("#/gitlet/repos/web");

  const panel = await openCode();
  const zip = zipOf(panel);
  expect(zip.getAttribute("aria-disabled")).toBe("true");
  expect(document.getElementById(zip.getAttribute("aria-describedby") ?? "")?.textContent).toBe(
    "Waiting for the commit to download.",
  );
  release();
  await waitFor(() => expect(zipOf(panel).tagName).toBe("A"));
});

// A LISTING THAT FAILED IS NO DOWNLOAD: held, saying the commit could not be read.
// Mutation: say it waits for the commit instead, and this fails.
test("Download ZIP is held with its reason when the listing fails", async () => {
  stubArchive(405);
  const engine = vi.mocked(fetch).getMockImplementation()!;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) =>
      new URL(String(input)).pathname === "/query/code_tree"
        ? engineJson({ error: "query_failed" }, 500)
        : engine(input, init),
    ),
  );
  mount("#/gitlet/repos/web");

  const panel = await openCode();
  await waitFor(() => {
    const zip = zipOf(panel);
    expect(zip.getAttribute("aria-disabled")).toBe("true");
    expect(document.getElementById(zip.getAttribute("aria-describedby") ?? "")?.textContent).toBe(
      "The commit could not be read, so there is nothing to download yet.",
    );
  });
});

// THE EMPTY REPOSITORY CLONES FROM THE ENGINE'S ADDRESS, and Code is at the
// root's bar only. Mutation: draw the menu under a path, and this fails.
test("the empty repository clones from the engine's address, and Code is at the root only", async () => {
  stubEngine(detail({ head: undefined, branches: 0 }));
  mount("#/gitlet/repos/web");
  expect(await screen.findByText("This repository is empty")).toBeTruthy();
  expect(screen.getByText(CLONE_URL)).toBeTruthy();
  cleanup();

  stubEngine();
  mount("#/gitlet/repos/web?path=src");
  expect(await screen.findByRole("link", { name: "app.go" })).toBeTruthy();
  expect(
    screen
      .queryAllByRole("button", { name: "Code" })
      .filter((b) => b.getAttribute("aria-haspopup") === "dialog"),
  ).toHaveLength(0);
  expect(screen.getByRole("button", { name: "Branch or tag: main" })).toBeTruthy();
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

  const permalink = screen.getByRole("link", { name: "Permalink: this file at 0123456" });
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

/** The engine answering `what` from `answer`, everything else as it answered before. */
function stubQuery(asked: URL[], what: string, answer: (p: URLSearchParams) => Response) {
  const engine = vi.mocked(fetch).getMockImplementation()!;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname !== `/query/${what}`) return engine(input, init);
      asked.push(url);
      return answer(url.searchParams);
    }),
  );
}

/** The engine answering `code_refs` in detail from `answer`, everything else as stubEngine does. */
function stubListing(
  answer: (p: URLSearchParams) => Response,
  repo: Record<string, unknown> = detail(),
  acted: Acted[] = [],
) {
  const asked = stubEngine(repo, acted);
  const engine = vi.mocked(fetch).getMockImplementation()!;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname !== "/query/code_refs" || url.searchParams.get("detail") !== "true")
        return engine(input, init);
      asked.push(url);
      return answer(url.searchParams);
    }),
  );
  return asked;
}

const listed = (asked: URL[]) =>
  reads(asked, "code_refs").filter((u) => u.searchParams.get("order") === "moved");

// A BRANCH OPENS ITS FILES: the default at the repository's own address, any other
// by name; the Overview asks by when each moved since the reader's midnight 90 days
// back. Mutation: link the default by name, or count the window from now.
test("the branches are the engine's refs, each opening its files", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-28T12:00:00Z"));
  pinZoneForTest("America/New_York");
  try {
    const asked = stubEngine();
    mount("#/gitlet/repos/web?tab=branches");

    const active = await screen.findByRole("table", { name: /^Active branches/ });
    const first = listed(asked)[0]!.searchParams;
    expect(first.get("detail")).toBe("true");
    expect(first.get("moved_since")).toBe("2026-07-30T04:00:00.000Z");
    expect(first.get("active_since")).toBe("2026-07-30T04:00:00.000Z");
    const found = reads(asked, "code_refs").find((u) => u.searchParams.get("contains") === "main");
    expect(found?.searchParams.get("detail")).toBe("true");

    const main = await screen.findByRole("table", { name: "Default branch" });
    expect(within(main).getByText("Default")).toBeTruthy();
    expect(
      within(main)
        .getAllByRole("link")
        .map((a) => a.getAttribute("href")),
    ).toContain("#/gitlet/repos/web");
    // THE DEFAULT BRANCH IS ITS OWN BOX, never one of the active ones.
    const links = within(active)
      .getAllByRole("link")
      .map((a) => a.getAttribute("href"));
    expect(links).toContain("#/gitlet/repos/web?ref=feature");
    expect(links).not.toContain("#/gitlet/repos/web");
    expect([...active.querySelectorAll(".code-branch-chip")].map((c) => c.textContent)).toEqual([
      "feature",
    ]);
  } finally {
    pinZoneForTest("");
  }
});

// A CURSOR PAGE NEVER ASKS THE ACTIVE COUNT, which the engine refuses past a
// first page. Mutation: spread the first page's parameters into the cursor's.
test("a cursor page never asks the active count", async () => {
  const asked = stubListing((p) =>
    json({
      ...refsListing(p, p.get("cursor") ? [REF_ROWS[1]!] : [REF_ROWS[0]!]),
      ...(p.get("cursor") ? {} : { next: "m1" }),
    }),
  );
  mount("#/gitlet/repos/web?tab=branches&view=active");
  fireEvent.click(await screen.findByRole("button", { name: "Load more branches" }));
  await waitFor(() => expect(listed(asked).some((u) => u.searchParams.get("cursor"))).toBe(true));
  const [page1, page2] = [
    listed(asked).find((u) => !u.searchParams.get("cursor"))!.searchParams,
    listed(asked).find((u) => u.searchParams.get("cursor"))!.searchParams,
  ];
  expect(page1.get("active_since")).toBeTruthy();
  expect(page2.get("cursor")).toBe("m1");
  expect(page2.get("active_since")).toBeNull();
  expect(page2.get("moved_since")).toBe(page1.get("moved_since"));
});

// WHO MOVED A BRANCH IS ITS PUSHER, Gitlet for a merge or a queue landing, and past
// the push history it is said so. Mutation: draw the merger's or the queue's actor,
// or call a mover past 181 days unknown.
test("who moved a branch is its pusher's face, Gitlet's for a merge, and unknown past the history", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-28T12:00:00Z"));
  const at = (days: number) => new Date(Date.parse("2026-10-28T12:00:00Z") - days * 86_400_000);
  stubListing((p) =>
    json(
      refsListing(p, [
        ...REF_ROWS,
        {
          ref: "refs/heads/landed",
          oid: THIRD,
          position: POSITION,
          moved_at: "2026-10-27T09:00:00Z",
          moved_by: mover("cto", "queue"),
        },
        { ref: "refs/heads/kept", oid: THIRD, position: POSITION, moved_at: at(100).toISOString() },
        { ref: "refs/heads/gone", oid: THIRD, position: POSITION, moved_at: at(200).toISOString() },
      ]),
    ),
  );
  mount("#/gitlet/repos/web?tab=branches&view=all");
  const all = await screen.findByRole("table", { name: /^All branches/ });
  const updated = (name: string) =>
    within(all).getByRole("link", { name }).closest("tr")!.querySelector(".code-refs__updated")!;
  const merged = updated("main").querySelector<HTMLElement>(".code-byline")!;
  expect(merged.textContent).toBe("Gitletyesterday");
  expect(merged.title).toBe("Gitlet moved main when CTO merged a change");
  expect(merged.querySelector(".code-face")).not.toBeNull();
  const landed = updated("landed").querySelector<HTMLElement>(".code-byline")!;
  expect(landed.textContent).toBe("Gitletyesterday");
  expect(landed.title).toBe("Gitlet moved landed landing its merge queue's train, for CTO");
  expect(landed.querySelector(".code-face")).not.toBeNull();
  const pushed = updated("feature").querySelector<HTMLElement>(".code-byline")!;
  expect(pushed.textContent).toBe("CTOyesterday");
  expect(pushed.title).toBe("Pushed with git");
  expect(pushed.querySelector(".code-face")).toBeNull();
  expect(updated("kept").textContent).toMatch(/^Unknown/);
  expect(updated("gone").textContent).toMatch(/^No longer kept/);
  expect(updated("gone").querySelector<HTMLElement>(".code-byline")!.title).toBe(
    "Who moved it is no longer kept: pushes are kept 181 days.",
  );
});

// A BRANCH'S CHANGE IS ITS PILL, opening the change, and Protected names its
// rules in words. Mutation: link the pill anywhere else, or drop a clause.
test("a branch's change is its pill, and protection names its rules", async () => {
  stubListing((p) =>
    json(
      refsListing(p, [
        ...REF_ROWS,
        {
          ref: "refs/heads/release/2026.11",
          oid: THIRD,
          position: POSITION,
          moved_at: "2026-10-26T10:00:00Z",
          moved_by: mover("cto", "git"),
          protection: { patterns: ["refs/heads/release/*"], direct_push: "none" },
          change: { number: 41, title: "Cut the release", state: "merged" },
          delete_refusal: "protected_delete",
          delete_detail: "release/2026.11 is protected: its rule refuses a delete",
        },
      ]),
    ),
  );
  mount("#/gitlet/repos/web?tab=branches&view=all");
  const all = await screen.findByRole("table", { name: /^All branches/ });
  const open = within(all).getByTitle("Change web!42, open: Stream the shell");
  expect(open.getAttribute("href")).toBe("#/gitlet/repos/web/changes/42");
  expect(within(all).getByTitle("Change web!41, merged: Cut the release")).toBeTruthy();
  const release = within(all).getByRole("link", { name: "release/2026.11" }).closest("tr")!;
  expect(within(release).getByText("Protected").closest("[title]")?.getAttribute("title")).toBe(
    "Protected by release/*: no force-push, no delete, pushes through changes only",
  );
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
  // THE CHECKS TAB'S MARKER asks the default branch's head too, on every tab.
  await waitFor(() => expect(marks).toHaveLength(3));
  expect(marks).toEqual([{ commits: HEAD }, { commits: HEAD }, { commits: OTHER }]);

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
  // ONE ASK BY THE HISTORY, one by the Checks tab's marker; Retry is the history's.
  expect(marksAsked).toHaveLength(2);
  fireEvent.click(within(failed).getByRole("button", { name: "Retry" }));
  await waitFor(() => expect(marksAsked).toHaveLength(3));
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
test("the About rail says how far an import went, and its Mirror where the mirror stands", async () => {
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
  // THE PULL MIRROR IS THE RAIL'S MIRROR: its address, then what it does.
  const mirror = screen.getByRole("region", { name: "Mirror" });
  expect(within(mirror).getByRole("link", { name: "forge.example.com/acme/web" })).toBeTruthy();
  expect(within(mirror).getByText(/^Follows it;/).textContent).toBe("Follows it; not synced yet.");
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

  await screen.findByRole("table", { name: "Default branch" });
  expect(await screen.findByRole("button", { name: "Delete feature" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Delete main" })).toBeNull();
});

// EACH BRANCH'S CHECKS ARE ITS HEAD'S, asked once a page, for itself else for the
// default; no rollup is never a cross, and past 30 days is said no longer kept.
// Mutation: ask per row, read only the branch's own target, or drop the retention word.
test("each branch's checks are its head's, for itself or for the default branch", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-28T12:00:00Z"));
  const old = {
    ref: "refs/heads/old",
    oid: "4444444444444444444444444444444444444444",
    position: POSITION,
    moved_at: "2026-08-01T10:00:00Z",
  };
  const bare = {
    ref: "refs/heads/bare",
    oid: THIRD,
    position: POSITION,
    moved_at: "2026-10-26T10:00:00Z",
  };
  const asked = stubListing((p) => json(refsListing(p, [...REF_ROWS, bare, old])));
  stubQuery(asked, "code_commit_marks", () =>
    json({
      commit_marks: [
        { commit: HEAD, checks: [rollup("refs/heads/main", "passed")] },
        { commit: OTHER, checks: [rollup("refs/heads/main", "running")] },
      ],
      position: POSITION,
      level: "session",
    }),
  );
  mount("#/gitlet/repos/web?tab=branches&view=all");
  const all = await screen.findByRole("table", { name: /^All branches/ });
  const checks = (name: string) =>
    within(all)
      .getByRole("link", { name })
      .closest("tr")!
      .querySelector<HTMLElement>(".code-refs__checks")!;
  await waitFor(() => expect(checks("main").textContent).toBe("Passed 2 of 2"));
  expect(checks("feature").textContent).toBe("Running");
  expect(within(checks("bare")).getByText("No check ran")).toBeTruthy();
  expect(checks("bare").querySelector(".code-check-mark")).toBeNull();
  expect(within(checks("old")).getByText("Checks no longer kept")).toBeTruthy();
  // The Checks tab's marker reads the head alone; the table asks once for every tip.
  const marks = reads(asked, "code_commit_marks").filter(
    (u) => u.searchParams.get("commits") !== HEAD,
  );
  expect(marks).toHaveLength(1);
  expect(marks[0]!.searchParams.get("commits")).toBe([OTHER, HEAD, THIRD, old.oid].join(","));
});

// HOW FAR EACH BRANCH IS FROM THE DEFAULT is read once, by whole ids: the heads
// against the default branch's own head. Mutation: name the base by its branch,
// or draw a count the read ran out of time on.
test("how far each branch is from the default is read once, by whole ids", async () => {
  const slow = {
    ref: "refs/heads/slow",
    oid: THIRD,
    position: POSITION,
    moved_at: "2026-10-26T10:00:00Z",
  };
  const asked = stubListing((p) => json(refsListing(p, [...REF_ROWS, slow])));
  stubQuery(asked, "code_tips", (p) =>
    json({
      repo: "web",
      kind: "tips",
      tips: {
        base: HEAD,
        tips: [tipOf(OTHER, 0, 3), tipOf(THIRD)].filter((t) =>
          p.get("oids")!.split(",").includes(t.oid),
        ),
      },
    }),
  );
  mount("#/gitlet/repos/web?tab=branches&view=all");
  const all = await screen.findByRole("table", { name: /^All branches/ });
  const drift = (name: string) =>
    within(all)
      .getByRole("link", { name })
      .closest("tr")!
      .querySelector<HTMLElement>(".code-refs__drift")!;
  expect(
    await within(all).findByRole("img", { name: "0 commits behind main, 3 ahead" }),
  ).toBeTruthy();
  expect(within(drift("slow")).getByText("Not counted in the read's time")).toBeTruthy();
  expect(drift("main").textContent).toBe("");
  const tips = reads(asked, "code_tips");
  expect(tips).toHaveLength(1);
  expect(tips[0]!.searchParams.get("base")).toBe(HEAD);
  expect(tips[0]!.searchParams.get("oids")).toBe([OTHER, THIRD].join(","));
});

// DELETE IS HELD WITH THE ENGINE'S OWN SENTENCE where its rules refuse it, and
// the client's own hold, which knows the reader's seat, wins over it.
// Mutation: drop the listing's sentence, or let it win over the client's.
test("Delete is held with the engine's own sentence", async () => {
  const refused = {
    ref: "refs/heads/release",
    oid: THIRD,
    position: POSITION,
    moved_at: "2026-10-26T10:00:00Z",
    delete_refusal: "protected_delete",
    delete_detail: "release is protected: its rule refuses a delete",
  };
  stubListing((p) => json(refsListing(p, [...REF_ROWS, refused])), detail({ managed_by: "tools" }));
  mount("#/gitlet/repos/web?tab=branches&view=all", "web", MAINTAINER);
  const del = await screen.findByRole("button", { name: "Delete release" });
  await waitFor(() =>
    expect(del.getAttribute("title")).toBe("release is protected: its rule refuses a delete"),
  );
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Delete feature" }).getAttribute("title")).toBeNull(),
  );
  cleanup();

  stubListing(
    (p) => json(refsListing(p, [...REF_ROWS, refused])),
    detail({ managed_by: "tools", access: "write" }),
  );
  mount("#/gitlet/repos/web?tab=branches&view=all", "web", MAINTAINER);
  const held = await screen.findByRole("button", { name: "Delete release" });
  await waitFor(() =>
    expect(held.getAttribute("title")).toBe("You write web; deleting a branch takes maintain."),
  );
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
  expect(screen.getByRole("button", { name: "Branch or tag: main" })).toBeTruthy();
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
  mount("#/gitlet/repos/web?tab=branches&view=all", "web", MAINTAINER);

  const all = await screen.findByRole("table", { name: /^All branches/ });
  const link = await within(all).findByRole("link", { name: "Protection rule" });
  expect(link.getAttribute("href")).toContain("tab=settings");
  expect(within(all).getAllByRole("link", { name: "Protection rule" })).toHaveLength(1);
  expect(within(all).getAllByRole("button", { name: "Protect" })).toHaveLength(1);
  expect(
    within(all).getByText("Protected").closest("tr")?.querySelector(".code-branch-chip")
      ?.textContent,
  ).toBe("main");
});

// THE VIEWS ARE THE ADDRESS, each its own window of when a branch moved.
// Mutation: hold the view in state, or ask Stale with moved_since.
test("the views are the address", async () => {
  const asked = stubEngine();
  mount("#/gitlet/repos/web?tab=branches");
  await screen.findByRole("table", { name: /^Active branches/ });
  fireEvent.click(screen.getByRole("radio", { name: "Stale" }));
  await waitFor(() => expect(location.hash).toContain("view=stale"));
  await screen.findByRole("table", { name: /^Stale branches/ });
  const stale = listed(asked).at(-1)!.searchParams;
  expect(stale.get("moved_before")).toBeTruthy();
  expect(stale.get("moved_since")).toBeNull();
  fireEvent.click(screen.getByRole("radio", { name: "All" }));
  await screen.findByRole("table", { name: /^All branches/ });
  const all = listed(asked).at(-1)!.searchParams;
  expect(all.get("moved_before")).toBeNull();
  expect(all.get("moved_since")).toBeNull();
  expect(location.hash).toContain("view=all");
  fireEvent.click(screen.getByRole("radio", { name: "Tags" }));
  await waitFor(() => expect(location.hash).toContain("view=tags"));
  await screen.findByRole("table", { name: /^Tags/ });
  // Mutation: list the default branch in Active, and its rows outnumber the engine's count.
  fireEvent.click(screen.getByRole("radio", { name: "Active" }));
  const active = await screen.findByRole("table", { name: /^Active branches/ });
  expect(screen.queryByRole("table", { name: "Default branch" })).toBeNull();
  expect([...active.querySelectorAll(".code-branch-chip")].map((c) => c.textContent)).toEqual([
    "feature",
  ]);
});

// A FIND NARROWS EVERY VIEW BY ANY PART OF A NAME, written to the address once
// typing rests; a page the engine's scan bound ended with nothing yet offers
// to keep looking. Mutation: ask per keystroke, or draw an empty state there.
test("a find narrows every view by any part of a name", async () => {
  const asked = stubListing((p) =>
    p.get("contains") === "zzz" && !p.get("cursor")
      ? json({ refs_detail: [], next: "z1", position: POSITION, level: "session" })
      : json(refsListing(p)),
  );
  mount("#/gitlet/repos/web?tab=branches");
  await screen.findByRole("table", { name: /^Active branches/ });
  const find = screen.getByRole("searchbox", { name: "Find a branch" });
  fireEvent.change(find, { target: { value: "f" } });
  fireEvent.change(find, { target: { value: "fea" } });
  await waitFor(() => expect(location.hash).toContain("q=fea"));
  const found = await screen.findByRole("table", { name: "Branches matching “fea”" });
  expect(within(found).getAllByRole("link", { name: "feature" })).toHaveLength(1);
  expect(listed(asked).filter((u) => u.searchParams.get("contains") === "f")).toHaveLength(0);

  fireEvent.change(find, { target: { value: "zzz" } });
  expect(await screen.findByText("None among the branches read so far")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Keep looking" })).toBeTruthy();
  // Mutation: let Escape only empty the box, and the address keeps the find a pause longer.
  fireEvent.keyDown(find, { key: "Escape" });
  expect(location.hash).not.toContain("q=");
  expect((find as HTMLInputElement).value).toBe("");
});

// THE OVERVIEW LEADS TO EVERY ACTIVE BRANCH only when the engine counts more than
// it shows. Mutation: never draw the link, or draw it whatever the count.
test("the Overview links to all active branches only past the rows it shows", async () => {
  let count = 5;
  stubListing((p) =>
    json({
      ...refsListing(p),
      ...(p.get("active_since") ? { active_branches: { count } } : {}),
    }),
  );
  mount("#/gitlet/repos/web?tab=branches");
  const more = (await screen.findByRole("table", { name: /^Active branches/ })).closest("section")!;
  expect(
    within(more).getByRole("link", { name: "View all active branches" }).getAttribute("href"),
  ).toBe("#/gitlet/repos/web?tab=branches&view=active");
  cleanup();

  count = 1;
  mount("#/gitlet/repos/web?tab=branches");
  const all = (await screen.findByRole("table", { name: /^Active branches/ })).closest("section")!;
  expect(within(all).getByRole("link", { name: "feature" })).toBeTruthy();
  expect(within(all).queryByRole("link", { name: "View all active branches" })).toBeNull();
});

// THE DEFAULT BRANCH IS FOUND PAST A FIND'S FIRST PAGE, and one never found is said
// unread. Mutation: look only at the first page, and main is drawn unread.
test("the default branch is found on a later page of its find, else drawn unread", async () => {
  let found = true;
  stubListing((p) => {
    if (p.get("contains") !== "main") return json(refsListing(p));
    if (!p.get("cursor")) return json({ refs_detail: [], next: "d1", position: POSITION });
    return json({ refs_detail: found ? [REF_ROWS[1]] : [], position: POSITION });
  });
  mount("#/gitlet/repos/web?tab=branches");
  const main = await screen.findByRole("table", { name: "Default branch" });
  expect(within(main).getByText("Gitlet")).toBeTruthy();
  expect(within(main).queryByText("Not read")).toBeNull();
  cleanup();

  found = false;
  mount("#/gitlet/repos/web?tab=branches");
  const unread = await screen.findByRole("table", { name: "Default branch" });
  expect(within(unread).getByText("Not read")).toBeTruthy();
});

// TAGS ARE BY DATE, the newest Latest, each at its commit; the Overview shows three
// and leads to the Tags view, which pages them all. Mutation: mark Latest on every
// page's first row, or link a tag of a tree to a commit's page.
test("tags are by date, the newest Latest, each at its commit", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-10-28T12:00:00Z"));
  const asked = stubEngine();
  mount("#/gitlet/repos/web?tab=branches");
  const box = await screen.findByRole("table", { name: /^Tags/ });
  expect(reads(asked, "code_tags")[0]!.searchParams.get("limit")).toBe("3");
  expect(within(box).getByRole("link", { name: "View all" }).getAttribute("href")).toBe(
    "#/gitlet/repos/web?tab=branches&view=tags",
  );
  cleanup();

  const again = stubEngine();
  mount("#/gitlet/repos/web?tab=branches&view=tags");
  const tags = await screen.findByRole("table", { name: /^Tags/ });
  expect(reads(again, "code_tags")[0]!.searchParams.get("limit")).toBe("50");
  expect(screen.queryByRole("searchbox", { name: "Find a branch" })).toBeNull();
  const rows = [...tags.querySelectorAll<HTMLElement>("tbody tr")];
  expect(rows.map((r) => r.querySelector(".code-tag__name")?.textContent)).toEqual([
    "v3",
    "v2",
    "v1",
  ]);
  expect(within(tags).getAllByText("Latest")).toHaveLength(1);
  expect(within(rows[0]!).getByText("Latest")).toBeTruthy();
  expect(within(rows[0]!).getByRole("link", { name: "v3" }).getAttribute("href")).toBe(
    "#/gitlet/repos/web?ref=refs%2Ftags%2Fv3",
  );
  expect(
    within(rows[0]!)
      .getByRole("link", { name: HEAD.slice(0, 7) })
      .getAttribute("href"),
  ).toBe(`#/gitlet/repos/web/commits/${HEAD}`);
  expect(rows[0]!.querySelector("time")?.textContent).toBe("5h ago");
  expect(within(rows[1]!).getByText("after the year 9999")).toBeTruthy();
  expect(within(rows[2]!).getByText("undated")).toBeTruthy();
  expect(within(rows[2]!).queryByRole("link", { name: "0f1e2d3" })).toBeNull();
  expect(
    within(rows[0]!).getByRole("link", { name: "Compare v3 with main" }).getAttribute("href"),
  ).toBe("#/gitlet/repos/web/compare?base=refs%2Ftags%2Fv3&head=main");
});

// J WALKS A BOX'S BRANCHES: j focuses the first row's name and Enter opens its
// files. Mutation: drop the keys from the box.
test("j walks a box's branches", async () => {
  stubEngine();
  mount("#/gitlet/repos/web?tab=branches&view=all");
  const all = await screen.findByRole("table", { name: /^All branches/ });
  fireEvent.keyDown(window, { key: "j" });
  const link = within(all).getByRole("link", { name: "feature" });
  expect(document.activeElement).toBe(link);
  link.blur();
  fireEvent.keyDown(window, { key: "Enter" });
  await waitFor(() => expect(location.hash).toBe("#/gitlet/repos/web?ref=feature"));
});

// A REFUSED LISTING IS SAID IN ITS BOX, with the engine's sentence and no
// table, never a fallback to the plain listing. Mutation: draw the rows of a
// name listing in its place.
test("a refused listing is said in its box", async () => {
  const asked = stubListing(() =>
    engineJson({ error: "unavailable", detail: "the node answering web runs an older build" }, 503),
  );
  mount("#/gitlet/repos/web?tab=branches&view=all");
  expect(await screen.findByRole("heading", { name: "All branches" })).toBeTruthy();
  await waitFor(() => expect(screen.queryByText(/older build/)).not.toBeNull());
  expect(screen.queryByRole("table", { name: /All branches/ })).toBeNull();
  expect(reads(asked, "code_refs").every((u) => u.searchParams.get("detail") === "true")).toBe(
    true,
  );
});

// REFUSED CHECKS AND DRIFT ARE SAID ONCE, above the boxes, and the rows draw
// without them. Mutation: say it in every row, or drop the rows with it.
test("refused checks and drift are said once, the rows drawn without them", async () => {
  const asked = stubEngine();
  const refuse = () =>
    engineJson({ error: "unavailable", detail: "this node answers no marks yet" }, 503);
  stubQuery(asked, "code_commit_marks", refuse);
  stubQuery(asked, "code_tips", refuse);
  mount("#/gitlet/repos/web?tab=branches");
  expect(
    await screen.findAllByText(
      "Checks and how far each branch is from main are unavailable: this node answers no marks yet.",
    ),
  ).toHaveLength(1);
  const active = await screen.findByRole("table", { name: /^Active branches/ });
  expect(within(active).getByRole("link", { name: "feature" })).toBeTruthy();
});

// A FILE EDITED IN THE BROWSER IS ONE COMMIT ON THE BRANCH ON SCREEN, at the
// commit the page's read resolved, carrying the whole file. Mutation: commit
// at the address's spelling rather than the resolution, or drop the head, and
// this fails.
test("a file edited here is one commit on the branch read, at the commit it resolved", async () => {
  const acted: Acted[] = [];
  stubEngine(detail({ managed_by: "tools" }), acted);
  mount("#/gitlet/repos/web?path=src%2Fapp.go", "web", MAINTAINER);

  const edit = await screen.findByRole("button", { name: "Edit app.go" });
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

// EDIT IS A PENCIL AND DELETE IS IN MORE, each a commit at the head read, and each
// held with its reason at a tag. Mutation: drop the menu entry's hold, or send the
// delete without the head read, and this fails.
test("Edit is a pencil and Delete is in More, each held with its reason at a tag", async () => {
  const acted: Acted[] = [];
  stubEngine(detail({ managed_by: "tools" }), acted);
  mount("#/gitlet/repos/web?path=src%2Fapp.go", "web", MAINTAINER);
  const edit = await screen.findByRole("button", { name: "Edit app.go" });
  await waitFor(() => expect(edit.getAttribute("title")).toBeNull());
  fireEvent.click(screen.getByRole("button", { name: "More file actions" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: /^Delete file/ }));
  const dialog = await screen.findByRole("dialog", { name: "Delete src/app.go?" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Delete file" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]!.args).toMatchObject({
    branch: "main",
    expected_head: HEAD,
    files: [{ op: "delete", path: "src/app.go" }],
  });

  cleanup();
  stubEngine(detail({ managed_by: "tools" }), []);
  mount("#/gitlet/repos/web?ref=refs%2Ftags%2Fv1&path=src%2Fapp.go", "web", MAINTAINER);
  const held = await screen.findByRole("button", { name: "Edit app.go" });
  await waitFor(() => expect(held.getAttribute("title")).toMatch(/^Choose a branch/));
  fireEvent.click(screen.getByRole("button", { name: "More file actions" }));
  const del = await screen.findByRole("menuitem", { name: /^Delete file/ });
  expect(del.getAttribute("aria-disabled")).toBe("true");
  expect(del.textContent).toContain("Choose a branch to commit to: a tag or a commit takes none.");
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

// A README'S FENCE IS COLOURED BY ITS INFO STRING, its bytes unchanged; an unknown or
// absent one stays plain `md-code`. Mutation: drop the README's fence hook, and this fails.
test("a README's fence is coloured by its info string", async () => {
  const text = [
    "# web",
    "",
    "```sh",
    "make dev   # the server on :8080",
    "```",
    "",
    "```go",
    "package main",
    "```",
    "",
    "```diff",
    "-a",
    "```",
    "",
    "```",
    "plain words",
    "```",
    "",
  ].join("\n");
  stubEngine(detail(), [], { "README.md": { oid: OTHER, text } });
  mount("#/gitlet/repos/web");
  const readme = await screen.findByRole("region", { name: "README.md" });
  await waitFor(() => expect(readme.querySelector(".code-syn-comment")).toBeTruthy());
  const fences = [...readme.querySelectorAll<HTMLElement>("pre.md-code")];
  expect(fences.map((f) => f.textContent)).toEqual([
    "make dev   # the server on :8080",
    "package main",
    "-a",
    "plain words",
  ]);
  expect(fences[0]!.querySelector(".code-syn-comment")!.textContent).toBe("# the server on :8080");
  await waitFor(() =>
    expect(fences[1]!.querySelector(".code-syn-keyword")?.textContent).toBe("package"),
  );
  expect(fences[2]!.querySelector("[class^='code-syn-']")).toBeNull();
  expect(fences[3]!.querySelector("[class^='code-syn-']")).toBeNull();
});

// GO TO FILE ASKS ONCE TYPING RESTS, at the commit the listing resolved, and opens a file at
// the address's revision. Mutation: ask per keystroke, or at the branch, and this fails.
test("Go to file finds a file at the listing's commit and opens it at the address's revision", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const asked = stubEngine();
  mount("#/gitlet/repos/web?ref=feature");
  const field = await screen.findByRole("combobox", { name: "Go to file" });
  await headRow("Start the storefront");
  for (const typed of ["m", "ma", "mai", "main"])
    fireEvent.change(field, { target: { value: typed } });
  await act(async () => {
    vi.advanceTimersByTime(200);
    await afterMicrotasks();
  });
  const finds = reads(asked, "code_find");
  expect(finds.map((u) => u.searchParams.get("q"))).toEqual(["main"]);
  expect(finds[0]!.searchParams.get("ref")).toBe(HEAD);
  expect(finds[0]!.searchParams.get("limit")).toBe("20");
  const list = await screen.findByRole("listbox", { name: "Files" });
  const rows = within(list).getAllByRole("option");
  expect(rows.map((r) => r.textContent)).toEqual([
    "cmd/web/main.go",
    "src/main_test.go",
    "The best 2 of 43 files: type more to narrow",
  ]);
  expect([...rows[0]!.querySelectorAll(".code-find__hit")].map((h) => h.textContent)).toEqual([
    "main",
  ]);
  fireEvent.mouseDown(rows[0]!);
  await waitFor(() =>
    expect(location.hash).toBe("#/gitlet/repos/web?path=cmd%2Fweb%2Fmain.go&ref=feature"),
  );
});

test("Go to file says when no path holds the query, and an engine's refusal in its own words", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  stubEngine();
  mount("#/gitlet/repos/web");
  const field = await screen.findByRole("combobox", { name: "Go to file" });
  await headRow("Start the storefront");
  fireEvent.change(field, { target: { value: "zzz" } });
  await act(async () => {
    vi.advanceTimersByTime(200);
    await afterMicrotasks();
  });
  expect(await screen.findByText("No file's path holds “zzz”, among 1,204 files")).toBeTruthy();

  cleanup();
  stubAnswering("code_find", () =>
    engineJson({ error: "unavailable", detail: "this node does not find files yet" }, 503),
  );
  mount("#/gitlet/repos/web");
  const again = await screen.findByRole("combobox", { name: "Go to file" });
  await headRow("Start the storefront");
  fireEvent.change(again, { target: { value: "main" } });
  await act(async () => {
    vi.advanceTimersByTime(200);
    await afterMicrotasks();
  });
  expect(
    await screen.findByText("Finding a file is unavailable: this node does not find files yet"),
  ).toBeTruthy();
});

// A QUERY PAST THE ENGINE'S 256 BYTES IS ASKED CUT at a code point, and its answer drawn.
// Mutation: ask nothing past the bound, and the list says "Looking for" forever.
test("Go to file asks a query past 256 bytes cut to them", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  const asked = stubEngine();
  mount("#/gitlet/repos/web");
  const field = await screen.findByRole("combobox", { name: "Go to file" });
  await headRow("Start the storefront");
  fireEvent.change(field, { target: { value: "界".repeat(90) } });
  await act(async () => {
    vi.advanceTimersByTime(200);
    await afterMicrotasks();
  });
  const list = await screen.findByRole("listbox", { name: "Files" });
  expect(await within(list).findByText("cmd/web/")).toBeTruthy();
  expect(reads(asked, "code_find").map((u) => u.searchParams.get("q"))).toEqual(["界".repeat(85)]);
});

// t IS GO TO FILE, never typed into the field it focuses; inside a field it is a letter, and g
// then t is still Chatlet's. Mutation: bind t while typing, and this fails.
test("t focuses Go to file; a t typed into a field stays a letter", async () => {
  stubEngine();
  mount("#/gitlet/repos/web");
  const field = await screen.findByRole("combobox", { name: "Go to file" });
  const t = new KeyboardEvent("keydown", { key: "t", bubbles: true, cancelable: true });
  act(() => {
    window.dispatchEvent(t);
  });
  expect(document.activeElement).toBe(field);
  expect(t.defaultPrevented).toBe(true);
  const typed = new KeyboardEvent("keydown", { key: "t", bubbles: true, cancelable: true });
  act(() => {
    field.dispatchEvent(typed);
  });
  expect(typed.defaultPrevented).toBe(false);
});

/** The engine with the head row's commit and its marks as a case draws them. */
function stubHead(commit: Record<string, unknown>, marks: () => Response) {
  const asked = stubAnswering("code_commit_marks", marks);
  const engine = vi.mocked(fetch).getMockImplementation()!;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname !== "/query/code_log") return engine(input, init);
      asked.push(url);
      return json(byRef(url, { kind: "log", log: { commits: [commit], scanned: 1 } }));
    }),
  );
  return asked;
}

const MERGE = {
  oid: HEAD,
  parent_count: 2,
  author: "cto",
  committer: "Crewlet",
  engine_committed: true,
  authored_at: "2026-10-22T09:00:00Z",
  committed_at: "2026-10-22T10:00:00Z",
  subject: "Merge change web!41: cache the cart badge for 30s",
};

const PASSED_MAIN = {
  target: "refs/heads/main",
  state: "passed",
  counts: { passed: 2 },
  required: 2,
  required_passed: 2,
  last_ended: "2026-10-22T10:00:00Z",
};

// THE HEAD ROW IS GITLET'S WHERE GITLET WROTE THE MERGE, its check a glyph whose word a
// screen reader says; refused marks leave the commit's own author and no check.
// Mutation: decide Gitlet without the marks' landing, and this fails.
test("the head row is Gitlet's where Gitlet wrote the merge, with its check", async () => {
  stubHead(MERGE, () =>
    json({
      commit_marks: [
        {
          commit: HEAD,
          checks: [PASSED_MAIN],
          merged: [
            {
              repo: "web",
              ref: "web!41",
              title: "cache the cart badge",
              state: "merged",
              merge_oid: HEAD,
              merged_by: "cto",
              merged_via: "direct",
            },
          ],
        },
      ],
      position: POSITION,
      level: "session",
    }),
  );
  mount("#/gitlet/repos/web");
  await headRow(MERGE.subject);
  await waitFor(() =>
    expect(document.querySelector(".code-latest__author")?.textContent).toBe("Gitlet"),
  );
  expect(document.querySelector(".code-latest__who")!.getAttribute("title")).toBe(
    "Gitlet wrote this merge commit when CTO merged web!41 through its gate",
  );
  const check = document.querySelector<HTMLElement>(".code-latest__check .code-check-mark")!;
  expect(check.textContent).toBe("Passed");
  expect(check.getAttribute("title")).toMatch(/^2 checks: 2 passed\. 2 of 2 required passed\./);
  expect(document.querySelector(".code-latest__when")!.getAttribute("dateTime")).toBe(
    MERGE.committed_at,
  );

  cleanup();
  stubHead(MERGE, () => engineJson({ error: "unavailable", detail: "no marks here yet" }, 503));
  mount("#/gitlet/repos/web");
  await headRow(MERGE.subject);
  await waitFor(() =>
    expect(document.querySelector(".code-latest__author")?.textContent).toBe("CTO"),
  );
  expect(document.querySelector(".code-latest__check .code-check-mark")).toBeNull();
});

/** The root's listing naming the push that brought HEAD, long past the checks kept. */
function pushedLongAgo() {
  const engine = vi.mocked(fetch).getMockImplementation()!;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname !== "/query/code_tree") return engine(input, init);
      const answer = (await (await engine(input, init)).json()) as Record<string, unknown>;
      return json({
        ...answer,
        pushed_by: {
          [HEAD]: {
            push_id: "p1",
            position: POSITION,
            at: "2026-10-22T10:00:00Z",
            principal: { kind: "agent", seat: "cto" },
            via: "git",
          },
        },
      });
    }),
  );
}

// PAST THE CHECKS KEPT, THE HEAD ROW SAYS SO at the root, where the listing names the push
// that brought the commit; refused marks leave the slot empty.
// Mutation: draw nothing for an old commit, or drop LatestCheck's `failed`, and this fails.
test("the head row says the checks are no longer kept past 30 days at the root", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-12-28T12:00:00Z"));
  stubHead(MERGE, () => engineJson({ error: "unavailable", detail: "no marks here yet" }, 503));
  pushedLongAgo();
  mount("#/gitlet/repos/web");
  // THE WRITER WAITS FOR THE MARKS on a merge, so its name says the marks have settled.
  await waitFor(() => expect(document.querySelector(".code-latest__author")).toBeTruthy());
  expect(document.querySelector(".code-latest__check")!.textContent).toBe("");

  cleanup();
  stubAnswering("code_commit_marks", () =>
    json({ commit_marks: [{ commit: HEAD }], position: POSITION, level: "session" }),
  );
  pushedLongAgo();
  mount("#/gitlet/repos/web");
  const kept = await screen.findByText("Checks no longer kept");
  expect(kept.closest(".code-latest__check")).toBeTruthy();
  expect(kept.getAttribute("title")).toBe("Checks are kept 30 days.");
});

/** The engine with a root holding a README, a LICENSE, or both, each file's text its own. */
function stubDocs(names: string[], acted: Acted[] = [], repo = detail({ managed_by: "tools" })) {
  const files: Record<string, Coded> = {
    "README.md": { oid: OTHER, text: "# Hello storefront\n" },
    LICENSE: { oid: BIG_OID, text: "MIT License\n\nCopyright the storefront's authors\n" },
  };
  stubEngine(repo, acted, files);
  const engine = vi.mocked(fetch).getMockImplementation()!;
  const asked: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      asked.push(url);
      if (url.pathname !== "/query/code_tree") return engine(input, init);
      return json(
        byRef(url, {
          kind: "tree",
          tree: {
            tree: OTHER,
            entries: names.map((path) => ({
              mode: "100644",
              type: "blob",
              oid: OTHER,
              path,
              size: 20,
            })),
            total: names.length,
          },
        }),
      );
    }),
  );
  return asked;
}

// THE README AND THE LICENSE ARE TABS OF ONE CARD, the choice the address's `doc=`, each read
// at the listing's commit; one file is one tab, and neither is no card.
// Mutation: read the LICENSE at the branch, or keep the choice out of the address, and this fails.
test("README and LICENSE are tabs whose choice is the address", async () => {
  const asked = stubDocs(["LICENSE", "README.md"]);
  mount("#/gitlet/repos/web?doc=license");
  const card = await screen.findByRole("region", { name: "LICENSE" });
  expect(await within(card).findByText(/MIT License/)).toBeTruthy();
  const read = reads(asked, "code_blob").find((u) => u.searchParams.get("path") === "LICENSE");
  expect(read?.searchParams.get("ref")).toBe(HEAD);
  fireEvent.click(within(card).getByRole("tab", { name: "README" }));
  expect(await screen.findByRole("heading", { name: "Hello storefront" })).toBeTruthy();
  expect(location.hash).toBe("#/gitlet/repos/web");

  cleanup();
  stubDocs(["README.md"]);
  mount("#/gitlet/repos/web?doc=license");
  const only = await screen.findByRole("region", { name: "README.md" });
  expect(
    within(only)
      .getAllByRole("tab")
      .map((t) => t.textContent),
  ).toEqual(["README"]);

  cleanup();
  stubDocs(["go.mod"]);
  mount("#/gitlet/repos/web");
  await headRow("Start the storefront");
  expect(screen.queryByRole("tablist", { name: "The repository's documents" })).toBeNull();
});

// THE README'S EDIT IS THE FILE'S EDIT: the editor on it at the branch read, held at a tag
// with the reason. Mutation: open the editor at a tag, and this fails.
test("the README's Edit opens the editor on it at the branch read, and is held at a tag with the reason", async () => {
  const acted: Acted[] = [];
  stubDocs(["README.md"], acted);
  mount("#/gitlet/repos/web", "web", MAINTAINER);
  const edit = await screen.findByRole("button", { name: "Edit README.md" });
  await waitFor(() => expect(edit.getAttribute("title")).toBeNull());
  fireEvent.click(edit);
  const dialog = await screen.findByRole("dialog", { name: "Edit README.md" });
  const text = (await within(dialog).findByLabelText("File contents")) as HTMLTextAreaElement;
  expect(text.value).toBe("# Hello storefront\n");
  fireEvent.change(text, { target: { value: "# Hello\n" } });
  fireEvent.change(within(dialog).getByLabelText("Commit message"), {
    target: { value: "Say hello" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Commit to main" }));
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]!.args).toMatchObject({
    branch: "main",
    expected_head: HEAD,
    files: [{ op: "put", path: "README.md", content: "# Hello\n" }],
  });

  cleanup();
  stubDocs(["README.md"]);
  mount("#/gitlet/repos/web?ref=refs%2Ftags%2Fv1", "web", MAINTAINER);
  const held = await screen.findByRole("button", { name: "Edit README.md" });
  await waitFor(() => expect(held.getAttribute("title")).toMatch(/Choose a branch/));
});

// THE HEADER NAMES THE REPOSITORY UNDER ITS OWNER'S PATH, each unit a link to it, with who
// reads it and its push mirror; a seat's repository is under the seat, and a unit the chart
// lacks is its key. Mutation: drop the owner's path, or the mirror's mark, and this fails.
test("the header names the repository under its unit's path, with its visibility and its push mirror", async () => {
  stubEngine(
    detail({
      remotes: {
        mirror_to: { url: "https://example.com/acme/web.git", as: "sre", credential: "${TOKEN}" },
      },
    }),
  );
  mount("#/gitlet/repos/web");
  const title = await screen.findByRole("heading", { name: "Engineering, web" });
  expect(within(title).getByRole("link", { name: "Engineering" }).getAttribute("href")).toMatch(
    /peek=unit%3Aengineering/,
  );
  const header = within(title.closest("header")!);
  expect(header.getByText("Company").closest("[title]")?.getAttribute("title")).toBe(
    "Every seat reads it; every person does.",
  );
  expect(header.getByText("Push mirror").closest("[title]")?.getAttribute("title")).toBe(
    "Pushes its default branch and refs/tags/* to https://example.com/acme/web.git as sre.",
  );
  expect(document.body.textContent).not.toContain("${TOKEN}");

  cleanup();
  stubEngine(detail({ owner: { kind: "seat", ref: "cto" }, kind: "scratch", visibility: "unit" }));
  mount("#/gitlet/repos/web");
  const seat = await screen.findByRole("heading", { name: "CTO, web" });
  expect(within(seat).getByRole("link", { name: "CTO" })).toBeTruthy();
  const seatHeader = within(seat.closest("header")!);
  expect(seatHeader.getByText("Unit")).toBeTruthy();
  expect(seatHeader.queryByText("Push mirror")).toBeNull();

  cleanup();
  stubEngine(detail({ owner: { kind: "unit", ref: "gone" } }));
  mount("#/gitlet/repos/web");
  const lost = await screen.findByRole("heading", { name: "gone, web" });
  expect(within(lost).queryByRole("link")).toBeNull();
});

// CHANGES COUNTS THE LIVE CHANGES, its own read; a refused count draws no figure and the page
// stays whole. Mutation: read the count off the repository's row, and this fails.
test("the Changes tab counts the live changes, and a refused count draws no count", async () => {
  const counts = { live: 4, draft: 1, open: 3, closed: 0, merged: 1 };
  const asked = stubAnswering("code_counts", () =>
    json({ counts: { changes: counts }, position: POSITION, level: "session" }),
  );
  mount("#/gitlet/repos/web");
  const tab = await screen.findByRole("tab", { name: /Changes/ });
  await waitFor(() => expect(tab.textContent).toBe("Changes4"));
  const read = reads(asked, "code_counts")[0]!;
  expect([read.searchParams.get("repo"), read.searchParams.get("parts")]).toEqual([
    "web",
    "changes",
  ]);

  cleanup();
  stubAnswering("code_counts", () =>
    engineJson({ error: "unavailable", detail: "no counts" }, 503),
  );
  mount("#/gitlet/repos/web");
  await headRow("Start the storefront");
  expect(screen.getByRole("tab", { name: /Changes/ }).textContent).toBe("Changes");
});

// CHECKS IS MARKED BY THE DEFAULT BRANCH'S HEAD: a dot when it passed, a count when it failed,
// nothing while it runs or when refused. Mutation: mark a running head, and this fails.
test("the Checks tab is marked by the default branch's head: a dot when it passed, a count when it failed, nothing while it runs", async () => {
  const marked = (rollup: Record<string, unknown>) => () =>
    json({
      commit_marks: [{ commit: HEAD, checks: [{ ...PASSED_MAIN, ...rollup }] }],
      position: POSITION,
      level: "session",
    });
  const checksTab = () => screen.getByRole("tab", { name: /Checks/ });
  const asked = stubAnswering("code_commit_marks", marked({}));
  mount("#/gitlet/repos/web?tab=pushes");
  await waitFor(() => expect(checksTab().textContent).toBe("ChecksEvery check on main passed"));
  expect(reads(asked, "code_commit_marks")[0]!.searchParams.get("commits")).toBe(HEAD);

  cleanup();
  stubAnswering(
    "code_commit_marks",
    marked({ state: "failing", counts: { failed: 1, errored: 1 }, required_passed: 0 }),
  );
  mount("#/gitlet/repos/web?tab=pushes");
  await waitFor(() => expect(checksTab().textContent).toBe("Checks22 checks on main failed"));

  cleanup();
  stubAnswering("code_commit_marks", marked({ state: "running", counts: { running: 2 } }));
  mount("#/gitlet/repos/web?tab=pushes");
  await screen.findByRole("tab", { name: /Checks/ });
  await afterMicrotasks();
  expect(checksTab().textContent).toBe("Checks");

  cleanup();
  stubAnswering("code_commit_marks", () => engineJson({ error: "unavailable" }, 503));
  mount("#/gitlet/repos/web?tab=pushes");
  await afterMicrotasks();
  expect(checksTab().textContent).toBe("Checks");
});

// THE BAR'S COUNTS LEAD TO THEIR LISTS: Branches to the branches, Tags to the tags' view.
test("the root's bar counts the branches and tags, each a link to its list", async () => {
  stubEngine();
  mount("#/gitlet/repos/web");
  const branches = await screen.findByRole("link", { name: "2 Branches" });
  expect(branches.getAttribute("href")).toBe("#/gitlet/repos/web?tab=branches");
  expect(screen.getByRole("link", { name: "1 Tag" }).getAttribute("href")).toBe(
    "#/gitlet/repos/web?tab=branches&view=tags",
  );
});

/** A tree entry as `code_tree` lists it. */
const entry = (path: string, type: "tree" | "blob" = "blob") => ({
  mode: type === "tree" ? "040000" : "100644",
  type,
  oid: OTHER,
  path,
});

/** The repository's tree by directory, each in git's order. */
const TREE: Record<string, ReturnType<typeof entry>[]> = {
  "": [
    entry(".github", "tree"),
    entry("Makefile"),
    entry("cmd", "tree"),
    entry("go.mod"),
    entry("internal", "tree"),
  ],
  ".github": [entry(".github/ci.yml")],
  cmd: [entry("cmd/web", "tree")],
  "cmd/web": [entry("cmd/web/main.go")],
  internal: [entry("internal/render", "tree"), entry("internal/server.go")],
  "internal/render": [entry("internal/render/page.go")],
};

/** The engine with TREE as its tree, `answer` taking any directory it answers first. */
function stubTree(answer?: (url: URL, dir: string) => Response | undefined) {
  return stubAnswering("code_tree", (url) => {
    const dir = url.searchParams.get("path") ?? "";
    const own = answer?.(url, dir);
    if (own) return own;
    const entries = TREE[dir] ?? [];
    return json(
      byRef(url, {
        kind: "tree",
        tree: { tree: OTHER, ...(dir ? { path: dir } : {}), entries, total: entries.length },
      }),
    );
  });
}

const MAIN_GO_AT = "#/gitlet/repos/web?path=cmd%2Fweb%2Fmain.go";
const treeDirs = (asked: URL[]) =>
  reads(asked, "code_tree").map((u) => u.searchParams.get("path") ?? "");
const item = (tree: HTMLElement, name: string) => within(tree).getByRole("treeitem", { name });
const levelNames = (tree: HTMLElement, level: number) =>
  within(tree)
    .getAllByRole("treeitem")
    .filter((i) => i.getAttribute("aria-level") === String(level))
    .map((i) => i.textContent);

// A FILE IS READ BESIDE ITS REPOSITORY'S TREE, opened down to it, each directory
// read once at the commit the file was read at. Mutation: read the tree at the
// branch's name, or read every directory on arrival, and this fails.
test("a file opens beside its repository's tree, opened down to it at the file's commit", async () => {
  const asked = stubTree();
  mount(MAIN_GO_AT);

  const tree = await screen.findByRole("tree", { name: "Files of web at main" });
  const file = await within(tree).findByRole("treeitem", { name: "main.go" });
  expect(file.getAttribute("aria-current")).toBe("page");
  expect(file.getAttribute("href")).toBe(MAIN_GO_AT);
  expect(item(tree, "cmd").getAttribute("aria-expanded")).toBe("true");
  expect(item(tree, "web").getAttribute("aria-expanded")).toBe("true");
  expect(item(tree, "internal").getAttribute("aria-expanded")).toBe("false");
  expect(levelNames(tree, 1)).toEqual([".github", "cmd", "internal", "go.mod", "Makefile"]);
  expect(treeDirs(asked).sort()).toEqual(["", "cmd", "cmd/web"]);
  for (const u of reads(asked, "code_tree")) expect(u.searchParams.get("ref")).toBe(HEAD);
  expect(document.querySelector(".toolbar")).toBeNull();
});

// A DIRECTORY OPENS IN PLACE, read once for the visit; the address does not move.
// Mutation: drop the kept levels, or navigate on a directory's press, and this fails.
test("a directory in the tree opens and closes in place, read once", async () => {
  const asked = stubTree();
  mount(MAIN_GO_AT);
  const tree = await screen.findByRole("tree", { name: "Files of web at main" });
  await within(tree).findByRole("treeitem", { name: "main.go" });

  fireEvent.click(item(tree, "internal"));
  const server = await within(tree).findByRole("treeitem", { name: "server.go" });
  expect(server.getAttribute("aria-level")).toBe("2");
  expect(item(tree, "internal").getAttribute("aria-expanded")).toBe("true");
  fireEvent.click(item(tree, "internal"));
  expect(within(tree).queryByRole("treeitem", { name: "server.go" })).toBeNull();
  fireEvent.click(item(tree, "internal"));
  await within(tree).findByRole("treeitem", { name: "server.go" });
  expect(treeDirs(asked).filter((d) => d === "internal")).toHaveLength(1);
  expect(location.hash).toBe(MAIN_GO_AT);
});

// ANOTHER FILE OPENED FROM THE TREE KEEPS THE TREE and the row's focus while it reads.
// Mutation: hand the pane the live commit ("" while the file reads), and this fails.
test("opening another file keeps the tree drawn and its row focused while the file reads", async () => {
  const asked = stubTree();
  let release!: () => void;
  const held = new Promise<void>((r) => (release = r));
  const engine = vi.mocked(fetch).getMockImplementation()!;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      if (url.pathname === "/query/code_blob" && url.searchParams.get("path") === "go.mod") {
        await held;
      }
      return engine(input, init);
    }),
  );
  mount(MAIN_GO_AT);
  const tree = await screen.findByRole("tree", { name: "Files of web at main" });
  await within(tree).findByRole("treeitem", { name: "main.go" });
  const goMod = item(tree, "go.mod");
  act(() => goMod.focus());
  act(() => {
    location.hash = goMod.getAttribute("href")!;
  });
  await waitFor(() => expect(item(tree, "go.mod").getAttribute("aria-current")).toBe("page"));
  expect(screen.getByRole("tree", { name: "Files of web at main" })).toBe(tree);
  expect(item(tree, "main.go")).toBeTruthy();
  expect(document.activeElement).toBe(goMod);
  expect(treeDirs(asked).sort()).toEqual(["", "cmd", "cmd/web"]);
  release();
  await screen.findByRole("button", { name: "Line 1" });
  expect(screen.getByRole("tree", { name: "Files of web at main" })).toBe(tree);
});

// THE TREE MOVES BY THE KEYS with one tab stop, and a file's Enter is its link's own.
// Mutation: give every row a tab stop, or swallow Enter on a file, and this fails.
test("the tree moves by the keys, and Enter on a file opens it at the address's revision", async () => {
  stubTree();
  mount("#/gitlet/repos/web?ref=feature&path=cmd%2Fweb%2Fmain.go");
  const tree = await screen.findByRole("tree", { name: "Files of web at feature" });
  await within(tree).findByRole("treeitem", { name: "main.go" });
  const focused = () => document.activeElement?.textContent;

  act(() => item(tree, "cmd").focus());
  fireEvent.keyDown(item(tree, "cmd"), { key: "ArrowDown" });
  expect(focused()).toBe("web");
  fireEvent.keyDown(item(tree, "web"), { key: "ArrowLeft" });
  expect(item(tree, "web").getAttribute("aria-expanded")).toBe("false");
  fireEvent.keyDown(item(tree, "web"), { key: "ArrowLeft" });
  expect(focused()).toBe("cmd");
  fireEvent.keyDown(item(tree, "cmd"), { key: "ArrowRight" });
  expect(focused()).toBe("web");
  fireEvent.keyDown(item(tree, "web"), { key: "ArrowRight" });
  expect(item(tree, "web").getAttribute("aria-expanded")).toBe("true");
  fireEvent.keyDown(item(tree, "web"), { key: "End" });
  expect(focused()).toBe("Makefile");
  fireEvent.keyDown(item(tree, "Makefile"), { key: "Home" });
  expect(focused()).toBe(".github");
  fireEvent.keyDown(item(tree, ".github"), { key: "g" });
  expect(focused()).toBe("go.mod");
  expect(
    within(tree)
      .getAllByRole("treeitem")
      .filter((i) => i.tabIndex === 0),
  ).toHaveLength(1);

  const goMod = item(tree, "go.mod");
  expect(goMod.getAttribute("href")).toBe("#/gitlet/repos/web?ref=feature&path=go.mod");
  expect(fireEvent.keyDown(goMod, { key: "Enter" })).toBe(true);
});

// A LONG DIRECTORY PAGES with Load more, in git's order until whole, then a code
// host's. Mutation: sort a paged directory, or never ask its cursor, and this fails.
test("a long directory pages with Load more, in git's order until it is whole", async () => {
  const asked = stubTree((url, dir) => {
    if (dir !== "internal") return undefined;
    const second = url.searchParams.get("cursor") === "p2";
    return json(
      byRef(url, {
        kind: "tree",
        ...(second ? {} : { next: "p2" }),
        tree: {
          tree: OTHER,
          path: dir,
          entries: second ? [entry("internal/render", "tree")] : [entry("internal/zeta.go")],
          total: 2,
        },
      }),
    );
  });
  mount(MAIN_GO_AT);
  const tree = await screen.findByRole("tree", { name: "Files of web at main" });
  await within(tree).findByRole("treeitem", { name: "main.go" });
  fireEvent.click(item(tree, "internal"));
  const more = await within(tree).findByRole("treeitem", { name: "Load more in internal" });
  expect(levelNames(tree, 2)).toEqual(["web", "zeta.go", "Load more in internal"]);

  act(() => more.focus());
  fireEvent.keyDown(more, { key: "Enter" });
  await within(tree).findByRole("treeitem", { name: "render" });
  expect(levelNames(tree, 2)).toEqual(["web", "render", "zeta.go"]);
  expect(
    reads(asked, "code_tree").filter((u) => u.searchParams.get("cursor") === "p2"),
  ).toHaveLength(1);
});

// A DIRECTORY REFUSED OR FAILED SAYS SO UNDER ITS ROW, and the file still draws.
// Mutation: drop the refusal's sentence, or blank the file on a tree's failure, and this fails.
test("a directory refused or failed says so under its row; the file still draws", async () => {
  let internal = 0;
  const asked = stubTree((_url, dir) => {
    if (dir === "internal") {
      internal++;
      return internal === 1
        ? engineJson({ error: "unavailable", detail: "this node lists no trees yet" }, 503)
        : undefined;
    }
    if (dir === ".github") return json({ error: "query_failed" }, 500);
    return undefined;
  });
  mount(MAIN_GO_AT);
  const tree = await screen.findByRole("tree", { name: "Files of web at main" });
  await within(tree).findByRole("treeitem", { name: "main.go" });
  fireEvent.click(item(tree, "internal"));
  expect(
    await within(tree).findByText(
      "This directory's listing is unavailable: this node lists no trees yet",
    ),
  ).toBeTruthy();

  fireEvent.click(item(tree, ".github"));
  const failed = await within(tree).findByRole("treeitem", { name: /could not be read/ });
  fireEvent.click(failed);
  await waitFor(() => expect(treeDirs(asked).filter((d) => d === ".github")).toHaveLength(2));
  expect(screen.getByRole("button", { name: "Line 1" })).toBeTruthy();

  cleanup();
  forgetTrees();
  stubTree((_url, dir) =>
    dir === ""
      ? engineJson({ error: "unavailable", detail: "this node lists no trees yet" }, 503)
      : undefined,
  );
  mount(MAIN_GO_AT);
  expect(
    await screen.findByText(
      "This directory's listing is unavailable: this node lists no trees yet",
    ),
  ).toBeTruthy();
  expect(screen.queryByRole("tree")).toBeNull();
  expect(await screen.findByRole("button", { name: "Line 1" })).toBeTruthy();
});

// THE FILE'S LATEST COMMIT IS A CARD OF ITS OWN, read at the file's commit, and
// History is in the crumbs' row. Mutation: read it at the branch, or keep History
// in the card, and this fails.
test("the file's latest commit is a card over the file, with its check and no History", async () => {
  const asked = stubTree();
  mount(MAIN_GO_AT);
  await headRow("Start the storefront");
  const card = document.querySelector<HTMLElement>(".code-latest--card")!;
  const log = reads(asked, "code_log")[0]!.searchParams;
  expect([log.get("path"), log.get("limit"), log.get("ref")]).toEqual([
    "cmd/web/main.go",
    "1",
    HEAD,
  ]);
  await waitFor(() =>
    expect(card.querySelector(".code-check-mark")?.getAttribute("title")).toMatch(/^2 checks/),
  );
  expect(within(card).queryByRole("link", { name: "History" })).toBeNull();
  const crumbs = document.querySelector<HTMLElement>(".code-file-crumbs")!;
  expect(within(crumbs).getByRole("link", { name: "History" }).getAttribute("href")).toBe(
    "#/gitlet/repos/web?tab=commits&path=cmd%2Fweb%2Fmain.go",
  );
});

// THE CRUMBS LEAD BACK TO EACH DIRECTORY and copy the path from the root, never a URL.
// Mutation: copy the address, or reset a refusal, and this fails.
test("the crumbs lead back to each directory and copy the path", async () => {
  let written = "";
  let refuse = false;
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: {
      writeText: (t: string) =>
        refuse ? Promise.reject(new Error("denied")) : ((written = t), Promise.resolve()),
    },
  });
  stubTree();
  mount("#/gitlet/repos/web?ref=feature&path=cmd%2Fweb%2Fmain.go");
  const path = await screen.findByRole("navigation", { name: "Path of the file" });
  expect(
    within(path)
      .getAllByRole("link")
      .map((a) => a.getAttribute("href")),
  ).toEqual([
    "#/gitlet/repos/web?ref=feature",
    "#/gitlet/repos/web?ref=feature&path=cmd",
    "#/gitlet/repos/web?ref=feature&path=cmd%2Fweb",
  ]);
  fireEvent.click(screen.getByRole("button", { name: "Copy the path cmd/web/main.go" }));
  expect(await screen.findByRole("button", { name: "Copied cmd/web/main.go" })).toBeTruthy();
  expect(written).toBe("cmd/web/main.go");

  refuse = true;
  fireEvent.click(screen.getByRole("button", { name: "Copied cmd/web/main.go" }));
  expect(
    await screen.findByRole("button", { name: "The browser refused the clipboard" }),
  ).toBeTruthy();
});

// `t` FOCUSES GO TO FILE IN THE PANE, showing a hidden pane first; typed into the
// field it stays a letter. Mutation: bind `t` only in the pane, and this fails.
test("t on a file's view focuses Go to file in the tree, showing a hidden tree first", async () => {
  stubTree();
  mount(MAIN_GO_AT);
  const pane = await screen.findByRole("complementary", { name: "Files of web at main" });
  const press = () => {
    const t = new KeyboardEvent("keydown", { key: "t", bubbles: true, cancelable: true });
    act(() => {
      window.dispatchEvent(t);
    });
    return t;
  };
  expect(press().defaultPrevented).toBe(true);
  expect(document.activeElement).toBe(within(pane).getByRole("combobox", { name: "Go to file" }));

  fireEvent.click(within(pane).getByRole("button", { name: "Collapse the file tree" }));
  expect(screen.queryByRole("complementary", { name: "Files of web at main" })).toBeNull();
  press();
  const field = await screen.findByRole("combobox", { name: "Go to file" });
  await waitFor(() => expect(document.activeElement).toBe(field));
  const typed = new KeyboardEvent("keydown", { key: "t", bubbles: true, cancelable: true });
  act(() => {
    field.dispatchEvent(typed);
  });
  expect(typed.defaultPrevented).toBe(false);
});

// THE PANE'S CHOICE LASTS THE VISIT, with the picker kept in reach while it is
// hidden. Mutation: hold the choice in the component, and the second mount fails.
test("the tree collapses and comes back for the visit", async () => {
  stubTree();
  mount(MAIN_GO_AT);
  fireEvent.click(await screen.findByRole("button", { name: "Collapse the file tree" }));
  const show = screen.getByRole("button", { name: "Show the file tree" });
  await waitFor(() => expect(document.activeElement).toBe(show));
  const crumbs = document.querySelector<HTMLElement>(".code-file-crumbs")!;
  expect(within(crumbs).getByRole("button", { name: "Branch or tag: main" })).toBeTruthy();

  cleanup();
  mount("#/gitlet/repos/web?path=go.mod");
  await screen.findByRole("button", { name: "Line 1" });
  expect(screen.queryByRole("tree")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Show the file tree" }));
  const collapse = await screen.findByRole("button", { name: "Collapse the file tree" });
  await waitFor(() => expect(document.activeElement).toBe(collapse));
  expect(await screen.findByRole("tree", { name: "Files of web at main" })).toBeTruthy();
});

// A REVISION CHOSEN IN THE PANE KEEPS THE FILE, and the tree is read at its commit.
// Mutation: drop `path` on a choice, or keep the old commit's tree, and this fails.
test("choosing a revision from the pane keeps the file", async () => {
  const asked = stubTree();
  const engine = vi.mocked(fetch).getMockImplementation()!;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      const answer = await engine(input, init);
      if (url.pathname !== "/query/code_blob" || url.searchParams.get("ref") !== "feature") {
        return answer;
      }
      const body = (await answer.json()) as Record<string, unknown>;
      return json({ ...body, resolved: { ref: { ref: "refs/heads/feature", oid: OTHER } } });
    }),
  );
  mount(MAIN_GO_AT);
  const pane = await screen.findByRole("complementary", { name: "Files of web at main" });
  fireEvent.click(within(pane).getByRole("button", { name: "Branch or tag: main" }));
  const panel = await screen.findByRole("dialog", { name: "Switch branches or tags" });
  await within(panel).findByRole("option", { name: /^feature/ });
  const field = within(panel).getByRole("combobox", { name: "Find a branch" });
  await waitFor(() => expect(document.activeElement).toBe(field));
  fireEvent.keyDown(field, { key: "ArrowDown" });
  fireEvent.keyDown(field, { key: "Enter" });
  await waitFor(() => {
    const p = new URLSearchParams(location.hash.split("?")[1]);
    expect([p.get("ref"), p.get("path")]).toEqual(["feature", "cmd/web/main.go"]);
  });
  await screen.findByRole("tree", { name: "Files of web at feature" });
  await waitFor(() =>
    expect(
      reads(asked, "code_tree").filter((u) => u.searchParams.get("ref") === OTHER),
    ).not.toHaveLength(0),
  );
});

/** The reason a held control gives, as the kit describes it. */
const reasonOf = (el: HTMLElement) =>
  document.getElementById(el.getAttribute("aria-describedby") ?? "")?.textContent;

// RAW AND DOWNLOAD NAME THE FILE'S COMMIT, whole, never the branch or the tag, which
// the route refuses. Mutation: build `rev` from the address, and this fails.
test("Raw and Download name the file's commit, never the branch", async () => {
  stubArchive(405);
  mount(MAIN_GO_AT);
  const raw = await screen.findByRole("link", { name: "Raw" });
  const path = "path=cmd%2Fweb%2Fmain.go";
  expect(raw.getAttribute("href")).toBe(`/git/web/-/raw?rev=${HEAD}&${path}`);
  expect(raw.getAttribute("title")).toBe("The file's bytes as committed at 0123456");
  const download = screen.getByRole("link", { name: "Download main.go" });
  expect(download.getAttribute("href")).toBe(`/git/web/-/raw?rev=${HEAD}&${path}&download=1`);
  expect(download.getAttribute("download")).toBe("main.go");

  cleanup();
  stubArchive(405);
  mount("#/gitlet/repos/web?ref=refs%2Ftags%2Fv1&path=cmd%2Fweb%2Fmain.go");
  expect((await screen.findByRole("link", { name: "Raw" })).getAttribute("href")).toBe(
    `/git/web/-/raw?rev=${HEAD}&${path}`,
  );
});

// AN OLDER FLEET HOLDS RAW AND DOWNLOAD with the reason, probed once for the page.
// Mutation: draw the links on a 404, or probe on every file, and this fails.
test("Raw and Download are held on a fleet that serves no downloads, probed once", async () => {
  const { probes } = stubArchive(404);
  mount(MAIN_GO_AT);
  const raw = await waitFor(() => {
    const held = screen.getByRole("button", { name: "Raw" });
    expect(reasonOf(held)).toBe("Downloading a file is not available on this fleet yet.");
    return held;
  });
  expect(raw.getAttribute("aria-disabled")).toBe("true");
  const download = screen.getByRole("button", { name: "Download main.go" });
  expect(reasonOf(download)).toBe("Downloading a file is not available on this fleet yet.");
  act(() => {
    location.hash = "#/gitlet/repos/web?path=go.mod";
  });
  await screen.findByRole("button", { name: "Download go.mod" });
  expect(probes).toEqual(["HEAD"]);
});

/** A file of two pages (BIG_PAGE_LINES and ten more), its lines `l<n>`. */
const TWO_PAGES: Coded = {
  oid: BIG_OID,
  text: Array.from({ length: BIG_PAGE_LINES + 10 }, (_, i) => `l${i + 1}\n`).join(""),
};

/** The clipboard stubbed: what it was written, or a refusal while `refuse` holds. */
function stubClipboard() {
  const box = { written: [] as string[], refuse: false };
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: {
      writeText: (t: string) =>
        box.refuse ? Promise.reject(new Error("denied")) : (box.written.push(t), Promise.resolve()),
    },
  });
  return box;
}

// COPY COPIES THE WHOLE FILE, reading at the commit the pages not loaded; a file
// wholly loaded asks nothing. Mutation: copy the loaded pages only, and this fails.
test("Copy copies the whole file, reading the pages not loaded", async () => {
  const clip = stubClipboard();
  const asked = stubEngine(detail(), [], { "src/two.go": TWO_PAGES });
  mount("#/gitlet/repos/web?path=src%2Ftwo.go");
  await screen.findByRole("button", { name: "Load more lines" });
  fireEvent.click(screen.getByRole("button", { name: "Copy the file's contents" }));
  expect(await screen.findByRole("button", { name: "Copied the file's contents" })).toBeTruthy();
  expect(clip.written).toEqual([TWO_PAGES.text]);
  const atCommit = reads(asked, "code_blob").filter((u) => u.searchParams.get("ref") === HEAD);
  expect(atCommit.map((u) => u.searchParams.get("cursor"))).toEqual([
    null,
    `l${BIG_PAGE_LINES + 1}`,
  ]);

  cleanup();
  const whole = stubEngine();
  mount("#/gitlet/repos/web?path=src%2Fapp.go");
  await screen.findByRole("button", { name: "Line 3" });
  const before = reads(whole, "code_blob").length;
  fireEvent.click(screen.getByRole("button", { name: "Copy the file's contents" }));
  await screen.findByRole("button", { name: "Copied the file's contents" });
  expect(clip.written.at(-1)).toBe("package app\n\nfunc Run() {}\n");
  expect(reads(whole, "code_blob")).toHaveLength(before);
});

// COPY IS HELD WITH ITS REASON for a binary file and past 1 MiB, and says when the
// browser refused. Mutation: draw Copy on a binary file, or reset a refusal, and this fails.
test("Copy is held for a binary file and past 1 MiB, and says when the clipboard refused", async () => {
  stubEngine(detail(), [], {
    "bin/app": { oid: OTHER, text: "x\n", binary: true },
    "big.json": { oid: OTHER, text: "{}\n", size: Math.round(3.2 * 1024 * 1024) },
  });
  mount("#/gitlet/repos/web?path=bin%2Fapp");
  const binary = await screen.findByRole("button", { name: "Copy the file's contents" });
  await waitFor(() =>
    expect(reasonOf(binary)).toBe("A binary file has no text to copy: download it."),
  );
  act(() => {
    location.hash = "#/gitlet/repos/web?path=big.json";
  });
  await waitFor(() =>
    expect(reasonOf(screen.getByRole("button", { name: "Copy the file's contents" }))).toBe(
      "This file is 3.2 MiB: copy one past 1 MiB from Raw.",
    ),
  );

  cleanup();
  const clip = stubClipboard();
  clip.refuse = true;
  stubEngine();
  mount("#/gitlet/repos/web?path=src%2Fapp.go");
  await screen.findByRole("button", { name: "Line 3" });
  fireEvent.click(screen.getByRole("button", { name: "Copy the file's contents" }));
  const refused = await screen.findByRole("button", { name: "The browser refused the clipboard" });
  await act(async () => {
    await new Promise((r) => setTimeout(r, 0));
  });
  expect(refused.getAttribute("aria-label") ?? refused.textContent).toContain("refused");
});
