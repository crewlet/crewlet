/**
 * The message box, and the two things it must not decide on the engine's
 * behalf.
 *
 * WHAT A MESSAGE NAMES IS THE ENGINE'S READING OF IT: the tool reads the
 * handles and `@channel` off the body against the company's seats, so the box
 * sends what was typed and nothing about it. The `@` list only helps somebody
 * type a handle correctly.
 *
 * AND ITS KEYS ARE THE KEYMAP'S: a chat message sends on Enter with Shift for
 * a new line (`chatbox.send`), while a reply in the Inbox keeps the
 * command key (`compose.send`).
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";

import { keyRow } from "~/app/keymap.ts";
import { MAX_BODY_BYTES, MAX_MESSAGE_LINKS } from "~/contract/chat.ts";
import { PAGE_ADDRESS_PREFIX } from "~/contract/links.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import type { QueryName } from "~/protocol/index.ts";
import { ADA, actEngine, applied, mountChat, type Pressed } from "~/test/chat.tsx";

import { SHORTEN_FIRST, WRITE_FIRST } from "./Composer.tsx";
import { ReadCursors } from "./cursor.ts";
import { LoneRoom } from "./testkit.tsx";

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 40 },
};

const channel = {
  v: 1,
  id: "room-1",
  kind: "public",
  name: "general",
  created_at: "2026-01-01T00:00:00Z",
};

function message(seq: number, over: Record<string, unknown> = {}) {
  return {
    message: {
      v: 1,
      id: `m-${seq}`,
      channel_id: "room-1",
      author: "bo",
      author_kind: "agent",
      body: `line ${seq}`,
      created_at: "2026-01-01T00:00:00Z",
      ...over,
    },
    channel_seq: seq,
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
  };
}

let answers: Partial<Record<QueryName, unknown>>;
let presses: Pressed[];

beforeEach(() => {
  presses = actEngine((tool) => applied(tool, { channel: "room-1" }));
  answers = {
    chat_channel: { ...served, channel, revision: 3, message_seq: 2, member: true, members: [] },
    chat_messages: { ...served, channel_id: "room-1", messages: [message(2), message(1)] },
  };
});

afterEach(() => {
  cleanup();
  localStorage.clear();
  location.hash = "#/";
});

function mount() {
  const cursors = new ReadCursors({ flush: async () => undefined });
  return mountChat(
    <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
    { answers, viewer: ADA },
  );
}

async function box(): Promise<HTMLTextAreaElement> {
  const el = (await screen.findByLabelText("Message #general")) as HTMLTextAreaElement;
  // SEND OPENS ONCE THE VIEWER READ SAYS WHO IS WRITING.
  await waitFor(() =>
    expect(screen.getByRole("button", { name: /^send$/i }).getAttribute("aria-disabled")).toBe(
      "true",
    ),
  );
  return el;
}

const posts = () => presses.filter((press) => press.tool === "post_message");

describe("the records a message links to", () => {
  // A RECORD A PERSON LINKS IS PREVIEWED like one a seat links: the composer
  // sends the records its words link to as the message's links, whole
  // addresses on this dashboard, once each, and never a page that is not a
  // record or an address somewhere else.
  const whole = (hash: string) => new URL(hash, location.href).href;
  const artifact = "0b0e9a8e-2f4c-4f7e-9d8a-5d1f0c6b7a10";
  const page = "6a3c1e2b-8d4f-4a9e-b7c6-1f2e3d4c5b6a";

  test("are sent as its links", async () => {
    mount();
    const field = await box();
    fireEvent.change(field, {
      target: {
        value: [
          "[GP-12 Rotate the tokens](#/worklet/GP-12) and",
          `[the plan](${PAGE_ADDRESS_PREFIX}${page}), [the deck](#/artifacts/${artifact}),`,
          "[GP-12 again](#/worklet/GP-12), [the room](#/chatlet/browse)",
          "and [elsewhere](https://example.com/a)",
        ].join(" "),
      },
    });
    fireEvent.keyDown(field, { key: "Enter" });
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]!.args.links).toEqual([
      whole("#/worklet/GP-12"),
      whole(`${PAGE_ADDRESS_PREFIX}${page}`),
      whole(`#/artifacts/${artifact}`),
    ]);
  });

  test("are as many as a message carries, and none for words alone", async () => {
    mount();
    const field = await box();
    const many = Array.from(
      { length: MAX_MESSAGE_LINKS + 2 },
      (_, n) => `[GP-${n + 1}](#/worklet/GP-${n + 1})`,
    );
    fireEvent.change(field, { target: { value: many.join(" ") } });
    fireEvent.keyDown(field, { key: "Enter" });
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]!.args.links).toHaveLength(MAX_MESSAGE_LINKS);

    fireEvent.change(field, { target: { value: "just words" } });
    fireEvent.keyDown(field, { key: "Enter" });
    await waitFor(() => expect(posts()).toHaveLength(2));
    expect(posts()[1]!.args).not.toHaveProperty("links");
  });
});

describe("saying it", () => {
  test("Enter sends a chat message and Shift and Enter is a new line", async () => {
    expect(keyRow("chatbox.send").presses).toEqual([{ key: "enter" }]);
    // A LETTER IN THE INBOX KEEPS THE COMMAND KEY.
    expect(keyRow("compose.send").presses).toEqual([{ key: "enter", mod: true }]);
    mount();
    const field = await box();
    fireEvent.change(field, { target: { value: "first line" } });
    fireEvent.keyDown(field, { key: "Enter", shiftKey: true });
    expect(posts()).toHaveLength(0);
    fireEvent.keyDown(field, { key: "Enter" });
    await waitFor(() => expect(posts()).toHaveLength(1));
  });

  test("what was typed is what is sent, mentions and all, for the engine to read", async () => {
    mount();
    const field = await box();
    fireEvent.change(field, { target: { value: "  ship it @bo and @channel  " } });
    fireEvent.keyDown(field, { key: "Enter" });
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]!.args).toEqual({ channel: "room-1", body: "ship it @bo and @channel" });
    // THE BOX EMPTIES for the next message.
    expect(field.value).toBe("");
  });

  test("an empty message is no message, and Send says so", async () => {
    mount();
    const field = await box();
    fireEvent.keyDown(field, { key: "Enter" });
    expect(posts()).toHaveLength(0);
    expect(screen.getByRole("button", { name: /^send$/i }).getAttribute("title") ?? "").toContain(
      WRITE_FIRST.slice(0, 5),
    );
  });

  test("a body over the cap is held with why rather than cut to fit", async () => {
    mount();
    const field = await box();
    fireEvent.change(field, { target: { value: "x".repeat(MAX_BODY_BYTES + 1) } });
    fireEvent.keyDown(field, { key: "Enter" });
    expect(posts()).toHaveLength(0);
    expect(document.body.textContent).toContain(SHORTEN_FIRST.slice(0, 7));
  });

  test("a browser that cannot send holds Send with why and leaves the box open", async () => {
    const { store } = mount();
    const field = await box();
    act(() => store.setConnected(false));
    fireEvent.change(field, { target: { value: "while offline" } });
    await waitFor(() =>
      expect(document.querySelector(".chat-composer-held")?.textContent).toBe(
        WRITE_REASONS.offline,
      ),
    );
    expect(field.disabled).toBe(false);
    fireEvent.keyDown(field, { key: "Enter" });
    expect(posts()).toHaveLength(0);
  });
});

describe("writing it", () => {
  test("the formatting row writes Markdown round the selection, by the kit's rules", async () => {
    mount();
    const field = await box();
    fireEvent.change(field, { target: { value: "ship it" } });
    field.setSelectionRange(0, 4);
    fireEvent.click(
      within(screen.getByRole("toolbar", { name: "Formatting" })).getByLabelText("Bold"),
    );
    await waitFor(() => expect(field.value).toBe("**ship** it"));
  });

  test("whether the formatting row shows is one choice, remembered", async () => {
    mount();
    await box();
    expect(screen.getByRole("toolbar", { name: "Formatting" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Formatting" }));
    expect(screen.queryByRole("toolbar", { name: "Formatting" })).toBeNull();
    expect(localStorage.getItem(STORAGE_KEYS.chatletFormatting)).toBe("0");
  });

  test("@ lists the company's seats, and Enter writes the handle the engine reads", async () => {
    mount();
    const field = await box();
    fireEvent.change(field, { target: { value: "@" } });
    field.setSelectionRange(1, 1);
    fireEvent.select(field);
    const list = await screen.findByRole("listbox", { name: "People and agents" });
    expect(within(list).getAllByRole("option").length).toBeGreaterThan(0);
    // @channel is there for a room, the whole room by its one handle.
    expect(within(list).getByText("@channel")).toBeTruthy();
    fireEvent.keyDown(field, { key: "Enter" });
    expect(posts()).toHaveLength(0);
    await waitFor(() => expect(field.value).toMatch(/^@[a-z0-9-]+ $/));
  });

  test("Up in an empty box opens the reader's last message for a rewrite", async () => {
    answers.chat_messages = {
      ...served,
      channel_id: "room-1",
      messages: [message(3, { author: "ada", author_kind: "human", body: "mine" }), message(2)],
    };
    mount();
    const field = await box();
    fireEvent.keyDown(field, { key: "ArrowUp" });
    expect(((await screen.findByLabelText("Edit your message")) as HTMLTextAreaElement).value).toBe(
      "mine",
    );
  });
});
