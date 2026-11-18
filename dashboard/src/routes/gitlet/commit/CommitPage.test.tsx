/**
 * A commit's page and a comparison read the engine over REST by whole ids,
 * and Open change writes through the act transport — never optimistically.
 *
 * The invariants worth breaking a build over: a commit's diff is against its
 * first parent, and a root commit's against git's empty tree; who pushed it,
 * its turn, its executor and its work item are the engine's record, not the
 * commit's author line; the changes it belongs to link their pages; a
 * comparison's files are what head changed since the merge base, read by both
 * whole ids; Open change is ONE `code_open_change` of the branch compared into
 * the base branch, prefilled with the work item head's push was charged to,
 * and held, with the reason, where no change can be opened.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactElement } from "react";
import { afterEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { EMPTY_TREE } from "~/lib/code.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { load } from "~/lib/syntax/index.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { blobAnswer, inkedLine, type StubBlob } from "~/test/codeScreens.tsx";
import { NearBy } from "~/test/nearBy.ts";
import { withDerived } from "~/test/org.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { CommitPage } from "./CommitPage.tsx";
import { ComparePage } from "./ComparePage.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  location.hash = "";
});

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };
const SHA = "0123456789abcdef0123456789abcdef01234567";
const PARENT = "89abcdef0123456789abcdef0123456789abcdef";
const MAIN = "fedcba9876543210fedcba9876543210fedcba98";
const RUN = "5f0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f";
const TURN = "6f0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f";

const ORG = withDerived({
  name: "Acme",
  roles: [
    { name: "CTO", handle: "cto" },
    { name: "Code Reviewer", handle: "reviewer" },
  ],
  units: [{ id: "engineering", name: "Engineering", roles: [] }],
  services: { tracker: "worklet", knowledge: "doclet", chat: "chatlet", code: "gitlet" },
}) as unknown as OrgProjection;

/** The reader: offered the open. */
const VIEWER = { ...ADMIN_VIEWER, acts: ["code_open_change"] };

const json = (payload: unknown, status = 200) =>
  new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });

const PATCH = ["--- a/app.go", "+++ b/app.go", "@@ -1,2 +1,2 @@", " keep", "-old", "+new"].join(
  "\n",
);

const REPO = {
  id: "id-web",
  name: "web",
  status: "active",
  managed_by: "config",
  kind: "project",
  owner: { kind: "unit", ref: "engineering" },
  visibility: "company",
  search: "indexed",
  default_branch: "main",
  refs: 2,
  live_packs: 1,
  live_bytes: 100,
  live_objects: 4,
  position: POSITION,
  head: MAIN,
  pushes: 2,
  branches: 2,
  tags: 0,
  tombstones: 0,
  created_at: "2026-10-01T10:00:00Z",
};

/** The commit as `code_commit` answers it, with what a case overrides. */
function commit(extra: Record<string, unknown> = {}, commitExtra: Record<string, unknown> = {}) {
  return {
    repo: "web",
    commit: {
      oid: SHA,
      parents: [PARENT],
      author: "Ada",
      author_email: "ada@example.com",
      authored_at: "2026-10-20T10:00:00Z",
      committed_at: "2026-10-20T10:00:00Z",
      subject: "Retry the flaky upload",
      ...commitExtra,
    },
    pushed_by: {
      push_id: "p1",
      position: POSITION,
      at: "2026-10-20T10:05:00Z",
      principal: {
        kind: "agent",
        seat: "cto",
        run: {
          turn_id: TURN,
          work_backend: "native",
          work_key: "ENG-42",
          run_id: RUN,
          harness: "claude-code",
        },
      },
      via: "git",
    },
    revisions: [
      { change: { repo: "web", ref: "web!7", title: "Retry uploads", state: "merged" }, n: 2 },
    ],
    position: POSITION,
    level: "session",
    ...extra,
  };
}

/** A listed file the engine writes a patch for: not binary, with lines changed. */
const patched = (f: Record<string, unknown>) =>
  !f.binary && Number(f.added ?? 0) + Number(f.deleted ?? 0) > 0;

/** The walk of every patch as the engine pages it: the first file's patch a page at a time, every
 *  other file's whole on the last page. */
function walkOf(files: readonly (readonly Record<string, unknown>[])[], patch: readonly string[]) {
  const listed = files.flat();
  const first = listed[0];
  const pages = patch.map((text, n) =>
    first && patched(first)
      ? [
          {
            file: 0,
            path: first.path,
            status: first.status,
            patch: text,
            continues: n + 1 < patch.length,
          },
        ]
      : [],
  );
  listed.forEach((f, i) => {
    if (i > 0 && patched(f)) {
      pages
        .at(-1)!
        .push({ file: i, path: f.path, status: f.status, patch: patch.join(""), continues: false });
    }
  });
  return pages;
}

interface Acted {
  tool: string;
  args: Record<string, unknown>;
}

/** The engine: the repository, a commit, a comparison and their diffs — every request and act recorded. */
function stubEngine({
  commitAnswer = commit(),
  compare = {},
  act = () => json({ outcome: "applied", position: "CREWLET_CODE_LOG@1:13", receipt: {} }),
  patch = [PATCH],
  files = [[{ path: "app.go", status: "M", added: 1, deleted: 1 }]],
  blobs = {},
  walk,
  totals = { added: 1, deleted: 1 },
}: {
  commitAnswer?: unknown;
  compare?: Record<string, unknown>;
  act?: () => Promise<Response> | Response;
  /** A file's patch as its pages: page n is answered from the cursor `p:<n>`, as the engine cuts one at a line. */
  patch?: readonly string[];
  /** The diff's files as their pages: page n is answered from the cursor `f:<n>`. */
  files?: readonly (readonly Record<string, unknown>[])[];
  /** Each side's blob by its whole id, read with no path. */
  blobs?: Readonly<Record<string, StubBlob>>;
  /** The walk's pages of fragments (cursor `w:<n>`), or refused as an older fleet does. */
  walk?: readonly (readonly Record<string, unknown>[])[] | "refused";
  /** The lines the diff adds and deletes, as each answer totals them. */
  totals?: { added: number; deleted: number };
} = {}) {
  const asked: URL[] = [];
  const acted: Acted[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input));
      asked.push(url);
      const p = url.searchParams;
      if (url.pathname === "/auth/session") return new Promise<Response>(() => {});
      if (url.pathname.startsWith("/operator/act/")) {
        acted.push({
          tool: decodeURIComponent(url.pathname.split("/operator/act/")[1] ?? ""),
          args: (JSON.parse(String(init?.body)) as { args: Record<string, unknown> }).args,
        });
        return act();
      }
      switch (url.pathname) {
        case "/query/code_repo":
          return json({ repo: REPO, position: POSITION, level: "session" });
        case "/query/code_refs":
          if (p.get("detail") === "true") {
            return json({
              refs_detail: [
                {
                  ref: "refs/heads/retry",
                  oid: SHA,
                  position: POSITION,
                  moved_at: "2026-10-27T11:00:00Z",
                },
                {
                  ref: "refs/heads/main",
                  oid: MAIN,
                  position: POSITION,
                  moved_at: "2026-10-27T10:00:00Z",
                },
              ],
              position: POSITION,
              level: "session",
            });
          }
          return json({
            refs: [
              { ref: "refs/heads/main", oid: MAIN, position: POSITION },
              { ref: "refs/heads/retry", oid: SHA, position: POSITION },
            ],
            position: POSITION,
            level: "session",
          });
        case "/query/code_commit":
          return json(commitAnswer);
        case "/query/code_blob":
          return blobAnswer(url, blobs);
        case "/query/code_diff": {
          if (p.get("patches") === "true") {
            if (walk === "refused") {
              return json({ error: "bad_params", detail: "unknown key patches" }, 400);
            }
            const pages = walk ?? walkOf(files, patch);
            const at = Number((p.get("cursor") ?? "w:0").slice(2));
            return json({
              repo: "web",
              kind: "diff",
              ...(at + 1 < pages.length ? { next: `w:${at + 1}` } : {}),
              diff: {
                base: p.get("base"),
                head: p.get("head"),
                files: [],
                total: files.flat().length,
                ...totals,
                patches: pages[at],
              },
            });
          }
          const file = p.get("file");
          const page = Number((p.get("cursor") ?? "p:0").slice(2));
          const pages = file !== null ? patch.length : files.length;
          return json({
            repo: "web",
            kind: "diff",
            ...(page + 1 < pages ? { next: `${file !== null ? "p" : "f"}:${page + 1}` } : {}),
            diff: {
              base: p.get("base"),
              head: p.get("head"),
              // A FILE'S PATCH IS ANSWERED WITH THAT FILE, as the engine's
              // is; a listing with its page of files.
              files: file !== null ? files.flat().filter((f) => f.path === file) : files[page],
              total: files.flat().length,
              ...totals,
              ...(file !== null ? { patch: patch[page] } : {}),
            },
          });
        }
        case "/query/code_compare":
          return json({
            repo: "web",
            kind: "compare",
            compare: {
              base: MAIN,
              head: SHA,
              merge_base: PARENT,
              base_is_ancestor: false,
              head_is_ancestor: false,
              ahead: 2,
              behind: 1,
              files: [{ path: "app.go", status: "M", added: 1, deleted: 1 }],
              total: 1,
              added: 1,
              deleted: 1,
            },
            resolved: {
              base: { ref: "refs/heads/main", oid: MAIN },
              head: { ref: `refs/heads/${p.get("head")}`, oid: SHA },
            },
            position: POSITION,
            preview: { state: "conflict", conflicts: ["app.go"] },
            ...compare,
          });
      }
      return json({}, 404);
    }),
  );
  return { asked, acted };
}

function mount(ui: ReactElement, hash: string, viewer: unknown = VIEWER) {
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
          <Router>{ui}</Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

const reads = (asked: URL[], what: string) => asked.filter((u) => u.pathname === `/query/${what}`);

/** A file's section of the stack, named by its head's path. */
const section = (path: string) => screen.findByRole("region", { name: path });

/** Sections come near only when a case says so (`NearBy.near`). */
function holdSectionsFar() {
  NearBy.made = [];
  vi.stubGlobal("IntersectionObserver", NearBy);
}

/** An element's words without the faces beside its names. */
function wordsOf(el: Element): string {
  const copy = el.cloneNode(true) as Element;
  for (const face of copy.querySelectorAll('[aria-hidden="true"], .crewlet-avatar, svg')) {
    face.remove();
  }
  return (copy.textContent ?? "").replace(/\s+/g, " ").trim();
}

/** The provenance card's rows: each sentence, its second line and its time, in words. */
async function provenance() {
  const card = await screen.findByRole("region", { name: "Provenance" });
  return [...card.querySelectorAll(".code-prov__row")].map((row) => ({
    row: row as HTMLElement,
    sentence: wordsOf(row.querySelector(".code-prov__sentence")!),
    more: row.querySelector(".code-prov__more")
      ? wordsOf(row.querySelector(".code-prov__more")!)
      : "",
    time: row.querySelector("time"),
  }));
}

const MERGE = "c71d0e9a2b4f68135ea0d7c2b9f4e61a3d8c5b02";
const TIP = "9a8b7c6d5e4f30211f2e3d4c5b6a79881a2b3c4d";

/** web!41 merged direct by CTO, approved by Code Reviewer, its 2 required checks passed. */
const landing = (extra: Record<string, unknown> = {}, gate: Record<string, unknown> = {}) => ({
  repo: "web",
  ref: "web!41",
  title: "Cache the badge",
  state: "merged",
  merge_oid: MERGE,
  merged_at: "2026-10-20T12:00:00Z",
  merged_via: "direct",
  merged_by: "cto",
  merged_by_kind: "agent",
  gate: {
    approvals: [{ handle: "reviewer", kind: "agent", revision: 1 }],
    checks_on: SHA,
    checks: { state: "passed", counts: { passed: 2 }, required: 2, required_passed: 2 },
    ...gate,
  },
  ...extra,
});

// WHO PUSHED IT IS THE ENGINE'S RECORD: the seat, its run linking its turn, and
// its executor linked by turn. Mutation: name the pusher from the author line,
// or link the executor by its job id.
test("a commit's page says who pushed it, from which turn and run, and its changes", async () => {
  stubEngine();
  mount(<CommitPage repo="web" sha={SHA} />, `#/gitlet/repos/web/commits/${SHA}`);

  const [push, revision] = await provenance();
  expect(push!.sentence).toBe("Pushed by CTO, a run for ENG-42, with git");
  expect(push!.more).toBe(`A git push · which refs it moved is no longer kept · executor ${RUN}`);
  expect(
    within(push!.row).getByRole("link", { name: "a run for ENG-42" }).getAttribute("href"),
  ).toBe(`#/live/turns/${TURN}`);
  expect(within(push!.row).getByRole("link", { name: RUN }).getAttribute("href")).toBe(
    `#/live/executors/${TURN}`,
  );
  expect(revision!.sentence).toBe("Revision 2 of web!7 Merged");
  expect(revision!.more).toBe("How it landed is unavailable on this fleet yet.");
  expect(within(revision!.row).getByRole("link", { name: "web!7" }).getAttribute("href")).toBe(
    "#/gitlet/repos/web/changes/7",
  );
  expect(screen.getByText("Retry the flaky upload")).toBeTruthy();
});

// A CHANGE WEARS ITS STATE PILL, and the refs a push moved are said after
// "to", a branch as its chip with its glyph and a tag as its name. Mutation:
// draw the tag as a chip, or the refused command as moved.
test("a commit's changes wear their state pill, and the refs it moved a branch chip", async () => {
  const command = (ref: string, result = "ok") => ({ ref, old: PARENT, new: SHA, result });
  stubEngine({
    commitAnswer: commit({
      push: {
        commands: [
          command("refs/heads/main"),
          command("refs/tags/v1.2"),
          command("refs/heads/held", "ng"),
        ],
      },
    }),
  });
  mount(<CommitPage repo="web" sha={SHA} />, `#/gitlet/repos/web/commits/${SHA}`);

  const [push, revision] = await provenance();
  const pill = within(revision!.row).getByText("Merged").closest<HTMLElement>(".code-state");
  expect(pill?.dataset.state).toBe("merged");
  expect(push!.sentence).toBe("Pushed by CTO, a run for ENG-42, with git, to main, v1.2");
  expect(push!.more.startsWith("A git push · it moved 2 refs")).toBe(true);
  const chips = [...push!.row.querySelectorAll(".code-branch-chip")];
  expect(chips.map((c) => c.textContent)).toEqual(["main"]);
  expect(chips[0]!.querySelector("svg")).not.toBeNull();
  const tag = within(push!.row).getByText("v1.2");
  expect(tag.classList.contains("code-branch-chip")).toBe(false);
});

// A DIRECT MERGE'S LANDING IS SAID WITH ITS GATE: the merge commit, the merger,
// approvals, checks and when, titled with the date. Mutation: name the merge
// commit's author as the merger.
test("a direct merge's landing is said in sentences with its gate", async () => {
  stubEngine({ commitAnswer: commit({ revisions: [{ change: landing(), n: 1 }] }) });
  mount(<CommitPage repo="web" sha={SHA} />, `#/gitlet/repos/web/commits/${SHA}`);

  const [, landed] = await provenance();
  expect(landed!.sentence).toBe("Landed with web!41 Merged in merge commit c71d0e9 by Gitlet");
  expect(landed!.more).toBe(
    "CTO merged it through its gate: approved by Code Reviewer; 2 of 2 required checks passed.",
  );
  expect(within(landed!.row).getByRole("link", { name: "c71d0e9" }).getAttribute("href")).toBe(
    `#/gitlet/repos/web/commits/${MERGE}`,
  );
  expect(landed!.time?.getAttribute("dateTime")).toBe("2026-10-20T12:00:00Z");
  expect(landed!.time?.getAttribute("title")).toBeTruthy();
  expect(landed!.time?.textContent).toMatch(/ago|at /);
});

// A QUEUE LANDING NAMES THE TRAIN'S CHECKS by the commit they ran on, which
// links its page. Mutation: draw the merged head's checks for a queue landing.
test("a queue landing names the train's checks by the commit they ran on", async () => {
  stubEngine({
    commitAnswer: commit({
      revisions: [{ change: landing({ merged_via: "queue" }, { checks_on: TIP }), n: 1 }],
    }),
  });
  mount(<CommitPage repo="web" sha={SHA} />, `#/gitlet/repos/web/commits/${SHA}`);

  const [, landed] = await provenance();
  expect(landed!.more).toBe(
    "CTO added it to the merge queue, and it landed through its gate: approved by Code Reviewer; the train's checks on 9a8b7c6 2 of 2 required checks passed.",
  );
  expect(within(landed!.row).getByRole("link", { name: "9a8b7c6" }).getAttribute("href")).toBe(
    `#/gitlet/repos/web/commits/${TIP}`,
  );
});

// THE MERGE COMMIT SAYS GITLET MERGED THE CHANGE into the branch its push
// moved, for the merger, and draws no push row beside it. Mutation: draw
// the push row too.
test("the merge commit says Gitlet merged the change into its branch for the merger", async () => {
  stubEngine({
    commitAnswer: commit({
      pushed_by: {
        push_id: "p2",
        position: POSITION,
        at: "2026-10-20T12:00:00Z",
        principal: { kind: "agent", seat: "cto" },
        via: "merge",
      },
      push: { commands: [{ ref: "refs/heads/main", old: PARENT, new: SHA, result: "ok" }] },
      merged: [landing({ merge_oid: SHA })],
      revisions: [],
    }),
  });
  mount(<CommitPage repo="web" sha={SHA} />, `#/gitlet/repos/web/commits/${SHA}`);

  const rows = await provenance();
  expect(rows).toHaveLength(1);
  expect(rows[0]!.sentence).toBe("Gitlet merged web!41 Merged into main for CTO");
  expect(rows[0]!.more).toBe(
    "CTO merged it through its gate: approved by Code Reviewer; 2 of 2 required checks passed.",
  );
});

// A SEAT'S PUSH THROUGH THE ENGINE is one sentence with its run and branch.
test("a seat's push through the engine says its run, its branch and that it moved one branch", async () => {
  stubEngine({
    commitAnswer: commit({
      pushed_by: { ...commit().pushed_by, via: "engine" },
      push: { commands: [{ ref: "refs/heads/retry", old: PARENT, new: SHA, result: "ok" }] },
    }),
  });
  mount(<CommitPage repo="web" sha={SHA} />, `#/gitlet/repos/web/commits/${SHA}`);

  const [push] = await provenance();
  expect(push!.sentence).toBe("Pushed by CTO, a run for ENG-42, via the engine, to retry");
  expect(push!.more).toBe(
    `A commit the engine built from a tool's call · it moved one branch · executor ${RUN}`,
  );
});

// THE GATE'S OTHER WORDS, each as the rows keep them, never a guess.
test.each([
  [
    "failing checks are named",
    landing(
      {},
      {
        checks: {
          state: "failing",
          counts: { failed: 1, passed: 1 },
          required: 2,
          required_passed: 1,
          failing: [{ name: "unit", state: "failed", required: true }],
        },
      },
    ),
    "CTO merged it through its gate: approved by Code Reviewer; unit failed.",
  ],
  [
    "checks past retention are no longer kept",
    landing({}, { checks: undefined, checks_on: undefined, gone: true }),
    "CTO merged it through its gate: approved by Code Reviewer; its checks are no longer kept.",
  ],
  [
    "no check ran",
    landing({}, { checks: undefined, checks_on: undefined }),
    "CTO merged it through its gate: approved by Code Reviewer; no check ran on what it merged.",
  ],
  [
    "no approval",
    landing({}, { approvals: undefined }),
    "CTO merged it through its gate: with no approval; 2 of 2 required checks passed.",
  ],
  [
    "an override",
    landing({ override_reason: "hotfix" }),
    "CTO merged it past its gate: “hotfix”. As it stood: approved by Code Reviewer; 2 of 2 required checks passed.",
  ],
  [
    "a push merge",
    landing({ merged_via: "push" }),
    "It merged when its head was pushed to its branch.",
  ],
] as const)("the gate's words: %s", async (_what, change, said) => {
  stubEngine({ commitAnswer: commit({ revisions: [{ change, n: 1 }] }) });
  mount(<CommitPage repo="web" sha={SHA} />, `#/gitlet/repos/web/commits/${SHA}`);

  const [, landed] = await provenance();
  expect(landed!.more).toBe(said);
});

// A LIVE CHANGE'S REVISION reads as its revision, with no gate and no time.
// Mutation: draw a live revision as landed.
test("a live change's revision head reads as its revision", async () => {
  stubEngine({
    commitAnswer: commit({
      revisions: [{ change: { repo: "web", ref: "web!42", title: "Next", state: "open" }, n: 3 }],
    }),
  });
  mount(<CommitPage repo="web" sha={SHA} />, `#/gitlet/repos/web/commits/${SHA}`);

  const [, revision] = await provenance();
  expect(revision!.sentence).toBe("Revision 3 of web!42 Open");
  expect(revision!.more).toBe("");
  expect(revision!.time).toBeNull();
});

// A COMMIT'S DIFF IS AGAINST ITS FIRST PARENT, read by both whole ids, and
// every file's patch by the one walk. Mutation: diff against the default
// branch, or against the commit itself, and this fails.
test("a commit's diff is against its first parent, its patches walked", async () => {
  const { asked } = stubEngine();
  mount(<CommitPage repo="web" sha={SHA} />, `#/gitlet/repos/web/commits/${SHA}`);

  expect(await screen.findByText("new")).toBeTruthy();
  for (const u of reads(asked, "code_diff")) {
    expect(u.searchParams.get("base")).toBe(PARENT);
    expect(u.searchParams.get("head")).toBe(SHA);
  }
  expect(reads(asked, "code_diff").some((u) => u.searchParams.get("patches") === "true")).toBe(
    true,
  );
});

// THE WALK IS READ A PAGE AT A TIME, the page after asked from the cursor
// the page before ended at, its hunks drawn under the first's. Mutation: ask
// the next page without its cursor, or draw the first page alone.
test("a patch past one page of the walk draws the rest of its hunks when the reader asks", async () => {
  holdSectionsFar();
  const { asked } = stubEngine({
    patch: [
      `${["--- a/app.go", "+++ b/app.go", "@@ -1,2 +1,2 @@", " keep", "-old", "+new"].join("\n")}\n`,
      ["@@ -40,2 +40,2 @@", " still", "-alpha", "+omega"].join("\n"),
    ],
  });
  mount(<CommitPage repo="web" sha={SHA} />, `#/gitlet/repos/web/commits/${SHA}`);

  expect(await screen.findByText("new")).toBeTruthy();
  expect(screen.queryByText("omega")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Load more lines" }));

  expect(await screen.findByText("omega")).toBeTruthy();
  expect(screen.getByText("new")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Load more lines" })).toBeNull();
  const second = reads(asked, "code_diff").find((u) => u.searchParams.get("cursor") === "w:1");
  expect(second?.searchParams.get("patches")).toBe("true");
  expect(second?.searchParams.get("head")).toBe(SHA);
  expect(reads(asked, "code_diff").some((u) => u.searchParams.has("file"))).toBe(false);
});

// THE FILE THE ADDRESS NAMES IS OPENED WHEREVER IT IS LISTED: past the first
// page it is asked alone and the pages read on to it; a binary one is said as
// binary. Mutation: open only a file the loaded pages list.
test.each([
  ["its patch", { path: "z.go", status: "A", added: 2, deleted: 0 }, "new"],
  [
    "that it is binary",
    { path: "z.go", status: "A", added: 0, deleted: 0, binary: true },
    "A binary file: git draws no lines of it.",
  ],
])("a file the address names past the first page of files draws %s", async (_what, zgo, drawn) => {
  const { asked } = stubEngine({
    files: [[{ path: "app.go", status: "M", added: 1, deleted: 1 }], [zgo]],
  });
  mount(<CommitPage repo="web" sha={SHA} />, `#/gitlet/repos/web/commits/${SHA}?file=z.go`);

  const region = await section("z.go");
  expect(await within(region).findByText(drawn)).toBeTruthy();
  // THE PAGES ARE READ ON TO LIST IT, once the engine says the diff changes it.
  const list = screen.getByRole("tree", { name: "Files changed" });
  expect(within(list).getByRole("treeitem", { name: /z\.go/ })).toBeTruthy();
  expect(reads(asked, "code_diff").some((u) => u.searchParams.get("file") === "z.go")).toBe(true);
});

// THE LINE THE ADDRESS NAMES IS MARKED in its file's patch, on the newer
// side, where a check's failure points; and a line the patch does not draw is
// said to be outside what the diff changes, with the file itself at the head
// offered on that line. Mutation: drop the line from what the patch is
// handed, mark it on the older side, or offer the file at the base, and this
// fails.
test("a line the address names is marked in its file's patch", async () => {
  stubEngine();
  mount(
    <CommitPage repo="web" sha={SHA} />,
    `#/gitlet/repos/web/commits/${SHA}?file=app.go&line=2`,
  );

  const region = await section("app.go");
  await within(region).findByText("new");
  const marked = region.querySelectorAll(".crewlet-diff-view__line--picked");
  expect(marked.length).toBe(1);
  expect(marked[0]?.textContent).toContain("new");
  expect(within(region).queryByText(/outside what this diff changes/)).toBeNull();
});

test("a line the patch does not draw offers the file at the head on that line", async () => {
  stubEngine();
  mount(
    <CommitPage repo="web" sha={SHA} />,
    `#/gitlet/repos/web/commits/${SHA}?file=app.go&line=40`,
  );

  const region = await section("app.go");
  const open = await within(region).findByRole("link", { name: "Open app.go at line 40" });
  const target = new URL(open.getAttribute("href")!, "http://x");
  expect(target.hash).toContain("/gitlet/repos/web");
  // THE CODE TAB IS THE REPOSITORY'S LANDING, so the address names no tab.
  expect(target.hash).not.toContain("tab=");
  expect(target.hash).toContain(`ref=${SHA}`);
  expect(target.hash).toContain("path=app.go");
  expect(target.hash).toContain("lines=40");
  expect(region.querySelectorAll(".crewlet-diff-view__line--picked").length).toBe(0);
});

// THE PATCH IS FOLLOWED TO THE LINE, a page at a time with no press, and no
// further: a line on a later page is marked once the page holding it is
// read, and a line the hunks read reach past is outside the diff as soon as
// they do, with the pages after it left unread. Mutation: wait for the
// reader's press, judge "outside" only once the whole patch is read, or read
// past the line, and this fails.
const PAGED_PATCH = [
  `${["--- a/app.go", "+++ b/app.go", "@@ -1,2 +1,2 @@", " keep", "-old", "+new"].join("\n")}\n`,
  `${["@@ -40,2 +40,2 @@", " still", "-alpha", "+omega"].join("\n")}\n`,
  ["@@ -90,2 +90,2 @@", " later", "-beta", "+gamma"].join("\n"),
];

test("a line on a later page of the patch is read to and marked with no press", async () => {
  const { asked } = stubEngine({ patch: PAGED_PATCH });
  mount(
    <CommitPage repo="web" sha={SHA} />,
    `#/gitlet/repos/web/commits/${SHA}?file=app.go&line=41`,
  );

  const region = await section("app.go");
  expect(await within(region).findByText("omega")).toBeTruthy();
  const marked = region.querySelectorAll(".crewlet-diff-view__line--picked");
  expect(marked.length).toBe(1);
  expect(marked[0]?.textContent).toContain("omega");
  expect(within(region).queryByText(/outside what this diff changes/)).toBeNull();
  const cursors = reads(asked, "code_diff")
    .filter((u) => u.searchParams.get("file") === "app.go")
    .map((u) => u.searchParams.get("cursor"));
  expect(cursors).toEqual([null, "p:1"]);
});

test("a line between two hunks is outside the diff once a hunk past it is read", async () => {
  const { asked } = stubEngine({ patch: PAGED_PATCH });
  mount(
    <CommitPage repo="web" sha={SHA} />,
    `#/gitlet/repos/web/commits/${SHA}?file=app.go&line=20`,
  );

  const region = await section("app.go");
  expect(await within(region).findByRole("link", { name: "Open app.go at line 20" })).toBeTruthy();
  expect(within(region).getByText("omega")).toBeTruthy();
  expect(within(region).queryByText("gamma")).toBeNull();
  expect(within(region).getByRole("button", { name: "Load more lines" })).toBeTruthy();
  const cursors = reads(asked, "code_diff")
    .filter((u) => u.searchParams.get("file") === "app.go")
    .map((u) => u.searchParams.get("cursor"));
  expect(cursors).toEqual([null, "p:1"]);
});

// A LINE IS THE FILE'S IT WAS NAMED IN: opening another file from the list
// drops it from the address, so the other file's line of the same number is
// never marked as the one a failure points at. Mutation: keep the line when
// another file is opened, and this fails.
test("opening another file drops the line the address named", async () => {
  stubEngine({
    files: [
      [
        { path: "app.go", status: "M", added: 1, deleted: 1 },
        { path: "z.go", status: "M", added: 1, deleted: 1 },
      ],
    ],
  });
  mount(
    <CommitPage repo="web" sha={SHA} />,
    `#/gitlet/repos/web/commits/${SHA}?file=app.go&line=2`,
  );

  await section("app.go");
  const list = screen.getByRole("tree", { name: "Files changed" });
  fireEvent.click(within(list).getByRole("treeitem", { name: /z\.go/ }));

  const region = await section("z.go");
  await within(region).findByText("new");
  const query = new URLSearchParams(location.hash.split("?")[1] ?? "");
  expect(query.get("file")).toBe("z.go");
  expect(query.has("line")).toBe(false);
  expect(region.querySelectorAll(".crewlet-diff-view__line--picked").length).toBe(0);
});

// A ROOT COMMIT'S CHANGE IS EVERY FILE IT HOLDS, measured from git's empty
// tree. Mutation: skip the diff of a root commit, or diff it against itself,
// and this fails.
test("a root commit's diff is from git's empty tree", async () => {
  const { asked } = stubEngine({ commitAnswer: commit({}, { parents: undefined }) });
  mount(<CommitPage repo="web" sha={SHA} />, `#/gitlet/repos/web/commits/${SHA}`);

  await waitFor(() => expect(reads(asked, "code_diff").length).toBeGreaterThan(1));
  for (const u of reads(asked, "code_diff")) {
    expect(u.searchParams.get("base")).toBe(EMPTY_TREE);
  }
  expect(reads(asked, "code_diff").some((u) => u.searchParams.get("patches") === "true")).toBe(
    true,
  );
  // Mutation: drop the host's words from the toolbar.
  const words = await screen.findByText(/^Showing/);
  expect(words.textContent).toMatch(/of a root commit, every file it holds$/);
});

// A COMPARISON'S FILES ARE WHAT HEAD CHANGED SINCE THE MERGE BASE, read by
// both whole ids, and a conflicting merge preview names its paths.
// Mutation: diff base against head (two dots), or drop the conflicts, and
// this fails.
test("a comparison reads what head changed since the merge base, and its preview", async () => {
  const { asked } = stubEngine();
  mount(<ComparePage repo="web" />, "#/gitlet/repos/web/compare?head=retry");

  expect(await screen.findByText("Conflicts")).toBeTruthy();
  expect(screen.getByText(/conflicts in \d+ paths?$/)).toBeTruthy();
  await waitFor(() => expect(reads(asked, "code_diff").length).toBeGreaterThan(0));
  const stat = reads(asked, "code_diff").find((u) => !u.searchParams.has("file"));
  expect(stat?.searchParams.get("base")).toBe(PARENT);
  expect(stat?.searchParams.get("head")).toBe(SHA);
  const compare = reads(asked, "code_compare")[0];
  expect(compare?.searchParams.get("base")).toBe("HEAD");
  expect(compare?.searchParams.get("head")).toBe("retry");
  // THE BRANCH AND THE COMMIT ARE NAMED AS THEY ARE, never in the label
  // register's capitals.
  const changed = await screen.findByText(/changed since/);
  expect(changed.closest(".t-label")).toBeNull();
});

// A COMPARISON'S PICKERS READ ONLY WHEN OPENED, each panel named for what it chooses,
// and an unset head chooses the default branch by its name. Mutation: read the
// listings on the page again, or write the default as none, and this fails.
test("a comparison's pickers read only when opened, and each panel names what it chooses", async () => {
  const { asked } = stubEngine();
  mount(<ComparePage repo="web" />, "#/gitlet/repos/web/compare?head=retry");

  expect(await screen.findByText("Conflicts")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Compared: retry" })).toBeTruthy();
  expect(reads(asked, "code_refs")).toHaveLength(0);
  fireEvent.click(screen.getByRole("button", { name: "Base: main" }));
  const base = await screen.findByRole("dialog", { name: "Choose the base" });
  await within(base).findByRole("option", { name: /^retry/ });
  expect(reads(asked, "code_refs").map((u) => u.searchParams.get("order"))).toEqual(["moved"]);
  fireEvent.keyDown(within(base).getByRole("combobox"), { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

  act(() => {
    location.hash = "#/gitlet/repos/web/compare";
  });
  fireEvent.click(await screen.findByRole("button", { name: "Compared: Choose what to compare" }));
  const head = await screen.findByRole("dialog", { name: "Choose what to compare" });
  fireEvent.mouseDown(await within(head).findByRole("option", { name: /^main/ }));
  await waitFor(() =>
    expect(new URLSearchParams(location.hash.split("?")[1]).get("head")).toBe("main"),
  );
});

// OPEN CHANGE IS ONE code_open_change of the branch compared into the base
// branch, its title head's subject and its work item the one head's push was
// charged to, and the reader is taken to the change it opened. Its fields are
// labelled kit fields, reviewers a picker of the chart's seats and the draft
// the kit's checkbox. Mutation: send the commit id as the branch, drop the
// work item, open nothing on an applied answer, or draw placeholders for
// labels again, and this fails.
test("Open change proposes the branch into the base, prefilled with its work item", async () => {
  const { acted } = stubEngine({
    commitAnswer: commit({ revisions: [] }),
    act: () =>
      json({
        outcome: "applied",
        position: "CREWLET_CODE_LOG@1:13",
        receipt: { repo: "web", change: "web!9" },
      }),
  });
  mount(<ComparePage repo="web" />, "#/gitlet/repos/web/compare?head=retry");

  const open = await screen.findByRole("button", { name: "Open change" });
  await waitFor(() => expect(open.getAttribute("title")).toBeNull());
  fireEvent.click(open);
  const dialog = await screen.findByRole("dialog");
  expect((within(dialog).getByLabelText("Title") as HTMLInputElement).value).toBe(
    "Retry the flaky upload",
  );
  expect((within(dialog).getByLabelText(/^Work item/) as HTMLInputElement).value).toBe(
    "native:ENG-42",
  );
  // LABELLED, NOT PLACEHOLDERS: each field is named by a label the reader sees.
  for (const label of ["Title", "Description", "Work item", "Reviewers"]) {
    expect(within(dialog).getByText(label, { selector: "label, label *" }), label).toBeTruthy();
  }
  expect(within(dialog).queryByRole("link", { name: /^Read / })).toBeNull();
  fireEvent.click(within(dialog).getByRole("checkbox", { name: /Open it as a draft/ }));
  fireEvent.click(within(dialog).getByRole("button", { name: "Open change" }));

  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_open_change",
    args: {
      repo: "web",
      branch: "retry",
      target: "main",
      title: "Retry the flaky upload",
      draft: true,
      work_item: "native:ENG-42",
    },
  });
  await waitFor(() => expect(location.hash).toBe("#/gitlet/repos/web/changes/9"));
});

// HELD WITH THE REASON where no change can be opened: a live change already
// proposes head's commit. Mutation: offer the open anyway, and this fails.
test("Open change is held where a live change already proposes head's commit", async () => {
  stubEngine({
    commitAnswer: commit({
      revisions: [{ change: { repo: "web", ref: "web!8", title: "Retry", state: "open" }, n: 1 }],
    }),
  });
  mount(<ComparePage repo="web" />, "#/gitlet/repos/web/compare?head=retry");

  expect(await screen.findByRole("link", { name: "Open web!8 →" })).toBeTruthy();
  const open = screen.getByRole("button", { name: "Open change" });
  await waitFor(() =>
    expect(open.getAttribute("title")).toBe("web!8 already proposes the commit retry stands at."),
  );
  fireEvent.click(open);
  expect(screen.queryByRole("dialog")).toBeNull();
});

// OPEN CHANGE IS DRAWN FOR EVERY READER, held with the reason it cannot act.
// Mutation: hide it from a reader who cannot act, and this fails.
test.each([
  [
    "an anonymous reader",
    { login: "", grants: [], handle: "", owner: "", name: "", acts: [], anonymous: true },
    WRITE_REASONS.anonymous,
  ],
  ["a person the engine does not serve", { ...VIEWER, acts: [] }, WRITE_REASONS.not_served],
])("Open change is drawn for %s, held with the reason", async (_who, viewer, reason) => {
  const { acted } = stubEngine({ commitAnswer: commit({ revisions: [] }) });
  mount(<ComparePage repo="web" />, "#/gitlet/repos/web/compare?head=retry", viewer);

  const open = await screen.findByRole("button", { name: "Open change" });
  await waitFor(() => expect(open.getAttribute("title")).toBe(reason));
  fireEvent.click(open);
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(acted).toHaveLength(0);
});

// A COMMIT IS REVERTED ON A BRANCH CHOSEN, at the head that branch's listing
// showed: one code_revert, the commit whole, nothing on the first press.
// Mutation: drop expected_head, send the short id, or send before the dialog's
// press, and this fails.
test("a commit is reverted on the branch chosen, at the head its listing showed", async () => {
  const { acted } = stubEngine();
  mount(<CommitPage repo="web" sha={SHA} />, `#/gitlet/repos/web/commits/${SHA}`, {
    ...ADMIN_VIEWER,
    acts: ["code_revert", "code_cherry_pick"],
  });

  const group = await screen.findByRole("group", { name: "Commit actions" });
  const revert = within(group).getByRole("button", { name: "Revert" });
  await waitFor(() => expect(revert.getAttribute("title")).toBeNull());
  fireEvent.click(revert);
  const dialog = await screen.findByRole("dialog", { name: /Revert 0123456/ });
  expect(acted).toHaveLength(0);
  const send = within(dialog).getByRole("button", { name: "Revert on main" });
  await waitFor(() => expect(send.getAttribute("title")).toBeNull());
  fireEvent.click(send);
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_revert",
    args: { repo: "web", branch: "main", expected_head: MAIN, commit: SHA },
  });
});

// A CHERRY-PICK ONTO ANOTHER BRANCH names that branch's own head. Mutation:
// keep the default branch's head after the choice, and this fails.
test("a commit is picked onto another branch at that branch's head", async () => {
  const { acted } = stubEngine();
  mount(<CommitPage repo="web" sha={SHA} />, `#/gitlet/repos/web/commits/${SHA}`, {
    ...ADMIN_VIEWER,
    acts: ["code_revert", "code_cherry_pick"],
  });

  const group = await screen.findByRole("group", { name: "Commit actions" });
  const pick = within(group).getByRole("button", { name: "Cherry-pick" });
  await waitFor(() => expect(pick.getAttribute("title")).toBeNull());
  fireEvent.click(pick);
  const dialog = await screen.findByRole("dialog", { name: /Cherry-pick 0123456/ });
  await within(dialog).findByRole("button", { name: "Pick onto main" });
  // A SEARCHABLE SELECT IS OPENED BY ITS BUTTON; its search field is the combobox.
  fireEvent.click(within(dialog).getByRole("button", { name: "Onto the branch" }));
  fireEvent.mouseDown(await screen.findByRole("option", { name: "retry" }));
  const send = within(dialog).getByRole("button", { name: "Pick onto retry" });
  fireEvent.click(send);
  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_cherry_pick",
    args: { repo: "web", branch: "retry", expected_head: SHA, commit: SHA },
  });
});

// ---- Every changed file on one page ---------------------------------------------

const THREE = [
  [
    { path: "cmd/app.go", status: "M", added: 1, deleted: 1 },
    { path: "cmd/b.go", status: "M", added: 1, deleted: 1 },
    { path: "web/c.go", status: "M", added: 1, deleted: 1 },
  ],
];
const COMMIT_PAGE = `#/gitlet/repos/web/commits/${SHA}`;
const sectionPaths = () =>
  screen
    .getAllByRole("region")
    .filter((r) => r.classList.contains("code-file"))
    .map((r) => r.getAttribute("data-path"));

// EVERY CHANGED FILE IS A SECTION OF ONE STACK, read by the walk of code_diff
// and never file by file. Mutation: read each file alone.
test("every changed file is a section of one stack read by the walk of code_diff", async () => {
  const { asked } = stubEngine({ files: THREE });
  mount(<CommitPage repo="web" sha={SHA} />, COMMIT_PAGE);

  const c = await section("web/c.go");
  await within(c).findByText("new");
  expect(sectionPaths()).toEqual(["cmd/app.go", "cmd/b.go", "web/c.go"]);
  const walks = reads(asked, "code_diff").filter((u) => u.searchParams.get("patches") === "true");
  expect(walks).toHaveLength(1);
  expect(walks[0]!.searchParams.get("base")).toBe(PARENT);
  expect(walks[0]!.searchParams.get("head")).toBe(SHA);
  expect(reads(asked, "code_diff").some((u) => u.searchParams.has("file"))).toBe(false);
});

// A FILE THE ADDRESS NAMES OPENS THE STACK AT IT: its head takes the focus and
// its line is marked and scrolled to. Mutation: drop the arrival's jump, or
// drop the target from reveal.
test("a file the address names opens the stack at it, its line marked", async () => {
  const scrolled = vi.spyOn(Element.prototype, "scrollIntoView");
  stubEngine({ files: THREE });
  mount(<CommitPage repo="web" sha={SHA} />, `${COMMIT_PAGE}?file=web/c.go&line=2`);

  const c = await section("web/c.go");
  await within(c).findByText("new");
  expect(document.activeElement).toBe(within(c).getByRole("heading", { level: 3 }));
  const marked = c.querySelectorAll(".crewlet-diff-view__line--picked");
  expect(marked).toHaveLength(1);
  expect(marked[0]?.textContent).toContain("new");
  await waitFor(() => expect(scrolled.mock.contexts).toContain(marked[0]));
  const app = await section("cmd/app.go");
  expect(app.querySelectorAll(".crewlet-diff-view__line--picked")).toHaveLength(0);
});

// THE TOOLBAR SAYS THE DIFF'S TOTALS, not the page of files listed, and
// Split draws every section split. Mutation: count the listed page.
test("the toolbar says the diff's totals, and Split draws every file split", async () => {
  stubEngine({ files: [[THREE[0]![0]!], [THREE[0]![1]!]] });
  mount(<CommitPage repo="web" sha={SHA} />, `${COMMIT_PAGE}?layout=split`);

  const words = await screen.findByText(/^Showing/);
  expect(words.textContent).toBe("Showing 2 changed files with 1 addition and 1 deletion");
  expect(within(words).getByText("1 addition").className).toBe("code-diffstat__added");
  expect(within(words).getByText("1 deletion").className).toBe("code-diffstat__removed");
  const app = await section("cmd/app.go");
  await within(app).findByText("new");
  expect(app.querySelector(".crewlet-diff-view--split")).not.toBeNull();
  expect(screen.getByRole("radio", { name: "Split" }).getAttribute("aria-checked")).toBe("true");
});

// LARGE TOTALS ARE SHORT FIGURES, as the diffstat writes them. Mutation: write
// the whole figure.
test("the toolbar writes a total of ten thousand or more as a short figure", async () => {
  stubEngine({ files: THREE, totals: { added: 12_345, deleted: 1 } });
  mount(<CommitPage repo="web" sha={SHA} />, COMMIT_PAGE);

  const words = await screen.findByText(/^Showing/);
  expect(words.textContent).toBe("Showing 3 changed files with 12.3k additions and 1 deletion");
});

// A MERGE IS SAID AGAINST ITS FIRST PARENT, linked. Mutation: drop the host's
// words from the toolbar.
test("a merge's toolbar says it is against its first parent", async () => {
  stubEngine({ commitAnswer: commit({}, { parents: [PARENT, MAIN] }), files: THREE });
  mount(<CommitPage repo="web" sha={SHA} />, COMMIT_PAGE);

  const words = await screen.findByText(/^Showing/);
  expect(words.textContent).toMatch(/ against its first parent 89abcde$/);
  expect(within(words).getByRole("link", { name: "89abcde" }).getAttribute("href")).toBe(
    `#/gitlet/repos/web/commits/${PARENT}`,
  );
});

// A PATH THE DIFF DOES NOT CHANGE is said at the stack's lead, never opened.
// Mutation: turn the unchanged sentence off.
test("a file the address names that the diff does not change is said so", async () => {
  stubEngine({ files: [[THREE[0]![0]!], [THREE[0]![1]!]] });
  mount(<CommitPage repo="web" sha={SHA} />, `${COMMIT_PAGE}?file=docs/README.md`);

  expect(
    await screen.findByText(
      "The two revisions hold the same docs/README.md: this diff does not change it.",
    ),
  ).toBeTruthy();
  expect(screen.queryByRole("region", { name: "docs/README.md" })).toBeNull();
});

// FILTER FILES NARROWS THE TREE AND THE STACK alike. Mutation: filter the
// tree only, and the stack keeps three sections.
test("Filter files narrows the tree and the stack", async () => {
  stubEngine({ files: THREE });
  mount(<CommitPage repo="web" sha={SHA} />, `${COMMIT_PAGE}?q=cmd`);

  await section("cmd/b.go");
  expect(sectionPaths()).toEqual(["cmd/app.go", "cmd/b.go"]);
  const tree = screen.getByRole("tree", { name: "Files changed" });
  expect(within(tree).queryByRole("treeitem", { name: /c\.go/ })).toBeNull();
  expect(screen.getAllByText("Showing 2 of 3 files").length).toBeGreaterThan(0);
  fireEvent.click(screen.getAllByRole("button", { name: "Clear filter" })[0]!);
  expect(await section("web/c.go")).toBeTruthy();
});

// A COMMIT ASKS NOTHING OF A CHANGE and draws no finding on its lines.
// Mutation: hand the stack the change's lines.
test("a commit's page asks nothing of a change and draws no finding card", async () => {
  const { asked } = stubEngine({ files: THREE });
  mount(<CommitPage repo="web" sha={SHA} />, COMMIT_PAGE);

  const c = await section("web/c.go");
  await within(c).findByText("new");
  expect(reads(asked, "code_change")).toHaveLength(0);
  expect(document.querySelector(".code-finding")).toBeNull();
});

// AN OLDER FLEET REFUSES THE WALK: said once, and each file read when asked.
// Mutation: say it on every section, or read every file alone unasked.
test("a walk an older fleet refuses is said once, each file read when asked", async () => {
  const { asked } = stubEngine({ files: THREE, walk: "refused" });
  mount(<CommitPage repo="web" sha={SHA} />, COMMIT_PAGE);

  expect(
    await screen.findAllByText(/Every file's patch on one page is unavailable on this fleet yet/),
  ).toHaveLength(1);
  expect(reads(asked, "code_diff").some((u) => u.searchParams.has("file"))).toBe(false);
  const b = await section("cmd/b.go");
  fireEvent.click(within(b).getByRole("button", { name: "Show its changes" }));
  await within(b).findByText("new");
  const alone = reads(asked, "code_diff").filter((u) => u.searchParams.has("file"));
  expect(alone.map((u) => u.searchParams.get("file"))).toEqual(["cmd/b.go"]);
});

// A COMPARISON DRAWS THE SAME TOOLBAR, TREE AND STACK, compared with its
// merge base. Mutation: draw a commit's words on a comparison.
test("a comparison draws the same toolbar, tree and stack as a commit", async () => {
  const { asked } = stubEngine({ files: THREE });
  mount(<ComparePage repo="web" />, "#/gitlet/repos/web/compare?head=retry");

  const c = await section("web/c.go");
  await within(c).findByText("new");
  expect((await screen.findByText(/^Showing/)).textContent).toBe(
    "Showing 3 changed files with 1 addition and 1 deletion",
  );
  const pane = screen.getByRole("complementary", { name: "Files changed" });
  expect(within(pane).getByText(/Compared with the merge base at/).textContent).toBe(
    "Compared with the merge base at 89abcde",
  );
  const walk = reads(asked, "code_diff").find((u) => u.searchParams.get("patches") === "true");
  expect(walk?.searchParams.get("base")).toBe(PARENT);
  expect(walk?.searchParams.get("head")).toBe(SHA);
});

// ---- Code coloured by kind in a diff ------------------------------------------

const OLD_OID = "1".repeat(40);
const NEW_OID = "2".repeat(40);

/** A Go file whose block comment opens at line 10 and closes at 16, line 14 its edit. */
const notes = (edit: string) =>
  [
    "package app",
    "",
    "func A() {}",
    "",
    "// Notes.",
    "",
    "var x = 1",
    "",
    "",
    "/*",
    "Long notes,",
    "over lines.",
    "still notes",
    edit,
    "more notes",
    "*/",
    "func Later() {}",
  ].join("\n") + "\n";

const NOTES_PATCH = [
  "--- a/app.go",
  "+++ b/app.go",
  "@@ -14,4 +14,4 @@",
  "-the old note",
  "+the new note",
  " more notes",
  " */",
  " func Later() {}",
].join("\n");

const goFile = (extra: Record<string, unknown> = {}) => ({
  path: "app.go",
  status: "M",
  added: 1,
  deleted: 1,
  old_oid: OLD_OID,
  new_oid: NEW_OID,
  ...extra,
});

const blobReads = (asked: URL[], oid: string) =>
  reads(asked, "code_blob").filter((u) => u.searchParams.get("ref") === oid);

const COMMIT_FILE = (file: string) => `#/gitlet/repos/web/commits/${SHA}?file=${file}`;

// A HUNK TAKES THE STATE ITS BLOB'S LINE OPENS IN, read from line 1, so the
// edit inside the comment reads as comment. Mutation: start each hunk at "".
test("a hunk opening inside a block comment is coloured as a comment", async () => {
  const { asked } = stubEngine({
    patch: [NOTES_PATCH],
    files: [[goFile()]],
    blobs: {
      [OLD_OID]: { text: notes("the old note") },
      [NEW_OID]: { text: notes("the new note") },
    },
  });
  mount(<CommitPage repo="web" sha={SHA} />, COMMIT_FILE("app.go"));

  await waitFor(() =>
    expect(inkedLine("add", "the new note")).toEqual([
      "code-syn-comment:the ",
      "[new]",
      "code-syn-comment: note",
    ]),
  );
  expect(inkedLine("remove", "the old note")).toEqual([
    "code-syn-comment:the ",
    "[old]",
    "code-syn-comment: note",
  ]);
  expect(inkedLine("same", "more notes")).toEqual(["code-syn-comment:more notes"]);
  expect(inkedLine("same", "func Later() {}")).toContain("code-syn-keyword:func");
  // EACH BLOB IS READ ONCE, by its whole id and no path.
  expect(blobReads(asked, OLD_OID)).toHaveLength(1);
  expect(blobReads(asked, NEW_OID)).toHaveLength(1);
  expect(blobReads(asked, NEW_OID)[0]!.searchParams.has("path")).toBe(false);
});

// Mutation: start each hunk at "".
test("a hunk opening inside a Go raw string, and one inside a template literal", async () => {
  const query = (where: string) =>
    ["package app", "", "var q = `", "SELECT id", "FROM t", where, "`", "func B() {}"].join("\n") +
    "\n";
  stubEngine({
    patch: [
      [
        "--- a/q.go",
        "+++ b/q.go",
        "@@ -6,3 +6,3 @@",
        "-WHERE a = 1",
        "+WHERE a = 2",
        " `",
        " func B() {}",
      ].join("\n"),
    ],
    files: [[goFile({ path: "q.go" })]],
    blobs: { [OLD_OID]: { text: query("WHERE a = 1") }, [NEW_OID]: { text: query("WHERE a = 2") } },
  });
  mount(<CommitPage repo="web" sha={SHA} />, COMMIT_FILE("q.go"));
  await waitFor(() =>
    expect(inkedLine("add", "WHERE a = 2")).toEqual(["code-syn-string:WHERE a = ", "[2]"]),
  );
  expect(inkedLine("same", "func B() {}")).toContain("code-syn-keyword:func");
  cleanup();

  const page = (para: string) =>
    [
      "export const page = (title: string) => `",
      "<main>",
      "  <h1>${title}</h1>",
      para,
      "</main>`;",
      "const n = 1;",
    ].join("\n") + "\n";
  stubEngine({
    patch: [
      [
        "--- a/page.tsx",
        "+++ b/page.tsx",
        "@@ -3,4 +3,4 @@",
        "   <h1>${title}</h1>",
        "-<p>old</p>",
        "+<p>new</p>",
        " </main>`;",
        " const n = 1;",
      ].join("\n"),
    ],
    files: [[goFile({ path: "page.tsx" })]],
    blobs: { [OLD_OID]: { text: page("<p>old</p>") }, [NEW_OID]: { text: page("<p>new</p>") } },
  });
  mount(<CommitPage repo="web" sha={SHA} />, COMMIT_FILE("page.tsx"));
  // THE TEMPLATE'S TEXT IS A STRING, and the code inside `${…}` is code.
  await waitFor(() =>
    expect(inkedLine("same", "  <h1>${title}</h1>")).toContain("code-syn-string:  <h1>"),
  );
  const heading = inkedLine("same", "  <h1>${title}</h1>");
  expect(heading.filter((p) => p.includes("title"))).not.toContainEqual(
    expect.stringMatching(/^code-syn-string:/),
  );
  expect(inkedLine("same", "const n = 1;")).toContain("code-syn-keyword:const");
});

// PAST THE BOUND A HUNK STARTS FRESH, and the blob is read no further than the
// page that says so. Mutation: read on past the first page.
test("past the bound each hunk starts fresh, and only one page of the blob is read", async () => {
  const { asked } = stubEngine({
    patch: [
      [
        "--- a/app.go",
        "+++ b/app.go",
        "@@ -14,3 +14,4 @@",
        "-the old note",
        "+the new note",
        "+/* opened",
        " more notes",
        " */",
      ].join("\n"),
    ],
    files: [[goFile()]],
    blobs: {
      [OLD_OID]: { text: notes("the old note") },
      [NEW_OID]: { text: notes("the new note"), size: 300 * 1024 },
    },
  });
  mount(<CommitPage repo="web" sha={SHA} />, COMMIT_FILE("app.go"));

  await waitFor(() =>
    expect(inkedLine("add", "/* opened")).toEqual(["code-syn-comment:/* opened"]),
  );
  expect(inkedLine("same", "more notes")).toEqual(["code-syn-comment:more notes"]);
  expect(inkedLine("add", "the new note").join("")).not.toContain("code-syn-comment");
  // The older blob is under the bound: its line opens inside the comment.
  expect(inkedLine("remove", "the old note")[0]).toMatch(/^code-syn-comment:/);
  expect(blobReads(asked, NEW_OID)).toHaveLength(1);
});

// PLAIN UNTIL KNOWN: a line is never coloured from its hunk and then again.
// Mutation: colour from the hunk while a blob is read.
test("the lines draw plain while the blobs are read", async () => {
  await load("go");
  let release = () => {};
  const held = new Promise<void>((resolve) => (release = resolve));
  stubEngine({
    patch: [NOTES_PATCH],
    files: [[goFile()]],
    blobs: {
      [OLD_OID]: { text: notes("the old note"), held },
      [NEW_OID]: { text: notes("the new note"), held },
    },
  });
  mount(<CommitPage repo="web" sha={SHA} />, COMMIT_FILE("app.go"));

  expect(await screen.findByText("func Later() {}")).toBeTruthy();
  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(document.querySelector('.crewlet-diff-view [class^="code-syn-"]')).toBeNull();
  release();
  await waitFor(() =>
    expect(inkedLine("same", "more notes")).toEqual(["code-syn-comment:more notes"]),
  );
});

// A SIDE A FILE IS ABSENT FROM HAS NO BLOB, and a binary or lineless file is
// read for none. Mutations: an absent side read by the other side's id, and
// the lineless check dropped, each fail.
test.each([
  [
    "an added file reads only its newer blob",
    { path: "app.go", status: "A", added: 2, deleted: 0, new_oid: NEW_OID },
    ["--- /dev/null", "+++ b/app.go", "@@ -0,0 +1,2 @@", "+package app", "+func N() {}"],
    [NEW_OID],
  ],
  [
    "a deleted file reads only its older blob",
    { path: "app.go", status: "D", added: 0, deleted: 2, old_oid: OLD_OID },
    ["--- a/app.go", "+++ /dev/null", "@@ -1,2 +0,0 @@", "-package app", "-func N() {}"],
    [OLD_OID],
  ],
  ["a binary file reads none", { ...goFile(), binary: true }, [], []],
  [
    "a pure rename reads none",
    { ...goFile({ old_path: "old.go", status: "R", similarity: 100 }), added: 0, deleted: 0 },
    [],
    [],
  ],
] as const)("%s", async (_, file, patch, read) => {
  const text = "package app\nfunc N() {}\n";
  const { asked } = stubEngine({
    patch: [patch.join("\n")],
    files: [[file]],
    blobs: { [OLD_OID]: { text }, [NEW_OID]: { text } },
  });
  mount(<CommitPage repo="web" sha={SHA} />, COMMIT_FILE("app.go"));
  if (read.length > 0) {
    await waitFor(() => expect(document.querySelector(".code-syn-keyword")).not.toBeNull());
  } else {
    const said =
      "binary" in file
        ? "A binary file: git draws no lines of it."
        : "Renamed from old.go, its lines unchanged.";
    expect(await screen.findByText(said)).toBeTruthy();
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  expect(reads(asked, "code_blob").map((u) => u.searchParams.get("ref"))).toEqual(read);
});

// A PATCH LINE THAT IS NOT ITS BLOB'S LINE takes its hunk's state: here the
// blob's line 14 ends in a carriage return. Mutation: drop the text check.
test("a patch line that differs from its blob's line is coloured from its hunk", async () => {
  stubEngine({
    patch: [NOTES_PATCH],
    files: [[goFile()]],
    blobs: {
      [OLD_OID]: { text: notes("the old note") },
      [NEW_OID]: { text: notes("the new note\r") },
    },
  });
  mount(<CommitPage repo="web" sha={SHA} />, COMMIT_FILE("app.go"));

  await waitFor(() =>
    expect(inkedLine("same", "more notes")).toEqual(["code-syn-comment:more notes"]),
  );
  expect(inkedLine("add", "the new note").join("")).not.toContain("code-syn-comment");
  expect(inkedLine("remove", "the old note")[0]).toMatch(/^code-syn-comment:/);
});

// EACH SIDE TAKES ITS OWN PATH'S LANGUAGE: a rename may change it.
// Mutation: take the newer path's language for both sides.
test("a renamed file's sides each take their own language", async () => {
  const js = "const a = 1;\n";
  const ts = "const b = 1;\n";
  const { asked } = stubEngine({
    patch: [
      ["--- a/a.js", "+++ b/a.ts", "@@ -1 +1 @@", "-const a = 1;", "+const b = 1;"].join("\n"),
    ],
    files: [[goFile({ path: "a.ts", old_path: "a.js", status: "R" })]],
    blobs: { [OLD_OID]: { text: js }, [NEW_OID]: { text: ts } },
  });
  mount(<CommitPage repo="web" sha={SHA} />, COMMIT_FILE("a.ts"));
  await waitFor(() =>
    expect(inkedLine("remove", "const a = 1;")).toContain("code-syn-keyword:const"),
  );
  expect(inkedLine("add", "const b = 1;")).toContain("code-syn-keyword:const");
  expect(blobReads(asked, OLD_OID)).toHaveLength(1);
  cleanup();

  const go = "func New() {}\n";
  const renamed = stubEngine({
    patch: [
      ["--- a/x.txt", "+++ b/x.go", "@@ -1 +1 @@", "-func Old() {}", "+func New() {}"].join("\n"),
    ],
    files: [[goFile({ path: "x.go", old_path: "x.txt", status: "R" })]],
    blobs: { [OLD_OID]: { text: "func Old() {}\n" }, [NEW_OID]: { text: go } },
  });
  mount(<CommitPage repo="web" sha={SHA} />, COMMIT_FILE("x.go"));
  await waitFor(() => expect(inkedLine("add", "func New() {}")).toContain("code-syn-keyword:func"));
  expect(inkedLine("remove", "func Old() {}").join("")).not.toContain("code-syn-");
  expect(blobReads(renamed.asked, OLD_OID)).toHaveLength(0);
});
