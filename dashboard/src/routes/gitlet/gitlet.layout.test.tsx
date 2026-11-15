/**
 * GITLET'S PAGES, LAID OUT: a comparison's title stays clear of the files'
 * sticky toolbar under it, and the clone panel shows its whole address.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { InertWebSocket, LayoutClient } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";
import { ComparePage } from "./commit/ComparePage.tsx";
import { RepositoryPage } from "./repo/RepositoryPage.tsx";

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };
const BASE = "3a3ecae0123456789abcdef0123456789abcdef0";
const HEAD = "74066b10123456789abcdef0123456789abcdef0";

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [{ name: "Jane Founder", handle: "jane", kind: "human" }],
  units: [],
};

const json = (payload: unknown) =>
  new Response(JSON.stringify(payload), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });

/** The engine, for one repository's comparison of feature into main. */
function answer(url: URL): Response {
  switch (url.pathname) {
    case "/query/code_repo":
      return json({
        repo: {
          id: "repo-web",
          name: "web",
          kind: "project",
          status: "active",
          managed_by: "tools",
          owner: { kind: "unit", ref: "engineering" },
          visibility: "company",
          search: "grep",
          default_branch: "main",
          refs: 2,
          live_packs: 1,
          live_bytes: 2048,
          live_objects: 9,
          position: POSITION,
          head: BASE,
          pushes: 3,
          branches: 2,
          tags: 0,
          tombstones: 0,
          created_at: "2026-11-01T10:00:00Z",
        },
        position: POSITION,
        level: "session",
      });
    case "/query/code_refs":
      return json({
        refs: [
          { ref: "refs/heads/feature", oid: HEAD, position: POSITION },
          { ref: "refs/heads/main", oid: BASE, position: POSITION },
        ],
        position: POSITION,
        level: "session",
      });
    case "/query/code_compare":
      return json({
        repo: "web",
        compare: {
          base: BASE,
          head: HEAD,
          merge_base: BASE,
          ahead: 1,
          behind: 0,
          total: 1,
          added: 4,
          deleted: 0,
        },
        preview: { state: "clean" },
        position: POSITION,
        level: "session",
      });
    case "/query/code_tree":
      return json({
        repo: "web",
        ref: url.searchParams.get("ref") ?? BASE,
        commit: BASE,
        kind: "tree",
        tree: {
          tree: BASE,
          entries: [
            { mode: "040000", type: "tree", oid: HEAD, path: "cmd" },
            { mode: "100644", type: "blob", oid: HEAD, path: "go.mod", size: 1200 },
          ],
          total: 2,
        },
        position: POSITION,
        level: "session",
      });
    case "/query/code_diff":
      return json({
        repo: "web",
        diff: {
          total: 1,
          added: 4,
          deleted: 0,
          files: [{ path: "src/remember.go", status: "A", added: 4, deleted: 0 }],
        },
        position: POSITION,
        level: "session",
      });
  }
  return json({ position: POSITION, level: "session" });
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

function mount(node: React.ReactNode, width = 1200) {
  host = document.createElement("div");
  // THE SCREEN'S COLUMN, as the shell gives a view: the kit's gapped content.
  host.className = "app";
  host.innerHTML = `<div class="crewlet-app-shell__content" style="display:flex;flex-direction:column;width:${width}px"></div>`;
  document.body.append(host);
  root = createRoot(host.firstElementChild!);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={{}} org={ORG}>
        {node}
      </LayoutClient>,
    ),
  );
}

// A DIFF'S TITLE IS NEVER UNDER ITS TOOLBAR. The files' toolbar is a sticky,
// opaque band that hands its top padding back to the gap above it; drawn
// right under the comparison's caption in a plain section, it covered the
// caption's lower half ("What feature changed since 3a3ecae" cut in two).
// Mutation: draw the title in a section with no gap again, and the band's
// top rises above the title's bottom.
test("a comparison's title stays clear of the files' toolbar", async () => {
  location.hash = "#/gitlet/repos/web/compare?base=main&head=feature";
  mount(<ComparePage repo="web" />);
  const title = await eventually(() => {
    const el = [...document.querySelectorAll(".code-section__title, p.t-caption")].find((e) =>
      e.textContent?.startsWith("What feature changed since"),
    );
    expect(el).toBeTruthy();
    return el!;
  });
  const toolbar = await eventually(() => {
    const bar = document.querySelector('[aria-label="Layout"]')?.closest(".toolbar");
    expect(bar).toBeTruthy();
    return bar!;
  });
  const under = title.getBoundingClientRect();
  const band = toolbar.getBoundingClientRect();
  expect(band.top).toBeGreaterThanOrEqual(Math.floor(under.bottom));
});

// THE CLONE PANEL SHOWS ITS WHOLE ADDRESS, with no sideways scroll once it
// focuses its copy button. Mutation: `.code-clone { width: 30rem }` fails it.
for (const width of [1440, 390]) {
  test(`the clone panel holds its whole address, inside its card and the viewport, at ${width}px`, async () => {
    await page.viewport(width, 900);
    location.hash = "#/gitlet/repos/web";
    mount(<RepositoryPage repo="web" />, width);
    const clone = await eventually(() => {
      const button = [...document.querySelectorAll("button")].find(
        (b) => b.textContent === "Clone",
      );
      expect(button).toBeTruthy();
      return button!;
    });
    const card = await eventually(() => {
      const listing = document.querySelector(".code-browser");
      expect(listing?.textContent).toContain("go.mod");
      return listing!;
    });
    clone.click();
    // Measured once placed and focused: the focus is what scrolled it.
    const panel = await eventually(() => {
      const el = document.querySelector<HTMLElement>('.crewlet-popover[aria-label="Clone web"]');
      expect(el).toBeTruthy();
      expect(el!.style.visibility).not.toBe("hidden");
      expect(el!.contains(document.activeElement)).toBe(true);
      return el!;
    });
    expect(panel.scrollWidth).toBeLessThanOrEqual(panel.clientWidth);
    expect(panel.scrollLeft).toBe(0);
    const box = panel.getBoundingClientRect();
    for (const part of panel.querySelectorAll(
      ".code-clone__head, .code-clone__url, .code-clone__note",
    )) {
      const at = part.getBoundingClientRect();
      expect(at.left).toBeGreaterThanOrEqual(box.left);
      expect(at.right).toBeLessThanOrEqual(box.right);
    }
    const head = panel.querySelector(".code-clone__head")!;
    expect(head.textContent).toBe("Clone with HTTPS");
    expect(head.scrollWidth).toBeLessThanOrEqual(head.clientWidth);
    expect(box.left).toBeGreaterThanOrEqual(0);
    expect(box.right).toBeLessThanOrEqual(window.innerWidth);
    const under = card.getBoundingClientRect();
    expect(box.left).toBeGreaterThanOrEqual(Math.floor(under.left));
    expect(box.right).toBeLessThanOrEqual(Math.ceil(under.right));
  });
}
