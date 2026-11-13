/**
 * THE INBOX, LAID OUT: an answer's advance opens the next item at its head, the
 * open item's title fills its first line, and a held call stays in the column.
 */
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { Inbox } from "./Inbox.tsx";
import { InertWebSocket, LayoutClient, actsApplied, firstLineRoom } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";
import heldWords from "../../../../internal/agent/builtin/testdata/ssh_operate_held.json";

const VIEWER = {
  login: "U0FOUNDER",
  grants: ["admin", "worklet"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["comment_on_work_item", "update_work_item", "mark_inbox"],
};

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human" },
    { name: "Site Reliability Engineer", handle: "sre", kind: "agent" },
  ],
  units: [],
};

const CONTEXT =
  "The worker restarted cleanly and has held for a day; the volume is mounted where it should be. ".repeat(
    40,
  );

/** An ask put to the reader, with a long context to scroll through. */
function ask(n: number, question: string) {
  return {
    kind: "ask",
    at: new Date(Date.now() - n * 60_000).toISOString(),
    ask: {
      id: `t-${n}`,
      key: `ENG-${n}`,
      project: "ENG",
      title: `Restart loop on host ${n}`,
      type: "task",
      status: "todo",
      comment: `c-${n}`,
      asked_by: "sre",
      asked_at: new Date(Date.now() - n * 60_000).toISOString(),
      body: CONTEXT,
      decision: {
        question,
        options: [
          { id: "keep", label: `Confirmed, keep host ${n} done` },
          { id: "reopen", label: "Reopen it" },
        ],
        recommended: "keep",
        rationale: CONTEXT,
        role: "approver",
      },
      answer_with: "",
    },
  };
}

const FIRST = "Is host 1 stable after the restart?";
const SECOND =
  "Should the restart loop fix ship to every host tonight, or wait until the volume reset is confirmed on staging?";

const ANSWERS: Record<string, unknown> = {
  viewer: VIEWER,
  decisions: { handle: "jane", items: [ask(1, FIRST), ask(2, SECOND)], total: 2, capped: false },
  inbox: {
    handle: "jane",
    notices: [],
    sources: [{ name: "tracker", primary_reasons: [] }],
    unread: 0,
    primary: 0,
  },
  answered_decisions: { handle: "jane", items: [] },
  work_person: {
    handle: "jane",
    version: 1,
    held: true,
    complete: true,
    max_snooze_ahead: 30 * 86_400,
  },
  work_comments: { item: "t-1", key: "ENG-1", title: "Restart loop on host 1", comments: [] },
  work_items: { items: [], total_hint: 0 },
  executors: { runs: [] },
};

let root: Root | null = null;
let host: HTMLElement | null = null;

beforeEach(() => {
  vi.stubGlobal("WebSocket", InertWebSocket);
  vi.stubGlobal("fetch", actsApplied());
  location.hash = "#/inbox?row=ask%3Ac-1";
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
  host = document.createElement("div");
  host.style.cssText = `display: flex; flex-direction: column; width: ${width}px; height: 720px;`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={ANSWERS} org={ORG}>
        <Inbox />
      </LayoutClient>,
    ),
  );
  await eventually(() => expect(title()?.textContent).toBe(FIRST));
  await document.fonts.ready;
}

const title = () => document.querySelector(".crewlet-reading-pane__title");
const scroller = () => document.querySelector<HTMLElement>(".inbox-pane");

test("the next item after an answer opens at its head, wherever the last one was scrolled to", async () => {
  await page.viewport(1440, 900);
  await mount(1200);
  const pane = scroller()!;
  pane.scrollTop = pane.scrollHeight;
  expect(pane.scrollTop).toBeGreaterThan(0);
  [...document.querySelectorAll("button")]
    .find((b) => b.textContent?.includes("Confirmed, keep host 1 done"))!
    .click();
  await eventually(() => expect(title()?.textContent).toBe(SECOND));
  expect(pane.scrollTop).toBe(0);
  const head = title()!.getBoundingClientRect();
  const view = pane.getBoundingClientRect();
  expect(head.top).toBeGreaterThanOrEqual(view.top);
  expect(head.bottom).toBeLessThanOrEqual(view.bottom);
});

test("the open item's title fills its first line to within one word, at every width", async () => {
  await page.viewport(1440, 900);
  location.hash = "#/inbox?row=ask%3Ac-2";
  const loose: string[] = [];
  for (let width = 760; width <= 1240; width += 40) {
    ANSWERS.decisions = {
      handle: "jane",
      items: [ask(2, SECOND), ask(1, FIRST)],
      total: 2,
      capped: false,
    };
    host = document.createElement("div");
    host.style.cssText = `display: flex; flex-direction: column; width: ${width}px; height: 720px;`;
    document.body.append(host);
    root = createRoot(host);
    flushSync(() =>
      root!.render(
        <LayoutClient answers={ANSWERS} org={ORG}>
          <Inbox />
        </LayoutClient>,
      ),
    );
    await eventually(() => expect(title()?.textContent).toBe(SECOND));
    await document.fonts.ready;
    const room = firstLineRoom(title()!);
    if (room !== null) loose.push(`${width}px (gap ${room})`);
    flushSync(() => root?.unmount());
    host.remove();
  }
  ANSWERS.decisions = {
    handle: "jane",
    items: [ask(1, FIRST), ask(2, SECOND)],
    total: 2,
    capped: false,
  };
  expect(loose, "widths where the first line left room for the next two words").toEqual([]);
});

/** A run holding an ssh_operate call, worded as the engine words it (`make ssh-held-words`). */
function heldRun() {
  const at = new Date(Date.now() - 5 * 60_000).toISOString();
  return {
    turn_id: "turn-9",
    launch_id: "q9",
    agent_handle: "sre",
    status: "awaiting_clarification",
    question: heldWords.long.question,
    question_context: heldWords.long.context,
    decision: {
      question: heldWords.long.question,
      options: [
        { id: "once", label: "Allow once" },
        { id: "deny", label: "Deny" },
      ],
      role: "approver",
    },
    audience_handles: ["jane"],
    held_call: {
      id: "h-9",
      digest: "",
      held_at: at,
      server: "ssh",
      tool: "ssh_operate",
      raw: "ssh_operate",
      fixed: true,
      approvers: "ssh",
      // AS THE ENGINE SENDS IT: the act is what a held call's title is read by.
      act: "ssh_operate",
      delegation: null,
    },
    started_at: at,
    updated_at: at,
  };
}

test("a held call's 400-character command and reason stay inside the reading column, and nothing scrolls sideways", async () => {
  const run = heldRun();
  const answers = {
    ...ANSWERS,
    decisions: {
      handle: "jane",
      items: [{ kind: "run", at: run.updated_at, run }],
      total: 1,
      capped: false,
    },
    executors: { runs: [run] },
  };
  location.hash = "#/inbox?row=run%3Aturn-9";
  const outside: string[] = [];
  for (const [width, height] of [
    [1440, 900],
    [2560, 1154],
    [390, 844],
  ] as const) {
    await page.viewport(width, height);
    host = document.createElement("div");
    host.style.cssText = `display: flex; flex-direction: column; width: ${width}px; height: ${height}px;`;
    document.body.append(host);
    root = createRoot(host);
    flushSync(() =>
      root!.render(
        <LayoutClient answers={answers} org={ORG}>
          <Inbox />
        </LayoutClient>,
      ),
    );
    await eventually(() => expect(title()?.textContent).toBe(heldWords.long.question));
    await eventually(() =>
      expect(document.querySelector(".crewlet-reading-pane__lead pre")).not.toBeNull(),
    );
    await document.fonts.ready;
    const column = document.querySelector(".crewlet-reading-pane__column")!;
    const inner =
      column.getBoundingClientRect().right - parseFloat(getComputedStyle(column).paddingRight);
    const blocks = document.querySelectorAll(
      ".crewlet-reading-pane__column > *, .crewlet-reading-pane__lead > *",
    );
    expect(blocks.length).toBeGreaterThan(4);
    for (const block of blocks) {
      const right = block.getBoundingClientRect().right;
      if (right > inner + 0.5) {
        outside.push(
          `${width}px: <${block.tagName.toLowerCase()}> ends ${Math.round(right - inner)}px past the column`,
        );
      }
    }
    for (const box of document.querySelectorAll<HTMLElement>(
      ".inbox-pane, .crewlet-reading-pane__lead pre",
    )) {
      if (box.scrollWidth > box.clientWidth) {
        outside.push(
          `${width}px: .${box.className || box.tagName} scrolls sideways by ${box.scrollWidth - box.clientWidth}px`,
        );
      }
    }
    flushSync(() => root?.unmount());
    host.remove();
    host = null;
    root = null;
  }
  expect(outside, "blocks past the reading column, and boxes that scroll sideways").toEqual([]);
});
