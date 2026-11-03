/**
 * The reader's views: Activity, Threads, Saved, Drafts and sent, New message.
 *
 * THE INVARIANTS: each view is the engine's answer about the reader, asked by
 * its own name; a row read in Activity is marked read through its room's
 * cursor; Unfollow and Remove write the reader's own marks and are drawn for
 * every reader, held with why (`app/writeGate.test.tsx` names this suite for
 * `Views.tsx`); a new message opens its conversation only when there is
 * something to say, and lands the reader in it.
 */

import type { ReactNode } from "react";
import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { ADA, actEngine, applied, type Pressed } from "~/test/chat.tsx";
import { MAX_DIRECT_PARTICIPANTS } from "~/contract/chat.ts";
import { CHAT_ORG, SERVED, WRITE_READERS, heldWith } from "~/test/chatAccess.ts";

import { mountInFrame } from "./frameKit.tsx";
import { ActivityView, DraftsView, NewMessageView, SavedView, ThreadsView } from "./Views.tsx";

const ROOM = "room-1";

const view = (id: string, author: string, extra: Record<string, unknown> = {}) => ({
  message: {
    v: 1,
    id,
    channel_id: ROOM,
    author,
    author_kind: "human",
    body: `said ${id}`,
    created_at: "2026-10-30T09:00:00Z",
    ...extra,
  },
  channel_seq: 1,
  position: SERVED.position,
});

const RAIL = { ...SERVED, read_state: true, channels: [] };

let presses: Pressed[];

beforeEach(() => {
  presses = actEngine((tool) =>
    applied(tool, tool === "open_dm" ? { channel: "dm-1", kind: "dm" } : {}),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function mount(
  children: ReactNode,
  hash: string,
  answers: Record<string, unknown>,
  viewer: Record<string, unknown> = ADA,
) {
  const mounted = mountInFrame(children, {
    hash,
    viewer,
    answers: { chat_channels: RAIL, ...answers } as never,
  });
  act(() => mounted.store.applyOrg(CHAT_ORG as never));
  return mounted;
}

describe("Activity", () => {
  const item = {
    key: "mention:m-2",
    kind: "mention",
    message_id: "m-2",
    channel_id: ROOM,
    channel_name: "launch",
    channel_kind: "public",
    actor: "bo",
    actor_kind: "human",
    excerpt: "@ada can you look?",
    at: "2026-10-30T09:00:00Z",
    unread: true,
    read_through: 41,
  };
  const FEED = { ...SERVED, items: [item], unread: { mention: 1, reply: 0 }, read_state: true };

  test("lists what reached the reader, and reads the chosen one in place", async () => {
    const { socket } = mount(<ActivityView />, "#/chatlet/activity", {
      chat_activity: FEED,
      chat_message: { ...SERVED, channel_kind: "public", view: view("m-2", "bo") },
    });
    expect(await screen.findByText(/mentioned you in #launch/)).toBeTruthy();
    expect(screen.getByText("@ada can you look?")).toBeTruthy();
    await waitFor(() =>
      expect(socket.query.mock.calls.some(([what]) => what === "chat_message")).toBe(true),
    );
  });

  test("choosing an unread row marks it read through its room's cursor", async () => {
    mount(<ActivityView />, "#/chatlet/activity", {
      chat_activity: FEED,
      chat_message: { ...SERVED, channel_kind: "public", view: view("m-2", "bo") },
    });
    fireEvent.click(await screen.findByText(/mentioned you in #launch/));
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]).toMatchObject({
      tool: "mark_chat_read",
      args: { cursors: { [ROOM]: 41 } },
    });
  });

  // A CONVERSATION OF THE READER AND THE ACTOR ALONE IS NOT NAMED: its title
  // is the actor's own name, which the row has just said. A group
  // conversation and a named room still are.
  test("a line names the room, except a conversation of the reader and the actor alone", async () => {
    const summary = (id: string, kind: string, participants: string[]) => ({
      channel: { v: 1, id, kind, created_at: "2026-10-01T00:00:00Z" },
      participants,
      unread: 0,
      mentions: 0,
    });
    const row = (key: string, kind: string, channel: string, channelKind: string) => ({
      ...item,
      key,
      kind,
      emoji: "👍",
      message_id: key,
      channel_id: channel,
      channel_kind: channelKind,
      channel_name: channelKind === "public" ? "launch" : undefined,
      unread: false,
    });
    mount(<ActivityView />, "#/chatlet/activity", {
      chat_channels: {
        ...RAIL,
        channels: [
          summary("dm-1", "dm", ["ada", "bo"]),
          summary("grp-1", "group", ["ada", "bo", "ops"]),
        ],
      },
      chat_activity: {
        ...FEED,
        items: [
          row("reply-dm", "reply", "dm-1", "dm"),
          row("mention-dm", "mention", "dm-1", "dm"),
          row("reaction-dm", "reaction", "dm-1", "dm"),
          row("reply-group", "reply", "grp-1", "group"),
          row("reply-room", "reply", ROOM, "public"),
        ],
      },
      chat_message: { ...SERVED, channel_kind: "dm", view: view("reply-dm", "bo") },
    });
    const qualifiers = async () =>
      (await screen.findAllByText(/^(replied|mentioned|reacted)/)).map((el) => el.textContent);
    await waitFor(async () => expect(await qualifiers()).toHaveLength(5));
    expect(await qualifiers()).toEqual([
      "replied to a thread",
      "mentioned you",
      "reacted 👍 to your message",
      "replied to a thread in Bo Chen, Ops Agent",
      "replied to a thread in #launch",
    ]);
  });

  test("Unread only asks the engine for the unread rows", async () => {
    const { socket } = mount(<ActivityView />, "#/chatlet/activity", {
      chat_activity: FEED,
      chat_message: { ...SERVED, channel_kind: "public", view: view("m-2", "bo") },
    });
    fireEvent.click(await screen.findByRole("switch", { name: /unread only/i }));
    await waitFor(() =>
      expect(
        socket.query.mock.calls.some(
          ([what, params]) => what === "chat_activity" && (params as { unread?: boolean })?.unread,
        ),
      ).toBe(true),
    );
  });
});

describe("Threads", () => {
  const THREADS = {
    ...SERVED,
    read_state: true,
    threads: [
      {
        channel_id: ROOM,
        channel_name: "launch",
        channel_kind: "public",
        reason: "replied",
        root: view("m-1", "bo"),
        latest: [view("m-2", "ops", { thread_root: "m-1" })],
      },
    ],
  };

  test("Unfollow writes the reader's own mark", async () => {
    mount(<ThreadsView />, "#/chatlet/threads", { chat_threads: THREADS });
    fireEvent.click(await screen.findByRole("button", { name: /^unfollow$/i }));
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]).toMatchObject({
      tool: "mark_chat_read",
      args: { unfollow: [{ channel_id: ROOM, root_id: "m-1" }] },
    });
  });

  test.each(WRITE_READERS)("Unfollow is drawn for $who, held with why", async (reader) => {
    mount(<ThreadsView />, "#/chatlet/threads", { chat_threads: THREADS }, reader.viewer);
    await heldWith(await screen.findByRole("button", { name: /^unfollow$/i }), reader.reason);
  });
});

describe("Saved", () => {
  const SAVED = {
    ...SERVED,
    read_state: true,
    saved: [
      {
        mark: { channel_id: ROOM, message_id: "m-1", at: "2026-10-30T09:00:00Z" },
        channel_name: "launch",
        channel_kind: "public",
        view: view("m-1", "bo"),
      },
      {
        mark: { channel_id: ROOM, message_id: "m-9", at: "2026-10-29T09:00:00Z" },
        channel_name: "launch",
        channel_kind: "public",
        gone: true,
      },
    ],
  };

  test("a message that is gone says so, and can still be removed", async () => {
    mount(<SavedView />, "#/chatlet/saved", { chat_saved: SAVED });
    expect(await screen.findByText(/isn’t there any more/)).toBeTruthy();
    const removes = screen.getAllByRole("button", { name: /^remove$/i });
    fireEvent.click(removes[1]!);
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]).toMatchObject({
      tool: "mark_chat_read",
      args: { unsave: [{ channel_id: ROOM, message_id: "m-9" }] },
    });
  });

  test.each(WRITE_READERS)("Remove is drawn for $who, held with why", async (reader) => {
    mount(<SavedView />, "#/chatlet/saved", { chat_saved: SAVED }, reader.viewer);
    const [first] = await screen.findAllByRole("button", { name: /^remove$/i });
    await heldWith(first!, reader.reason);
  });
});

describe("Drafts and sent", () => {
  test("nothing waiting says what lands here", async () => {
    mount(<DraftsView />, "#/chatlet/drafts", { chat_sent: { ...SERVED, messages: [] } });
    expect(await screen.findByText("Nothing waiting")).toBeTruthy();
  });

  test("Sent lists what the reader said, newest first, from the engine", async () => {
    const { socket } = mount(<DraftsView />, "#/chatlet/drafts", {
      chat_sent: {
        ...SERVED,
        messages: [{ channel_name: "launch", channel_kind: "public", view: view("m-5", "ada") }],
      },
    });
    fireEvent.click(await screen.findByRole("tab", { name: /^sent/i }));
    expect(await screen.findByText("said m-5")).toBeTruthy();
    expect(socket.query.mock.calls.some(([what]) => what === "chat_sent")).toBe(true);
  });
});

describe("New message", () => {
  test("opens the conversation only to say something, then lands in it", async () => {
    mount(<NewMessageView />, "#/chatlet/new?to=bo", {});
    const to = await screen.findByRole("textbox", { name: /who the message is to/i });
    expect(within(to.closest("div")!.parentElement!).getByText("Bo Chen")).toBeTruthy();
    expect(screen.getByText(/starts a new conversation with Bo Chen/)).toBeTruthy();
    expect(presses).toHaveLength(0);
    const box = screen.getByRole("textbox", { name: /^message$/i });
    fireEvent.change(box, { target: { value: "hello" } });
    fireEvent.keyDown(box, { key: "Enter" });
    await waitFor(() => expect(presses.map((p) => p.tool)).toEqual(["open_dm", "post_message"]));
    expect(presses[0]!.args).toEqual({ to: ["bo"] });
    expect(presses[1]!.args).toMatchObject({ channel: "dm-1", body: "hello" });
    await waitFor(() => expect(location.hash).toContain("/chatlet/dm-1"));
  });

  test("holds a conversation to the engine's direct limit, the reader included", async () => {
    // EIGHT OTHERS ARE NINE PEOPLE, one past `MAX_DIRECT_PARTICIPANTS`: the
    // engine would refuse the conversation, so the message is held with why
    // and nobody more is offered.
    const crowd = Array.from({ length: MAX_DIRECT_PARTICIPANTS }, (_, n) => `p${n}`);
    mount(<NewMessageView />, `#/chatlet/new?to=${crowd.join(",")}`, {});
    expect(await screen.findByText(/That’s 9 people counting you/, { selector: "p" })).toBeTruthy();
    fireEvent.change(screen.getByRole("textbox", { name: /^message$/i }), {
      target: { value: "hello" },
    });
    await heldWith(
      screen.getByRole("button", { name: /^send$/i }),
      `That’s 9 people counting you, and a conversation holds at most ${MAX_DIRECT_PARTICIPANTS}. For more, start a room.`,
    );
    fireEvent.change(screen.getByRole("textbox", { name: /who the message is to/i }), {
      target: { value: "b" },
    });
    expect(screen.queryByText("Bo Chen")).toBeNull();
    expect(presses).toHaveLength(0);
  });
});
