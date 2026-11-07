/**
 * The room holds still around the box somebody is typing in: typing, a change
 * of who is typing and the clock ticking render no message row again.
 *
 * A row's renders are counted at the one hook only a message row calls
 * (`useChatPerson`, its author's face), so the count is the transcript's.
 */

import { act, cleanup, fireEvent, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import type { ChatPresencePush, QueryName } from "~/protocol/index.ts";
import { ADA, actEngine, applied, mountChat } from "~/test/chat.tsx";
import { healthFrame } from "~/test/health.ts";

import { ReadCursors } from "./cursor.ts";
import { LoneRoom } from "./testkit.tsx";

const rows = vi.hoisted(() => ({ renders: 0 }));

vi.mock("./people.ts", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./people.ts")>();
  return {
    ...actual,
    useChatPerson: (...args: Parameters<typeof actual.useChatPerson>) => {
      rows.renders += 1;
      return actual.useChatPerson(...args);
    },
  };
});

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 40 },
};

function message(seq: number, over: Record<string, unknown> = {}) {
  return {
    message: {
      v: 1,
      id: `m-${seq}`,
      channel_id: "room-1",
      author: seq % 2 ? "bo" : "ada",
      author_kind: "human",
      body: `line ${seq}`,
      created_at: "2026-01-01T00:00:00Z",
      ...over,
    },
    channel_seq: seq,
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
  };
}

let answers: Partial<Record<QueryName, unknown>>;

beforeEach(() => {
  rows.renders = 0;
  actEngine((tool) => applied(tool, { channel: "room-1" }));
  const thread = {
    replies: 2,
    last_reply_at: "2026-01-01T00:05:00Z",
    last_reply_position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 30 },
    repliers: ["bo"],
    replier_count: 1,
  };
  answers = {
    chat_channel: {
      ...served,
      channel: {
        v: 1,
        id: "room-1",
        kind: "public",
        name: "general",
        created_at: "2026-01-01T00:00:00Z",
      },
      revision: 3,
      message_seq: 12,
      member: true,
      members: [],
    },
    chat_messages: {
      ...served,
      channel_id: "room-1",
      messages: Array.from({ length: 12 }, (_, i) =>
        message(12 - i, 12 - i === 6 ? { thread } : {}),
      ),
    },
    chat_reach: { channel_id: "room-1", reach: {}, quiet: "no_agent" },
  };
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

function mount() {
  const cursors = new ReadCursors({ flush: async () => undefined });
  return mountChat(
    <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
    { answers, viewer: ADA },
  );
}

/** A `chat_presence` roster for room-1 in which `seat` is typing. */
function typing(seat: string): ChatPresencePush {
  return {
    channel_id: "room-1",
    roster: true,
    upsert: [
      {
        session: "s-1",
        seat,
        kind: "human",
        activity: "editing",
        focus: {},
        at: "2026-01-01T00:00:00Z",
      },
    ],
    leave: [],
    truncated: false,
  };
}

// TYPING, AND SOMEBODY ELSE STARTING TO TYPE, RENDER NO ROW. The room renders
// for both (the box, the typing line under it), and handed the transcript new
// arrays and closures each time, which drew every row again under the hands
// of the person typing. Mutation: render the transcript and its rows without
// their memos, and the count moves.
test("typing in the box, and a change of who is typing, render no message row", async () => {
  const { store } = mount();
  await screen.findByText("line 12");
  const box = (await screen.findByLabelText(/^Message #general/)) as HTMLTextAreaElement;
  const before = rows.renders;
  expect(before).toBeGreaterThan(0);
  const words = "steady as she goes";
  for (let i = 1; i <= words.length; i++) {
    fireEvent.change(box, { target: { value: words.slice(0, i) } });
  }
  act(() => {
    store.applyChatPresence(typing("bo"));
  });
  await screen.findByText(/bo is typing/);
  expect(rows.renders).toBe(before);
});

// THE CLOCK RENDERS NO ROW while what it says on screen stands still. Every
// row read the instant for a thread line's age, and the transcript for
// today's date, so the whole list redrew every second. Mutation: read
// `useNow()` in the row again, and the count moves.
test("a minute of the clock ticking renders no message row whose words stand", async () => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
  mount();
  await screen.findByText("line 12");
  const before = rows.renders;
  act(() => {
    vi.advanceTimersByTime(60_000);
  });
  expect(rows.renders).toBe(before);
});

// THE ENGINE'S HEALTH TICK RENDERS NO ROW. It arrives every five seconds and
// renders whatever reads the connection, and the viewer, the write access and
// the acts a room's scope holds were each rebuilt as a new value on that
// render, so every row redrew with each tick. Mutation: build the outbox's
// handle as a fresh object again, and the count moves.
test("the engine's health tick renders no message row", async () => {
  const { store } = mount();
  await screen.findByText("line 12");
  const before = rows.renders;
  for (let tick = 0; tick < 3; tick++) {
    act(() => {
      store.applyHealth(healthFrame({ nodes: 1 }));
    });
  }
  expect(rows.renders).toBe(before);
});

// AN AGENT'S PUSH LEAVES THE BOX ALONE while its @ menu is closed. The composer
// read the whole roster for that menu, so every push from a seat at work
// redrew the box and its formatting row rewrote each button's tabindex.
// Mutation: read `useChatPeople()` in the composer again and the count moves.
test("an agent's push writes nothing into the box while its menu is closed", async () => {
  const { store } = mount();
  await screen.findByText("line 12");
  const box = await screen.findByLabelText("Message #general");
  const form = box.closest(".crewlet-composer")!;
  const writes: MutationRecord[] = [];
  const watch = new MutationObserver((list) => writes.push(...list));
  watch.observe(form, { subtree: true, attributes: true, childList: true });
  for (let push = 0; push < 3; push++) {
    act(() => {
      store.applySeats([
        { agent_id: "ops", id: "ops", name: `Ops ${push}`, status: "working" } as never,
      ]);
    });
  }
  await act(async () => new Promise((r) => setTimeout(r, 20)));
  watch.disconnect();
  expect(writes.filter((m) => m.attributeName === "tabindex")).toEqual([]);
});
