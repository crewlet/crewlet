/**
 * Where a copied link to a message opens. A room page holds the room's own
 * posts and never a reply, so a link that named a reply in the room read the
 * room back to its start looking for it and landed nowhere.
 */
import { expect, test } from "vitest";

import { messageAddress } from "./MessageRow.tsx";
import { placeHref } from "./nav.tsx";

test("a room post links to the room at the post", () => {
  const { place, pane } = messageAddress("room-1", { id: "m-1", thread_root: "" });
  expect(placeHref(place, pane)).toBe(placeHref({ kind: "room", id: "room-1", message: "m-1" }));
});

test("a reply links to the room at its thread's root, with the thread open", () => {
  const { place, pane } = messageAddress("room-1", { id: "m-9", thread_root: "m-1" });
  expect(place).toEqual({ kind: "room", id: "room-1", message: "m-1" });
  expect(pane).toEqual({ kind: "thread", room: "room-1", root: "m-1" });
  expect(placeHref(place, pane)).not.toContain("m-9");
});
