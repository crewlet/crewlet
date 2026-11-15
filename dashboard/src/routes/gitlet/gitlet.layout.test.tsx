/**
 * GITLET'S PAGES, LAID OUT: a comparison's title stays clear of the files'
 * sticky toolbar under it.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { InertWebSocket, LayoutClient } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";
import { ComparePage } from "./commit/ComparePage.tsx";

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
