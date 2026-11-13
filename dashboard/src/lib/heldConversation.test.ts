import { expect, test } from "vitest";
import {
  HELD_CLASS,
  HELD_OUT_OF_MEMORY,
  HELD_REQUEST_REFUSED,
  heldItem,
  heldReason,
  heldSetting,
  itemHold,
} from "./heldConversation.ts";

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

// A HOLD SAYS WHY BY ITS CAUSE, and what besides writing releases it: a context
// that did not fit is the models', work that ran out of memory is the
// sandbox's, a request the provider refused is the models' or the tool
// servers'. A hold that names no cause is the one the engine held first.
test("a hold is worded by its cause", () => {
  expect(heldReason(held)).toBe("its context did not fit its models");
  expect(heldSetting(held)).toBe("models");
  const oom = { ...held, cause: HELD_OUT_OF_MEMORY };
  expect(heldReason(oom)).toBe("its work ran out of its sandbox's memory");
  expect(heldSetting(oom)).toBe("sandbox");
  const refused = { ...held, cause: HELD_REQUEST_REFUSED };
  expect(heldReason(refused)).toBe("its models' provider refused the request its runs make");
  expect(heldSetting(refused)).toBe("models or tool servers");
  expect(heldReason({ ...held, cause: "context_exhausted" })).toBe(
    "its context did not fit its models",
  );
});
