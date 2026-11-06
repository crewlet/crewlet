/**
 * Browse rooms: every room the reader may read, from the engine's one answer,
 * filtered and sorted here, with Join only where the engine would take one.
 *
 * THE INVARIANTS: the list is the engine's (`chat_rooms`), never a guess; a
 * quiet room never sorts ahead of a busy one for lack of an instant; Join is
 * offered on a public room the reader is not in and on nothing else, and is
 * drawn for every reader with why it is held (`app/writeGate.test.tsx` names
 * this suite for `Browse.tsx`); a join that lands takes the reader in, and
 * one nobody can establish does not.
 */

import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { actEngine, applied, unknown, type Pressed } from "~/test/chat.tsx";
import { SERVED, WRITE_READERS, heldWith } from "~/test/chatAccess.ts";
import type { ChatDirectoryRoom } from "~/protocol/index.ts";

import { BrowseView, keptByKind, matchesRoom } from "./Browse.tsx";
import { mountInFrame } from "./frameKit.tsx";

const room = (
  id: string,
  name: string,
  extra: Partial<ChatDirectoryRoom> & {
    kind?: string;
    topic?: string;
    unit?: string;
    archived_at?: string;
  } = {},
): ChatDirectoryRoom => {
  const { kind = "public", topic, unit, archived_at, ...rest } = extra;
  return {
    channel: {
      v: 1,
      id,
      kind,
      name,
      created_at: "2026-10-01T00:00:00Z",
      ...(topic ? { topic } : {}),
      ...(unit ? { unit } : {}),
      ...(archived_at ? { archived_at } : {}),
    },
    members: 3,
    ...rest,
  } as ChatDirectoryRoom;
};

const ROOMS = [
  room("r-launch", "launch", {
    topic: "Shipping October",
    members: 5,
    last_at: "2026-10-30T09:00:00Z",
  }),
  room("r-quiet", "quiet", { members: 9 }),
  room("r-platform", "platform", {
    kind: "unit",
    unit: "Platform",
    last_at: "2026-10-29T09:00:00Z",
  }),
  room("r-mine", "mine", { member: true, last_at: "2026-10-30T10:00:00Z" }),
  room("r-old", "old", { archived_at: "2026-09-01T00:00:00Z" }),
];

const ANSWER = { ...SERVED, rooms: ROOMS };

let presses: Pressed[];

beforeEach(() => {
  presses = actEngine((tool) => applied(tool, { channel: "r-launch" }));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

const mount = (viewer?: Record<string, unknown>) =>
  mountInFrame(<BrowseView />, {
    hash: "#/chatlet/browse",
    answers: { chat_rooms: ANSWER },
    ...(viewer ? { viewer } : {}),
  });

describe("what is listed", () => {
  // THE ORDER IS THE GRID'S HEADS NOW, over rows handed in by name, so a tie
  // under any head keeps the name order.
  const names = () =>
    [...document.querySelectorAll<HTMLElement>(".crewlet-data-grid__row")].map(
      (row) => row.querySelector(".truncate")?.textContent,
    );

  test("the most recent activity first, a quiet room after every room somebody spoke in", async () => {
    mount();
    await screen.findByText("launch");
    expect(names()).toEqual(["mine", "launch", "platform", "quiet"]);
    const head = screen.getByRole("columnheader", { name: /Last message/ });
    expect(head.getAttribute("aria-sort")).toBe("descending");
  });

  test("by members from its head, the largest first, ties by name", async () => {
    mount();
    await screen.findByText("launch");
    // Ascending first, then descending: the kit's press cycle from no sort.
    const press = () =>
      fireEvent.click(
        within(screen.getByRole("columnheader", { name: /Members/ })).getByRole("button"),
      );
    press();
    if (
      screen.getByRole("columnheader", { name: /Members/ }).getAttribute("aria-sort") !==
      "descending"
    )
      press();
    expect(names()).toEqual(["quiet", "launch", "mine", "platform"]);
  });

  test("archived rooms are their own filter, and kept out of every other", () => {
    expect(ROOMS.filter((r) => keptByKind("archived", r)).map((r) => r.channel.name)).toEqual([
      "old",
    ]);
    expect(ROOMS.filter((r) => keptByKind("all", r)).map((r) => r.channel.name)).not.toContain(
      "old",
    );
    expect(ROOMS.filter((r) => keptByKind("unit", r)).map((r) => r.channel.name)).toEqual([
      "platform",
    ]);
  });

  test("a room is found by its name or its topic", () => {
    expect(matchesRoom(ROOMS[0]!, "october")).toBe(true);
    expect(matchesRoom(ROOMS[0]!, "LAUNCH")).toBe(true);
    expect(matchesRoom(ROOMS[0]!, "design")).toBe(false);
  });

  test("draws each room with its size and last message, and Joined on the reader's own", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Rooms" });
    expect(within(table).getByText("launch")).toBeTruthy();
    expect(within(table).getByText("Shipping October")).toBeTruthy();
    const launch = within(table).getByText("launch").closest<HTMLElement>('[role="row"]')!;
    expect(within(launch).getByText("5")).toBeTruthy();
    const mine = within(table).getByText("mine").closest<HTMLElement>('[role="row"]')!;
    expect(within(mine).getByText("Joined")).toBeTruthy();
    expect(within(table).queryByText("old")).toBeNull();
  });
});

describe("Join", () => {
  test("is offered on a public room the reader is not in, and nowhere else", async () => {
    mount();
    const row = async (name: string) =>
      (await screen.findByText(name)).closest<HTMLElement>('[role="row"]')!;
    expect(within(await row("launch")).getByRole("button", { name: /^join$/i })).toBeTruthy();
    expect(within(await row("mine")).queryByRole("button", { name: /^join$/i })).toBeNull();
    // A UNIT'S ROOM IS THE ORG CHART'S: Open, and the sentence that says so.
    const platform = await row("platform");
    expect(within(platform).queryByRole("button", { name: /^join$/i })).toBeNull();
    // ON THE LINE UNDER THE ROW, which describes it.
    const note = document.getElementById(`${platform.id}-accessory`)!;
    expect(within(note).getByText(/set in the org chart/)).toBeTruthy();
  });

  test.each(WRITE_READERS)("is drawn for $who, held with why", async (reader) => {
    mount(reader.viewer);
    const launch = (await screen.findByText("launch")).closest<HTMLElement>('[role="row"]')!;
    await heldWith(within(launch).getByRole("button", { name: /^join$/i }), reader.reason);
  });

  test("a join that lands takes the reader into the room", async () => {
    mount();
    const launch = (await screen.findByText("launch")).closest<HTMLElement>('[role="row"]')!;
    fireEvent.click(within(launch).getByRole("button", { name: /^join$/i }));
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]).toMatchObject({ tool: "join_channel", args: { channel: "r-launch" } });
    await waitFor(() => expect(location.hash).toContain("/chatlet/r-launch"));
  });

  test("a join nobody can establish does not walk into the room, and says how to ask again", async () => {
    presses = actEngine((tool) => unknown(tool));
    mount();
    const launch = (await screen.findByText("launch")).closest<HTMLElement>('[role="row"]')!;
    fireEvent.click(within(launch).getByRole("button", { name: /^join$/i }));
    expect(await screen.findByText(/Couldn’t confirm you joined/)).toBeTruthy();
    expect(location.hash).not.toContain("/chatlet/r-launch");
  });
});
