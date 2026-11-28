/**
 * The reader's rooms are read once for the frame, and what waits on them in
 * Chatlet is counted from that one answer.
 *
 * THE CLAIMS: the badge counts every place the engine says waits on the
 * reader (a direct message, a mention, a reply in a thread they follow), each
 * room and thread its own share, says a floor where the engine did, and draws nothing
 * where the counts were not measured; the badge and a surface that draws the
 * rooms share ONE read; the badge alone is asked only of a reader the company
 * serves Chatlet to, while a surface that draws the rooms is asked on its own
 * terms; and a live frame re-reads at the room's rate while a surface draws
 * the rooms and at the badge's slower rate otherwise.
 */

import type { ReactNode } from "react";
import { act, cleanup, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";

import type { ChatChannelsAnswer } from "~/protocol/index.ts";
import { ADA, mountChat, NATIVE_SERVICES } from "~/test/chat.tsx";
import { settled } from "~/test/settle.ts";
import { LIVE_REFETCH_MS } from "./chatLive.ts";
import {
  BADGE_REFETCH_MS,
  ChatRoomsProvider,
  attentionFigure,
  badgesAreMeasured,
  chatAttention,
  chatWaiting,
  useChatAttention,
  useChatRooms,
} from "./chatRooms.ts";

type Row = ChatChannelsAnswer["channels"][number];

const position = { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 12 };

function row(id: string, kind: string, counts: Partial<Row> = {}): Row {
  return {
    channel: { v: 1, id, kind, name: kind === "dm" ? "" : id, created_at: "2026-01-01T00:00:00Z" },
    revision: 1,
    unread: 0,
    ...counts,
  } as Row;
}

function answer(channels: Row[], over: Partial<ChatChannelsAnswer> = {}): ChatChannelsAnswer {
  return {
    read_level: "session",
    complete: true,
    position,
    read_state: true,
    waiting: [],
    waiting_capped: false,
    channels,
    ...over,
  } as ChatChannelsAnswer;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("what waits on the reader", () => {
  // THE ENGINE'S PLACES (`waiting`): a direct message, mentions in a named room,
  // and replies in threads the reader started, replied in or follows, the
  // reply naming them counted once, muted rooms left out by the engine.
  const places = [
    { channel_id: "ada-bo", root_id: "", count: 1, capped: false },
    { channel_id: "general", root_id: "", count: 2, capped: false },
    { channel_id: "general", root_id: "started", count: 3, capped: false },
    { channel_id: "general", root_id: "replied", count: 1, capped: false },
    { channel_id: "launch", root_id: "followed", count: 1, capped: false },
  ];
  const rooms = [
    row("ada-bo", "dm", { unread: 1 }),
    row("general", "public", { unread: 12 }),
    row("launch", "public", { unread: 0 }),
    row("noisy", "public", { unread: 40, muted: true }),
  ];

  test("is every place the engine counts, a thread's replies included", () => {
    const counted = chatAttention(answer(rooms, { waiting: places }));
    expect(counted).toEqual({ value: 8, capped: false });
    expect(attentionFigure(counted!)).toBe("8");
  });

  test("in one room is its posts and its threads' replies, and in one thread its own", () => {
    const waiting = chatWaiting(answer(rooms, { waiting: places }))!;
    expect(waiting.inRoom("general")).toBe(6);
    expect(waiting.inRoom("launch")).toBe(1);
    expect(waiting.inRoom("ada-bo")).toBe(1);
    expect(waiting.inRoom("noisy")).toBe(0);
    expect(waiting.inThread("general", "started")).toBe(3);
    expect(waiting.inThread("launch", "followed")).toBe(1);
    expect(waiting.inThread("general", "followed")).toBe(0);
  });

  test("in a room is a floor where the engine's count of the room stopped at its window", () => {
    const floored = places.map((p) => (p.channel_id === "launch" ? { ...p, capped: true } : p));
    const waiting = chatWaiting(answer(rooms, { waiting: floored }))!;
    expect(waiting.floorIn("launch")).toBe(true);
    expect(waiting.floorIn("general")).toBe(false);
  });

  test("is a floor where the engine counted to its bound, or rooms were left out", () => {
    const bounded = chatAttention(answer(rooms, { waiting: places, waiting_capped: true }));
    expect(bounded).toEqual({ value: 8, capped: true });
    expect(attentionFigure(bounded!)).toBe("8+");
    expect(chatAttention(answer(rooms, { waiting: places, truncated: true }))).toEqual({
      value: 8,
      capped: true,
    });
  });

  // A COUNT WITH NO READ STATE BEHIND IT IS NOT A ZERO, and not a figure.
  test("draws nothing where the counts were not measured, or nothing waits", () => {
    expect(chatAttention(null)).toBeNull();
    expect(
      chatAttention(
        answer(rooms, { waiting: places, read_state: false } as Partial<ChatChannelsAnswer>),
      ),
    ).toBeNull();
    expect(
      chatWaiting(answer(rooms, { read_state: false } as Partial<ChatChannelsAnswer>)),
    ).toBeNull();
    expect(chatAttention(answer(rooms))).toBeNull();
    expect(chatWaiting(answer(rooms))?.inRoom("general")).toBe(0);
    expect(badgesAreMeasured({ read_state: true })).toBe(true);
    expect(badgesAreMeasured({ read_state: false })).toBe(false);
    expect(badgesAreMeasured(null)).toBe(false);
  });
});

/** The sidebar's half: the figure, drawn as text a case can read. */
function Badge() {
  const attention = useChatAttention();
  return <output aria-label="badge">{attention ? attentionFigure(attention) : ""}</output>;
}

/** A surface that draws the rooms: Chatlet's list, Home's widgets. */
function Rooms() {
  const reading = useChatRooms();
  return <output aria-label="rooms">{reading.data?.channels.length ?? ""}</output>;
}

function mount(
  children: ReactNode,
  { viewer = ADA, rows = [row("ada-bo", "dm", { unread: 2 })] } = {},
) {
  const mounted = mountChat(<ChatRoomsProvider>{children}</ChatRoomsProvider>, {
    viewer,
    services: NATIVE_SERVICES,
    answer: async (what) =>
      what === "chat_channels"
        ? answer(rows, {
            waiting: [{ channel_id: "ada-bo", root_id: "", count: 2, capped: false }],
          })
        : {},
  });
  const asked = () => mounted.socket.query.mock.calls.filter(([what]) => what === "chat_channels");
  return { ...mounted, asked };
}

/** One live frame in a room, as the socket applies it. */
function frame(store: ReturnType<typeof mount>["store"], seq: number) {
  act(() => {
    store.applyChatChange({
      channel_id: "ada-bo",
      op: "post",
      op_id: `op-${seq}`,
      message_id: `m-${seq}`,
      channel_seq: seq,
      at: "2026-01-01T00:01:00Z",
      position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
    } as never);
  });
}

describe("the frame's one reading", () => {
  test("is the badge's and the screen's, asked once", async () => {
    const { asked, view } = mount(
      <>
        <Badge />
        <Rooms />
      </>,
    );
    await waitFor(() => expect(view.getByLabelText("badge").textContent).toBe("2"));
    expect(view.getByLabelText("rooms").textContent).toBe("1");
    expect(asked()).toHaveLength(1);
  });

  test("is asked for the badge only of a reader who holds Chatlet", async () => {
    const { asked, socket, view } = mount(<Badge />, { viewer: { ...ADA, grants: [] } });
    await settled(() => socket.query.mock.calls.length);
    expect(asked()).toHaveLength(0);
    expect(view.getByLabelText("badge").textContent).toBe("");
  });

  // THE SCREEN IS MOUNTED ONLY WHERE THE FRAME ADMITTED THE READER, and a
  // viewer read that failed says nothing about their grants: the screen's
  // question is put, and the engine's answer says what is true.
  test("is asked on the screen's own terms while a surface draws the rooms", async () => {
    const { asked } = mount(<Rooms />, { viewer: { ...ADA, grants: [] } });
    await waitFor(() => expect(asked()).toHaveLength(1));
  });

  test("is never asked for a credential acting as no seat", async () => {
    const { asked, socket } = mount(<Rooms />, {
      viewer: { ...ADA, handle: "", owner: "token:ops" },
    });
    await settled(() => socket.query.mock.calls.length);
    expect(asked()).toHaveLength(0);
  });

  test("re-reads on a live frame at the room's rate while a surface draws the rooms", async () => {
    const { asked, store } = mount(<Rooms />);
    await waitFor(() => expect(asked()).toHaveLength(1));
    frame(store, 1);
    await waitFor(() => expect(asked()).toHaveLength(2));
    // A SECOND FRAME INSIDE THE WINDOW is the trailing read, at the room's rate.
    frame(store, 2);
    await new Promise((r) => setTimeout(r, LIVE_REFETCH_MS + 50));
    expect(asked()).toHaveLength(3);
  });

  test("and at the badge's slower rate while only the badge reads it", async () => {
    const { asked, store } = mount(<Badge />);
    await waitFor(() => expect(asked()).toHaveLength(1));
    frame(store, 1);
    await waitFor(() => expect(asked()).toHaveLength(2));
    frame(store, 2);
    await new Promise((r) => setTimeout(r, LIVE_REFETCH_MS + 50));
    // STILL WAITING: the badge's window is longer than the room's.
    expect(BADGE_REFETCH_MS).toBeGreaterThan(LIVE_REFETCH_MS + 50);
    expect(asked()).toHaveLength(2);
  });

  test("re-reads when this person's own read state moves past what it drew", async () => {
    const { asked, store } = mount(<Badge />);
    await waitFor(() => expect(asked()).toHaveLength(1));
    act(() => store.applyChatRead({ handle: "ada", revision: 7, cursors: { "ada-bo": 3 } }));
    await waitFor(() => expect(asked()).toHaveLength(2));
  });
});

describe("outside the frame", () => {
  test("a surface that draws the rooms says so rather than asking for itself", () => {
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      expect(() => mountChat(<Rooms />)).toThrow(/outside a ChatRoomsProvider/);
    } finally {
      spy.mockRestore();
    }
  });
});
