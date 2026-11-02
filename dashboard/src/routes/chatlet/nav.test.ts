/**
 * Chatlet's addresses: every place and pane is spelled one way, and read back
 * as what it was written as.
 */

import { describe, expect, test } from "vitest";

import { parseHash } from "~/app/router.tsx";
import { CHAT_VIEWS, ID_SHAPE } from "~/app/routes.ts";

import {
  paneAfter,
  paneOf,
  paneParam,
  placeHref,
  placeOf,
  PANE_PARAM,
  samePlace,
  type ChatPane,
  type ChatPlace,
} from "./nav.tsx";

const ROOM = "0b0e9a8e-2f4c-4f7e-9d8a-5d1f0c6b7a10";
const OTHER = "6a3c1e2b-8d4f-4a9e-b7c6-1f2e3d4c5b6a";
const ROOT = "c1d2e3f4-a5b6-4c7d-8e9f-0a1b2c3d4e5f";

/** Read an href back the way the router and the screen do. */
function readBack(hash: string): { place: ChatPlace; pane: ChatPane | null } {
  const route = parseHash(hash);
  const [, first] = route.path;
  const where =
    first === undefined
      ? {}
      : ID_SHAPE.test(first)
        ? { id: first }
        : { view: CHAT_VIEWS.find((view) => view === first) };
  return { place: placeOf(where, route.query), pane: paneOf(route.query.get(PANE_PARAM)) };
}

describe("a place and its pane round-trip through the address", () => {
  const places: ChatPlace[] = [
    { kind: "room", id: ROOM },
    { kind: "room", id: ROOM, message: ROOT },
    { kind: "activity" },
    { kind: "threads" },
    { kind: "drafts" },
    { kind: "saved" },
    { kind: "search" },
    { kind: "search", q: "release notes", room: ROOM },
    { kind: "browse" },
    { kind: "new" },
    { kind: "new", to: ["ada", "pm"] },
  ];
  const panes: (ChatPane | null)[] = [
    null,
    { kind: "thread", room: ROOM, root: ROOT },
    { kind: "details", room: ROOM, tab: "members" },
    { kind: "profile", handle: "backend-engineer" },
    { kind: "catchup", room: ROOM, mode: "catchup" },
    { kind: "catchup", room: ROOM, mode: "summarize" },
  ];
  for (const place of places) {
    for (const pane of panes) {
      test(`${placeHref(place, pane)}`, () => {
        const back = readBack(placeHref(place, pane));
        expect(back.place).toEqual(place);
        expect(back.pane).toEqual(pane);
      });
    }
  }
});

// THE BARE `#/chatlet` IS NO PLACE OF ITS OWN: it is Activity, Chatlet's
// landing, said differently, or New message to the seat a `dm=` names, and the
// frame replaces it with that place's own spelling. Mutation: read the bare
// address as any other place and these go red.
describe("the bare address names the place it means", () => {
  test("Activity, Chatlet's landing", () => {
    expect(readBack("#/chatlet").place).toEqual({ kind: "activity" });
    expect(placeHref(readBack("#/chatlet").place)).toBe("#/chatlet/activity");
  });
  test("New message to the seat a dm= names", () => {
    expect(readBack("#/chatlet?dm=bo").place).toEqual({ kind: "new", to: ["bo"] });
  });
});

test("every view is a reserved word no room id can be", () => {
  for (const view of CHAT_VIEWS) expect(ID_SHAPE.test(view), view).toBe(false);
});

describe("a pane parameter that names nothing drawable is no pane", () => {
  for (const raw of [
    "",
    "thread",
    `thread:${ROOM}`,
    `details:${ROOM}:history`,
    "profile",
    "catchup",
    `sideways:${ROOM}`,
  ]) {
    test(JSON.stringify(raw), () => expect(paneOf(raw)).toBeNull());
  }
});

test("a pane's parameter is its kind first, so a reader of the address can tell", () => {
  expect(paneParam({ kind: "thread", room: ROOM, root: ROOT })).toBe(`thread:${ROOM}:${ROOT}`);
  expect(paneParam({ kind: "catchup", room: ROOM, mode: "summarize" })).toBe(`summarize:${ROOM}`);
});

describe("which pane survives a move", () => {
  const thread: ChatPane = { kind: "thread", room: ROOM, root: ROOT };
  const profile: ChatPane = { kind: "profile", handle: "pm" };

  test("a room's pane stays in its own room", () => {
    expect(paneAfter(thread, { kind: "room", id: ROOM })).toBe(thread);
  });

  test("a room's pane closes in another room and in every view", () => {
    // Mutation: return the pane unconditionally and both go red.
    expect(paneAfter(thread, { kind: "room", id: OTHER })).toBeNull();
    expect(paneAfter(thread, { kind: "threads" })).toBeNull();
  });

  test("a profile is nobody's room, so it stays wherever the reader goes", () => {
    expect(paneAfter(profile, { kind: "room", id: OTHER })).toBe(profile);
    expect(paneAfter(profile, { kind: "saved" })).toBe(profile);
  });
});

test("the same room with and without a flashed message is the same place", () => {
  expect(samePlace({ kind: "room", id: ROOM }, { kind: "room", id: ROOM, message: ROOT })).toBe(
    true,
  );
  expect(samePlace({ kind: "room", id: ROOM }, { kind: "room", id: OTHER })).toBe(false);
  expect(samePlace({ kind: "search", q: "a" }, { kind: "search", q: "b" })).toBe(false);
});
