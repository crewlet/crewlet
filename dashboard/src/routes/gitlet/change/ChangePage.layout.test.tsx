/**
 * A CHANGE'S CONVERSATION, LAID OUT: its discs and faces on one rail, the head's
 * diffstat at the tab row's end, the header's pill and face on one line, and
 * nothing sideways at a phone's width.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { InertWebSocket, LayoutClient, overflowing, sidewaysScrollers } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";
import { ChangePage } from "./ChangePage.tsx";

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };
const at = (seq: number) => ({ ...POSITION, seq });
const HEAD = "4f1c2a90123456789abcdef0123456789abcdef0";
const PAGE = { position: POSITION, level: "session" };
const LONG_LINE = `\tmux.Handle("GET /p/{slug}", ${"withCSPNonce(".repeat(12)}r.Stream("product"))`;

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [
    { name: "Frontend Engineer", handle: "frontend", kind: "agent" },
    { name: "Code Reviewer", handle: "reviewer", kind: "agent" },
    { name: "Jane Founder", handle: "jane", kind: "human" },
  ],
  units: [],
};

const CHANGE = {
  id: "change-42",
  repo_id: "repo-web",
  repo: "web",
  ref: "web!42",
  number: 42,
  title: "render: stream the product page's shell before its data",
  body: "Streams the product page's **shell** before its data.",
  state: "open",
  source_ref: "refs/heads/frontend/ENG-415-stream-the-product-page-shell",
  target_ref: "refs/heads/main",
  head: HEAD,
  revision: 2,
  owner: "frontend",
  owner_kind: "agent",
  opened_at: "2026-11-17T09:00:00Z",
  opened_position: POSITION,
  version: 5,
  position: POSITION,
  authors: [{ author: "frontend", kind: "agent", first_revision: 1, position: POSITION }],
  work_item: { backend: "worklet", id: "ENG-415", key: "ENG-415", project: "ENG" },
};

const FINDING = {
  id: "f-1",
  review_id: "r1",
  raiser: "reviewer",
  raiser_kind: "agent",
  revision: 1,
  commit_oid: HEAD,
  path: "internal/server/routes.go",
  blob_oid: HEAD,
  side: "new",
  line_start: 19,
  line_end: 19,
  severity: "major",
  category: "security",
  title: "The shell flushes before the CSP nonce is set",
  suggestion: `${LONG_LINE}\n`,
  state: "open",
  position: at(21),
};

const PARTS: Record<string, unknown> = {
  "": {
    change: CHANGE,
    revisions: [
      {
        n: 2,
        head: HEAD,
        pusher: "frontend",
        pusher_kind: "agent",
        position: at(40),
        at: "2026-11-17T11:00:00Z",
      },
      {
        n: 1,
        head: HEAD,
        pusher: "frontend",
        pusher_kind: "agent",
        position: at(10),
        at: "2026-11-17T09:00:00Z",
      },
    ],
  },
  reviews: {
    reviews: [
      {
        id: "r1",
        revision: 1,
        reviewer: "reviewer",
        reviewer_kind: "agent",
        verdict: "request_changes",
        summary: "The shell flushes before the CSP nonce is set.",
        findings: 1,
        position: at(20),
        at: "2026-11-17T10:00:00Z",
      },
    ],
  },
  comments: {
    comments: [
      {
        id: "c1",
        thread_id: "c1",
        author: "jane",
        author_kind: "person",
        body: "Is the 95 ms measured with the cache warm or cold?",
        position: at(30),
        at: "2026-11-17T10:10:00Z",
      },
    ],
  },
  reviewers: {
    reviewers: [
      {
        reviewer: "jane",
        kind: "person",
        reason: "path_owner",
        state: "requested",
        requested_by: "frontend",
        requested_by_kind: "agent",
        owned_paths: ["internal/server"],
        position: at(50),
        at: "2026-11-17T11:30:00Z",
      },
    ],
  },
  findings: { revision: 2, findings: [FINDING] },
  excerpts: {
    excerpts: {
      "f-1": {
        id: "f-1",
        status: "M",
        lines: [
          { mark: "+", new: 18, text: "\t// reads land" },
          { mark: "+", new: 19, text: LONG_LINE },
          { mark: "+", new: 20, text: '\tmux.Handle("GET /c/{slug}", r.Page("category"))' },
        ],
      },
    },
  },
  files: {
    revision: 2,
    diff: { base: HEAD, head: HEAD, files: [], total: 5, added: 207, deleted: 51 },
  },
};

const json = (payload: unknown) =>
  new Response(JSON.stringify(payload), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });

/** Files changed's five files, each of `n` added lines, for the stack's cases. */
const STACK = [
  "internal/render/page.go",
  "internal/render/stream.go",
  "internal/server/routes.go",
  `web/src/product/${"deeply/nested/".repeat(6)}Page.tsx`,
  "web/src/product/Shell.tsx",
].map((path) => ({ path, status: "A", added: 40, deleted: 0 }));
// THE LAST FILE IS A RENAME from a long path, for the heads' phone case.
STACK[4] = {
  ...STACK[4]!,
  old_path: `web/src/product/${"legacy/".repeat(4)}Shell.tsx`,
  status: "R",
};

const added = (n: number) =>
  `@@ -0,0 +1,${n} @@\n${Array.from({ length: n }, (_, i) => `+line ${i + 1}`).join("\n")}\n`;

function answer(url: URL): Response {
  if (url.pathname !== "/query/code_change") return json(PAGE);
  const part = url.searchParams.get("part") ?? "";
  if (part === "files" && location.hash.includes("tab=files")) {
    return json({
      ...PAGE,
      revision: 2,
      diff: { base: HEAD, head: HEAD, files: STACK, total: 5, added: 200, deleted: 0 },
    });
  }
  if (part === "diff") {
    const patches = STACK.map((f, file) => ({ file, path: f.path, status: "A", patch: added(40) }));
    return json({
      ...PAGE,
      revision: 2,
      diff: { base: HEAD, head: HEAD, files: [], total: 5, added: 200, deleted: 0, patches },
    });
  }
  return json({ ...PAGE, ...(PARTS[part] as object) });
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

function mount(width: number, hash = "#/gitlet/repos/web/changes/42", height = 0) {
  location.hash = hash;
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
        <ChangePage repo="web" number={42} />
      </LayoutClient>,
    ),
  );
}

/** The page once its timeline holds the finding's excerpt. */
async function drawn(): Promise<HTMLElement> {
  return eventually(() => {
    const excerpt = host!.querySelector<HTMLElement>(".code-excerpt .diff-view");
    expect(excerpt).not.toBeNull();
    return host!.querySelector<HTMLElement>(".crewlet-app-shell__content")!;
  });
}

const centre = (el: Element) => {
  const r = el.getBoundingClientRect();
  return r.left + r.width / 2;
};

// ONE RAIL: every disc and every face sits on it. Mutation: move the face
// column off the rail, and the faces' centres drift from it.
test("the timeline's discs and faces sit on one rail at 1440 px", async () => {
  mount(1440);
  await drawn();
  const list = host!.querySelector<HTMLElement>(".code-timeline")!;
  const col = parseFloat(getComputedStyle(list).getPropertyValue("--tl-col"));
  const rail = list.getBoundingClientRect().left + col / 2;
  const marks = [...list.querySelectorAll(".code-disc, .code-tl-face")];
  expect(marks.length).toBeGreaterThanOrEqual(6);
  for (const mark of marks) expect(Math.abs(centre(mark) - rail), mark.className).toBeLessThan(1);
  const diffstat = host!.querySelector<HTMLElement>(".code-diffstat");
  expect(diffstat?.getAttribute("aria-label")).toBe("207 lines added and 51 removed in revision 2");
});

// NOTHING SIDEWAYS ON A PHONE: a long line wraps inside its excerpt's card.
// Mutation: give the timeline a fixed width, and the page scrolls sideways.
test("a change's conversation holds its width at 390 px, a long line inside its excerpt", async () => {
  mount(390);
  const page = await drawn();
  await eventually(() => expect(page.querySelectorAll(".code-tl-event").length).toBeGreaterThan(2));
  // THE HEADER AND THE PANEL: the kit's layer host holds the menus' panels, laid out apart.
  const header = page.querySelector("header")!;
  const panel = page.querySelector('[role="tabpanel"]')!;
  expect([...overflowing(header), ...overflowing(panel)]).toEqual([]);
  // THE HEADER'S SENTENCE starts on the pill's line: the pill, then the face.
  const middle = (el: Element) => {
    const r = el.getBoundingClientRect();
    return r.top + r.height / 2;
  };
  const pill = header.querySelector(".code-change-byline__state")!;
  const face = header.querySelector(".code-change-byline > .code-byline")!.firstElementChild!;
  expect(Math.abs(middle(pill) - middle(face))).toBeLessThan(4);
  expect(sidewaysScrollers(panel)).toEqual([]);
  // A LONG LINE WRAPS in the excerpt, as every diff's does, inside its card.
  const excerpt = page.querySelector<HTMLElement>(".code-excerpt")!;
  expect(excerpt.textContent).toContain(LONG_LINE.trim());
  const card = page.querySelector<HTMLElement>(".code-finding")!;
  expect(card.scrollWidth).toBeLessThanOrEqual(card.clientWidth);
});

const FILES = "#/gitlet/repos/web/changes/42?tab=files";

/** Files changed once every file of the stack is drawn. */
async function stacked(): Promise<HTMLElement> {
  return eventually(() => {
    const sections = host!.querySelectorAll(".code-stack > .code-file");
    expect(sections.length).toBe(5);
    for (const s of sections) expect(s.querySelector(".diff-view")).not.toBeNull();
    return host!.querySelector<HTMLElement>(".crewlet-app-shell__content")!;
  });
}

// A FILE'S HEAD STAYS BELOW THE FILES' TOOLBAR as the stack scrolls under it.
// Mutation: `top: 0` on the head, and it slides under the toolbar.
test("a file's head stays below the files' toolbar as the stack scrolls, at 1440 px", async () => {
  await page.viewport(1440, 700);
  mount(1150, FILES, 700);
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
    expect(head.top).toBeGreaterThanOrEqual(bar.bottom - 0.5);
    expect(Math.abs(head.top - bar.bottom)).toBeLessThan(1);
  });
});

// THE TREE SCROLLS ALONE BESIDE THE STACK, stuck in the page's scroller.
// Mutation: drop the pane's `position: sticky`, and it scrolls away with the page.
test("the files' tree stays beside the stack as the page scrolls, at 1440 px", async () => {
  await page.viewport(1440, 700);
  mount(1150, FILES, 700);
  await stacked();
  const main = host!.querySelector<HTMLElement>("main")!;
  const pane = host!.querySelector<HTMLElement>(".code-files__pane")!;
  main.scrollTop = 1500;
  await eventually(() => {
    const bar = host!.querySelector(".code-files-bar")!.getBoundingClientRect();
    const box = pane.getBoundingClientRect();
    expect(box.top).toBeGreaterThanOrEqual(bar.bottom - 0.5);
    expect(box.bottom).toBeLessThanOrEqual(main.getBoundingClientRect().bottom + 0.5);
  });
});

// NOTHING SIDEWAYS ON A PHONE but the diffs' own boxes, Finish review in view, and
// a long path wrapped in its head. Mutation: `white-space: nowrap` on the path.
test("the Files changed tab holds its width at 390 px", async () => {
  await page.viewport(390, 800);
  mount(390, FILES);
  const content = await stacked();
  const panel = content.querySelector('[role="tabpanel"]')!;
  expect(overflowing(panel)).toEqual([]);
  expect(sidewaysScrollers(panel)).toEqual([]);
  const finish = [...panel.querySelectorAll("button")].find((b) =>
    b.textContent?.startsWith("Finish review"),
  )!;
  const box = finish.getBoundingClientRect();
  expect(box.right).toBeLessThanOrEqual(390 + 0.5);
  const head = panel.querySelectorAll(".code-file__head")[3]!;
  expect(head.scrollWidth).toBeLessThanOrEqual(head.clientWidth);
});

/** The middle of an element's first line box, and of its last. */
const lineMiddles = (el: Element) => {
  const lines = [...el.getClientRects()];
  const mid = (r: DOMRect) => r.top + r.height / 2;
  return { first: mid(lines[0]!), last: mid(lines.at(-1)!), lines: lines.length };
};
const middle = (el: Element) => {
  const r = el.getBoundingClientRect();
  return r.top + r.height / 2;
};

// A HEAD KEEPS ITS COUNTS AND MENU AT ITS FIRST LINE'S END, however long its
// path wraps. Mutation: let the head wrap as one row, and the counts drop below.
test("a file's head keeps its counts on its first line with a long path, at 820 px", async () => {
  await page.viewport(820, 800);
  mount(820, FILES);
  const content = await stacked();
  const head = content.querySelectorAll(".code-file__head")[3]!;
  const path = lineMiddles(head.querySelector("h3")!);
  expect(path.lines).toBeGreaterThan(1);
  const counts = head.querySelector(".code-diffstat")!;
  expect(Math.abs(middle(counts) - path.first)).toBeLessThan(6);
});

// ON A PHONE A RENAME'S HEAD STARTS WITH ITS PATH beside the fold, and its copy
// follows the path's last word. Mutation: make the lead a wrapping row, and the
// fold stands alone on the first line.
test("a renamed file's head keeps its fold and copy beside its path, at 390 px", async () => {
  await page.viewport(390, 800);
  mount(390, FILES);
  const content = await stacked();
  const head = content.querySelectorAll(".code-file__head")[4]!;
  const path = lineMiddles(head.querySelector("h3")!);
  expect(Math.abs(middle(head.querySelector(".code-file__fold")!) - path.first)).toBeLessThan(6);
  const copy = [...head.querySelectorAll("button")].find((b) =>
    b.getAttribute("aria-label")?.startsWith("Copy"),
  )!;
  expect(Math.abs(middle(copy) - path.last)).toBeLessThan(6);
});
