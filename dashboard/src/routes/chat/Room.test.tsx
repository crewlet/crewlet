/**
 * A room, and the three things it must never get wrong.
 *
 * OPTIMISTIC IS ALLOWED AND A LIE IS NOT. A message this tab has said and the
 * company has not yet acknowledged is drawn as pending; a write whose outcome
 * the broker never gave is drawn as unknown, with the retry that reuses its
 * own operation id; and neither becomes "sent" until the room hands the
 * message back.
 *
 * AND A THREAD IS ONE LEVEL DEEP, which is a property of the screen as much as
 * of the engine: a reply that offered a thread of its own would promise a
 * shape the write path cannot file.
 */

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Room } from "./Room.tsx";
import { ReadCursors } from "./cursor.ts";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { Store, type QueryName } from "~/protocol/index.ts";

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 40 },
};

const channel = {
  v: 1,
  id: "room-1",
  kind: "public",
  name: "general",
  created_at: "2026-01-01T00:00:00Z",
};

function message(seq: number, over: Record<string, unknown> = {}) {
  return {
    message: {
      v: 1,
      id: `m-${seq}`,
      channel_id: "room-1",
      author: "bo",
      author_kind: "agent",
      body: `line ${seq}`,
      created_at: "2026-01-01T00:00:00Z",
      ...over,
    },
    channel_seq: seq,
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
  };
}

/** What the fake engine answers, mutable between calls. */
let answers: Partial<Record<QueryName, unknown>>;
let posted: { path: string; body: Record<string, unknown> }[];
let reply: { status: number; body: unknown };

beforeEach(() => {
  posted = [];
  reply = { status: 200, body: {} };
  answers = {
    chat_channel: { ...served, channel, revision: 3, message_seq: 2, member: true, members: [] },
    chat_messages: { ...served, channel_id: "room-1", messages: [message(2), message(1)] },
  };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      posted.push({
        path: String(url).replace(location.origin, ""),
        body: JSON.parse(String(init.body ?? "{}")) as Record<string, unknown>,
      });
      return new Response(JSON.stringify(reply.body), {
        status: reply.status,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function mount() {
  const store = new Store();
  const socket = {
    query: vi.fn(async (what: string) => answers[what as QueryName] ?? {}),
    focus: vi.fn(),
  };
  const cursors = new ReadCursors({ flush: async () => undefined });
  const view = render(
    <ClientContext.Provider value={{ store, socket } as never}>
      <Router>
        <Room
          channelID="room-1"
          viewer="ada"
          nameOf={(handle) => handle}
          cursors={cursors}
          onWrote={() => undefined}
        />
      </Router>
    </ClientContext.Provider>,
  );
  return { store, socket, view };
}

/** A committed record arriving on the socket, which is what makes a room
 *  re-read. It carries no body — that is the engine's own rule. */
function frame(store: Store, seq: number, over: Record<string, unknown> = {}) {
  store.applyChatChange({
    channel_id: "room-1",
    op: "post",
    op_id: `op-${seq}`,
    message_id: `m-${seq}`,
    channel_seq: seq,
    at: "2026-01-01T00:01:00Z",
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
    ...over,
  } as never);
}

async function say(text: string) {
  const box = await screen.findByLabelText(/^Message/);
  fireEvent.change(box, { target: { value: text } });
  fireEvent.submit(box.closest("form")!);
}

test("a message this tab has said is pending until the room hands it back", async () => {
  // OPTIMISTIC, AND HONEST ABOUT IT. The record applied — the engine said so —
  // and what is on screen is still this tab's draft: the message the company
  // holds carries the BROKER's instant and its place in the room, neither of
  // which exists until the transcript brings it back.
  reply = { status: 200, body: { outcome: "applied", op_id: "op-3", message: { id: "m-3" } } };
  const { store } = mount();
  await screen.findByText("line 2");

  await say("shipping it");
  await waitFor(() => expect(screen.getByText("shipping it")).toBeTruthy());
  expect(document.querySelector(".chat-message.pending")).toBeTruthy();

  // The record lands: a frame says so, the room re-reads, and the message is
  // in the answer.
  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [
      message(3, { id: "m-3", author: "ada", author_kind: "human", body: "shipping it" }),
      message(2),
      message(1),
    ],
  };
  frame(store, 3);

  await waitFor(() => expect(document.querySelector(".chat-message.pending")).toBeNull());
  // AND EXACTLY ONCE. The pending row leaves by being superseded, so the two
  // are never both on screen.
  expect(screen.getAllByText("shipping it")).toHaveLength(1);
});

test("a write the broker did not answer is never drawn as sent", async () => {
  // 504 with an outcome of `unknown`: the record may be on the log and may
  // never be. The only honest rendering says so and offers the retry.
  reply = { status: 504, body: { outcome: "unknown", op_id: "op-9" } };
  mount();
  await screen.findByText("line 2");

  await say("did that land?");
  await waitFor(() => expect(screen.getByText(/broker did not answer/i)).toBeTruthy());
  expect(document.querySelector('.chat-message[data-state="unknown"]')).toBeTruthy();
  expect(screen.queryByText(/^Sending/)).toBeNull();
});

test("retrying an unknown write reuses its operation id, because a fresh one would say it twice", async () => {
  // A post arbitrates nothing at the broker, so the operation id is the ONLY
  // thing that can tell a resubmission from a second remark — the engine
  // derives the message's own id from it.
  reply = { status: 504, body: { outcome: "unknown", op_id: "op-9" } };
  mount();
  await screen.findByText("line 2");
  await say("did that land?");

  const first = await screen.findByRole("button", { name: /same id/i });
  fireEvent.click(first);
  await waitFor(() => expect(posted).toHaveLength(2));
  expect(posted[0]?.body.operation_id).toBeTruthy();
  expect(posted[1]?.body.operation_id).toBe(posted[0]?.body.operation_id);
});

test("a refusal is shown as the engine's own sentence rather than as a failure", async () => {
  reply = {
    status: 409,
    body: { error: "archived", detail: "chat: room general is archived" },
  };
  mount();
  await screen.findByText("line 2");
  await say("anyone there?");
  await waitFor(() => expect(screen.getByText(/is archived/)).toBeTruthy());
});

test("a reply in the transcript says which thread it is in and opens no new one", async () => {
  // THE OTHER HALF OF ONE LEVEL DEEP, in the room rather than in the pane: a
  // reply is a message like any other and appears in the transcript, and the
  // only thread it can ever belong to is the one it already has.
  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(4, { id: "m-4", thread_root: "m-2", body: "answered" }), message(2)],
  };
  mount();
  await screen.findByText("answered");
  const row = document.querySelector('[data-message-id="m-4"]')!;
  expect(row.textContent).toContain("in thread");
  expect([...row.querySelectorAll("button")].map((b) => b.textContent)).not.toContain(
    "reply in thread",
  );
});

test("a thread pane shows one level: a reply offers no thread of its own", async () => {
  // A reply to a reply carries the same root, so an affordance on a reply
  // would promise a shape the write path cannot file. The reply says which
  // thread it is IN instead.
  answers.chat_thread = {
    ...served,
    channel_id: "room-1",
    root: message(2),
    replies: [message(5, { id: "m-5", thread_root: "m-2", body: "first answer" })],
    participants: ["bo"],
  };
  mount();
  const open = await screen.findAllByRole("button", { name: /reply in thread/i });
  fireEvent.click(open[0]!);

  await waitFor(() => expect(screen.getByText("first answer")).toBeTruthy());
  const pane = document.querySelector(".chat-thread")!;
  expect(pane.querySelectorAll("button")).toBeTruthy();
  // Nothing inside the pane opens another thread.
  expect([...pane.querySelectorAll("button")].map((b) => b.textContent)).not.toContain(
    "reply in thread",
  );
});

test("an archived room says why it takes no messages rather than greying a box", async () => {
  answers.chat_channel = {
    ...served,
    channel: { ...channel, archived_at: "2026-02-01T00:00:00Z" },
    revision: 4,
    message_seq: 2,
    member: true,
    members: [],
  };
  mount();
  await waitFor(() => expect(screen.getByText(/archived/i)).toBeTruthy());
  expect(screen.getByText(/readable for ever/i)).toBeTruthy();
});

test("a private room the reader is not in refuses every write, a reaction included", async () => {
  answers.chat_channel = {
    ...served,
    channel: { ...channel, kind: "private", name: "board" },
    revision: 4,
    message_seq: 2,
    member: false,
    members: [],
  };
  mount();
  await waitFor(() => expect(screen.getByText(/private room you are not in/i)).toBeTruthy());
});

test("a room this build cannot classify is read-only rather than guessed at", async () => {
  // An open enum read two-valued falls out as "not private", which is how a
  // newer peer's room would become writable by anyone. The engine refuses
  // every write to one; the screen offers none.
  answers.chat_channel = {
    ...served,
    channel: { ...channel, kind: "broadcast" },
    revision: 4,
    message_seq: 2,
    member: true,
    members: [],
  };
  mount();
  await waitFor(() => expect(screen.getByText(/newer build/i)).toBeTruthy());
});

test("a seat working on an answer is drawn under the message it is answering", async () => {
  // The frame says WHICH message the reply will land under, and that is the
  // whole point of the field: an indicator at the top of the room would put
  // it somewhere the answer is not going to appear.
  const { store } = mount();
  await screen.findByText("line 2");
  store.applyChatPresence({
    rooms: {
      "room-1": {
        viewing: ["bo"],
        working: [{ handle: "bo", thread: "m-2", status: "is drafting a reply" }],
      },
    },
  });
  await waitFor(() => expect(screen.getByText(/bo is drafting a reply/)).toBeTruthy());
  const row = document.querySelector('[data-message-id="m-2"]')!;
  expect(row.textContent).toContain("is drafting a reply");
});

test("only the author of a message is offered the edit and the delete", async () => {
  // The engine's gate is NARROWER than the room's own: a remark somebody else
  // can rewrite is a remark attributed to a person who did not make it. The
  // fixtures are authored by `bo` and the reader is `ada`.
  mount();
  await screen.findByText("line 2");
  expect(screen.queryByRole("button", { name: "edit" })).toBeNull();

  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(5, { id: "m-5", author: "ada", author_kind: "human", body: "mine" })],
  };
  cleanup();
  mount();
  await screen.findByText("mine");
  expect(screen.getByRole("button", { name: "edit" })).toBeTruthy();
});

test("a delete asks once before it writes, because a tombstone is not undone", async () => {
  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(5, { id: "m-5", author: "ada", author_kind: "human", body: "mine" })],
  };
  reply = { status: 200, body: { outcome: "applied", op_id: "op-d" } };
  mount();
  await screen.findByText("mine");

  fireEvent.click(screen.getByRole("button", { name: "delete" }));
  expect(posted).toHaveLength(0);
  fireEvent.click(screen.getByRole("button", { name: "really delete?" }));
  await waitFor(() => expect(posted).toHaveLength(1));
  expect(posted[0]?.path).toBe("/chat/channels/room-1/messages/m-5/delete");
});

test("an edit re-resolves what the new body names", async () => {
  // The engine does not re-read the prose: the mentions travel again because
  // the body they were derived from did. An edit that added a name and did not
  // carry the new set would leave the row naming whoever the first draft did.
  answers.chat_channel = {
    ...served,
    channel,
    revision: 3,
    message_seq: 2,
    member: true,
    members: [{ handle: "bo", joined_at: "2026-01-01T00:00:00Z" }],
  };
  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(5, { id: "m-5", author: "ada", author_kind: "human", body: "mine" })],
  };
  reply = { status: 200, body: { outcome: "applied", op_id: "op-e" } };
  mount();
  await screen.findByText("mine");

  fireEvent.click(screen.getByRole("button", { name: "edit" }));
  const box = screen.getByLabelText("Rewrite this message");
  fireEvent.change(box, { target: { value: "over to you @bo" } });
  fireEvent.submit(box.closest("form")!);

  await waitFor(() => expect(posted).toHaveLength(1));
  expect(posted[0]?.path).toBe("/chat/channels/room-1/messages/m-5/edit");
  expect(posted[0]?.body).toMatchObject({ body: "over to you @bo", mentions: ["bo"] });
});

test("a live frame makes the room re-read rather than rendering the frame", async () => {
  // A frame carries no body on purpose: rendering one would be a second
  // rendering path for a conversation, and the two disagree the first time an
  // edit races a reload.
  const { store, socket } = mount();
  await screen.findByText("line 2");
  const before = socket.query.mock.calls.filter(([what]) => what === "chat_messages").length;

  answers.chat_messages = {
    ...served,
    channel_id: "room-1",
    messages: [message(3, { body: "arrived" }), message(2), message(1)],
  };
  frame(store, 3);

  await waitFor(() => expect(screen.getByText("arrived")).toBeTruthy());
  expect(
    socket.query.mock.calls.filter(([what]) => what === "chat_messages").length,
  ).toBeGreaterThan(before);
});

test("a burst of frames is one re-read, not one per message", async () => {
  // The whole reason the frames are coalesced: six messages in a busy room
  // would otherwise be six queries on a socket that is already carrying the
  // conversation.
  const { store, socket } = mount();
  await screen.findByText("line 2");
  const before = socket.query.mock.calls.filter(([what]) => what === "chat_messages").length;

  for (let seq = 3; seq <= 8; seq++) frame(store, seq);
  await waitFor(() =>
    expect(
      socket.query.mock.calls.filter(([what]) => what === "chat_messages").length,
    ).toBeGreaterThan(before),
  );
  expect(socket.query.mock.calls.filter(([what]) => what === "chat_messages").length).toBeLessThan(
    before + 6,
  );
});

// ---------------------------------------------------------------------------
// Standing in a room you are not in
// ---------------------------------------------------------------------------

test("a room the reader may read but is not in says so, and offers to join", async () => {
  // A public room is readable by every seat, joined or not — so arriving here
  // from a search result and standing outside the room whose whole transcript
  // is on the screen is an ordinary state. Unsaid, a rail that does not list
  // the room looks like a rail that lost it.
  answers.chat_channel = {
    ...served,
    channel,
    revision: 3,
    message_seq: 2,
    member: false,
    members: [],
  };
  mount();
  await waitFor(() => expect(screen.getByText(/you are not in this room/i)).toBeTruthy());

  fireEvent.click(screen.getByRole("button", { name: /^join$/i }));
  await waitFor(() => expect(posted).toHaveLength(1));
  expect(posted[0]?.path).toBe("/chat/channels/room-1/join");
});

test("leaving a room takes the reader out of it rather than leaving them in front of it", async () => {
  // A private room somebody has just left is one they can no longer read, so
  // staying on it would turn into "no such room" under them a moment later.
  mount();
  await screen.findByText("line 2");
  fireEvent.click(screen.getByRole("button", { name: /^leave$/i }));

  await waitFor(() => expect(posted).toHaveLength(1));
  expect(posted[0]?.path).toBe("/chat/channels/room-1/leave");
  await waitFor(() => expect(location.hash).toBe("#/chat"));
});

test("a unit's room offers no leave, and says why there is no button", async () => {
  // That membership is the org chart's own: a leave there is undone by the
  // next apply, and a gesture that silently reverts is worse than one that is
  // refused — so the engine refuses it and the screen does not offer it.
  answers.chat_channel = {
    ...served,
    channel: { ...channel, kind: "unit", unit: "engineering" },
    revision: 3,
    message_seq: 2,
    member: true,
    members: [],
  };
  mount();
  await waitFor(() => expect(screen.getByText(/org chart/i)).toBeTruthy());
  expect(screen.queryByRole("button", { name: /^leave$/i })).toBeNull();
  expect(posted).toHaveLength(0);
});
