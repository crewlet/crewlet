/** A commit's page laid out in a real browser: the stack's heads under the files' toolbar, and
 *  nothing sideways on a phone but the diffs' own boxes. */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { InertWebSocket, LayoutClient, overflowing, sidewaysScrollers } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";
import { CommitPage } from "./CommitPage.tsx";

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };
const PAGE = { position: POSITION, level: "session" };
const SHA = "1e8f3a6c9d0b24751fa3c8e6d2b9a04c7e5f1d38";
const PARENT = "6d8e0a2c4b6f81357d9e1a3c5b7f9d0e2a4c6b83";
const MERGE = "c71d0e9a2b4f68135ea0d7c2b9f4e61a3d8c5b02";
const ago = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [
    { name: "Frontend Engineer", handle: "frontend", kind: "agent" },
    { name: "Code Reviewer", handle: "reviewer", kind: "agent" },
    { name: "CTO", handle: "cto", kind: "agent" },
  ],
  units: [],
};

const FILES = [
  "internal/cart/badge.go",
  "internal/cart/badge_test.go",
  `web/src/cart/${"a-component-named-past-any-pane-".padEnd(90, "x")}.tsx`,
].map((path) => ({ path, status: "A", added: 40, deleted: 0 }));

const added = (n: number) =>
  `@@ -0,0 +1,${n} @@\n${Array.from({ length: n }, (_, i) => `+line ${i + 1}`).join("\n")}\n`;

const COMMIT = {
  repo: "web",
  commit: {
    oid: SHA,
    parents: [PARENT],
    parent_count: 1,
    author: "Frontend Engineer",
    author_email: "frontend@seats.example",
    authored_at: ago(420),
    committed_at: ago(420),
    subject: "cart: cache the badge count for 30s",
  },
  pushed_by: {
    push_id: "p1",
    position: POSITION,
    at: ago(420),
    principal: {
      kind: "agent",
      seat: "frontend",
      run: { turn_id: "t-1", work_backend: "worklet", work_key: "ENG-399", run_id: "exec-7f3a21" },
    },
    via: "engine",
  },
  push: {
    push_id: "p1",
    repo_id: "repo-web",
    position: POSITION,
    at: ago(420),
    actor: "frontend",
    actor_kind: "agent",
    via: "engine",
    commands: [
      {
        ref: "refs/heads/frontend/ENG-399-cache-the-cart-badge-count-for-thirty-seconds",
        old: PARENT,
        new: SHA,
        result: "ok",
      },
    ],
  },
  revisions: [
    {
      change: {
        repo: "web",
        ref: "web!41",
        title: "cart: cache the badge count for 30s",
        state: "merged",
        merge_oid: MERGE,
        merged_at: ago(300),
        merged_via: "direct",
        merged_by: "cto",
        merged_by_kind: "agent",
        gate: {
          approvals: [{ handle: "reviewer", kind: "agent", revision: 1 }],
          checks_on: SHA,
          checks: { state: "passed", counts: { passed: 2 }, required: 2, required_passed: 2 },
        },
      },
      n: 1,
    },
  ],
  ...PAGE,
};

const json = (payload: unknown) =>
  new Response(JSON.stringify(payload), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });

function answer(url: URL): Response {
  switch (url.pathname) {
    case "/query/code_commit":
      return json(COMMIT);
    case "/query/code_diff": {
      const diff = { base: PARENT, head: SHA, total: 3, added: 120, deleted: 0 };
      if (url.searchParams.get("patches") === "true") {
        const patches = FILES.map((f, file) => ({
          file,
          path: f.path,
          status: "A",
          patch: added(40),
        }));
        return json({ ...PAGE, repo: "web", kind: "diff", diff: { ...diff, files: [], patches } });
      }
      return json({ ...PAGE, repo: "web", kind: "diff", diff: { ...diff, files: FILES } });
    }
  }
  return json(PAGE);
}

let root: Root | null = null;
let host: HTMLElement | null = null;

beforeEach(() => {
  vi.stubGlobal("WebSocket", InertWebSocket);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => answer(new URL(String(input), location.origin))),
  );
});

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  vi.unstubAllGlobals();
  location.hash = "";
});

function mount(width: number, height = 0) {
  location.hash = `#/gitlet/repos/web/commits/${SHA}`;
  host = document.createElement("div");
  host.className = "app";
  const content = `<div class="crewlet-app-shell__content" style="display:flex;flex-direction:column;width:${width}px"></div>`;
  // A SCROLLER OF `height` where a case scrolls, as the shell's main is.
  host.innerHTML = height
    ? `<main class="crewlet-app-shell__main" style="display:block;block-size:${height}px;overflow-y:auto">${content}</main>`
    : content;
  document.body.append(host);
  root = createRoot(host.querySelector(".crewlet-app-shell__content")!);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={{}} org={ORG}>
        <CommitPage repo="web" sha={SHA} />
      </LayoutClient>,
    ),
  );
}

/** The page once every file of the stack is drawn. */
async function stacked(): Promise<HTMLElement> {
  return eventually(() => {
    const sections = host!.querySelectorAll(".code-stack > .code-file");
    expect(sections.length).toBe(3);
    for (const s of sections) expect(s.querySelector(".code-file__diff")).not.toBeNull();
    return host!.querySelector<HTMLElement>(".crewlet-app-shell__content")!;
  });
}

// A FILE'S HEAD STAYS BELOW THE FILES' TOOLBAR as a commit's stack scrolls.
// Mutation: `top: 0` on the head, and it slides under the toolbar.
test("a commit's file heads stay below the files' toolbar as the stack scrolls, at 1440 px", async () => {
  await page.viewport(1440, 700);
  mount(1150, 700);
  await stacked();
  const main = host!.querySelector<HTMLElement>("main")!;
  const second = host!.querySelectorAll<HTMLElement>(".code-stack > .code-file")[1]!;
  main.scrollTop = second.offsetTop + 300;
  await eventually(() => {
    const bar = host!.querySelector(".code-files-bar")!.getBoundingClientRect();
    const box = second.getBoundingClientRect();
    expect(box.top).toBeLessThan(bar.bottom);
    expect(box.bottom).toBeGreaterThan(bar.bottom + 40);
    const head = second.querySelector(".code-file__head")!.getBoundingClientRect();
    expect(Math.abs(head.top - bar.bottom)).toBeLessThan(1);
  });
});

// NOTHING SIDEWAYS ON A PHONE but the diffs' own boxes, a long path wrapped
// in its head. Mutation: `white-space: nowrap` on the path.
test("a commit's page holds its width at 390 px", async () => {
  await page.viewport(390, 800);
  mount(390);
  const content = await stacked();
  expect(overflowing(content)).toEqual([]);
  expect(sidewaysScrollers(content)).toEqual([]);
  const head = content.querySelectorAll(".code-file__head")[2]!;
  expect(head.scrollWidth).toBeLessThanOrEqual(head.clientWidth);
});
