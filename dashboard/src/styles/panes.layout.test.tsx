/**
 * NO PANE SCROLLS SIDEWAYS. The dashboard's narrow panes are laid out in a
 * real browser at a rail's and a phone's widths, holding what used to push
 * them wider than themselves: a long unbroken path and a one-line command in
 * inline code, a long address, a labelled link, a long reference, a long
 * title, a wide table and a wide code block. Only a table or a code block may
 * scroll, inside its own box.
 */
import "~/test/layout.tsx";
// Chatlet's own sheets, lazy in the app, after the app's sheet is placed.
import "~/styles/chatlet-room.css";
import "~/styles/chatlet-panes.css";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import type { ReactNode } from "react";
import { ChatThread, ListDetail, PeekRail } from "@crewlethq/ui";
import { ItemPeek } from "~/routes/worklet/WorkItem.tsx";
import { MessageBody } from "~/routes/chatlet/Body.tsx";
import { renderMarkdown } from "~/lib/markdown.ts";
import {
  InertWebSocket,
  LayoutClient,
  overflowing,
  sidewaysScrollers,
  strandedPunctuation,
} from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";

const PATH = `/srv/releases/${"route-cache-worker-".repeat(6)}bundle.tar.zst`;
const COMMAND =
  "docker compose -f /home/deploy/compose.production.yml --project-name route-cache up -d --force-recreate --remove-orphans route-cache-worker";
const ADDRESS = `https://status.example.com/incidents/2031-04-16/${"route-cache-worker-restart-loop-".repeat(3)}?tab=timeline`;
const HOST = "https://route-cache-worker-restart-loop.staging-west-observability.example.com/d/a";
const REFERENCE = "example/route-cache-worker-restart-loop-after-volume-reset#1432";
const TOKEN = "ROUTE_CACHE_WORKER_RESTART_LOOP_AFTER_VOLUME_RESET_BACKOFF_SECONDS_MAXIMUM";
const TITLE = `Restore ${PATH} and recreate the worker with ${TOKEN}`;

const HOSTILE = [
  `Restored from \`${PATH}\` after the reset. Run \`${COMMAND}\` on every host.`,
  "",
  `The timeline is at ${ADDRESS}, the bare address <${ADDRESS}>, and [the restart loop dashboard for the staging cluster in the western region](${HOST}).`,
  "",
  `Tracked in ${REFERENCE}. Override: ${TOKEN}.`,
  "",
  "- Level one with `make deploy-route-cache`",
  "  - Level two with `docker compose up -d worker`",
  "    - Level three with `systemctl restart route-cache`",
  "",
  "| Host | Service | Volume | Mount path | Last restart | Exit code | Restarts | Owner |",
  "|---|---|---|---|---|---|---|---|",
  `| app-west-1a.internal | route-cache-worker | route-cache-data | ${PATH} | 2031-04-16T05:41:59Z | 137 | 1432 | backend |`,
  "",
  "```",
  `${COMMAND} && ${COMMAND}`,
  "```",
].join("\n");

const VIEWER = {
  login: "U0FOUNDER",
  grants: ["admin", "worklet"],
  handle: "jane",
  owner: "jane",
  name: "Jane Founder",
  kind: "human",
  acts: ["update_work_item", "comment_on_work_item"],
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

const ITEM = {
  id: "t-1",
  key: "ENG-42",
  project: "ENG",
  title: TITLE,
  body: HOSTILE,
  status: "in_progress",
  version: 7,
};

const ANSWERS: Record<string, unknown> = {
  viewer: VIEWER,
  inbox: { handle: "jane", notices: [], primary_reasons: [], unread: 0, primary: 0 },
  decisions: { handle: "jane", items: [], total: 0, capped: false },
  work_item: {
    task: ITEM,
    reassignment_budget: 8,
    complete: true,
    links: [{ kind: "waiting_on", other: "t-2", key: "ENG-2", title: TITLE }],
  },
  work_project: {
    key: "ENG",
    name: "Engineering",
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
    key: "ENG-42",
    title: TITLE,
    comments: [
      {
        id: "c-1",
        task: "t-1",
        author: "sre",
        author_kind: "agent",
        body: HOSTILE,
        created_at: "2031-04-16T09:00:00Z",
      },
    ],
  },
  work_item_turns: { item: "t-1", key: "ENG-42", turns: [], complete: true },
};

let root: Root | null = null;
let host: HTMLElement | null = null;

beforeEach(() => {
  vi.stubGlobal("WebSocket", InertWebSocket);
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } }),
    ),
  );
});

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  vi.unstubAllGlobals();
});

/** Renders `node` into a slot exactly `width` wide, and waits until it has settled and laid out. */
async function mount(node: ReactNode, width: number, settle: () => boolean): Promise<HTMLElement> {
  host = document.createElement("div");
  // A slot that scrolls, as the pane or page around a pane does: what a pane
  // spills lands here.
  host.style.cssText = `display: grid; grid-template-columns: minmax(0, 1fr); width: ${width}px; height: 720px; overflow: auto;`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <LayoutClient answers={ANSWERS} org={ORG}>
        {node}
      </LayoutClient>,
    ),
  );
  await eventually(() => expect(settle()).toBe(true));
  await document.fonts.ready;
  await new Promise((resolve) => requestAnimationFrame(() => resolve(undefined)));
  return host;
}

const PANES: {
  name: string;
  selector: string;
  render: (width: number) => ReactNode;
  ready: () => boolean;
}[] = [
  {
    name: "the item detail peek",
    selector: ".crewlet-peek-rail__body",
    render: (width) => (
      <PeekRail
        label="Task detail"
        shape="column"
        width={width}
        minWidth={width}
        onClose={() => {}}
      >
        <ItemPeek itemKey="ENG-42" />
      </PeekRail>
    ),
    // The description and the comment, each with its table, have drawn.
    ready: () => document.querySelectorAll(".crewlet-peek-rail__body .md-table").length >= 2,
  },
  {
    name: "a chat thread",
    selector: ".crewlet-chat-thread__scroller",
    render: () => (
      <ChatThread label="Conversation">
        <MessageBody message={{ body: HOSTILE }} viewer="jane" nameOf={(handle) => handle} />
      </ChatThread>
    ),
    ready: () => !!document.querySelector(".chat-body .md-table"),
  },
  {
    name: "a reading detail",
    selector: ".crewlet-list-detail__detail",
    render: () => (
      <ListDetail list={<nav aria-label="Asks">Asks</nav>} showing="detail">
        <div className="prose md">{renderMarkdown(HOSTILE)}</div>
      </ListDetail>
    ),
    ready: () => !!document.querySelector(".crewlet-list-detail__detail .md-table"),
  },
];

for (const viewport of [
  { width: 1440, height: 900 },
  { width: 390, height: 844 },
]) {
  describe(`at a ${viewport.width}px viewport`, () => {
    for (const pane of PANES) {
      // A slot is never wider than the window it is in.
      for (const width of [330, 390, 420].filter((slot) => slot <= viewport.width)) {
        test(`${pane.name} at ${width}px holds long paths, commands, links and wide blocks without scrolling sideways`, async () => {
          await page.viewport(viewport.width, viewport.height);
          const at = await mount(pane.render(width), width, pane.ready);
          const box = at.querySelector(pane.selector)!;
          expect(
            box.getBoundingClientRect().width,
            `${pane.selector} stays in its slot`,
          ).toBeLessThanOrEqual(width + 0.5);
          expect(overflowing(box), `${pane.selector} at ${width}px`).toEqual([]);
          expect(sidewaysScrollers(box), `around ${pane.selector} at ${width}px`).toEqual([]);
        });
      }
    }
  });
}

const SENTENCE =
  "Approve merging backend#51 (`make deploy`) into main, and see the timeline (https://status.example.com/incidents/2031-04-16) first.";

// A TOKEN KEEPS THE PUNCTUATION AROUND IT: the dashboard's words hand what
// opens and closes around a code span or an address to the token
// (`peelPunctuation`), so no line ends on "(". Before, at many widths a
// line did, while the token moved whole to the next.
test("a token in rendered words keeps its punctuation on its line, at every width a line holds it", async () => {
  await page.viewport(1440, 900);
  const stranded: string[] = [];
  // From the width that holds "(https://status.example.com", the address's unit.
  for (let width = 260; width <= 600; width += 4) {
    const at = await mount(<div className="prose md">{renderMarkdown(SENTENCE)}</div>, width, () =>
      Boolean(document.querySelector(".prose.md code")),
    );
    const found = strandedPunctuation(at.querySelector(".prose.md")!);
    if (found.length) stranded.push(`${width}px: ${found.join(", ")}`);
    flushSync(() => root?.unmount());
    host?.remove();
  }
  expect(stranded).toEqual([]);
});
