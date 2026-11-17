/**
 * A CHANGE'S CONVERSATION, LAID OUT: its discs and faces on one rail, the head's
 * diffstat at the tab row's end, the header's pill and face on one line, and
 * nothing sideways at a phone's width.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
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

function answer(url: URL): Response {
  if (url.pathname !== "/query/code_change") return json(PAGE);
  return json({ ...PAGE, ...(PARTS[url.searchParams.get("part") ?? ""] as object) });
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

function mount(width: number) {
  location.hash = "#/gitlet/repos/web/changes/42";
  host = document.createElement("div");
  host.className = "app";
  host.innerHTML = `<div class="crewlet-app-shell__content" style="display:flex;flex-direction:column;width:${width}px"></div>`;
  document.body.append(host);
  root = createRoot(host.firstElementChild!);
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
    const excerpt = host!.querySelector<HTMLElement>(".code-excerpt .crewlet-diff-view");
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
