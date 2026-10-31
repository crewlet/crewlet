/**
 * The reader's rooms are read once for the frame, and what waits on them in
 * Chatlet is counted from that one answer.
 *
 * THE CLAIMS: the badge counts what is addressed to the reader (a mention in
 * a named room, every unread message in a direct conversation) in the rooms
 * they have not muted, says a floor where the engine did, and draws nothing
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
import { LIVE_REFETCH_MS } from "./chatLive.ts";
import {
  BADGE_REFETCH_MS,
  ChatRoomsProvider,
  attentionFigure,
  badgesAreMeasured,
  chatAttention,
  roomWaiting,
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
    mentions: 0,
    ...counts,
  } as Row;
}

function answer(channels: Row[], over: Partial<ChatChannelsAnswer> = {}): ChatChannelsAnswer {
  return {
    read_level: "session",
    complete: true,
    position,
    read_state: true,
    channels,
    ...over,
  } as ChatChannelsAnswer;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("what waits on the reader", () => {
  test("is the mentions in named rooms and every unread direct message, muted rooms aside", () => {
    const counted = chatAttention(
      answer([
        row("general", "public", { unread: 12, mentions: 2 }),
        row("launch", "private", { unread: 4, mentions: 0 }),
        row("ada-bo", "dm", { unread: 3, mentions: 0 }),
        row("trio", "group", { unread: 1, mentions: 1 }),
        row("noisy", "public", { unread: 40, mentions: 5, muted: true }),
        row("hushed", "dm", { unread: 2, muted: true }),
      ]),
    );
    expect(counted).toEqual({ value: 6, capped: false });
    expect(attentionFigure(counted!)).toBe("6");
  });

  test("in one room is its mentions, a direct conversation's every unread message, and nothing muted", () => {
    expect(roomWaiting(row("general", "public", { unread: 12, mentions: 2 }))).toBe(2);
    expect(roomWaiting(row("ada-bo", "dm", { unread: 3, mentions: 1 }))).toBe(3);
    expect(roomWaiting(row("trio", "group", { unread: 4 }))).toBe(4);
    expect(roomWaiting(row("noisy", "public", { unread: 40, mentions: 5, muted: true }))).toBe(0);
    expect(roomWaiting(row("hushed", "dm", { unread: 2, muted: true }))).toBe(0);
  });

  test("is a floor where a counted room stopped at the engine's bound, or rooms were left out", () => {
    const bounded = chatAttention(
      answer([row("ada-bo", "dm", { unread: 100, unread_capped: true })]),
    );
    expect(bounded).toEqual({ value: 100, capped: true });
    expect(attentionFigure(bounded!)).toBe("100+");
    // A ROOM COUNTED TO ITS BOUND THAT HOLDS NOTHING FOR THE READER says
    // nothing about what waits on them.
    expect(
      chatAttention(
        answer([
          row("general", "public", { unread: 100, unread_capped: true, mentions: 0 }),
          row("launch", "public", { unread: 1, mentions: 1 }),
        ]),
      ),
    ).toEqual({ value: 1, capped: false });
    expect(
      chatAttention(
        answer([row("launch", "public", { unread: 1, mentions: 1 })], { truncated: true }),
      ),
    ).toEqual({ value: 1, capped: true });
  });

  // A COUNT WITH NO READ STATE BEHIND IT IS NOT A ZERO, and not a figure.
  test("draws nothing where the counts were not measured, or nothing waits", () => {
    expect(chatAttention(null)).toBeNull();
    expect(
      chatAttention(
        answer([row("ada-bo", "dm", { unread: 3 })], {
          read_state: false,
        } as Partial<ChatChannelsAnswer>),
      ),
    ).toBeNull();
    expect(chatAttention(answer([row("general", "public", { unread: 9 })]))).toBeNull();
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
    answer: async (what) => (what === "chat_channels" ? answer(rows) : {}),
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
    const { asked, view } = mount(<Badge />, { viewer: { ...ADA, grants: [] } });
    await act(async () => {
      for (let i = 0; i < 5; i++) await Promise.resolve();
    });
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
    const { asked } = mount(<Rooms />, { viewer: { ...ADA, handle: "", owner: "token:ops" } });
    await act(async () => {
      for (let i = 0; i < 5; i++) await Promise.resolve();
    });
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
