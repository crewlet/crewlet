/**
 * EVERY CHAT WRITE IS DRAWN FOR A READER WHO CANNOT MAKE IT, AND SAYS WHY.
 *
 * A chat change is a press through the act transport, made as the principal
 * the session resolves to, as the seat it holds (ADR-0024) — so nobody signed
 * in, a person without the Chatlet app, a person the engine does not make the
 * change for, or a credential acting as no seat can say nothing, join nothing
 * and make no room. The controls are DRAWN for all four and held with the
 * sentence that says what would change that (`lib/useWriteAccess.ts`): a
 * composer that vanished would teach somebody the product cannot do it, and
 * one that said nothing would teach them it is broken.
 *
 * This is the suite `app/writeGate.test.tsx` names for every chat module that
 * makes a change: it renders each of their controls for each of the four, and
 * holds the one write with no control — the read cursor's background flush —
 * to sending nothing at all.
 */

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ToastProvider } from "@crewlethq/ui";

import { pick } from "~/testing.tsx";
import { noAccessReason, WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { needsWords } from "~/lib/access.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { Store } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { ADA, actEngine, mountChat, type Pressed } from "~/test/chat.tsx";
import { LoneRoom } from "./testkit.tsx";

import { Browse } from "./Browse.tsx";
import { Chat } from "./Chat.tsx";
import { packPosition, ReadCursors, useReadCursors } from "./cursor.ts";
import { RoomSettings } from "./RoomSettings.tsx";
import { StartDirect } from "./StartDirect.tsx";
import { StartRoom } from "./StartRoom.tsx";

/** The four readers who may not make a chat change, and the sentence each is told. */
const READERS = [
  {
    who: "nobody signed in",
    viewer: { login: "", owner: "", handle: "", name: "", kind: "", grants: [], acts: [] },
    reason: WRITE_REASONS.anonymous,
  },
  {
    // THE APP FIRST: a remedy they can ask an admin for, where "this engine
    // does not make this change for you" would send them nowhere.
    who: "a person without the Chatlet app",
    viewer: { ...ADA, grants: ["worklet"], acts: [] },
    reason: noAccessReason(["chatlet"]),
  },
  {
    who: "a person the engine makes no chat change for",
    viewer: { ...ADA, acts: [] },
    reason: WRITE_REASONS.not_served,
  },
  {
    // HOLDING CHATLET AND NO SEAT: the engine offers a seated verb to nobody
    // acting as themselves, so `acts` names none and every control says so.
    who: "a credential acting as no seat",
    viewer: {
      ...ADA,
      login: "ci:deploy",
      owner: "ci:deploy",
      handle: "",
      name: "",
      kind: "",
      acts: [],
    },
    reason: WRITE_REASONS.not_served,
  },
];

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 4 },
};

const channel = {
  v: 1,
  id: "room-1",
  kind: "public",
  name: "general",
  topic: "Shipping",
  created_at: "2026-01-01T00:00:00Z",
};

const said = {
  message: {
    v: 1,
    id: "m-1",
    channel_id: "room-1",
    author: "ada",
    author_kind: "human",
    body: "morning",
    created_at: "2026-01-01T00:00:00Z",
  },
  channel_seq: 1,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 1 },
  reactions: [{ emoji: ":eyes:", count: 1 }],
};

let presses: Pressed[];

beforeEach(() => {
  location.hash = "#/";
  presses = actEngine();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/** A control held with `reason`: drawn, refusing the press, and saying why. */
async function heldWith(control: HTMLElement, reason: string) {
  await waitFor(() => expect(control.getAttribute("title")).toBe(reason));
  expect(
    control.getAttribute("aria-disabled") === "true" || (control as HTMLButtonElement).disabled,
  ).toBe(true);
}

test.each(READERS)("a room's every write is drawn for $who, held with why", async (reader) => {
  const cursors = new ReadCursors({ flush: async () => undefined });
  mountChat(
    <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
    {
      viewer: reader.viewer,
      answers: {
        chat_channel: {
          ...served,
          channel,
          revision: 1,
          message_seq: 1,
          member: true,
          members: [],
        },
        chat_messages: { ...served, channel_id: "room-1", messages: [said] },
      },
    },
  );
  await screen.findByText("morning");

  // THE COMPOSER: the box stays open — it is the browser that cannot send,
  // not the room that takes nothing — and Send is held with the sentence.
  const box = screen.getByLabelText(/^Message/) as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "morning again" } });
  await heldWith(screen.getByRole("button", { name: /^send$/i }), reader.reason);
  expect(box.disabled).toBe(false);
  fireEvent.submit(box.closest("form")!);

  // THE ROOM'S OWN GESTURES — leaving it, muting it, and opening what it is
  // for, whose Save could never be pressed.
  await heldWith(screen.getByRole("button", { name: /^leave$/i }), reader.reason);
  await heldWith(screen.getByRole("button", { name: /^mute$/i }), reader.reason);
  await heldWith(screen.getByRole("button", { name: /^topic$/i }), reader.reason);

  // AND A MESSAGE'S: its edit, its delete and its reactions.
  const row = document.querySelector('[data-message-id="m-1"]') as HTMLElement;
  await heldWith(within(row).getByRole("button", { name: /^edit$/i }), reader.reason);
  await heldWith(within(row).getByRole("button", { name: /^delete$/i }), reader.reason);
  await heldWith(within(row).getByRole("button", { name: /:eyes:/ }), reader.reason);

  fireEvent.click(screen.getByRole("button", { name: /^leave$/i }));
  fireEvent.click(within(row).getByRole("button", { name: /^delete$/i }));
  await new Promise((r) => setTimeout(r, 20));
  expect(presses).toHaveLength(0);
});

test.each(READERS)("a thread's reply is drawn for $who, held with why", async (reader) => {
  // THE THREAD'S COMPOSER IS A COMPOSER LIKE THE ROOM'S: the box stays open —
  // it is the browser that cannot send — and Reply is held with the sentence
  // `reply_in_thread` is held with for this reader. Nothing is sent.
  const cursors = new ReadCursors({ flush: async () => undefined });
  location.hash = "#/chatlet/room-1?thread=m-1";
  mountChat(
    <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
    {
      viewer: reader.viewer,
      answers: {
        chat_channel: {
          ...served,
          channel,
          revision: 1,
          message_seq: 1,
          member: true,
          members: [],
        },
        chat_messages: { ...served, channel_id: "room-1", messages: [said] },
        chat_thread: { ...served, channel_id: "room-1", root: said, replies: [] },
      },
    },
  );
  const pane = await waitFor(() => {
    const found = document.querySelector<HTMLElement>(".chat-thread");
    if (!found) throw new Error("no thread pane");
    return found;
  });
  const box = within(pane).getByLabelText(/^Reply/) as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "and in the thread" } });
  await heldWith(within(pane).getByRole("button", { name: /^reply$/i }), reader.reason);
  expect(box.disabled).toBe(false);
  fireEvent.submit(box.closest("form")!);
  await new Promise((r) => setTimeout(r, 20));
  expect(presses).toHaveLength(0);
});

/** A message's three changes, and the control each is pressed through. */
const MESSAGE_CHANGES = [
  { tool: "edit_message", control: /^edit$/i },
  { tool: "delete_message", control: /^delete$/i },
  { tool: "react_to_message", control: /:eyes:/ },
];

test.each(MESSAGE_CHANGES)(
  "a message's $tool is held by its own tool, never by the composer's",
  async ({ tool, control }) => {
    // THE ENGINE SERVES EACH CHANGE TO A PERSON ON ITS OWN (`viewer.acts`),
    // so a person it posts for and does not make THIS change for is told so
    // on this control alone — and the others, the composer included, stay
    // theirs to press. Held by the composer's access, every control here
    // would be pressable for this person and refused by the engine.
    const cursors = new ReadCursors({ flush: async () => undefined });
    mountChat(
      <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
      {
        viewer: { ...ADA, acts: ADA.acts.filter((t) => t !== tool) },
        answers: {
          chat_channel: {
            ...served,
            channel,
            revision: 1,
            message_seq: 1,
            member: true,
            members: [],
          },
          chat_messages: { ...served, channel_id: "room-1", messages: [said] },
        },
      },
    );
    await screen.findByText("morning");
    const row = document.querySelector('[data-message-id="m-1"]') as HTMLElement;
    await heldWith(within(row).getByRole("button", { name: control }), WRITE_REASONS.not_served);
    for (const other of MESSAGE_CHANGES.filter((change) => change.tool !== tool)) {
      const button = within(row).getByRole("button", { name: other.control });
      expect(button.getAttribute("aria-disabled"), other.tool).not.toBe("true");
    }
    const box = screen.getByLabelText(/^Message/) as HTMLTextAreaElement;
    await waitFor(() => expect(box.disabled).toBe(false));

    fireEvent.click(within(row).getByRole("button", { name: control }));
    await new Promise((r) => setTimeout(r, 20));
    expect(presses).toHaveLength(0);
  },
);

/** The rail's two gestures that open a change, and the tool each dialog presses. */
const OPENERS = [
  { tool: "create_channel", control: /^new room$/i },
  { tool: "open_dm", control: /^message$/i },
];

test.each(OPENERS)(
  "the rail's $tool opener is held by its own tool, and finding a room by none",
  async ({ tool, control }) => {
    // A DIALOG WHOSE CHANGE COULD NEVER BE MADE is not opened to find that
    // out: the opener is held as its change is (`viewer.acts`), drawn for
    // everybody, and the other opener stays theirs. Finding a room is a read,
    // and is never held.
    mountChat(<Chat />, {
      viewer: { ...ADA, acts: ADA.acts.filter((t) => t !== tool) },
      answers: { chat_channels: { ...served, read_state: true, channels: [] } },
    });
    const rail = await screen.findByRole("navigation", { name: "Your rooms" });
    await heldWith(within(rail).getByRole("button", { name: control }), WRITE_REASONS.not_served);
    for (const other of OPENERS.filter((opener) => opener.tool !== tool)) {
      const button = within(rail).getByRole("button", { name: other.control });
      expect(button.getAttribute("aria-disabled"), other.tool).not.toBe("true");
    }
    const find = within(rail).getByRole("button", { name: /^find a room$/i });
    expect(find.getAttribute("aria-disabled")).not.toBe("true");

    fireEvent.click(within(rail).getByRole("button", { name: control }));
    expect(screen.queryByRole("dialog")).toBeNull();
  },
);

test.each(READERS)("a new room is drawn for $who, held with why", async (reader) => {
  mountChat(<StartRoom people={[]} onClose={() => {}} />, { viewer: reader.viewer });
  fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: "launch" } });
  await heldWith(screen.getByRole("button", { name: /create/i }), reader.reason);
});

test.each(READERS)("a direct conversation is drawn for $who, held with why", async (reader) => {
  const people = [{ value: "bo", label: "Bo Chen", description: "bo", group: "People" }];
  mountChat(<StartDirect people={people} onClose={() => {}} />, { viewer: reader.viewer });
  pick(screen.getByRole("combobox", { name: /people in this conversation/i }), /Bo Chen/);
  await heldWith(screen.getByRole("button", { name: /^open$/i }), reader.reason);
});

test.each(READERS)("what a room is for is drawn for $who, held with why", async (reader) => {
  mountChat(<RoomSettings channel={channel as never} member onClose={() => {}} />, {
    viewer: reader.viewer,
  });
  fireEvent.change(screen.getByLabelText(/^Topic/), { target: { value: "Shipping October" } });
  await heldWith(screen.getByRole("button", { name: /^save$/i }), reader.reason);
});

// THE ROOM SEARCH IS CHATLET'S, so a reader who does not hold Chatlet — nobody
// signed in among them — is not asked for at all (`useSearchQuery`): no room
// comes back for a Join to be held on, and the dialog says what the search
// needs instead. Everybody else finds the room, and its Join is held with
// their reason.
const holdsChat = (reader: (typeof READERS)[number]) =>
  (reader.viewer.grants as readonly string[]).includes("chatlet");

/** The Find a room dialog, searched for "launch", answering one room. */
function browse(viewer: Record<string, unknown>) {
  const view = mountChat(<Browse onClose={() => {}} />, {
    viewer,
    answer: async (what) => {
      if (what === "chat_search") {
        return {
          hits: [{ message_id: "m-9", channel_id: "open-1", at: "", score: 1 }],
          searched: 1,
          read_level: "session",
          position: served.position,
        };
      }
      if (what === "chat_channel") {
        return {
          ...served,
          channel: { ...channel, id: "open-1", name: "launch" },
          revision: 1,
          message_seq: 1,
          member: false,
          members: [],
        };
      }
      return {};
    },
  });
  fireEvent.change(screen.getByRole("searchbox", { name: /find a room/i }), {
    target: { value: "launch" },
  });
  fireEvent.click(screen.getByRole("button", { name: /^search$/i }));
  return view;
}

test.each(READERS.filter(holdsChat))(
  "a room to join is drawn for $who, held with why",
  async (reader) => {
    browse(reader.viewer);
    await screen.findByText("#launch");
    await heldWith(screen.getByRole("button", { name: /^join$/i }), reader.reason);
  },
);

test.each(READERS.filter((reader) => !holdsChat(reader)))(
  "the room search is not asked for $who, and says what it needs",
  async (reader) => {
    const { socket } = browse(reader.viewer);
    expect(await screen.findByText(needsWords("Search", ["chatlet"]))).toBeTruthy();
    expect(socket.query.mock.calls.some(([what]) => what === "chat_search")).toBe(false);
    expect(screen.queryByRole("button", { name: /^join$/i })).toBeNull();
  },
);

/** A tab's read cursor, flushed at once. */
function Cursor({ onReady }: { onReady: (cursors: ReadCursors) => void }) {
  onReady(useReadCursors({ current: "ada" }));
  return null;
}

test.each(READERS)("the read cursor flushes nothing for $who", async (reader) => {
  // THE ONE CHAT WRITE WITH NO CONTROL: written in the background as the
  // reader reads. A reader the engine will never record a cursor for is sent
  // nothing — and the cursor is let go rather than re-sent every interval,
  // because nothing that reader can do from this tab changes the answer.
  expect(reader.reason).not.toBe("");
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  const socket = { query: vi.fn(async (what: string) => (what === "viewer" ? reader.viewer : {})) };
  let cursors: ReadCursors | null = null;
  render(
    <ToastProvider>
      <ClientContext.Provider value={{ store, socket } as never}>
        <ViewerProvider>
          <Cursor onReady={(held) => (cursors = held)} />
        </ViewerProvider>
      </ClientContext.Provider>
    </ToastProvider>,
  );
  await act(async () => {
    await new Promise((r) => setTimeout(r, 20));
  });
  cursors!.see("room-1", packPosition(served.position));
  await act(async () => {
    cursors!.flushNow();
    await new Promise((r) => setTimeout(r, 20));
  });
  expect(presses).toHaveLength(0);
});
