import { expect, test } from "vitest";
import { HELD_CLASS, heldItem, itemHold } from "./heldConversation.ts";

const held = {
  class: HELD_CLASS,
  subject: { kind: "seat", seat: "dev", name: "task:t-1" },
  since: "2026-01-01T10:00:00Z",
};

// A HOLD ON AN ITEM'S OWN THREAD IS THAT ITEM'S, and nothing else's.
test("an item's hold is read off the notices by its conversation", () => {
  expect(itemHold([held], "t-1")).toEqual(held);
  expect(itemHold([held], "t-2")).toBeUndefined();
  expect(itemHold([{ ...held, class: "alarm" }], "t-1")).toBeUndefined();
  expect(itemHold(undefined, "t-1")).toBeUndefined();
  expect(heldItem(held)).toBe("t-1");
  expect(
    heldItem({ ...held, subject: { kind: "seat", seat: "dev", name: "chat:room/1" } }),
  ).toBeUndefined();
});
