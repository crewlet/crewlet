/**
 * A STANDING TASK'S LONG THREAD READS, measured: a task that collects a long
 * report a day folds every report but the newest behind Show more, Jump to
 * latest lands on the newest with the keyboard, its prose keeps a reading
 * measure while its tables take the card's width, the rail's first property
 * stands level with the title, and the watchers are one stack of one size on
 * one line with the reader's Unwatch.
 */
import "~/test/layout.tsx";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";

import { InertWebSocket, LayoutClient, actsApplied } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";

import { WorkItem as WorkItemPage } from "../WorkItem.tsx";

const JANE = {
  login: "U0FOUNDER",
  grants: ["admin", "worklet", "doclet"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["update_work_item", "comment_on_work_item", "create_work_item"],
};

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "Site Reliability Engineer", handle: "sre", kind: "agent" },
    { name: "CTO", handle: "cto", kind: "agent" },
  ],
  units: [],
};

/** One day's report: a title, a table, a list, prose and a block of code. */
function report(run: number): string {
  return [
    `## Platform health check, run ${run}`,
    "",
    "Everything held within its objectives over the last window. Two notes worth reading below, neither of which needs any action today, and the follow-up from yesterday is closed.",
    "",
    "### Service objectives",
    "",
    "| Service | Availability | p99 latency | Error rate | Status |",
    "|---|---|---|---|---|",
    "| API gateway | 99.98% | 412 ms | 0.02% | OK |",
    "| Scheduler | 100.00% | 38 ms | 0.00% | OK |",
    "| Worker pool | 99.95% | 1.9 s | 0.04% | OK |",
    "",
    "### What I checked",
    "",
    "1. Certificates: every public endpoint renews more than 21 days out.",
    "2. Disk: the stream volume is at 61% of its reservation and growing about 0.8% a day.",
    "3. Backups: last night's snapshot restored cleanly into the scratch cluster.",
    "4. Queues: no consumer lagged more than 30 seconds; the dead-letter queue is empty.",
    "",
    "```text",
    `$ healthcheck --window 24h --run ${run}`,
    "api-gateway   availability=99.98  p99=412ms  errors=0.02%",
    "```",
  ].join("\n");
}

const COMMENTS = Array.from({ length: 8 }, (_, i) => ({
  id: `c-${i + 1}`,
  task: "t-1",
  author: "sre",
  author_kind: "agent",
  body: report(i + 1),
  created_at: new Date(Date.UTC(2031, 3, 1 + i, 9, 0)).toISOString(),
})).reverse(); // newest first, as the engine answers

const ANSWERS: Record<string, unknown> = {
  viewer: JANE,
  inbox: { handle: "jane", notices: [], primary_reasons: [], unread: 0, primary: 0 },
  decisions: { handle: "jane", items: [], total: 0, capped: false },
  work_item: {
    task: {
      id: "t-1",
      key: "OPS-7",
      project: "OPS",
      title: "Daily platform health check",
      status: "todo",
      version: 3,
      assignee: "sre",
      watchers: ["jane", "sre", "cto"],
    },
    reassignment_budget: 8,
    complete: true,
  },
  work_project: {
    key: "OPS",
    name: "Operations",
    unit: { resolved: true },
    lead: {},
    task_counts: { todo: 1, active: 0, done: 0, closed: 0 },
    version: 1,
    statuses: [],
    types: [],
    tags: [],
    fields: [],
    policy_stamp: 1,
    complete: true,
  },
  work_items: { items: [], complete: true },
  work_activity: { records: [], complete: true },
  work_comments: {
    item: "t-1",
    key: "OPS-7",
    title: "Daily platform health check",
    comments: COMMENTS,
  },
  work_item_turns: { item: "t-1", key: "OPS-7", turns: [], complete: true },
};

let root: Root | null = null;
let host: HTMLElement | null = null;

beforeEach(() => {
  vi.stubGlobal("WebSocket", InertWebSocket);
  vi.stubGlobal("fetch", actsApplied());
});

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  vi.unstubAllGlobals();
});

async function mount(): Promise<HTMLElement> {
  await page.viewport(1440, 900);
  host = document.createElement("div");
  host.className = "app";
  host.style.cssText = "display: flex; flex-direction: column; width: 1200px; height: 860px;";
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={ANSWERS} org={ORG}>
        <WorkItemPage id="OPS-7" />
      </LayoutClient>,
    ),
  );
  await document.fonts.ready;
  await eventually(() => expect(host!.querySelectorAll(".task-comment")).toHaveLength(8));
  // The folds measure themselves after the comments are drawn.
  await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
  return host;
}

const box = (el: Element) => el.getBoundingClientRect();

test("every report but the newest is folded at one height, with Show more; the newest is open", async () => {
  const at = await mount();
  const folds = [...at.querySelectorAll<HTMLElement>(".task-comment .crewlet-clamp")];
  expect(folds).toHaveLength(8);
  const newest = folds[folds.length - 1]!;
  expect(newest.getAttribute("data-state")).toBe("open");
  expect(newest.textContent).toContain("run 8");
  const heights = new Set(
    folds
      .slice(0, -1)
      .map((fold) => Math.round(box(fold.querySelector(".crewlet-clamp__body")!).height)),
  );
  expect(heights.size).toBe(1);
  const [height] = [...heights];
  expect(height).toBeLessThan(box(newest.querySelector(".crewlet-clamp__body")!).height);
  for (const fold of folds.slice(0, -1)) {
    expect(fold.querySelector("button")!.textContent).toBe("Show more");
  }
});

test("Jump to latest lands on the newest entry, in view and holding the focus", async () => {
  const at = await mount();
  const main = at.querySelector<HTMLElement>(".task-main")!;
  const jump = [...at.querySelectorAll("button")].find((b) => b.textContent === "Jump to latest")!;
  expect(jump).toBeTruthy();
  jump.click();
  await new Promise((resolve) => requestAnimationFrame(() => resolve(undefined)));
  const newest = [...at.querySelectorAll<HTMLElement>(".task-comment")].at(-1)!;
  expect(document.activeElement).toBe(newest);
  const [m, n] = [box(main), box(newest)];
  expect(n.top).toBeGreaterThanOrEqual(m.top - 1);
  expect(n.top).toBeLessThan(m.bottom);
});

test("a report's prose keeps a reading measure while its table takes the card's width", async () => {
  const at = await mount();
  const body = [...at.querySelectorAll<HTMLElement>(".task-comment-body")].at(-1)!;
  const probe = document.createElement("span");
  probe.style.cssText = "display: block; width: 70ch;";
  body.querySelector("p")!.append(probe);
  const measure = box(probe).width;
  probe.remove();
  const paragraph = body.querySelector("p")!;
  expect(box(paragraph).width).toBeLessThanOrEqual(measure + 0.5);
  const table = body.querySelector(".md-table")!;
  expect(box(table).width).toBeGreaterThan(measure);
  expect(Math.abs(box(table).width - box(body).width)).toBeLessThanOrEqual(1);
});

test("the rail's first property stands level with the title", async () => {
  const at = await mount();
  const title = box(at.querySelector(".task-title")!);
  const term = box(at.querySelector(".task-rail dt")!);
  // The first property's line sits within the title's first line.
  expect(term.top).toBeGreaterThanOrEqual(title.top);
  expect(term.bottom).toBeLessThanOrEqual(title.top + title.height + 1);
});

test("the watchers are one stack, one size on one line, beside Unwatch", async () => {
  const at = await mount();
  const section = at.querySelector<HTMLElement>(".task-watching")!;
  const stack = section.querySelector<HTMLElement>(".crewlet-avatar-stack")!;
  expect(stack.getAttribute("role")).toBe("img");
  expect(stack.getAttribute("aria-label")).toBe(
    "Watching: Jane Founder, Site Reliability Engineer and CTO",
  );
  const faces = [...stack.querySelectorAll(".crewlet-avatar")].map(box);
  expect(faces).toHaveLength(3);
  for (const face of faces) {
    expect(face.top).toBe(faces[0]!.top);
    expect(face.height).toBe(faces[0]!.height);
    expect(face.width).toBe(faces[0]!.width);
  }
  // Even overlap: each face starts the same step after the one before.
  expect(faces[1]!.left - faces[0]!.left).toBe(faces[2]!.left - faces[1]!.left);
  const unwatch = [...section.querySelectorAll("button")].find((b) =>
    b.textContent?.includes("Unwatch"),
  )!;
  // On the stack's row, centred on it, at the row's far end.
  const [s, u] = [box(stack), box(unwatch)];
  expect(Math.abs(s.top + s.height / 2 - (u.top + u.height / 2))).toBeLessThanOrEqual(1);
  expect(Math.abs(u.right - box(section).right)).toBeLessThanOrEqual(1);
});
