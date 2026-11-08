/**
 * The message box, and the two things it must not decide on the engine's
 * behalf.
 *
 * WHAT A MESSAGE NAMES IS THE ENGINE'S READING OF IT: the tool reads the
 * handles and `@channel` off the body against the company's seats, so the box
 * sends what was typed and nothing about it. The `@` list only helps somebody
 * type a handle correctly.
 *
 * AND ITS KEYS ARE EVERY MESSAGE BOX'S: Enter sends, Shift+Enter is a new
 * line (`compose.send`), never mid composition and never an Enter the open
 * `@` list takes.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { keyRow } from "~/app/keymap.ts";
import { MAX_BODY_BYTES, MAX_MESSAGE_LINKS } from "~/contract/chat.ts";
import { PAGE_ADDRESS_PREFIX } from "~/contract/links.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { QUIET_MS } from "~/lib/useQuiet.ts";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import type { QueryName } from "~/protocol/index.ts";
import { ADA, actEngine, applied, mountChat, type Pressed } from "~/test/chat.tsx";
import { afterMicrotasks } from "~/test/settle.ts";

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

describe("what a message links to that the room previews", () => {
  // A RECORD OR A PULL REQUEST A PERSON LINKS IS PREVIEWED like one a seat
  // links: the composer sends this dashboard's records its words link to, as
  // whole addresses on it, and the github.com pull requests they name, once
  // each, and never a page that is not a record or any other address.
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

  test("take a github.com pull request pasted or linked, and no other address", async () => {
    mount();
    const field = await box();
    fireEvent.change(field, {
      target: {
        value: [
          "Review https://github.com/acme/api/pull/42 please,",
          "[the same one](https://github.com/acme/api/pull/42),",
          "not https://github.com/acme/api/issues/3, https://github.com/acme/api/pull/42/files,",
          "http://github.com/acme/api/pull/5 or https://example.com/acme/api/pull/6",
        ].join(" "),
      },
    });
    fireEvent.keyDown(field, { key: "Enter" });
    await waitFor(() => expect(posts()).toHaveLength(1));
    expect(posts()[0]!.args.links).toEqual(["https://github.com/acme/api/pull/42"]);
  });

  test("are as many as a message carries, and none for words alone", async () => {
    mount();
    const field = await box();
    const many = Array.from({ length: MAX_MESSAGE_LINKS + 2 }, (_, n) =>
      n % 2 === 0
        ? `[GP-${n + 1}](#/worklet/GP-${n + 1})`
        : `https://github.com/acme/api/pull/${n + 1}`,
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
    expect(keyRow("compose.send").presses).toEqual([{ key: "enter" }]);
    mount();
    const field = await box();
    fireEvent.change(field, { target: { value: "first line" } });
    fireEvent.keyDown(field, { key: "Enter", shiftKey: true });
    expect(posts()).toHaveLength(0);
    fireEvent.keyDown(field, { key: "Enter" });
    await waitFor(() => expect(posts()).toHaveLength(1));
  });

  test("an input method's Enter never sends, and the command key with Enter still does", async () => {
    mount();
    const field = await box();
    fireEvent.change(field, { target: { value: "안녕하세요" } });
    fireEvent.compositionStart(field);
    fireEvent.keyDown(field, { key: "Enter", isComposing: true });
    fireEvent.keyDown(field, { key: "Enter", keyCode: 229 });
    fireEvent.compositionEnd(field);
    expect(posts()).toHaveLength(0);
    fireEvent.keyDown(field, { key: "Enter", metaKey: true });
    await waitFor(() => expect(posts()).toHaveLength(1));
  });

  test("the hint under the box and the field's description name the same keys", async () => {
    mount();
    const field = await box();
    fireEvent.change(field, { target: { value: "draft" } });
    const hint = await waitFor(() => document.querySelector(".chat-composer-hint")!);
    expect(hint.textContent).toMatch(/to send, .*for a new line$/);
    const described = (field.getAttribute("aria-describedby") ?? "")
      .split(" ")
      .map((id) => document.getElementById(id)?.textContent);
    expect(described).toContain("Enter to send, Shift plus Enter for a new line");
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

  test("a narrow box folds the row into More formatting, which writes the same Markdown", async () => {
    mount();
    const field = await box();
    fireEvent.change(field, { target: { value: "step one" } });
    field.setSelectionRange(0, 8);
    // BOTH ARE DRAWN; the row's own width decides which shows, which jsdom
    // cannot measure, so the menu is driven directly.
    fireEvent.click(screen.getByRole("button", { name: "More formatting" }));
    const menu = await screen.findByRole("menu", { name: "More formatting" });
    expect(
      within(menu)
        .getAllByRole("menuitem")
        .map((item) => item.textContent),
    ).toEqual(["Link", "Numbered list", "Bulleted list", "Quote", "Code"]);
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Bulleted list" }));
    await waitFor(() => expect(field.value).toBe("- step one"));
  });

  test("the line-break hint shows only while the box holds words", async () => {
    mount();
    const field = await box();
    expect(document.querySelector(".chat-composer-hint")).toBeNull();
    fireEvent.change(field, { target: { value: "first line" } });
    expect(document.querySelector(".chat-composer-hint")?.textContent).toContain("for a new line");
    fireEvent.change(field, { target: { value: "" } });
    expect(document.querySelector(".chat-composer-hint")).toBeNull();
  });

  test("whether the formatting row shows is one choice, remembered", async () => {
    mount();
    await box();
    expect(screen.getByRole("toolbar", { name: "Formatting" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Formatting" }));
    expect(screen.queryByRole("toolbar", { name: "Formatting" })).toBeNull();
    expect(localStorage.getItem(STORAGE_KEYS.chatletFormatting)).toBe("0");
  });

  test("the list's first row rests until the arrows, and the first Down takes it rather than the next", async () => {
    mount();
    const field = await box();
    fireEvent.change(field, { target: { value: "@" } });
    field.setSelectionRange(1, 1);
    fireEvent.select(field);
    const list = await screen.findByRole("listbox", { name: "People and agents" });
    const menu = list.closest(".crewlet-mention-menu")!;
    const first = within(list).getAllByRole("option")[0]!;
    expect(menu.classList.contains("crewlet-mention-menu--resting")).toBe(true);
    fireEvent.keyDown(field, { key: "ArrowDown" });
    expect(menu.classList.contains("crewlet-mention-menu--resting")).toBe(false);
    expect(field.getAttribute("aria-activedescendant")).toBe(first.id);
    // Typing again filters, and the list rests again.
    fireEvent.change(field, { target: { value: "@c" } });
    field.setSelectionRange(2, 2);
    fireEvent.select(field);
    await waitFor(() =>
      expect(
        document
          .querySelector(".crewlet-mention-menu")
          ?.classList.contains("crewlet-mention-menu--resting"),
      ).toBe(true),
    );
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

describe("whom it will reach", () => {
  // What the line under the box says, which announces itself politely as it
  // changes; the line itself stands while the box is open.
  const said = () =>
    document.querySelector<HTMLElement>(".chat-composer-reach > :not(.chat-composer-hint)");
  const reachLine = () =>
    waitFor(() => {
      const line = said();
      expect(line).not.toBeNull();
      return line!;
    });

  // THE LINE IS KEPT WHILE THE BOX IS OPEN, said or not, as the activity line
  // keeps its own: a line that arrived once the typing settled moved the
  // conversation above the box. Mutation: draw the line only with a reach.
  test("keeps the line under the box while it is open, so nothing above moves for it", async () => {
    answers.chat_reach = {
      channel_id: "room-1",
      reach: { recipients: [{ handle: "cto", reason: "answerer" }] },
    };
    mount();
    const field = await box();
    const line = document.querySelector(".chat-composer-reach");
    expect(line).not.toBeNull();
    expect(said()).toBeNull();
    fireEvent.change(field, { target: { value: "where are we on the release?" } });
    await reachLine();
    expect(document.querySelector(".chat-composer-reach")).toBe(line);
  });

  // THE ENGINE'S ANSWER, NOT THE BOX'S: whom a draft reaches is the post's
  // own routing run dry (`chat_reach`), asked once the typing settles, so the
  // line names exactly whom Send will wake.
  test("names the agents the post will wake once the typing settles", async () => {
    answers.chat_reach = {
      channel_id: "room-1",
      reach: { recipients: [{ handle: "cto", reason: "answerer" }] },
    };
    mount();
    const field = await box();
    expect(said()).toBeNull();
    fireEvent.change(field, { target: { value: "where are we on the release?" } });
    const line = await reachLine();
    expect(line.getAttribute("role")).toBe("status");
    await waitFor(() => expect(line.textContent).toContain("Will reach cto"));
  });

  test("warns, and says why, when the post will reach no agent", async () => {
    answers.chat_reach = { channel_id: "room-1", reach: {}, quiet: "no_agent" };
    mount();
    const field = await box();
    fireEvent.change(field, { target: { value: "anyone around?" } });
    const line = await reachLine();
    await waitFor(() => expect(line.textContent).toContain("Will reach no agent"));
    expect(line.textContent).toContain("Mention one with @ to ask it.");
  });

  test("says nothing over an empty box", async () => {
    answers.chat_reach = {
      channel_id: "room-1",
      reach: { recipients: [{ handle: "cto", reason: "answerer" }] },
    };
    mount();
    const field = await box();
    // ON A FAKED CLOCK, past the quiet the box waits out before it asks, so
    // the absence is asserted where a reach line would have been drawn, and
    // never after a guess at the machine's load.
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    try {
      fireEvent.change(field, { target: { value: "   " } });
      act(() => {
        vi.advanceTimersByTime(QUIET_MS);
      });
      await afterMicrotasks();
      expect(said()).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });
});
