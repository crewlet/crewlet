/**
 * What a room is for, edited from the room.
 *
 * TWO RULES ARE THE ENGINE'S AND ARE MIRRORED HERE SO A PERSON HEARS THEM
 * FIRST: an empty patch is refused — it would be a record on the log, a
 * history row and a wake for a change nobody made — and a topic past its cap
 * is refused rather than cut, because cutting somebody's sentence to fit is
 * losing what they wrote.
 *
 * AND ONE IS A CORRECTNESS RULE ABOUT WHAT IS NOT ON THE FORM: a direct
 * conversation's id IS its participant set, so adding somebody does not widen
 * it — it names a different room. A membership editor here would be a control
 * whose only possible outcome is a refusal.
 */

import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { MAX_TOPIC_CHARS } from "~/contract/chat.ts";
import {
  actEngine,
  applied,
  mountChat,
  pending,
  type ActReply,
  type Pressed,
} from "~/test/chat.tsx";
import { RoomSettings } from "./RoomSettings.tsx";
import type { ChatChannel } from "~/protocol/index.ts";

const channel = (over: Partial<ChatChannel> = {}): ChatChannel => ({
  v: 1,
  id: "room-1",
  kind: "public",
  name: "general",
  topic: "Shipping",
  purpose: "Everything about the release",
  created_at: "2026-01-01T00:00:00Z",
  ...over,
});

let reply: ActReply;
let presses: Pressed[];

beforeEach(() => {
  reply = applied("update_channel", { channel: "room-1" });
  presses = actEngine(() => reply);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function mount(room = channel(), member = true) {
  const onClose = vi.fn();
  mountChat(<RoomSettings channel={room} member={member} onClose={onClose} />);
  return { onClose };
}

const save = () => screen.getByRole("button", { name: /^save$/i });
/** Held, with the sentence that says why — the kit keeps a held button focusable. */
const held = () => save().getAttribute("aria-disabled") === "true";
/** Wait for the viewer read, which is what lets a press be sent at all. */
const pressable = () => waitFor(() => expect(held()).toBe(false));
const topic = () => screen.getByLabelText(/^Topic/);
const purpose = () => screen.getByLabelText(/^Purpose/);

test("a form nobody has changed has nothing to publish", async () => {
  // The engine refuses a patch that sets no field, so a Save that sent one
  // would be a button whose only outcome is a refusal.
  mount();
  expect(held()).toBe(true);
  fireEvent.click(save());
  await new Promise((r) => setTimeout(r, 20));
  expect(presses).toHaveLength(0);
});

test("only the field that moved travels", async () => {
  // Every field on a patch is absent-means-unchanged, which is the only shape
  // that tells "set this to empty" from "leave it alone" — so sending a field
  // nobody touched would be this dialog asserting a value it merely read.
  const { onClose } = mount();
  fireEvent.change(topic(), { target: { value: "Shipping October" } });
  await pressable();
  fireEvent.click(save());

  await waitFor(() => expect(presses).toHaveLength(1));
  expect(presses[0]!.tool).toBe("update_channel");
  expect(presses[0]!.args).toEqual({ channel: "room-1", topic: "Shipping October" });
  await waitFor(() => expect(onClose).toHaveBeenCalled());
});

test("emptying a field is a change rather than a field left out", async () => {
  mount();
  fireEvent.change(purpose(), { target: { value: "" } });
  await pressable();
  fireEvent.click(save());

  await waitFor(() => expect(presses).toHaveLength(1));
  expect(presses[0]!.args).toEqual({ channel: "room-1", purpose: "" });
});

test("a topic past its cap is refused here rather than cut to fit", async () => {
  mount();
  fireEvent.change(topic(), { target: { value: "x".repeat(MAX_TOPIC_CHARS + 1) } });
  expect(screen.getByText(/257 bytes, but a topic holds at most 256/i)).toBeTruthy();
  expect(held()).toBe(true);
  fireEvent.click(save());
  await new Promise((r) => setTimeout(r, 20));
  expect(presses).toHaveLength(0);
});

test("the cap is bytes, so a multi-byte character counts as what it costs", async () => {
  // The engine bounds the record's own field. Counted in characters, a topic
  // of emoji would pass here and be refused there, naming a number the person
  // cannot see.
  mount();
  fireEvent.change(topic(), { target: { value: "🙂".repeat(MAX_TOPIC_CHARS / 4) } });
  await pressable();
  fireEvent.change(topic(), { target: { value: "🙂".repeat(MAX_TOPIC_CHARS / 4 + 1) } });
  expect(held()).toBe(true);
});

test("a direct conversation is not a place to add somebody", () => {
  // Its id IS the people in it, so this form offers no membership at all and
  // says where that gesture lives instead.
  mount(channel({ kind: "dm", name: "" }), true);
  expect(screen.getByText(/different conversation/i)).toBeTruthy();
  expect(screen.queryByRole("combobox")).toBeNull();
});

test("an outcome this node has not applied is not drawn as saved", async () => {
  reply = pending("update_channel", { channel: "room-1" });
  const { onClose } = mount();
  fireEvent.change(topic(), { target: { value: "Shipping October" } });
  await pressable();
  fireEvent.click(save());

  await waitFor(() => expect(screen.getByText(/not shown here yet/i)).toBeTruthy());
  // THE DIALOG STAYS, because closing it would be the screen saying the change
  // is on the room when this node cannot yet read it back.
  expect(onClose).not.toHaveBeenCalled();
});
