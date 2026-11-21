/**
 * GITLET › CHECKS, LAID OUT: an open run's pane stands beside the list and scrolls alone, the end
 * of its output scrolls sideways alone, and a phone draws the pane above the list.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { InertWebSocket, LayoutClient, overflowing, sidewaysScrollers } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";
import { Checks } from "./Checks.tsx";

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };
const ORG = { name: "Acme", timezone: "UTC", roles: [], units: [] };
const sha = (short: string) => short.padEnd(40, "0");
const ago = (m: number) => new Date(Date.now() - m * 60_000).toISOString();
const RUN = `api:e2e:1:${sha("c17a9e2")}:refs/heads/main`;

const json = (payload: unknown) =>
  new Response(JSON.stringify(payload), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });

/** Twenty runs, the first api's failed e2e for api!17 in the queue. */
function rows() {
  return Array.from({ length: 20 }, (_, i) => ({
    repo_id: "id-api",
    repo: "api",
    oid: sha(i === 0 ? "c17a9e2" : `a${String(i).padStart(2, "0")}`),
    target: "refs/heads/main",
    name: i === 0 ? "e2e" : `check-${i}`,
    attempt: 1,
    state: i === 0 ? "failed" : "passed",
    purpose: "candidate",
    required: i === 0,
    change: { repo: "api", ref: "api!17", title: "api: page the orders listing", state: "open" },
    source_ref: "refs/heads/backend/orders-keyset",
    change_head: sha("9b2e4d7"),
    base: sha("5c3e8b1"),
    duration_ms: 112_000,
    started_at: ago(22 + i),
    finished_at: ago(20 + i),
  }));
}

/** api's e2e whole: a long output, one line of it `wide` characters. */
function whole(wide: number) {
  const tail = Array.from({ length: 80 }, (_, i) =>
    i === 3 ? `    orders_test.go:88: ${"x".repeat(wide)}` : `=== RUN   TestKeyset${i}`,
  );
  return {
    ...rows()[0],
    def: { name: "e2e", run: "make e2e", timeout_seconds: 900 },
    tests: { total: 214, failed: 1 },
    exit_code: 1,
    log_lines: 1201,
    tail_first_line: 1122,
    tail: `${tail.join("\n")}\n`,
    failures: [{ path: "internal/http/orders_test.go", line: 88, message: "got 49 rows, want 50" }],
    log: { object: "objects/log-api", hash: "h", size: 88_064 },
    url: "https://ci.example.com/runs/8812",
  };
}

let root: Root | null = null;
let host: HTMLElement | null = null;

function engine(wide: number) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), location.origin);
      if (url.pathname === "/query/code_checks") {
        return json({
          checks: rows(),
          standing_counts: { counts: { failed: 1, passed: 19 } },
          position: POSITION,
          level: "session",
        });
      }
      if (url.pathname === "/query/code_check") {
        return json({ check: whole(wide), position: POSITION, level: "session" });
      }
      return json({});
    }),
  );
}

beforeEach(() => {
  vi.stubGlobal("WebSocket", InertWebSocket);
});

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  vi.unstubAllGlobals();
  location.hash = "";
});

/** The section in the shell's scroller at `height`, its column `width` wide, `run=` open. */
async function mountChecks(viewport: number, width: number, height: number, wide = 40) {
  await page.viewport(viewport, height + 100);
  engine(wide);
  location.hash = `#/gitlet/checks?run=${encodeURIComponent(RUN)}`;
  host = document.createElement("div");
  host.className = "app";
  host.innerHTML = `<main class="crewlet-app-shell__main" style="display:block;block-size:${height}px;overflow-y:auto"><div class="crewlet-app-shell__content" style="display:flex;flex-direction:column;width:${width}px"></div></main>`;
  document.body.append(host);
  root = createRoot(host.querySelector(".crewlet-app-shell__content")!);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={{}} org={ORG}>
        <Checks />
      </LayoutClient>,
    ),
  );
  const main = host.querySelector<HTMLElement>("main")!;
  const tail = await eventually(() => {
    const el = document.querySelector<HTMLElement>(".code-check-tail");
    expect(el).not.toBeNull();
    expect(document.querySelectorAll(".code-list__row")).toHaveLength(20);
    return el!;
  });
  return {
    main,
    tail,
    list: document.querySelector<HTMLElement>(".code-checks__list")!,
    pane: document.querySelector<HTMLElement>(".code-checks__pane")!,
    body: document.querySelector<HTMLElement>(".code-check-pane__body")!,
  };
}

// Mutation: bound the pane by a literal, or let its body grow with what it holds.
test("a check's pane stands beside the list and scrolls alone at 1440 px", async () => {
  const { main, list, pane, body } = await mountChecks(1440, 1150, 700);
  expect(pane.getBoundingClientRect().left).toBeGreaterThanOrEqual(
    list.getBoundingClientRect().right,
  );
  expect(body.scrollHeight).toBeGreaterThan(body.clientHeight);
  main.scrollTop = 600;
  const step =
    parseFloat(getComputedStyle(document.documentElement).getPropertyValue("--spacing-4")) || 16;
  // STUCK UNDER THE PAGE'S TOOLBAR, one step below its band.
  const bar = document.querySelector<HTMLElement>(".toolbar")!;
  await eventually(() =>
    expect(
      Math.abs(pane.getBoundingClientRect().top - bar.getBoundingClientRect().bottom),
    ).toBeLessThanOrEqual(step + 1),
  );
  expect(pane.getBoundingClientRect().bottom).toBeLessThanOrEqual(
    main.getBoundingClientRect().bottom + 0.5,
  );
  const at = main.scrollTop;
  body.scrollTop = 300;
  body.dispatchEvent(new Event("scroll"));
  expect(body.scrollTop).toBeGreaterThan(0);
  expect(main.scrollTop).toBe(at);
});

// Mutation: wrap the output's lines, or let the box's width follow its longest line.
test("the end of a check's output scrolls sideways alone at 1024 px", async () => {
  const { tail, pane } = await mountChecks(1024, 760, 700, 300);
  expect(tail.scrollWidth).toBeGreaterThan(tail.clientWidth);
  expect(tail.getAttribute("tabindex")).toBe("0");
  expect(pane.scrollWidth).toBeLessThanOrEqual(pane.clientWidth);
  expect(overflowing(pane)).toEqual([]);
});

// Mutation: drop the narrow page's rule, and the pane squeezes the list beside it.
test("the checks list holds its width at 390 px with a run open", async () => {
  const { list, pane, tail } = await mountChecks(390, 358, 800, 120);
  expect(pane.getBoundingClientRect().bottom).toBeLessThanOrEqual(
    list.getBoundingClientRect().top + 0.5,
  );
  const checks = document.querySelector<HTMLElement>(".code-checks")!;
  const content = document.querySelector<HTMLElement>(".crewlet-app-shell__content")!;
  expect(checks.getBoundingClientRect().right).toBeLessThanOrEqual(
    content.getBoundingClientRect().right + 0.5,
  );
  expect(overflowing(checks)).toEqual([]);
  expect(sidewaysScrollers(list)).toEqual([]);
  expect(sidewaysScrollers(pane)).toEqual([]);
  expect(sidewaysScrollers(tail).filter((s) => !s.startsWith("div.code-check-tail"))).toEqual([]);
});
