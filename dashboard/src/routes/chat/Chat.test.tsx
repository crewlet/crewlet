/**
 * What the chat screen says before it says anything about a conversation.
 *
 * The three shapes a reader can arrive in are not degrees of the same thing: a
 * missing token, a token no seat names, and a person. The middle one is an
 * ordinary state with a one-line remedy, and it is the case this screen exists
 * to be clear about — the socket reduces the engine's own refusal to a CODE,
 * so if this screen does not name the field nothing does.
 */

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { Chat } from "./Chat.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { Store, type QueryName } from "~/protocol/index.ts";

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

/** A socket that answers each question with a fixture and never pushes. */
function mount(answers: Partial<Record<QueryName, unknown>>) {
  const store = new Store();
  const query = vi.fn(async (what: string) => answers[what as QueryName] ?? {});
  const socket = { query, focus: vi.fn() };
  const view = render(
    <ClientContext.Provider value={{ store, socket } as never}>
      <Router>
        <Chat />
      </Router>
    </ClientContext.Provider>,
  );
  return { store, query, socket, view };
}

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 12 },
};

function room(over: Record<string, unknown> = {}) {
  return {
    channel: {
      v: 1,
      id: "room-1",
      kind: "public",
      name: "general",
      created_at: "2026-01-01T00:00:00Z",
    },
    revision: 3,
    unread: 0,
    ...over,
  };
}

const bound = {
  operator_id: "U0FOUNDER",
  operator: true,
  handle: "ada",
  name: "Ada",
  kind: "human",
};

test("a token no seat names is told which field to change, not that chat is empty", async () => {
  // THE STRICTEST READ RULE IN THE TREE, and the one a person can actually
  // fix. The engine refuses with a sentence naming the field; the socket
  // carries only the code, so the screen says it from the one fact it holds
  // independently — an operator id with no handle.
  mount({ viewer: { operator_id: "U0CI", operator: true, handle: "", name: "", kind: "" } });
  await waitFor(() => expect(screen.getByText(/not bound to a seat/i)).toBeTruthy());
  expect(screen.getByText(/contact\.crewlet_operator_id/)).toBeTruthy();
});

test("an unbound reader is not asked for their rooms at all", async () => {
  // The engine would refuse it, so asking is a round trip whose only possible
  // answer is a refusal the screen has already rendered.
  const { query } = mount({
    viewer: { operator_id: "U0CI", operator: true, handle: "", name: "", kind: "" },
  });
  await waitFor(() => expect(screen.getByText(/not bound to a seat/i)).toBeTruthy());
  expect(query.mock.calls.map(([what]) => what)).not.toContain("chat_channels");
});

test("a reader with no token is told chat has no anonymous form", async () => {
  mount({ viewer: { operator_id: "", operator: false, handle: "", name: "", kind: "" } });
  await waitFor(() => expect(screen.getByText(/needs a credential/i)).toBeTruthy());
});

test("an unread badge stops at 99+ when the engine says it stopped counting", async () => {
  // The cap is `chat.UnreadLimit` and the answer SAYS when it was reached, so
  // "99+" renders a stated fact. Inferred from the number instead, a room with
  // exactly a hundred unread and one with four thousand would draw the same
  // badge and neither would be true.
  mount({
    viewer: bound,
    chat_channels: {
      ...served,
      read_state: true,
      channels: [room({ unread: 100, unread_capped: true })],
    },
  });
  await waitFor(() => expect(screen.getByText("99+")).toBeTruthy());
});

test("an exact unread count is drawn as itself", async () => {
  mount({
    viewer: bound,
    chat_channels: { ...served, read_state: true, channels: [room({ unread: 7 })] },
  });
  await waitFor(() => expect(screen.getByText("7")).toBeTruthy());
});

test("a count with no read state behind it is not drawn as nothing unread", async () => {
  // The engine zeroes every count when the coordination record could not be
  // read and says so. Drawing that as a quiet rail is indistinguishable from
  // being caught up — so the screen says which one this is, and draws no
  // badge rather than a zero.
  mount({
    viewer: bound,
    chat_channels: {
      ...served,
      read_state: false,
      channels: [room({ unread: 0 })],
    },
  });
  await waitFor(() => expect(screen.getByText(/could not be read/i)).toBeTruthy());
});

test("a muted room says so in the rail", async () => {
  mount({
    viewer: bound,
    chat_channels: {
      ...served,
      read_state: true,
      channels: [room({ unread: 3, muted: true })],
    },
  });
  await waitFor(() => expect(screen.getByText("muted")).toBeTruthy());
  // AND IT IS STILL BADGED. A mute suppresses NOTICE, never delivery: the
  // messages are there, and a room that hid its own count would make "muted"
  // mean "ignored".
  expect(screen.getByText("3")).toBeTruthy();
});

test("a reader in no rooms is told how a room comes to exist", async () => {
  mount({ viewer: bound, chat_channels: { ...served, read_state: true, channels: [] } });
  await waitFor(() => expect(screen.getByText(/in no rooms yet/i)).toBeTruthy());
});
