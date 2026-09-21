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

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { MAX_TOPIC_BYTES, RoomSettings } from "./RoomSettings.tsx";
import { patchChannel } from "./writes.ts";
import type { WriteResult } from "./writes.ts";
import type { ChatChannel } from "~/protocol/index.ts";

vi.mock("./writes.ts", () => ({ patchChannel: vi.fn() }));

const patched = vi.mocked(patchChannel);
const position = { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 9 };

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

const applied: WriteResult = {
  outcome: "applied",
  answer: { outcome: "applied", op_id: "op-1", position, revision: 4 },
  detail: "",
  code: "",
};

beforeEach(() => patched.mockReset());
afterEach(cleanup);

function mount(room = channel(), member = true) {
  const onClose = vi.fn();
  const onWrote = vi.fn();
  render(<RoomSettings channel={room} member={member} onClose={onClose} onWrote={onWrote} />);
  return { onClose, onWrote };
}

const save = () => screen.getByRole("button", { name: /^save$/i });
const topic = () => screen.getByLabelText(/^Topic/);
const purpose = () => screen.getByLabelText(/^Purpose/);

test("a form nobody has changed has nothing to publish", () => {
  // The engine refuses a patch that sets no field, so a Save that sent one
  // would be a button whose only outcome is a refusal.
  mount();
  expect(save().hasAttribute("disabled")).toBe(true);
  fireEvent.click(save());
  expect(patched).not.toHaveBeenCalled();
});

test("only the field that moved travels", async () => {
  // Every field on a patch is absent-means-unchanged, which is the only shape
  // that tells "set this to empty" from "leave it alone" — so sending a field
  // nobody touched would be this dialog asserting a value it merely read.
  patched.mockResolvedValue(applied);
  const { onWrote, onClose } = mount();
  fireEvent.change(topic(), { target: { value: "Shipping October" } });
  fireEvent.click(save());

  await waitFor(() => expect(patched).toHaveBeenCalled());
  expect(patched.mock.calls[0]![1]).toEqual({ topic: "Shipping October" });
  expect(onWrote).toHaveBeenCalled();
  expect(onClose).toHaveBeenCalled();
});

test("emptying a field is a change rather than a field left out", async () => {
  patched.mockResolvedValue(applied);
  mount();
  fireEvent.change(purpose(), { target: { value: "" } });
  fireEvent.click(save());

  await waitFor(() => expect(patched).toHaveBeenCalled());
  expect(patched.mock.calls[0]![1]).toEqual({ purpose: "" });
});

test("a topic past its cap is refused here rather than cut to fit", () => {
  mount();
  fireEvent.change(topic(), { target: { value: "x".repeat(MAX_TOPIC_BYTES + 1) } });
  expect(screen.getByText(/against a cap of 256/i)).toBeTruthy();
  expect(save().hasAttribute("disabled")).toBe(true);
  fireEvent.click(save());
  expect(patched).not.toHaveBeenCalled();
});

test("the cap is bytes, so a multi-byte character counts as what it costs", () => {
  // The engine bounds the record's own field. Counted in characters, a topic
  // of emoji would pass here and be refused there, naming a number the person
  // cannot see.
  mount();
  fireEvent.change(topic(), { target: { value: "🙂".repeat(MAX_TOPIC_BYTES / 4) } });
  expect(save().hasAttribute("disabled")).toBe(false);
  fireEvent.change(topic(), { target: { value: "🙂".repeat(MAX_TOPIC_BYTES / 4 + 1) } });
  expect(save().hasAttribute("disabled")).toBe(true);
});

test("a direct conversation is not a place to add somebody", () => {
  // Its id IS the people in it, so this form offers no membership at all and
  // says where that gesture lives instead.
  mount(channel({ kind: "dm", name: "" }), true);
  expect(screen.getByText(/different conversation/i)).toBeTruthy();
  expect(screen.queryByRole("combobox")).toBeNull();
});

test("an outcome this node has not applied is not drawn as saved", async () => {
  patched.mockResolvedValue({
    outcome: "pending",
    answer: { outcome: "pending", op_id: "op-1", position, revision: 4 },
    detail: "",
    code: "",
  });
  const { onClose } = mount();
  fireEvent.change(topic(), { target: { value: "Shipping October" } });
  fireEvent.click(save());

  await waitFor(() => expect(screen.getByText(/has not applied it yet/i)).toBeTruthy());
  // THE DIALOG STAYS, because closing it would be the screen saying the change
  // is on the room when this node cannot yet read it back.
  expect(onClose).not.toHaveBeenCalled();
});
