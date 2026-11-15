/**
 * A LONG ROOM SCROLLS UNDER THE READER'S HAND ALONE, measured: read down a
 * room of eleven days, a step at a time, the page never moves the scroll
 * itself until the reader reaches the foot, never moves it back up, and never
 * throws the reader to the foot.
 *
 * The transcript draws only the rows near the view and stands for the rest
 * with spacers, and the thread (`ChatThread`) holds the row at the top of the
 * view in place: every pixel a spacer is off the rows it stands for becomes a
 * scroll the page makes. Two of them were: a row the window had just removed
 * was recorded at no height at all (the observer reports it once more before
 * it lets go), and a day the window left out took its label and rule with it.
 * Each made the window move the spacers, the thread move the scroll, and the
 * scroll move the window, every frame near a window edge: the room flickered,
 * and the reader was thrown to the foot.
 */
import "~/test/layout.tsx";
import "~/styles/chatlet-frame.css";
import "~/styles/chatlet-room.css";
import "~/styles/chatlet-panes.css";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";

import { InertWebSocket, LayoutClient, actsApplied } from "~/test/layout.tsx";
import { ADA } from "~/test/chat.tsx";
import { eventually } from "~/test/settle.ts";

import { ReadCursors } from "./cursor.ts";
import { LoneRoom } from "./testkit.tsx";

const DAYS = 11;
const PER_DAY = 12;
const LONG =
  "The stream volume reaches its alarm in about a month at the current rate, so its reservation goes up before then, in a short window on the data nodes while the gateway drains traffic to the second region.";

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: DAYS * PER_DAY },
};

/** One message, its day and its length its own, so rows differ in height. */
function message(seq: number) {
  const day = Math.floor((seq - 1) / PER_DAY);
  const at = Date.UTC(2026, 9, 1 + day, 8, 0) + ((seq - 1) % PER_DAY) * 37 * 60_000;
  return {
    message: {
      v: 1,
      id: `m-${seq}`,
      channel_id: "room-1",
      author: seq % 3 ? "bo" : "ada",
      author_kind: "human",
      body: seq % 4 === 0 ? `${LONG}\n\n${LONG}` : seq % 4 === 1 ? LONG : `line ${seq}`,
      created_at: new Date(at).toISOString(),
    },
    channel_seq: seq,
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
  };
}

const ANSWERS: Record<string, unknown> = {
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
    message_seq: DAYS * PER_DAY,
    member: true,
    members: [],
  },
  chat_channels: { ...served, channels: [] },
  chat_messages: {
    ...served,
    channel_id: "room-1",
    // The engine answers newest first.
    messages: Array.from({ length: DAYS * PER_DAY }, (_, i) => message(DAYS * PER_DAY - i)),
  },
  chat_reach: { channel_id: "room-1", reach: {}, quiet: "no_agent" },
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

const frames = (n: number) =>
  new Promise<void>((resolve) => {
    const step = (left: number) =>
      left === 0 ? resolve() : requestAnimationFrame(() => step(left - 1));
    step(n);
  });

test("reading down a long room, the page moves the scroll only at the foot, and never back up", async () => {
  await page.viewport(1200, 800);
  host = document.createElement("div");
  host.style.cssText = "display: flex; flex-direction: column; width: 900px; height: 640px;";
  document.body.append(host);
  root = createRoot(host);
  const cursors = new ReadCursors({ flush: async () => undefined });
  flushSync(() =>
    root!.render(
      <LayoutClient answers={ANSWERS} org={{ name: "Acme", roles: [] }}>
        <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />
      </LayoutClient>,
    ),
  );
  await document.fonts.ready;
  const log = await eventually(() => {
    const found = host!.querySelector<HTMLElement>('[role="log"]');
    expect(found?.querySelector('[data-message-id="m-1"], [data-message-id]')).toBeTruthy();
    return found!;
  });

  // To the very start of the room, and settled there.
  const native = Object.getOwnPropertyDescriptor(Element.prototype, "scrollTop")!;
  for (let i = 0; i < 10; i++) {
    native.set!.call(log, 0);
    await frames(3);
  }
  expect(log.querySelector('[data-message-id="m-1"]')).toBeTruthy();

  // Every scroll the PAGE makes from here on, told apart from the reader's.
  const writes: { from: number; to: number }[] = [];
  Object.defineProperty(log, "scrollTop", {
    configurable: true,
    get: () => native.get!.call(log) as number,
    set: (to: number) => {
      const from = native.get!.call(log) as number;
      native.set!.call(log, to);
      writes.push({ from, to: native.get!.call(log) as number });
    },
  });

  const STEP = 120;
  const room = log.scrollHeight - log.clientHeight;
  let steps = 0;
  while (log.scrollHeight - log.clientHeight - log.scrollTop > 1 && steps < 400) {
    native.set!.call(log, (native.get!.call(log) as number) + STEP);
    steps++;
    await frames(2);
  }
  delete (log as { scrollTop?: number }).scrollTop;

  // The reader walked the whole room, never thrown to the foot.
  expect(steps).toBeGreaterThanOrEqual(Math.floor(room / STEP) - 2);
  // The page never moved the reader back up the room.
  expect(writes.filter((w) => w.to < w.from - 0.5)).toEqual([]);
  // And moved the scroll at all only to keep the foot in view once there.
  expect(writes.length).toBeLessThanOrEqual(3);
});
