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
});
