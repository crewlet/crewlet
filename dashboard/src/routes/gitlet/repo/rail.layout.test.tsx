/**
 * A REPOSITORY'S RAIL, LAID OUT: every section inside the rail's box, the spark's bars inside its
 * figure, and a long title, name or address giving way in one line, at every width.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { InertWebSocket, LayoutClient } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";
import { RepositoryPage } from "./RepositoryPage.tsx";

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };
const HEAD = "74066b10123456789abcdef0123456789abcdef0";
const PAGE = { position: POSITION, level: "session" };
const LONG_TITLE = "render: ".padEnd(120, "stream the product page's shell before its data ");
const LONG_HANDLE = "a-seat-whose-handle-runs-on-".padEnd(90, "and-on-");
const LONG_MIRROR = `https://github.com/${"nimbus-storefront-mirrors/".repeat(6)}web.git`;

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [{ name: "Jane Founder", handle: "jane", kind: "human" }],
  units: [],
};

const REPO = {
  id: "repo-web",
  name: "web",
  kind: "project",
  status: "active",
  managed_by: "tools",
  owner: { kind: "unit", ref: "engineering" },
  visibility: "company",
  search: "indexed",
  default_branch: "main",
  refs: 2,
  live_packs: 4,
  live_bytes: 19_293_798,
  live_objects: 1204,
  position: POSITION,
  head: HEAD,
  pushes: 214,
  last_push_at: "2026-11-16T09:35:00Z",
  branches: 2,
  tags: 3,
  tombstones: 0,
  created_at: "2026-11-01T10:00:00Z",
  remotes: { mirror_to: { url: LONG_MIRROR, as: LONG_HANDLE, credential: "" } },
};

const json = (payload: unknown) =>
  new Response(JSON.stringify(payload), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });

const change = (number: number, state: string, title: string) => ({
  id: `c${number}`,
  repo: "web",
  ref: `web!${number}`,
  number,
  title,
  state,
  owner: LONG_HANDLE,
  owner_kind: "agent",
  target_ref: "refs/heads/main",
  revision: 1,
  readiness: {
    met: 1,
    total: 2,
    first: "In the queue.",
    preconditions: [{ kind: "queue", state: "unmet", detail: "" }],
  },
});

/** The engine, for the repository's root and every section of its rail. */
function answer(url: URL): Response {
  const p = url.searchParams;
  switch (url.pathname) {
    case "/query/code_repo":
      return json({
        repo: {
          ...REPO,
          ...(p.has("day_bounds")
            ? {
                spark: [
                  ...Array<number>(16).fill(2),
                  9,
                  14,
                  6,
                  1,
                  3,
                  12,
                  17,
                  11,
                  8,
                  15,
                  19,
                  7,
                  13,
                  8,
                ],
                pushers: {
                  from: "2026-10-17T00:00:00Z",
                  until: "2026-11-16T00:00:00Z",
                  top: [
                    { kind: "agent", handle: LONG_HANDLE, pushes: 88 },
                    { kind: "gitlet", pushes: 23 },
                  ],
                },
              }
            : {}),
        },
        ...PAGE,
      });
    case "/query/code_tree":
      return json({
        repo: "web",
        kind: "tree",
        resolved: { ref: { ref: "refs/heads/main", oid: HEAD } },
        tree: {
          tree: HEAD,
          entries: [{ mode: "100644", type: "blob", oid: HEAD, path: "go.mod", size: 1200 }],
          total: 1,
        },
        ...PAGE,
      });
    case "/query/code_counts":
      return json({
        counts: { changes: { live: 2, draft: 1, open: 1, closed: 0, merged: 0 } },
        ...PAGE,
      });
    case "/query/code_changes":
      return json({
        changes:
          p.get("state") === "draft"
            ? [change(45, "draft", "Upgrade")]
            : [change(42, "open", LONG_TITLE)],
        ...PAGE,
      });
    case "/query/code_tags":
      return json({
        repo: "web",
        kind: "tags",
        tags: {
          tags: [
            {
              ref: "refs/tags/v2026.11.2",
              oid: HEAD,
              type: "commit",
              peeled: HEAD,
              peeled_type: "commit",
              date: "2026-11-16T05:00:00Z",
            },
          ],
          total: 3,
        },
      });
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

function mount(width: number) {
  location.hash = "#/gitlet/repos/web";
  host = document.createElement("div");
  host.className = "app";
  host.innerHTML = `<div class="crewlet-app-shell__content" style="display:flex;flex-direction:column;width:${width}px"></div>`;
  document.body.append(host);
  root = createRoot(host.firstElementChild!);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={{}} org={ORG}>
        <RepositoryPage repo="web" />
      </LayoutClient>,
    ),
  );
}

/** The rail once every section has drawn its answer. */
const drawnRail = () =>
  eventually(() => {
    const rail = document.querySelector<HTMLElement>(".code-repo-rail");
    expect(rail?.querySelectorAll(".crewlet-histogram__bar")).toHaveLength(14);
    expect(rail?.querySelectorAll(".code-mini")).toHaveLength(2);
    expect(rail?.textContent).toContain("v2026.11.2");
    expect(rail?.querySelector(".code-repo-rail__link")).toBeTruthy();
    return rail!;
  });

const inside = (inner: DOMRect, outer: DOMRect) => {
  expect(inner.left).toBeGreaterThanOrEqual(outer.left - 0.5);
  expect(inner.right).toBeLessThanOrEqual(outer.right + 0.5);
};

// THE RAIL HOLDS ITS SECTIONS, ITS SPARK AND ITS LONG WORDS AT EVERY WIDTH. Mutation: drop
// `white-space: nowrap` from a mini row's title, or set a fixed width on the figure, and this fails.
for (const [viewport, column] of [
  [1440, 1180],
  [1024, 760],
  [390, 358],
] as const) {
  test(`the rail holds its sections inside its box at ${viewport}px`, async () => {
    await page.viewport(viewport, 900);
    mount(column);
    const rail = await drawnRail();
    const box = rail.getBoundingClientRect();
    const content = document.querySelector<HTMLElement>(".crewlet-app-shell__content")!;
    expect(content.scrollWidth).toBeLessThanOrEqual(content.clientWidth);
    for (const sec of rail.querySelectorAll<HTMLElement>(".code-repo-rail__sec")) {
      expect(sec.scrollWidth, sec.textContent ?? "").toBeLessThanOrEqual(sec.clientWidth);
      inside(sec.getBoundingClientRect(), box);
      for (const child of sec.querySelectorAll<HTMLElement>("li, p, a, figure")) {
        inside(child.getBoundingClientRect(), box);
      }
    }
  });

  test(`the spark's bars sit inside its figure at ${viewport}px`, async () => {
    await page.viewport(viewport, 900);
    mount(column);
    const rail = await drawnRail();
    const figure = rail.querySelector<HTMLElement>("figure.code-spark")!;
    const frame = figure.getBoundingClientRect();
    for (const bar of figure.querySelectorAll(".crewlet-histogram__bar")) {
      inside(bar.getBoundingClientRect(), frame);
    }
    const ends = figure.querySelector<HTMLElement>(".code-spark__ends")!;
    const tops = [...ends.children].map((el) => Math.round(el.getBoundingClientRect().top));
    expect(new Set(tops).size).toBe(1);
  });

  test(`a long title, name or address gives way in one line at ${viewport}px`, async () => {
    await page.viewport(viewport, 900);
    mount(column);
    const rail = await drawnRail();
    const long = [
      rail.querySelector<HTMLElement>(".code-mini__title")!,
      rail.querySelector<HTMLElement>(".code-mini__name")!,
      rail.querySelector<HTMLElement>(".code-repo-rail__pusher .code-repo-rail__name")!,
      rail.querySelector<HTMLElement>(".code-repo-rail__address .code-repo-rail__link")!,
    ];
    // THE TITLE AND THE ADDRESS RUN PAST EVERY WIDTH, so they are cut, never wrapped.
    for (const el of [long[0]!, long[3]!]) {
      expect(el.scrollWidth, el.className).toBeGreaterThan(el.clientWidth);
    }
    for (const el of long) {
      // ONE LINE HIGH: a wrapped line would be two of its font's heights.
      const font = parseFloat(getComputedStyle(el).fontSize);
      expect(el.getBoundingClientRect().height, el.className).toBeLessThan(font * 2);
      expect(getComputedStyle(el).textOverflow, el.className).toBe("ellipsis");
      inside(el.getBoundingClientRect(), rail.getBoundingClientRect());
    }
  });
}
