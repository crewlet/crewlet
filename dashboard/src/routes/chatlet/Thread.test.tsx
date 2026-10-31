/**
 * A thread pane: its root, its replies, who a reply reaches, and following.
 *
 * THE INVARIANTS: a reader follows a thread they started, replied in or were
 * named in without a mark, and a mark by hand decides either way; Follow is
 * drawn for every reader and held with why (`app/writeGate.test.tsx` names
 * this suite for `Thread.tsx`), and writes the reader's own mark.
 */

import { act, cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { ADA, actEngine, applied, type Pressed } from "~/test/chat.tsx";
import { CHAT_ORG, SERVED, WRITE_READERS, heldWith } from "~/test/chatAccess.ts";

import { mountInFrame } from "./frameKit.tsx";
import { ThreadPane, follows } from "./Thread.tsx";

const ROOM = "room-1";

const view = (id: string, author: string, extra: Record<string, unknown> = {}) => ({
  message: {
    v: 1,
    id,
    channel_id: ROOM,
    author,
    author_kind: "human",
    body: `said ${id}`,
    created_at: "2026-10-30T09:00:00Z",
    ...extra,
  },
  channel_seq: 1,
  position: SERVED.position,
});

const ROOT = view("m-1", "bo");
const THREAD = {
  ...SERVED,
  channel_id: ROOM,
  root: {
    ...ROOT,
    thread: {
      replies: 2,
      last_reply_at: "",
      last_reply_position: 0,
      repliers: ["ops"],
      replier_count: 1,
    },
  },
  replies: [view("m-2", "ops", { thread_root: "m-1" }), view("m-3", "ops", { thread_root: "m-1" })],
  participants: ["bo", "ops"],
};

const DETAIL = {
  ...SERVED,
  channel: { v: 1, id: ROOM, kind: "public", name: "launch", created_at: "2026-10-01T00:00:00Z" },
  revision: 1,
  message_seq: 3,
  member: true,
  members: [],
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

function open(viewer: Record<string, unknown> = ADA) {
  const mounted = mountInFrame(<ThreadPane room={ROOM} root="m-1" onClose={() => {}} />, {
    hash: `#/chatlet/${ROOM}?pane=thread:${ROOM}:m-1`,
    viewer,
    answers: {
      chat_channel: DETAIL,
      chat_thread: THREAD,
      chat_channels: { ...SERVED, read_state: true, channels: [] },
    },
  });
  act(() => mounted.store.applyOrg(CHAT_ORG as never));
  return mounted;
}

describe("who follows a thread", () => {
  const root = ROOT as never;
  test.each([
    [
      "the reader started it",
      { ...ROOT, message: { ...ROOT.message, author: "ada" } },
      [],
      [],
      true,
    ],
    ["the reader replied in it", ROOT, ["ada"], [], true],
    ["the reader was named in a reply", ROOT, [], [view("m-2", "bo", { mentions: ["ada"] })], true],
    ["none of those", ROOT, ["bo"], [], false],
  ] as const)("%s: %s", (_, rootView, participants, replies, expected) => {
    expect(follows(null, "m-1", "ada", rootView as never, participants, replies as never)).toBe(
      expected,
    );
  });

  test("a mark by hand decides either way", () => {
    const unfollowed = {
      threads: [{ channel_id: ROOM, root_id: "m-1", following: false, at: "" }],
    };
    expect(follows(unfollowed as never, "m-1", "ada", root, ["ada"], [])).toBe(false);
    const followed = { threads: [{ channel_id: ROOM, root_id: "m-1", following: true, at: "" }] };
    expect(follows(followed as never, "m-1", "ada", root, [], [])).toBe(true);
  });
});

describe("the pane", () => {
  test("draws the root, the reply count, and who a reply reaches", async () => {
    open();
    expect(await screen.findByText("said m-1")).toBeTruthy();
    expect(screen.getByText("2 replies")).toBeTruthy();
    expect(screen.getByText(/Replies reach Bo Chen and Ops Agent\./)).toBeTruthy();
  });

  test("Follow writes the reader's own mark", async () => {
    open();
    fireEvent.click(await screen.findByRole("button", { name: /^follow$/i }));
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]).toMatchObject({
      tool: "mark_chat_read",
      args: { follow: [{ channel_id: ROOM, root_id: "m-1" }] },
    });
  });

  test.each(WRITE_READERS)("Follow is drawn for $who, held with why", async (reader) => {
    open(reader.viewer);
    await heldWith(await screen.findByRole("button", { name: /^follow$/i }), reader.reason);
  });
});
