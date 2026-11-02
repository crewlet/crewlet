/**
 * Chatlet's dialogs: each write drawn for every reader and held with why, and
 * what each sends.
 *
 * THE INVARIANTS: a dialog's change is never hidden from a reader who cannot
 * make it (`app/writeGate.test.tsx` names this suite for `Dialogs.tsx`); Add
 * people's field takes the first match on Enter and gives back the last chip
 * on Backspace, and a new room asks once before it is left with nobody in it;
 * a topic and a description are held to the engine's limit in CHARACTERS,
 * never in UTF-16 units, and only the field that moved travels; a template is
 * one change that names itself; the auto-join switch is an admin's, on a
 * public room only.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { MAX_TOPIC_CHARS } from "~/contract/chat.ts";
import { ADA, actEngine, applied, type Pressed } from "~/test/chat.tsx";
import { CHAT_ORG, SERVED, WRITE_READERS, heldWith } from "~/test/chatAccess.ts";

import { ChatDialogHost, characters } from "./Dialogs.tsx";
import { mountInFrame } from "./frameKit.tsx";
import type { ChatDialog } from "./nav.tsx";

const ROOM = "room-1";
const HASH = `#/chatlet/${ROOM}`;

const channel = (extra: Record<string, unknown> = {}) => ({
  v: 1,
  id: ROOM,
  kind: "public",
  name: "launch",
  created_at: "2026-10-01T00:00:00Z",
  created_by: "ada",
  ...extra,
});

const detail = (extra: Record<string, unknown> = {}) => ({
  ...SERVED,
  channel: channel(extra),
  revision: 1,
  message_seq: 3,
  member: true,
  members: [{ handle: "ada", joined_at: "2026-10-01T00:00:00Z" }],
});

/** The reader's rail: the room, with a message in it unless `fresh`. */
const rail = (fresh = false, extra: Record<string, unknown> = {}) => ({
  ...SERVED,
  read_state: true,
  channels: [
    {
      channel: channel(extra),
      revision: 1,
      unread: 0,
      mentions: 0,
      ...(fresh
        ? {}
        : {
            last: {
              message_id: "m-1",
              channel_seq: 1,
              at: "2026-10-01T00:00:00Z",
              position: SERVED.position,
            },
          }),
    },
  ],
});

const said = {
  message: {
    v: 1,
    id: "m-1",
    channel_id: ROOM,
    author: "bo",
    author_kind: "human",
    body: "morning",
    created_at: "2026-10-01T09:00:00Z",
  },
  channel_seq: 1,
  position: SERVED.position,
};

let presses: Pressed[];

beforeEach(() => {
  presses = actEngine((tool) => applied(tool, { channel: ROOM, added: ["bo"] }));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/** One dialog, open in the frame, over the room it is about. */
function open(
  dialog: ChatDialog,
  {
    viewer = ADA as Record<string, unknown>,
    fresh = false,
    room = {} as Record<string, unknown>,
  } = {},
) {
  const onClose = vi.fn();
  const mounted = mountInFrame(<ChatDialogHost dialog={dialog} onClose={onClose} />, {
    hash: HASH,
    viewer,
    answers: { chat_channel: detail(room), chat_channels: rail(fresh, room) },
  });
  act(() => mounted.store.applyOrg(CHAT_ORG as never));
  return { ...mounted, onClose };
}

describe("every dialog's write is drawn for every reader, held with why", () => {
  test.each(WRITE_READERS)("New room, for $who", async (reader) => {
    open({ kind: "new-room" }, { viewer: reader.viewer });
    fireEvent.change(await screen.findByLabelText(/^Name/), { target: { value: "launch" } });
    await heldWith(screen.getByRole("button", { name: /create room/i }), reader.reason);
  });

  test.each(WRITE_READERS)("Add people, for $who", async (reader) => {
    open({ kind: "people", room: ROOM }, { viewer: reader.viewer });
    fireEvent.click(await screen.findByRole("button", { name: /Bo Chen/ }));
    await heldWith(screen.getByRole("button", { name: /^add$/i }), reader.reason);
  });

  test.each(WRITE_READERS)("a topic, for $who", async (reader) => {
    open({ kind: "topic", room: ROOM }, { viewer: reader.viewer });
    fireEvent.change(await screen.findByRole("textbox"), { target: { value: "Shipping" } });
    await heldWith(screen.getByRole("button", { name: /^save$/i }), reader.reason);
  });

  test.each(WRITE_READERS)("a template, for $who", async (reader) => {
    open({ kind: "template", room: ROOM }, { viewer: reader.viewer });
    await heldWith(await screen.findByRole("button", { name: /use template/i }), reader.reason);
  });

  test.each(WRITE_READERS)("a delete, for $who", async (reader) => {
    open({ kind: "delete", channelID: ROOM, message: said as never }, { viewer: reader.viewer });
    await heldWith(await screen.findByRole("button", { name: /^delete$/i }), reader.reason);
  });
});

describe("Add people", () => {
  test("Enter takes the first match, Backspace gives back the last chip, Add sends the handles", async () => {
    const { onClose } = open({ kind: "people", room: ROOM });
    const field = await screen.findByRole("textbox", { name: /find people and agents/i });
    fireEvent.change(field, { target: { value: "bo" } });
    fireEvent.keyDown(field, { key: "Enter" });
    expect(await screen.findByRole("button", { name: /remove bo chen/i })).toBeTruthy();
    expect((field as HTMLInputElement).value).toBe("");

    fireEvent.keyDown(field, { key: "Backspace" });
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: /remove bo chen/i })).toBeNull(),
    );

    fireEvent.click(screen.getByRole("button", { name: /Ops Agent/ }));
    fireEvent.click(screen.getByRole("button", { name: /^add$/i }));
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]).toMatchObject({
      tool: "add_channel_members",
      args: { channel: ROOM, members: ["ops"] },
    });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });

  test("leaving a new room with nobody chosen asks once, and Skip for now leaves", async () => {
    const { onClose } = open({ kind: "people", room: ROOM }, { fresh: true });
    fireEvent.click(await screen.findByRole("button", { name: /skip for now/i }));
    const ask = await screen.findByRole("dialog", { name: /skip without adding anyone/i });
    expect(onClose).not.toHaveBeenCalled();
    fireEvent.click(within(ask).getByRole("button", { name: /skip for now/i }));
    expect(onClose).toHaveBeenCalled();
  });

  test("a room with messages in it is left without asking", async () => {
    const { onClose } = open({ kind: "people", room: ROOM });
    fireEvent.click(await screen.findByRole("button", { name: /^cancel$/i }));
    expect(onClose).toHaveBeenCalled();
    expect(screen.queryByText(/skip without adding anyone/i)).toBeNull();
  });

  test.each([
    ["an admin on a public room", ["chatlet", "admin"], "public", true],
    ["an admin on a private room", ["chatlet", "admin"], "private", false],
    ["a member who is not an admin", ["chatlet"], "public", false],
  ] as const)("the auto-join switch: %s sees it: %s", async (_, grants, kind, shown) => {
    open({ kind: "people", room: ROOM }, { viewer: { ...ADA, grants }, room: { kind } });
    await screen.findByRole("textbox", { name: /find people and agents/i });
    expect(!!screen.queryByRole("switch", { name: /when new agents and people join/i })).toBe(
      shown,
    );
  });

  test("the auto-join switch sends the room's flag", async () => {
    open({ kind: "people", room: ROOM }, { viewer: { ...ADA, grants: ["chatlet", "admin"] } });
    fireEvent.click(
      await screen.findByRole("switch", { name: /when new agents and people join/i }),
    );
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]).toMatchObject({
      tool: "update_channel",
      args: { channel: ROOM, auto_join: true },
    });
  });

  test("a team room's people are the org chart's, and nobody is added here", async () => {
    open({ kind: "people", room: ROOM }, { room: { kind: "unit", unit: "Platform" } });
    expect(await screen.findByText(/members come from the org chart/i)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^add$/i })).toBeNull();
  });
});

describe("a room's topic and description", () => {
  test("a character is a code point: an emoji counts once, as the engine counts it", () => {
    expect(characters("🚀🚀")).toBe(2);
    expect("🚀🚀".length).toBe(4);
  });

  test("past the limit in characters it is held, at the limit it is sent, and only the topic travels", async () => {
    open({ kind: "topic", room: ROOM });
    const box = await screen.findByRole("textbox");
    fireEvent.change(box, { target: { value: "🚀".repeat(MAX_TOPIC_CHARS + 1) } });
    await heldWith(screen.getByRole("button", { name: /^save$/i }), "Shorten it first.");
    fireEvent.change(box, { target: { value: "🚀".repeat(MAX_TOPIC_CHARS) } });
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]!.args).toEqual({ channel: ROOM, topic: "🚀".repeat(MAX_TOPIC_CHARS) });
  });

  test("an unchanged description is not sent", async () => {
    open({ kind: "description", room: ROOM }, { room: { purpose: "Release work" } });
    await heldWith(
      await screen.findByRole("button", { name: /^save$/i }),
      "Nothing has changed yet.",
    );
  });
});

describe("a template", () => {
  test("sets the topic and the description in one change that names the template", async () => {
    open({ kind: "template", room: ROOM });
    fireEvent.click(await screen.findByRole("button", { name: /use template/i }));
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]).toMatchObject({
      tool: "update_channel",
      args: { channel: ROOM, template: "Project room" },
    });
    expect(presses[0]!.args.topic).toBeTruthy();
    expect(presses[0]!.args.purpose).toBeTruthy();
  });
});

describe("a delete", () => {
  test("shows the message and deletes it", async () => {
    const { onClose } = open({ kind: "delete", channelID: ROOM, message: said as never });
    expect(await screen.findByText("morning")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /^delete$/i }));
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]).toMatchObject({
      tool: "delete_message",
      args: { channel: ROOM, message_id: "m-1" },
    });
    await waitFor(() => expect(onClose).toHaveBeenCalled());
  });
});

describe("New room", () => {
  test("claims the name as the engine spells it and opens the room it made", async () => {
    presses = actEngine((tool) => applied(tool, { channel: "room-new" }));
    open({ kind: "new-room" });
    fireEvent.change(await screen.findByLabelText(/^Name/), { target: { value: "Launch" } });
    fireEvent.click(screen.getByRole("checkbox", { name: /Bo Chen/ }));
    fireEvent.click(screen.getByRole("button", { name: /create room/i }));
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]).toMatchObject({
      tool: "create_channel",
      args: { channel: "launch", kind: "public", members: ["bo"] },
    });
    await waitFor(() => expect(location.hash).toContain("room-new"));
  });
});
