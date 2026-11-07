/**
 * A message on its way, as its row says it.
 *
 * THE INVARIANTS: a row that may be in the room is never said to be not sent;
 * the failed line says "Not sent" once; and a refusal a retry could land says,
 * while the browser is offline, that the reader is offline rather than the
 * sentence it was refused with.
 */

import { cleanup, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { SERVED } from "~/test/chatAccess.ts";
import { actEngine } from "~/test/chat.tsx";

import { mountInFrame } from "./frameKit.tsx";
import type { Outgoing } from "./outbox.ts";
import { PendingRow, pendingLine } from "./Pending.tsx";
import { RoomScopeProvider } from "./scope.tsx";

const ROOM = "room-1";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

const outgoing = (over: Partial<Outgoing>): Outgoing => ({
  opId: "op-1",
  channelID: ROOM,
  threadRoot: "",
  body: "anyone there?",
  links: [],
  state: "sending",
  messageID: "",
  detail: "",
  retryable: false,
  startedAt: Date.parse("2026-10-30T09:00:00Z"),
  answer: "",
  reasks: 0,
  reaskAt: 0,
  ...over,
});

/** The room the rows are said in, read, with the reader in it. */
const DETAIL = {
  data: {
    ...SERVED,
    channel: {
      v: 1,
      id: ROOM,
      kind: "public",
      name: "general",
      created_at: "2026-01-01T00:00:00Z",
    },
    revision: 1,
    message_seq: 1,
    member: true,
    members: [{ handle: "ada", joined_at: "2026-01-01T00:00:00Z" }],
  },
  error: null,
};

/** The row as a room draws it, on a browser that is `connected` or not. */
function mountRow(row: Outgoing, connected = true) {
  actEngine();
  mountInFrame(
    <RoomScopeProvider channelID={ROOM} detail={DETAIL as never}>
      <PendingRow row={row} />
    </RoomScopeProvider>,
    {
      // AS DRAFTS AND SENT DRAWS IT, so the room's rules are this detail's.
      hash: "#/chatlet/drafts",
      connected,
      answers: { chat_channels: { ...SERVED, read_state: true, channels: [] } } as never,
    },
  );
}

/** The kit's failed line, lead and reason as one run of words. */
const failedLine = () =>
  document.querySelector(".crewlet-chat-message-failure__words")?.textContent ?? null;

/** The row's own line, where the retry is held or there is none. */
const heldLine = () => document.querySelector(".chat-pending-line > span")?.textContent ?? null;

// Mutation: lead an unknown row with the kit's "Not sent." again and this
// goes red.
test("a row that may already be in the room is never said to be not sent", async () => {
  mountRow(outgoing({ state: "unknown" }));
  await waitFor(() =>
    expect(failedLine()).toBe("Couldn’t confirm this was sent. It may already be in the room."),
  );
});

// Mutation: hand the kit the whole line as its reason again and this goes red
// with "Not sent. Not sent. …".
test("a refusal a retry can land says Not sent once", async () => {
  mountRow(
    outgoing({
      state: "refused",
      retryable: true,
      detail: "The node is busy. Try again in a moment.",
    }),
  );
  await waitFor(() =>
    expect(failedLine()).toBe("Not sent. The node is busy. Try again in a moment."),
  );
});

// Mutation: word the row by its refusal alone and this goes red with the
// control's sentence ("Not sent. Offline. Reconnect to make changes.").
test("offline, a row nothing was sent for says the reader is offline", async () => {
  mountRow(outgoing({ state: "refused", retryable: true, detail: WRITE_REASONS.offline }), false);
  await waitFor(() => expect(heldLine()).toBe("Not sent: you’re offline."));
});

test("offline, a refusal no retry can change keeps the engine's words", () => {
  const row = outgoing({
    state: "refused",
    retryable: false,
    detail: "This room is archived, so it takes no new messages.",
  });
  mountRow(row, false);
  expect(heldLine()).toBe("Not sent. This room is archived, so it takes no new messages.");
  expect(pendingLine(row, true)).toBe(heldLine());
});
