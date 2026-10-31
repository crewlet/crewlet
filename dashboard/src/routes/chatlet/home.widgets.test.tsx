/**
 * Chatlet's widgets on Home: the frame's reading of the reader's rooms and the
 * Activity feed's mentions, read as the seat the session holds, and nothing
 * asked of a reader the engine would only refuse, or before the engine has
 * said the company runs Chatlet.
 */

import { act, cleanup, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";

import type { Store } from "~/protocol/index.ts";
import type { ReactNode } from "react";
import type { OrgServices } from "~/contract/services.ts";
import { ADA, NATIVE_SERVICES, mountChat } from "~/test/chat.tsx";
import { INCOMPLETE_NOTE } from "~/components/work.tsx";
import { NOTHING_TO_LIST } from "~/routes/home/board/kit.tsx";
import { widgets } from "./home.widgets.tsx";
import { LIVE_REFETCH_MS } from "~/lib/chatLive.ts";
import { ChatRoomsProvider } from "~/lib/chatRooms.ts";

/**
 * A widget mounted as Home mounts it, inside the frame's reading of the
 * reader's rooms, on a company the engine says runs Chatlet.
 */
function mountHome(children: ReactNode, opts: Parameters<typeof mountChat>[1] = {}) {
  return mountChat(<ChatRoomsProvider>{children}</ChatRoomsProvider>, {
    services: NATIVE_SERVICES,
    ...opts,
  });
}

/** One mention in the Activity feed, as `chat_activity` answers it. */
const mention = (extra: Record<string, unknown> = {}) => ({
  key: "mention:m-2",
  kind: "mention",
  message_id: "m-2",
  channel_id: "c-general",
  channel_name: "general",
  channel_kind: "public",
  actor: "bo",
  actor_kind: "human",
  excerpt: "@ada can you look?",
  at: "2026-10-18T09:00:00Z",
  unread: true,
  ...extra,
});

const FEED = (items: unknown[], extra: Record<string, unknown> = {}) => ({
  items,
  unread: { mention: items.length, reply: 0 },
  read_state: true,
  ...extra,
});

const ROOMS = widgets.find((w) => w.key === "chatlet.unread")!;
const MENTIONS = widgets.find((w) => w.key === "chatlet.mentions")!;

const room = (id: string, name: string, unread: number, extra: Record<string, unknown> = {}) => ({
  channel: { id, kind: "public", name },
  unread,
  last: { author: "bo", excerpt: `latest in ${name}`, at: "2026-10-18T09:00:00Z" },
  ...extra,
});

const RAIL = {
  read_state: true,
  read: { handle: "ada", revision: 3, cursors: {} },
  channels: [
    room("c-general", "general", 2),
    room("c-quiet", "quiet", 0),
    room("c-launch", "launch", 120, { unread_capped: true }),
    room("c-noise", "noise", 7, { muted: true }),
  ],
};

async function settle() {
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
}

/** How many times `what` has been asked. */
const asks = (socket: { query: ReturnType<typeof vi.fn> }, what: string) =>
  socket.query.mock.calls.filter(([asked]) => asked === what).length;

/** A message posted somewhere in the company, as the hub pushes it. */
const posted = (store: Store, seq: number) =>
  act(() =>
    store.applyChatChange({
      channel_id: "c-elsewhere",
      op: "post",
      op_id: `op-${seq}`,
      message_id: `m-${seq}`,
      channel_seq: seq,
      at: "2026-10-18T09:01:00Z",
      position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
    } as never),
  );

const pause = (ms: number) => act(() => new Promise((resolve) => setTimeout(resolve, ms)));

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("unread rooms", () => {
  test("lists the rooms with something unread, the engine's count on each, and counts the muted ones", async () => {
    const { Component } = ROOMS;
    const { socket } = mountHome(<Component params={{}} />, { answers: { chat_channels: RAIL } });
    await settle();
    expect(socket.query.mock.calls.map(([what]) => what)).toContain("chat_channels");
    expect(screen.getByText("Unread rooms · 2")).toBeTruthy();
    expect(screen.getByText("#general")).toBeTruthy();
    expect(screen.getByText("#launch")).toBeTruthy();
    // THE CAP IS THE ENGINE'S: "99+" only where the answer says it stopped.
    const row = (name: string) => screen.getByText(`#${name}`).closest("a")!;
    expect(within(row("launch")).getByText("99+")).toBeTruthy();
    expect(within(row("general")).getByText("2")).toBeTruthy();
    expect(screen.queryByText("#quiet")).toBeNull();
    expect(screen.queryByText("#noise")).toBeNull();
    expect(screen.getByText("1 muted room with unread messages left out.")).toBeTruthy();
    expect(screen.getByText("#general").closest("a")?.getAttribute("href")).toBe(
      "#/chatlet/c-general",
    );
  });

  test("an unread count with no read state behind it is not a zero", async () => {
    const { Component } = ROOMS;
    mountHome(<Component params={{}} />, {
      answers: { chat_channels: { ...RAIL, read_state: false } },
    });
    await settle();
    expect(screen.getByText(/couldn’t be read/)).toBeTruthy();
    expect(screen.queryByText("You’re caught up")).toBeNull();
    expect(screen.queryByText("#general")).toBeNull();
  });

  test("a muted room is counted even when it is all that is unread", async () => {
    const { Component } = ROOMS;
    mountHome(<Component params={{}} />, {
      answers: {
        chat_channels: {
          ...RAIL,
          channels: [room("c-quiet", "quiet", 0), room("c-noise", "noise", 7, { muted: true })],
        },
      },
    });
    await settle();
    // NOT "CAUGHT UP": a muted room has messages waiting, and the line that
    // says so goes where the list would have been.
    expect(screen.queryByText("You’re caught up")).toBeNull();
    expect(screen.getByText(NOTHING_TO_LIST)).toBeTruthy();
    expect(screen.getByText("1 muted room with unread messages left out.")).toBeTruthy();
    expect(screen.queryByText("#noise")).toBeNull();
  });

  test("a rail cut at the engine's bound says only its rooms are counted", async () => {
    const { Component } = ROOMS;
    const cut =
      "You’re in more rooms than Chatlet lists, so only your 4 most recently active are counted here.";
    mountHome(<Component params={{}} />, {
      answers: { chat_channels: { ...RAIL, truncated: true } },
    });
    await settle();
    expect(screen.getByText("#general")).toBeTruthy();
    expect(screen.getByText(cut)).toBeTruthy();
    cleanup();

    // AND NEVER "CAUGHT UP" over rooms nobody counted.
    mountHome(<Component params={{}} />, {
      answers: {
        chat_channels: {
          ...RAIL,
          truncated: true,
          channels: RAIL.channels.map((r) => ({ ...r, unread: 0 })),
        },
      },
    });
    await settle();
    expect(screen.queryByText("You’re caught up")).toBeNull();
    expect(screen.getByText(NOTHING_TO_LIST)).toBeTruthy();
    expect(screen.getByText(cut)).toBeTruthy();
  });

  test("a reader with nothing unread is caught up", async () => {
    const { Component } = ROOMS;
    mountHome(<Component params={{}} />, {
      answers: { chat_channels: { ...RAIL, channels: [room("c-quiet", "quiet", 0)] } },
    });
    await settle();
    expect(screen.getByText("You’re caught up")).toBeTruthy();
  });
});

// THE RAIL'S OWN LIVE HALF: a count on Home and the badge in the rail are one
// reading only while both re-read on the same signals — a frame anywhere, and
// the person's own read state once its revision passes what was drawn.
describe("unread rooms, live", () => {
  test("a frame anywhere re-reads the rooms", async () => {
    const { Component } = ROOMS;
    const { socket, store } = mountHome(<Component params={{}} />, {
      answers: { chat_channels: RAIL },
    });
    await waitFor(() => expect(screen.getByText("#general")).toBeTruthy());
    await pause(50);
    const before = asks(socket, "chat_channels");
    expect(before).toBe(1);
    posted(store, 1);
    await waitFor(() => expect(asks(socket, "chat_channels")).toBe(before + 1));
  });

  test("a newer read state re-reads the rooms, and the one they drew asks nothing", async () => {
    const { Component } = ROOMS;
    const { socket, store } = mountHome(<Component params={{}} />, {
      answers: { chat_channels: RAIL },
    });
    await waitFor(() => expect(screen.getByText("#general")).toBeTruthy());
    await pause(50);
    // THE ANSWER'S OWN READ STATE SEEDS THE TAB'S, and that costs no second read.
    const before = asks(socket, "chat_channels");
    expect(before).toBe(1);
    act(() => store.applyChatRead({ handle: "ada", revision: 3, cursors: {} } as never));
    await pause(LIVE_REFETCH_MS + 50);
    expect(asks(socket, "chat_channels")).toBe(before);
    act(() =>
      store.applyChatRead({ handle: "ada", revision: 4, cursors: { "c-general": 9 } } as never),
    );
    await waitFor(() => expect(asks(socket, "chat_channels")).toBe(before + 1));
  });
});

describe("mentions", () => {
  test("lists the newest mentions from Activity, a thread's opening its thread", async () => {
    const { Component } = MENTIONS;
    const { socket } = mountHome(<Component params={{}} />, {
      answers: { chat_activity: FEED([mention({ thread_root: "m-1" })]) },
    });
    await settle();
    // ACTIVITY'S NEWEST PAGE OF MENTIONS, cut to what the card draws.
    const asked = socket.query.mock.calls.find(([what]) => what === "chat_activity");
    expect(asked?.[1]).toMatchObject({ kind: "mention", limit: 5 });
    expect(screen.getByText(/@ada can you look\?/)).toBeTruthy();
    const link = screen
      .getByText(/#general · in a thread/)
      .closest("a")
      ?.getAttribute("href");
    expect(link).toContain("#/chatlet/c-general?");
    expect(decodeURIComponent(link ?? "")).toContain("pane=thread:c-general:m-1");
  });

  test("a mention in a room lands the room on its message", async () => {
    const { Component } = MENTIONS;
    mountHome(<Component params={{}} />, { answers: { chat_activity: FEED([mention()]) } });
    await settle();
    expect(screen.getByText("#general").closest("a")?.getAttribute("href")).toBe(
      "#/chatlet/c-general?message=m-2",
    );
  });

  test("a frame anywhere re-reads the newest page", async () => {
    const { Component } = MENTIONS;
    const { socket, store } = mountHome(<Component params={{}} />, {
      answers: { chat_activity: FEED([]) },
    });
    await waitFor(() => expect(screen.getByText("Nobody has named you")).toBeTruthy());
    await pause(50);
    const before = asks(socket, "chat_activity");
    expect(before).toBe(1);
    posted(store, 1);
    await waitFor(() => expect(asks(socket, "chat_activity")).toBe(before + 1));
  });

  test("nobody has named the reader", async () => {
    const { Component } = MENTIONS;
    mountHome(<Component params={{}} />, { answers: { chat_activity: FEED([]) } });
    await settle();
    expect(screen.getByText("Nobody has named you")).toBeTruthy();
  });
});

// A RAIL OR A FEED THAT COULD NOT ACCOUNT FOR EVERYTHING never claims the
// reader is caught up or unnamed: a record this build cannot decode met the
// read, so the rows it would have produced may be missing.
describe("an answer that could not account for everything", () => {
  test("the rail says so rather than that the reader is caught up", async () => {
    const { Component } = ROOMS;
    mountHome(<Component params={{}} />, {
      answers: {
        chat_channels: { ...RAIL, complete: false, channels: [room("c-quiet", "quiet", 0)] },
      },
    });
    await settle();
    expect(screen.queryByText("You’re caught up")).toBeNull();
    expect(screen.getByText(NOTHING_TO_LIST)).toBeTruthy();
    expect(screen.getByText(INCOMPLETE_NOTE, { exact: false })).toBeTruthy();
  });

  test("a room a newer engine made is counted as left out", async () => {
    const { Component } = ROOMS;
    mountHome(<Component params={{}} />, {
      answers: {
        chat_channels: { ...RAIL, unreadable: 1, channels: [room("c-quiet", "quiet", 0)] },
      },
    });
    await settle();
    expect(screen.queryByText("You’re caught up")).toBeNull();
    expect(screen.getByText(/1 room came from a newer version of the engine/)).toBeTruthy();
  });

  test("the feed says so rather than that nobody named the reader", async () => {
    const { Component } = MENTIONS;
    mountHome(<Component params={{}} />, {
      answers: { chat_activity: FEED([], { complete: false }) },
    });
    await settle();
    expect(screen.queryByText("Nobody has named you")).toBeNull();
    expect(screen.getByText(NOTHING_TO_LIST)).toBeTruthy();
  });
});

describe("a reader chat answers nothing for", () => {
  test.each([ROOMS, MENTIONS])(
    "%s: a credential acting as no seat is told so, and nothing is asked",
    async (def) => {
      const { Component } = def;
      const { socket } = mountHome(<Component params={{}} />, {
        viewer: { ...ADA, handle: "", owner: "token:ci" },
      });
      await settle();
      expect(
        screen.getByText("Chatlet is read as a seat, and this sign-in holds none."),
      ).toBeTruthy();
      expect(socket.query.mock.calls.map(([what]) => what)).toEqual(["viewer"]);
    },
  );

  // NOTHING IS ASKED OF A SERVICE THE COMPANY DOES NOT RUN, nor before the
  // engine has said: Home's board hides both widgets on a company talking on
  // a vendor's chat, and a cold load must not ask first and be refused.
  test.each([
    ["a company on a vendor's chat", { ...NATIVE_SERVICES, chat: "vendor" } as OrgServices],
    ["a company running no chat", { ...NATIVE_SERVICES, chat: "none" } as OrgServices],
    ["a company the engine has not yet described", undefined],
  ])("%s is asked nothing", async (_, services) => {
    for (const { Component } of [ROOMS, MENTIONS]) {
      const { socket } = mountChat(<Component params={{}} />, { services });
      await settle();
      expect(socket.query.mock.calls.map(([what]) => what)).toEqual(["viewer"]);
      cleanup();
    }
  });
});
