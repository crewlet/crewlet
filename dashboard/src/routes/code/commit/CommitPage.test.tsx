/**
 * A commit's page and a comparison read the engine over REST by whole ids,
 * and Open change writes through the act transport — never optimistically.
 *
 * The invariants worth breaking a build over: a commit's diff is against its
 * first parent, and a root commit's against git's empty tree; who pushed it,
 * its turn, its coding run and its work item are the engine's record, not the
 * commit's author line; the changes it belongs to link their pages; a
 * comparison's files are what head changed since the merge base, read by both
 * whole ids; Open change is ONE `code_open_change` of the branch compared into
 * the base branch, prefilled with the work item head's push was charged to,
 * and held, with the reason, where no change can be opened.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactElement } from "react";
import { afterEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { EMPTY_TREE } from "~/lib/code.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, type OrgProjection } from "~/protocol/index.ts";
import { withDerived } from "~/test/org.ts";
import { ADMIN_VIEWER } from "~/test/viewer.ts";
import { CommitPage } from "./CommitPage.tsx";
import { ComparePage } from "./ComparePage.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
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
  roles: [{ name: "CTO", handle: "cto" }],
  units: [{ id: "engineering", name: "Engineering", roles: [] }],
  services: { tracker: "native", knowledge: "native", code: "native" },
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
          coding_agent: "claude-code",
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

interface Acted {
  tool: string;
  args: Record<string, unknown>;
}

/** The engine: the repository, a commit, a comparison and their diffs — every request and act recorded. */
function stubEngine({
  commitAnswer = commit(),
  compare = {},
  act = () => json({ outcome: "applied", position: "CREWLET_CODE_LOG@1:13", receipt: {} }),
}: {
  commitAnswer?: unknown;
  compare?: Record<string, unknown>;
  act?: () => Promise<Response> | Response;
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
        case "/query/code_diff":
          return json({
            repo: "web",
            kind: "diff",
            diff: {
              base: p.get("base"),
              head: p.get("head"),
              files: [{ path: "app.go", status: "M", added: 1, deleted: 1 }],
              total: 1,
              added: 1,
              deleted: 1,
              ...(p.get("file") ? { patch: PATCH } : {}),
            },
          });
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

// WHO PUSHED IT IS THE ENGINE'S RECORD: the seat, its turn, its coding run
// and its work item from the push that introduced the commit, never the
// author line; and the change it heads a revision of links its page.
// Mutation: name the pusher from the author, or drop the run, and this fails.
test("a commit's page says who pushed it, from which turn and run, and its changes", async () => {
  stubEngine();
  mount(<CommitPage repo="web" sha={SHA} />, `#/code/repos/web/commits/${SHA}`);

  const pushed = await screen.findByRole("region", { name: "Pushed" });
  expect(within(pushed).getByText("CTO")).toBeTruthy();
  expect(within(pushed).getByRole("link", { name: TURN }).getAttribute("href")).toBe(
    `#/live/turns/${TURN}`,
  );
  expect(within(pushed).getByRole("link", { name: RUN }).getAttribute("href")).toBe(
    `#/live/runs/${RUN}`,
  );
  expect(within(pushed).getByText("ENG-42")).toBeTruthy();
  const changes = screen.getByRole("region", { name: "Changes" });
  expect(within(changes).getByRole("link", { name: "web!7" }).getAttribute("href")).toBe(
    "#/code/repos/web/changes/7",
  );
  expect(screen.getByText("Retry the flaky upload")).toBeTruthy();
});

// A COMMIT'S DIFF IS AGAINST ITS FIRST PARENT, read by both whole ids, and
// its first file's patch is drawn in the one DiffView. Mutation: diff against
// the default branch, or against the commit itself, and this fails.
test("a commit's diff is against its first parent, its patch drawn", async () => {
  const { asked } = stubEngine();
  mount(<CommitPage repo="web" sha={SHA} />, `#/code/repos/web/commits/${SHA}`);

  expect(await screen.findByText("new")).toBeTruthy();
  const stat = reads(asked, "code_diff").find((u) => !u.searchParams.has("file"));
  expect(stat?.searchParams.get("base")).toBe(PARENT);
  expect(stat?.searchParams.get("head")).toBe(SHA);
  const patch = reads(asked, "code_diff").find((u) => u.searchParams.get("file") === "app.go");
  expect(patch?.searchParams.get("base")).toBe(PARENT);
});

// A ROOT COMMIT'S CHANGE IS EVERY FILE IT HOLDS, measured from git's empty
// tree. Mutation: skip the diff of a root commit, or diff it against itself,
// and this fails.
test("a root commit's diff is from git's empty tree", async () => {
  const { asked } = stubEngine({ commitAnswer: commit({}, { parents: undefined }) });
  mount(<CommitPage repo="web" sha={SHA} />, `#/code/repos/web/commits/${SHA}`);

  await waitFor(() => expect(reads(asked, "code_diff").length).toBeGreaterThan(0));
  expect(reads(asked, "code_diff")[0]?.searchParams.get("base")).toBe(EMPTY_TREE);
});

// A COMPARISON'S FILES ARE WHAT HEAD CHANGED SINCE THE MERGE BASE, read by
// both whole ids, and a conflicting merge preview names its paths.
// Mutation: diff base against head (two dots), or drop the conflicts, and
// this fails.
test("a comparison reads what head changed since the merge base, and its preview", async () => {
  const { asked } = stubEngine();
  mount(<ComparePage repo="web" />, "#/code/repos/web/compare?head=retry");

  expect(await screen.findByText("Conflicts")).toBeTruthy();
  expect(screen.getByText(/conflicts in:/)).toBeTruthy();
  await waitFor(() => expect(reads(asked, "code_diff").length).toBeGreaterThan(0));
  const stat = reads(asked, "code_diff").find((u) => !u.searchParams.has("file"));
  expect(stat?.searchParams.get("base")).toBe(PARENT);
  expect(stat?.searchParams.get("head")).toBe(SHA);
  const compare = reads(asked, "code_compare")[0];
  expect(compare?.searchParams.get("base")).toBe("HEAD");
  expect(compare?.searchParams.get("head")).toBe("retry");
});

// OPEN CHANGE IS ONE code_open_change of the branch compared into the base
// branch, its title head's subject and its work item the one head's push was
// charged to — and the reader is taken to the change it opened. Mutation:
// send the commit id as the branch, drop the work item, or open nothing on an
// applied answer, and this fails.
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
  mount(<ComparePage repo="web" />, "#/code/repos/web/compare?head=retry");

  const open = await screen.findByRole("button", { name: "Open change" });
  await waitFor(() => expect(open.getAttribute("title")).toBeNull());
  fireEvent.click(open);
  const dialog = await screen.findByRole("dialog");
  expect((within(dialog).getByLabelText("Title") as HTMLInputElement).value).toBe(
    "Retry the flaky upload",
  );
  expect((within(dialog).getByLabelText("Work item") as HTMLInputElement).value).toBe(
    "native:ENG-42",
  );
  fireEvent.click(within(dialog).getByRole("button", { name: "Open change" }));

  await waitFor(() => expect(acted).toHaveLength(1));
  expect(acted[0]).toEqual({
    tool: "code_open_change",
    args: {
      repo: "web",
      branch: "retry",
      target: "main",
      title: "Retry the flaky upload",
      work_item: "native:ENG-42",
    },
  });
  await waitFor(() => expect(location.hash).toBe("#/code/repos/web/changes/9"));
});

// HELD WITH THE REASON where no change can be opened: a live change already
// proposes head's commit. Mutation: offer the open anyway, and this fails.
test("Open change is held where a live change already proposes head's commit", async () => {
  stubEngine({
    commitAnswer: commit({
      revisions: [{ change: { repo: "web", ref: "web!8", title: "Retry", state: "open" }, n: 1 }],
    }),
  });
  mount(<ComparePage repo="web" />, "#/code/repos/web/compare?head=retry");

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
  mount(<ComparePage repo="web" />, "#/code/repos/web/compare?head=retry", viewer);

  const open = await screen.findByRole("button", { name: "Open change" });
  await waitFor(() => expect(open.getAttribute("title")).toBe(reason));
  fireEvent.click(open);
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(acted).toHaveLength(0);
});
