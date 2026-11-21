/** Gitlet › Search, laid out: the facets beside the results, a long line scrolling in its own
 *  card, and a phone holding its width with the facets folded above. */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { InertWebSocket, LayoutClient, overflowing, sidewaysScrollers } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";
import { Search } from "./Search.tsx";

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };
const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [{ name: "Jane Founder", handle: "jane", kind: "human" }],
  units: [],
};
const none = { count: 0, names: [] };
const LONG = `\tlog.Printf("flush %s", ${'strings.Repeat("x", 40), '.repeat(7)}w.Flush())`;

const hit = (repo: string, path: string, line: number, text: string, total: number) => ({
  repo,
  path,
  line,
  text,
  blob: `${repo}:${path}`.padEnd(40, "0"),
  matches: [[text.indexOf("Flush("), text.indexOf("Flush(") + 6]],
  before: ["// one", "// two"],
  after: ["}"],
  file_matches: total,
});

const ANSWER = {
  hits: [
    hit("api", "internal/http/stream.go", 23, "func (s *streamWriter) Flush() {", 3),
    hit("web", "internal/server/middleware.go", 65, LONG, 4),
    hit("web", "internal/server/middleware.go", 95, "\tw.Flush()", 4),
  ],
  tips: { web: "c71d0e9".padEnd(40, "4"), api: "5c3e8b1".padEnd(40, "7") },
  coverage: {
    searched: 3,
    not_indexed: none,
    building: none,
    not_answering: null,
    not_permitted: none,
    grep_not_named: { count: 1, names: ["docs"] },
    stale: none,
    no_ref: none,
    past_limit: none,
    skipped_files: 0,
    truncated: false,
  },
};

const json = (payload: unknown) =>
  new Response(JSON.stringify(payload), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });

let root: Root | null = null;
let host: HTMLElement | null = null;

beforeEach(() => {
  vi.stubGlobal("WebSocket", InertWebSocket);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), location.origin);
      if (url.pathname === "/query/code_search") return json(ANSWER);
      return json({ repos: [], position: POSITION, level: "session" });
    }),
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

async function mountSearch(width: number) {
  location.hash = "#/gitlet/search?q=Flush(";
  host = document.createElement("div");
  host.className = "app";
  host.innerHTML = `<div class="crewlet-app-shell__content" style="display:flex;flex-direction:column;width:${width}px"></div>`;
  document.body.append(host);
  root = createRoot(host.firstElementChild!);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={{}} org={ORG}>
        <Search />
      </LayoutClient>,
    ),
  );
  await eventually(() => expect(document.querySelectorAll(".code-search-file")).toHaveLength(2));
  return document.querySelector<HTMLElement>(".crewlet-app-shell__content")!;
}

// Mutation: drop the grid's two columns, and the facets stand above the first card.
test("the facets stand beside the results at 1440 px", async () => {
  await page.viewport(1440, 900);
  await mountSearch(1180);
  const facets = await eventually(() => {
    const found = document.querySelector<HTMLElement>(".code-search-facets");
    expect(found).toBeTruthy();
    return found!;
  });
  const line = document.querySelector(".code-search-summary")!.getBoundingClientRect();
  const card = document.querySelector(".code-search-file")!.getBoundingClientRect();
  const column = facets.getBoundingClientRect();
  expect(column.right).toBeLessThanOrEqual(card.left);
  expect(Math.abs(column.top - line.top)).toBeLessThan(12);
});

// Mutation: drop `.code-search-lines`'s own scroll, and the card or the page scrolls instead.
test("a long line scrolls sideways in its card alone at 1024 px", async () => {
  await page.viewport(1024, 900);
  const content = await mountSearch(780);
  const lines = document.querySelectorAll<HTMLElement>(".code-search-lines")[1]!;
  expect(lines.scrollWidth).toBeGreaterThan(lines.clientWidth);
  const card = lines.closest(".code-search-file")!;
  expect(card.scrollWidth).toBeLessThanOrEqual(card.clientWidth);
  expect(content.scrollWidth).toBeLessThanOrEqual(content.clientWidth);
});

// Mutation: keep the facets' column on a phone, and the results overflow it.
test("the search holds its width at 390 px", async () => {
  await page.viewport(390, 844);
  const content = await mountSearch(358);
  await eventually(() =>
    expect(document.querySelector(".code-search--narrow .crewlet-disclosure")).toBeTruthy(),
  );
  const fold = document.querySelector(".code-search--narrow .crewlet-disclosure")!;
  const line = document.querySelector(".code-search-summary")!;
  expect(fold.getBoundingClientRect().bottom).toBeLessThanOrEqual(line.getBoundingClientRect().top);
  // The page's own parts, not the layer host the kit's popovers hang in.
  const form = document.querySelector(".work-search-form")!;
  const grid = document.querySelector(".code-search")!;
  expect([...overflowing(form), ...overflowing(grid)]).toEqual([]);
  expect(sidewaysScrollers(content)).toEqual([]);
  const search = [...document.querySelectorAll("button")].find((b) => b.textContent === "Search")!;
  const box = search.getBoundingClientRect();
  expect(box.right).toBeLessThanOrEqual(content.getBoundingClientRect().right + 0.5);
});
