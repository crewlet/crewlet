/**
 * GITLET › QUEUE, LAID OUT: a box's entries stand on its spine, their checks keep one column
 * down the box, and nothing scrolls sideways on a phone.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { InertWebSocket, LayoutClient, overflowing, sidewaysScrollers } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";
import { Queue } from "./Queue.tsx";

const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 12 };
const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [{ name: "Jane Founder", handle: "jane", kind: "human" }],
  units: [],
};
const oid = (short: string) => short.padEnd(40, "0");
const BASE = oid("c71d0e9");
const T1 = oid("e1e1e1e");
const ago = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();

function entry(place: number, number: number, title: string, more: object = {}) {
  return {
    repo_id: "id-web",
    repo: "web",
    entry_id: `web-${place}`,
    target_ref: "refs/heads/main",
    change_id: `c-web-${number}`,
    number,
    title,
    place,
    seq: POSITION,
    head_oid: oid("aaaaaaa"),
    method: "merge",
    enqueued_by: { handle: "jane", kind: "person" },
    state: "waiting",
    enqueued_at: ago(12),
    ...more,
  };
}

const QUEUE = [
  entry(1, 44, "server: drain inside the load balancer's deregistration delay", {
    state: "testing",
    candidate_id: "cand-1",
    train: "t1",
  }),
  entry(2, 43, "checkout: retry a declined card once with the network token"),
  entry(3, 42, "a change whose title runs past any phone's width ".repeat(3)),
];

const json = (payload: unknown) =>
  new Response(JSON.stringify({ ...(payload as object), position: POSITION, level: "session" }), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });

function answer(url: URL): Response {
  switch (url.pathname) {
    case "/query/code_queue":
      return json({
        queue: QUEUE,
        trains: [
          {
            candidate_id: "cand-1",
            target_ref: "refs/heads/main",
            train: "t1",
            ordinal: 1,
            base_oid: BASE,
            tip_oid: T1,
            entries: [{ entry_id: "web-1", commit: T1 }],
            state: "testing",
          },
        ],
        ejected: [
          {
            repo_id: "id-web",
            repo: "web",
            entry_id: "web-gone",
            target_ref: "refs/heads/main",
            change_id: "c-web-17",
            number: 17,
            title: "api: page the orders listing by keyset",
            head_oid: oid("bbbbbbb"),
            method: "merge",
            enqueued_by: { handle: "jane", kind: "person" },
            reason: "check_failed",
            checks: ["e2e"],
            enqueued_at: ago(23),
            ejected_at: ago(20),
            tested_in: { candidate_id: "cand-9", base_oid: BASE, commit_oid: T1 },
          },
        ],
        ejected_total: { total: 1 },
      });
    case "/query/code_checks":
      return json({
        checks: ["unit", "e2e"].map((name, i) => ({
          repo: "web",
          oid: T1,
          target: "refs/heads/main",
          name,
          attempt: 1,
          state: i === 0 ? "passed" : "running",
          required: true,
          timeout_seconds: 1800,
          started_at: ago(4),
        })),
      });
    case "/query/code_repo":
      return json({
        repo: {
          name: "web",
          gates: [{ pattern: "main", gate: { approvals: "one", checks: ["e2e", "unit"] } }],
        },
      });
    case "/query/code_refs":
      return json({
        refs_detail: [
          {
            ref: "refs/heads/main",
            oid: BASE,
            position: POSITION,
            protection: { patterns: ["main"], direct_push: "none" },
          },
        ],
      });
    case "/query/code_log":
      return json({
        log: {
          commits: [{ oid: BASE, subject: "Merge change web!41: cache the cart badge for 30s" }],
        },
      });
    case "/query/code_counts":
      return json({ counts: { queue: { live: 3 } } });
  }
  return json({});
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

async function mount(width: number) {
  await page.viewport(width, 900);
  location.hash = "#/gitlet/queue";
  host = document.createElement("div");
  host.className = "app";
  host.innerHTML = `<div class="crewlet-app-shell__content" style="display:flex;flex-direction:column;width:${width}px"></div>`;
  document.body.append(host);
  root = createRoot(host.firstElementChild!);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={{}} org={ORG}>
        <Queue />
      </LayoutClient>,
    ),
  );
  return eventually(() => {
    const list = document.querySelector<HTMLElement>(".code-list--boxes");
    expect(list?.querySelectorAll(".code-queue__checks")).toHaveLength(3);
    expect(list?.textContent).toContain("Merge change web!41");
    return list!;
  });
}

const centre = (el: Element) => {
  const r = el.getBoundingClientRect();
  return r.left + r.width / 2;
};

/** The spine's line: the x of a row's `::before` border, through its lead. */
function spineOf(row: Element): number {
  const style = getComputedStyle(row, "::before");
  return (
    row.getBoundingClientRect().left +
    parseFloat(style.left) +
    parseFloat(style.borderLeftWidth) / 2
  );
}

/** Every meter and bar in an entry's checks cell takes width; a flex item sizes to nothing. */
function marksHaveWidth(list: HTMLElement) {
  const marks = [...list.querySelectorAll(".code-queue__mark > *")];
  expect(marks.length).toBeGreaterThan(0);
  for (const mark of marks) expect(mark.getBoundingClientRect().width).toBeGreaterThan(0);
}

// Mutation: drop `.code-list--boxes .code-list__lead`'s centring, or the spine's offset, or the
// marks' `flex: 1 1 auto`.
test("a queue's entries stand on its spine at 1440 px", async () => {
  const list = await mount(1440);
  marksHaveWidth(list);
  const rows = [...list.querySelectorAll(".code-list__row")];
  const line = spineOf(rows[0]!);
  for (const badge of list.querySelectorAll(".code-queue__place")) {
    expect(Math.abs(centre(badge) - line)).toBeLessThanOrEqual(1);
  }
  expect(Math.abs(centre(list.querySelector(".code-queue__node")!) - line)).toBeLessThanOrEqual(1);
  for (const row of rows) expect(Math.abs(spineOf(row) - line)).toBeLessThanOrEqual(1);
});

// Mutation: let the checks cell size to its words (`grid-template-columns: auto auto auto`).
test("an entry's checks and Take out keep their columns at 1024 px", async () => {
  const list = await mount(1024);
  const cells = [...list.querySelectorAll(".code-queue__checks")];
  const lefts = cells.map((c) => Math.round(c.getBoundingClientRect().left));
  expect(new Set(lefts).size).toBe(1);
  const card = list.querySelector(".code-list__group")!.getBoundingClientRect();
  for (const out of list.querySelectorAll(".code-queue__out")) {
    expect(out.getBoundingClientRect().right).toBeLessThanOrEqual(card.right);
  }
});

// Mutation: drop the narrow rule that puts the facts under the entry's words.
test("the queues hold their width at 390 px", async () => {
  const list = await mount(390);
  const acts = await eventually(() => {
    const run = [...list.querySelectorAll("a")].find((a) => a.textContent === "View the run");
    expect(run).toBeTruthy();
    return [run!, list.querySelector('[aria-label="Add web!17 to the queue again"]')!];
  });
  const card = list.querySelector(".code-list__group")!.getBoundingClientRect();
  for (const el of acts) {
    const at = el.getBoundingClientRect();
    expect(at.width).toBeGreaterThan(0);
    expect(at.left).toBeGreaterThanOrEqual(card.left);
    expect(at.right).toBeLessThanOrEqual(card.right);
  }
  const row = list.querySelector(".code-list__row")!;
  const body = row.querySelector(".code-list__body")!.getBoundingClientRect();
  const facts = row.querySelector(".code-list__facts")!.getBoundingClientRect();
  expect(facts.top).toBeGreaterThanOrEqual(Math.floor(body.bottom));
  expect(overflowing(list)).toEqual([]);
  expect(sidewaysScrollers(list)).toEqual([]);
});
