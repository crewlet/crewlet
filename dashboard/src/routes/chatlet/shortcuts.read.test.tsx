/**
 * Escape in a room marks it read, as the frame binds the key.
 *
 * THE INVARIANTS: the key marks and says so only where something is unread,
 * since Escape is pressed by reflex and a confirmation of nothing is noise;
 * and its toast is one toast, which a repeat replaces rather than stacks.
 */

import { act, cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { CHAT_ORG, SERVED } from "~/test/chatAccess.ts";
import { actEngine } from "~/test/chat.tsx";

import { mountInFrame } from "./frameKit.tsx";

const ROOM = "6f1c2a3e-1b2c-4d5e-8f90-a1b2c3d4e5f6";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

const CHANNEL = {
  v: 1,
  id: ROOM,
  kind: "public",
  name: "general",
  created_at: "2026-01-01T00:00:00Z",
};

/** The frame at the room, its rail counting `unread` posts there. */
async function inRoom(unread: number) {
  actEngine();
  const mounted = mountInFrame(<p>the room</p>, {
    hash: `#/chatlet/${ROOM}`,
    answers: {
      chat_channels: {
        ...SERVED,
        read_state: true,
        read: { handle: "ada", revision: 1, cursors: {} },
        channels: [
          {
            channel: CHANNEL,
            revision: 1,
            unread,
            mentions: 0,
            last: {
              message_id: "m-9",
              channel_seq: 9,
              at: "2026-01-01T00:09:00Z",
              position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 9 },
            },
          },
        ],
      },
      chat_channel: {
        ...SERVED,
        channel: CHANNEL,
        revision: 1,
        message_seq: 9,
        member: true,
        members: [{ handle: "ada", joined_at: "2026-01-01T00:00:00Z" }],
      },
    } as never,
  });
  act(() => mounted.store.applyOrg(CHAT_ORG as never));
  await screen.findByText("the room");
  // THE RAIL HAS ANSWERED once the frame lists the room's count.
  await waitFor(() =>
    expect(mounted.socket.query.mock.calls.some(([what]) => what === "chat_channels")).toBe(true),
  );
  await act(async () => {
    await new Promise((r) => setTimeout(r, 0));
  });
  return mounted;
}

const escape = () => fireEvent.keyDown(document.body, { key: "Escape" });

/** The toasts on screen saying `text`. */
const toasts = (text: string) =>
  [...document.querySelectorAll(".crewlet-toast")].filter((t) => t.textContent?.includes(text));

// Mutation: drop the unread condition from the binding's `when` and this
// goes red.
test("Escape in a room with nothing unread marks nothing and says nothing", async () => {
  await inRoom(0);
  escape();
  escape();
  await act(async () => {
    await new Promise((r) => setTimeout(r, 20));
  });
  expect(toasts("Marked as read")).toHaveLength(0);
});

// Mutation: drop the toast's id and this goes red with two toasts.
test("Escape in a room with unread posts says it once, however often it is pressed", async () => {
  await inRoom(3);
  escape();
  await waitFor(() => expect(toasts("Marked as read")).toHaveLength(1));
  // AGAIN BEFORE THE RAIL SAYS IT IS READ, as a reflexive second press is.
  escape();
  await act(async () => {
    await new Promise((r) => setTimeout(r, 50));
  });
  expect(toasts("Marked as read")).toHaveLength(1);
});
