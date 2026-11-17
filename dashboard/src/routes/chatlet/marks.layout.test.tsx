/**
 * THE MARK AFTER A MESSAGE'S WORDS ("(edited)") ENDS ITS LAST LINE, measured,
 * and adds no height of its own to the message.
 */
import "~/test/layout.tsx";
import "~/styles/chatlet-frame.css";
import "~/styles/chatlet-room.css";
import "~/styles/chatlet-panes.css";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";

import type { ChatMessage } from "~/protocol/index.ts";
import { InertWebSocket, LayoutClient, actsApplied } from "~/test/layout.tsx";
import { ADA } from "~/test/chat.tsx";
import { eventually } from "~/test/settle.ts";

import { ReadCursors } from "./cursor.ts";
import { LoneRoom } from "./testkit.tsx";

const WORDS = "where are we on the release?";
const LONG =
  "The stream volume reaches its alarm in about a month at the current rate, so its reservation goes up before then.";

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 1 },
};

/** The viewer's message, with no reach recorded: it draws no receipt. */
const STATES = { unrecorded: {} } as const;

type State = keyof typeof STATES;

function answers(state: State, body: string, edited: boolean) {
  const { answered, ...said } = { answered: undefined, ...STATES[state] };
  const message: ChatMessage = {
    v: 1,
    id: "m-1",
    channel_id: "room-1",
    author: "ada",
    author_kind: "human",
    body,
    created_at: "2026-10-01T08:00:00Z",
    ...(edited ? { edited_at: "2026-10-01T08:00:30Z" } : {}),
    ...(said as Partial<ChatMessage>),
  };
  return {
    viewer: ADA,
    chat_channel: {
      ...served,
      channel: {
        v: 1,
        id: "room-1",
        kind: "public",
        name: "general",
        created_at: "2026-10-01T07:00:00Z",
      },
      revision: 3,
      message_seq: 1,
      member: true,
      members: [],
    },
    chat_channels: { ...served, channels: [] },
    chat_messages: {
      ...served,
      channel_id: "room-1",
      messages: [
        {
          message,
          channel_seq: 1,
          position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 1 },
          ...(answered ? { answered } : {}),
        },
      ],
    },
    chat_reach: { channel_id: "room-1", reach: {}, quiet: "no_agent" },
  };
}

let root: Root | null = null;
let host: HTMLElement | null = null;

beforeEach(() => {
  vi.stubGlobal("WebSocket", InertWebSocket);
  vi.stubGlobal("fetch", actsApplied());
});

function unmount() {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
}

afterEach(() => {
  unmount();
  vi.unstubAllGlobals();
});

/** The message mounted in a room `width` wide, read back once its words are drawn. */
async function mount(state: State, { width = 900, body = WORDS, edited = false } = {}) {
  await page.viewport(1200, 800);
  host = document.createElement("div");
  host.style.cssText = `display: flex; flex-direction: column; width: ${width}px; height: 480px;`;
  document.body.append(host);
  root = createRoot(host);
  const cursors = new ReadCursors({ flush: async () => undefined });
  flushSync(() =>
    root!.render(
      <LayoutClient answers={answers(state, body, edited)} org={{ name: "Acme", roles: [] }}>
        <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />
      </LayoutClient>,
    ),
  );
  await document.fonts.ready;
  return eventually(() => {
    const row = host!.querySelector<HTMLElement>('[data-message-id="m-1"]');
    const words = row?.querySelector<HTMLElement>(".chat-body");
    expect(words).toBeTruthy();
    if (edited) expect(row!.querySelector(".chat-edited")).toBeTruthy();
    return { row: row!, words: words! };
  });
}

/** The words' height with no mark after them, at the same width. */
async function plainHeight(width: number, body: string): Promise<number> {
  const { words } = await mount("unrecorded", { width, body });
  const height = words.getBoundingClientRect().height;
  unmount();
  return height;
}

/** The rect of each line the words' own text runs on, top to bottom, the marks left out. */
function textLines(words: HTMLElement): DOMRect[] {
  const out: DOMRect[] = [];
  const walk = document.createTreeWalker(words, NodeFilter.SHOW_TEXT);
  for (let node = walk.nextNode(); node; node = walk.nextNode()) {
    if (node.parentElement?.closest(".chat-body-after") || !node.textContent?.trim()) continue;
    const range = document.createRange();
    range.selectNodeContents(node);
    for (const rect of range.getClientRects()) {
      const same = out.find((line) => Math.abs(line.bottom - rect.bottom) < 2);
      if (!same) out.push(rect);
      else if (rect.right > same.right) out[out.indexOf(same)] = rect;
    }
  }
  return out.sort((a, b) => a.top - b.top);
}

// THE GAP BEFORE THE LAST PARAGRAPH HOLDS. The paragraph a mark follows runs
// inline and cannot take the block gap, so the one before it keeps it.
// Mutation: drop a class from that rule and the kit's margin reset wins.
test("an edited message keeps the gap before its last paragraph", async () => {
  const body = `${LONG}\n\nThoughts?`;
  const plain = await plainHeight(480, body);
  const { words } = await mount("unrecorded", { width: 480, body, edited: true });
  const lines = textLines(words);
  const edited = words.querySelector(".chat-edited")!.getBoundingClientRect();
  expect(Math.abs(edited.bottom - lines[lines.length - 1]!.bottom)).toBeLessThan(3);
  expect(words.getBoundingClientRect().height).toBeCloseTo(plain, 0);
});
