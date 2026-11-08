/**
 * Direct messages: every conversation the reader is in, newest first, from the
 * frame's own rooms, with the open one beside the list as its room draws it.
 */

import type { ReactNode } from "react";
import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { ADA, actEngine, applied, type Pressed } from "~/test/chat.tsx";
import { CHAT_ORG, SERVED } from "~/test/chatAccess.ts";

import { DirectMessagesView } from "./DirectMessages.tsx";
import { mountView } from "./frameKit.tsx";

const DM_BO = "0b0e9a8e-2f4c-4f7e-9d8a-5d1f0c6b7a10";
const DM_OPS = "6a3c1e2b-8d4f-4a9e-b7c6-1f2e3d4c5b6a";
const GROUP = "c1d2e3f4-a5b6-4c7d-8e9f-0a1b2c3d4e5f";
const ROOM = "9e8d7c6b-5a4f-4e3d-8c2b-1a0f9e8d7c6b";

const conversation = (
  id: string,
  kind: string,
  participants: string[],
  last: { author: string; excerpt: string; at: string },
  unread = 0,
) => ({
  channel: { v: 1, id, kind, created_at: "2026-10-01T00:00:00Z" },
  participants,
  member: true,
  revision: 1,
  unread,
  mentions: 0,
  last: { message_id: `${id}-last`, channel_seq: 3, position: SERVED.position, ...last },
});

const RAIL = {
  ...SERVED,
  read_state: true,
  channels: [
    conversation(DM_OPS, "dm", ["ada", "ops"], {
      author: "ops",
      excerpt: "The nightly export finished.",
      at: "2026-10-28T09:00:00Z",
    }),
    conversation(
      DM_BO,
      "dm",
      ["ada", "bo"],
      { author: "bo", excerpt: "Can you look at `plot_bookings`?", at: "2026-10-30T09:00:00Z" },
      2,
    ),
    conversation(GROUP, "group", ["ada", "bo", "ops"], {
      author: "ada",
      excerpt: "Signage on Thursday.",
      at: "2026-10-29T09:00:00Z",
    }),
    {
      channel: {
        v: 1,
        id: ROOM,
        kind: "public",
        name: "launch",
        created_at: "2026-10-01T00:00:00Z",
      },
      member: true,
      revision: 1,
      unread: 4,
      mentions: 0,
    },
  ],
};

let presses: Pressed[];

beforeEach(() => {
  presses = actEngine((tool) => applied(tool, {}));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function mount(children: ReactNode, hash: string) {
  const mounted = mountView(children, {
    hash,
    viewer: ADA,
    answers: {
      chat_channels: RAIL,
      // The open conversation's own detail, as the frame reads it once.
      chat_channel: { ...SERVED, channel: RAIL.channels[1]!.channel, members: [] },
    } as never,
  });
  act(() => mounted.store.applyOrg(CHAT_ORG as never));
  return mounted;
}

// A row a filter took out closes its height before it goes (the kit's leave),
// which jsdom never finishes: the rows still drawn are the ones not leaving.
const rows = () =>
  [
    ...document.querySelectorAll<HTMLElement>(
      ".chat-dms-list [data-item]:not(.crewlet-inbox-row--leaving)",
    ),
  ].map((row) => row.dataset.item);

test("lists every direct conversation, newest first, and no room", async () => {
  mount(<DirectMessagesView />, "#/chatlet/dms");
  await waitFor(() => expect(rows()).toEqual([DM_BO, GROUP, DM_OPS]));
  const bo = screen.getByRole("button", { name: /^Bo Chen [^,]/ });
  // The last words as said, with inline code, in two lines.
  expect(within(bo).getByText("plot_bookings").tagName).toBe("CODE");
  expect(bo.querySelector(".crewlet-inbox-row__text--lines-2")).not.toBeNull();
  // A group says who spoke last; the reader's own word says "You".
  expect(screen.getByRole("button", { name: /You: Signage on Thursday\./ })).toBeTruthy();
  // Each row is an item of a list, and nothing is open on arrival.
  expect(bo.closest("li")?.parentElement?.tagName).toBe("UL");
  expect(screen.getByText("Choose a conversation")).toBeTruthy();
  // The page's one heading is the trail's last crumb; the list's menu is a control.
  expect(screen.getAllByRole("heading", { level: 1 }).map((h) => h.textContent)).toEqual([
    "Direct messages",
  ]);
});

test("a row's date is long where the column is wide, and short, with the switch's word dropped, where it is narrow", async () => {
  mount(<DirectMessagesView />, "#/chatlet/dms");
  const ops = await screen.findByRole("button", { name: /^Ops Agent/ });
  expect(ops.querySelector("time")?.textContent).toBe("October 28");
  cleanup();
  // The list's own column, measured: under 384px it is narrow.
  const width = vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(300);
  try {
    mount(<DirectMessagesView />, "#/chatlet/dms");
    const narrow = await screen.findByRole("button", { name: /^Ops Agent/ });
    expect(narrow.querySelector("time")?.textContent).toBe("Oct 28");
    // Still named by its word, and New message still in the head.
    expect(screen.getByRole("switch", { name: "Unreads" })).toBeTruthy();
    const head = document.querySelector<HTMLElement>(".chat-dms-head")!;
    expect(within(head).getByRole("button", { name: "New message" })).toBeTruthy();
  } finally {
    width.mockRestore();
  }
});

test("opening a conversation keeps it in the address, draws its head, and Close gives the row focus back", async () => {
  mount(<DirectMessagesView />, "#/chatlet/dms");
  fireEvent.click(await screen.findByRole("button", { name: /^Bo Chen [^,]/ }));
  await waitFor(() => expect(location.hash).toContain(`room=${DM_BO}`));
  const head = await waitFor(() => {
    const found = document.querySelector<HTMLElement>(".chat-dms-open__head");
    expect(found).not.toBeNull();
    return found!;
  });
  expect(within(head).getByRole("heading", { name: "Bo Chen" })).toBeTruthy();
  expect(within(head).getByRole("button", { name: "Back to Direct messages" })).toBeTruthy();
  expect(screen.getByRole("button", { name: /^Bo Chen [^,]/ }).getAttribute("aria-current")).toBe(
    "true",
  );

  fireEvent.click(within(head).getByRole("button", { name: "Close" }));
  await waitFor(() => expect(location.hash).not.toContain("room="));
  await waitFor(() =>
    expect(document.activeElement).toBe(screen.getByRole("button", { name: /^Bo Chen [^,]/ })),
  );
});

test("Unreads keeps the unread conversations and the open one; the find field narrows by name", async () => {
  mount(<DirectMessagesView />, `#/chatlet/dms?room=${DM_OPS}`);
  await waitFor(() => expect(rows()).toHaveLength(3));
  fireEvent.click(screen.getByRole("switch", { name: "Unreads" }));
  await waitFor(() => expect(rows()).toEqual([DM_BO, DM_OPS]));
  fireEvent.click(screen.getByRole("switch", { name: "Unreads" }));
  fireEvent.change(screen.getByRole("textbox", { name: "Find a direct message" }), {
    target: { value: "ops" },
  });
  await waitFor(() => expect(rows()).toEqual([GROUP, DM_OPS]));
});

test("a row's quick actions: Mark as read on an unread conversation only, and Star", async () => {
  mount(<DirectMessagesView />, "#/chatlet/dms");
  const bo = (await screen.findByRole("button", { name: /^Bo Chen [^,]/ })).closest("li")!;
  const ops = screen.getByRole("button", { name: /^Ops Agent/ }).closest("li")!;
  expect(within(bo).getByRole("button", { name: "Mark as read" })).toBeTruthy();
  expect(within(ops).queryByRole("button", { name: "Mark as read" })).toBeNull();
  // Outside the row's own control, so each is a control of its own.
  expect(
    within(bo)
      .getByRole("button", { name: /^Bo Chen [^,]/ })
      .contains(within(bo).getByRole("button", { name: "Star" })),
  ).toBe(false);
  fireEvent.click(within(ops).getByRole("button", { name: "Star" }));
  await waitFor(() =>
    expect(
      presses.some(
        (press) => press.tool === "mark_chat_read" && JSON.stringify(press.args).includes(DM_OPS),
      ),
    ).toBe(true),
  );
});
