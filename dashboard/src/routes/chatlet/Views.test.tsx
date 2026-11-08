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

import { mountView } from "./frameKit.tsx";
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
  const mounted = mountView(children, {
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

  const asked = (socket: { query: { mock: { calls: unknown[][] } } }, what: string) =>
    socket.query.mock.calls.some(([name]) => name === what);
  const MESSAGE = { ...SERVED, channel_kind: "public", view: view("m-2", "bo") };

  // NOTHING OPEN ON ARRIVAL: the list is the whole view, and each item reads
  // as the message it is, in full, with why it is here under it.
  test("arrives with nothing open: the list is the whole view and each item reads in full", async () => {
    const { socket } = mount(<ActivityView />, "#/chatlet/activity", {
      chat_activity: FEED,
      chat_message: MESSAGE,
    });
    const row = await screen.findByRole("button", { name: /Mention in #launch/ });
    expect(within(row).getByText(/can you look\?/)).toBeTruthy();
    expect(document.querySelector(".crewlet-list-detail--alone")).not.toBeNull();
    expect(document.querySelector(".crewlet-list-detail__detail")).toBeNull();
    expect(row.querySelector(".crewlet-inbox-row__text--lines-all")).not.toBeNull();
    expect(row.getAttribute("aria-current")).toBeNull();
    // A DAY HEADS ITS ITEMS.
    expect(screen.getByRole("heading", { name: /2026|day|Thursday|Today|Yesterday/ })).toBeTruthy();
    // Nothing is read in place until something is opened.
    expect(asked(socket, "chat_message")).toBe(false);
    // A row is an item of a list, never an item of the navigation itself.
    expect(row.closest("li")?.parentElement?.tagName).toBe("UL");
  });

  test("opening an item splits the view beside it, clamps the list, marks it read and keeps it in the address", async () => {
    const { socket } = mount(<ActivityView />, "#/chatlet/activity", {
      chat_activity: FEED,
      chat_message: MESSAGE,
    });
    fireEvent.click(await screen.findByRole("button", { name: /Mention in #launch/ }));
    await waitFor(() => expect(location.hash).toContain("item=mention%3Am-2"));
    await waitFor(() =>
      expect(document.querySelector(".crewlet-list-detail__detail")).not.toBeNull(),
    );
    expect(document.querySelector(".crewlet-list-detail--half")).not.toBeNull();
    expect(document.querySelector(".crewlet-list-detail--alone")).toBeNull();
    const row = screen.getByRole("button", { name: /Mention in #launch/ });
    expect(row.getAttribute("aria-current")).toBe("true");
    expect(row.querySelector(".crewlet-inbox-row__text--lines-2")).not.toBeNull();
    await waitFor(() => expect(asked(socket, "chat_message")).toBe(true));
    const pane = document.querySelector<HTMLElement>(".chat-detail-pane")!;
    expect(within(pane).getByRole("button", { name: "Open in room" })).toBeTruthy();
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]).toMatchObject({ tool: "mark_chat_read", args: { cursors: { [ROOM]: 41 } } });
  });

  test("an address with an item opens it, as a reload does", async () => {
    mount(<ActivityView />, "#/chatlet/activity?item=mention%3Am-2", {
      chat_activity: FEED,
      chat_message: MESSAGE,
    });
    const row = await screen.findByRole("button", { name: /Mention in #launch/ });
    await waitFor(() => expect(row.getAttribute("aria-current")).toBe("true"));
    expect(document.querySelector(".crewlet-list-detail__detail")).not.toBeNull();
  });

  test("Escape and Close give the list the whole view back, with focus on the row", async () => {
    mount(<ActivityView />, "#/chatlet/activity?item=mention%3Am-2", {
      chat_activity: FEED,
      chat_message: MESSAGE,
    });
    const row = await screen.findByRole("button", { name: /Mention in #launch/ });
    await waitFor(() =>
      expect(document.querySelector(".crewlet-list-detail__detail")).not.toBeNull(),
    );
    row.focus();
    fireEvent.keyDown(row, { key: "Escape" });
    await waitFor(() => expect(location.hash).not.toContain("item="));
    await waitFor(() => expect(document.querySelector(".crewlet-list-detail__detail")).toBeNull());
    await waitFor(() =>
      expect(document.activeElement).toBe(
        screen.getByRole("button", { name: /Mention in #launch/ }),
      ),
    );

    fireEvent.click(screen.getByRole("button", { name: /Mention in #launch/ }));
    const back = await screen.findByRole("button", { name: "Back to Activity" });
    fireEvent.click(back);
    await waitFor(() => expect(document.querySelector(".crewlet-list-detail__detail")).toBeNull());
  });

  test("an item in a thread opens the thread pane a room uses", async () => {
    const { socket } = mount(<ActivityView />, "#/chatlet/activity", {
      chat_activity: {
        ...FEED,
        items: [{ ...item, key: "t:m-3", kind: "reply", message_id: "m-3", thread_root: "m-1" }],
      },
      chat_thread: { ...SERVED, root: view("m-1", "ada"), replies: [view("m-3", "bo")] },
    });
    fireEvent.click(await screen.findByRole("button", { name: /Reply in #launch thread/ }));
    expect(await screen.findByRole("heading", { name: "Thread" })).toBeTruthy();
    await waitFor(() => expect(asked(socket, "chat_thread")).toBe(true));
    expect(asked(socket, "chat_message")).toBe(false);
  });

  // A CONVERSATION OF THE READER AND THE ACTOR ALONE IS NOT NAMED: its title
  // is the actor's own name, which the row has just said. A group
  // conversation and a named room still are.
  test("the context line names the room, except a conversation of the reader and the actor alone", async () => {
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
      emoji: ":+1:",
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
    });
    const contexts = async () =>
      (await screen.findAllByText(/^(Reply|Mention|Reaction) in /)).map((el) => el.textContent);
    await waitFor(async () => expect(await contexts()).toHaveLength(5));
    expect(await contexts()).toEqual([
      "Reply in a direct message",
      "Mention in a direct message",
      "Reaction in a direct message",
      "Reply in Bo Chen, Ops Agent",
      "Reply in #launch",
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

  // THE LAST CARD'S PRESS HANDS FOCUS TO THE VIEW'S TITLE, which stands in the
  // top bar, outside the view. Mutation: look the title up inside the view.
  test("Unfollow on the last thread hands focus to the title in the top bar", async () => {
    mount(<ThreadsView />, "#/chatlet/threads", { chat_threads: THREADS });
    const unfollow = await screen.findByRole("button", { name: /^unfollow$/i });
    act(() => unfollow.focus());
    fireEvent.click(unfollow);
    const bar = screen.getByRole("navigation", { name: "Chatlet" });
    const title = within(bar).getByRole("heading", { level: 1, name: "Threads" });
    await waitFor(() => expect(document.activeElement).toBe(title));
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

  test("is titled as the Rooms panel and the breadcrumb name it", async () => {
    mount(<DraftsView />, "#/chatlet/drafts", { chat_sent: { ...SERVED, messages: [] } });
    expect(await screen.findByRole("heading", { level: 1, name: "Drafts and sent" })).toBeTruthy();
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

  // THE READER IS THE AUTHOR OF EVERY SENT CARD, so a card is headed by where
  // the message went. Mutation: head it by the room's title again and this
  // goes red with "Bo Chen", the other person's name, as its head.
  test("Sent heads each message by where it went: to whom, or in which room", async () => {
    const dm = {
      ...view("m-6", "ada"),
      message: { ...view("m-6", "ada").message, channel_id: "dm-1" },
    };
    mount(<DraftsView />, "#/chatlet/drafts", {
      chat_channels: {
        ...RAIL,
        channels: [
          {
            channel: { v: 1, id: "dm-1", kind: "dm", created_at: "2026-10-01T00:00:00Z" },
            participants: ["ada", "bo"],
            unread: 0,
            mentions: 0,
          },
        ],
      },
      chat_sent: {
        ...SERVED,
        messages: [
          { channel_kind: "dm", view: dm },
          { channel_name: "launch", channel_kind: "public", view: view("m-5", "ada") },
        ],
      },
    });
    fireEvent.click(await screen.findByRole("tab", { name: /^sent/i }));
    expect(await screen.findByText("To Bo Chen")).toBeTruthy();
    expect(screen.getByText("In #launch")).toBeTruthy();
    expect(screen.queryByText("Bo Chen")).toBeNull();
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
