/**
 * EVERY CHAT WRITE IS DRAWN FOR A READER WHO CANNOT MAKE IT, AND SAYS WHY.
 *
 * A chat change is a press through the act transport, made as the principal
 * the session resolves to, as the seat it holds (ADR-0024), so nobody signed
 * in, a person without the Chatlet app, a person the engine does not make the
 * change for, or a credential acting as no seat can say nothing, join nothing
 * and make no room. The controls are DRAWN for all four and held with the
 * sentence that says what would change that (`lib/useWriteAccess.ts`): a
 * composer that vanished would teach somebody the product cannot do it, and
 * one that said nothing would teach them it is broken.
 *
 * This is the suite `app/writeGate.test.tsx` names for the room, the outbox,
 * the read cursor and a room's message scope: it renders each of their
 * controls for each of the four, and holds the one write with no control (the
 * read cursor's background flush) to sending nothing at all. The room's own
 * gestures on the top bar and in its details are the top bar's and the
 * details' suites', and the openers' are the dialogs' and the views'.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { ToastProvider } from "@crewlethq/ui";

import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { Store } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";
import { ADA, actEngine, mountChat, type Pressed } from "~/test/chat.tsx";
import { WRITE_READERS, heldWith, holdOf } from "~/test/chatAccess.ts";
import { LoneRoom } from "./testkit.tsx";

import { packPosition, ReadCursors, useReadCursors } from "./cursor.ts";
import { mountInFrame } from "./frameKit.tsx";
import { ThreadPane } from "./Thread.tsx";

const READERS = WRITE_READERS;

/** A room's address is its uuid (`app/routes.ts`). */
const ROOM = "0b0e9a8e-2f4c-4f7e-9d8a-5d1f0c6b7a10";

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 4 },
};

const channel = {
  v: 1,
  id: ROOM,
  kind: "public",
  name: "general",
  topic: "Shipping",
  created_at: "2026-01-01T00:00:00Z",
};

const detail = {
  ...served,
  channel,
  revision: 1,
  message_seq: 1,
  member: true,
  members: [],
};

const said = {
  message: {
    v: 1,
    id: "m-1",
    channel_id: ROOM,
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

/** The room alone, read as `viewer`, its one message the reader's own. */
async function mountRoom(viewer: Record<string, unknown>): Promise<HTMLElement> {
  const cursors = new ReadCursors({ flush: async () => undefined });
  mountChat(
    <LoneRoom channelID={ROOM} viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
    {
      viewer,
      answers: {
        chat_channel: detail,
        chat_messages: { ...served, channel_id: ROOM, messages: [said] },
      },
    },
  );
  await screen.findByText("morning");
  return document.querySelector<HTMLElement>('article[data-message-id="m-1"]')!;
}

test.each(READERS)("a room's Join is drawn for $who, held with why", async (reader) => {
  // A ROOM THE READER MAY READ AND IS NOT IN draws a Join bar in the box's
  // place, held as every write here is, and pressing it sends nothing.
  const cursors = new ReadCursors({ flush: async () => undefined });
  mountChat(
    <LoneRoom channelID={ROOM} viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
    {
      viewer: reader.viewer,
      answers: {
        chat_channel: { ...detail, member: false },
        chat_messages: { ...served, channel_id: ROOM, messages: [said] },
      },
    },
  );
  const join = await screen.findByRole("button", { name: /^join room$/i });
  await saysHeld(join, reader.reason);
  fireEvent.click(join);
  await new Promise((r) => setTimeout(r, 20));
  expect(presses).toHaveLength(0);
});

/** A message's More menu item, its menu left open to press it. */
function moreItem(row: HTMLElement, name: RegExp): HTMLElement {
  fireEvent.click(within(row).getByRole("button", { name: "More actions" }));
  const menus = screen.getAllByRole("menu");
  return within(menus[menus.length - 1]!).getByRole("menuitem", { name });
}

/**
 * A control held with `reason`, wherever the kit says it: a title, a menu
 * item's description, or a reaction's hint, which opens on the pointer.
 */
async function saysHeld(control: HTMLElement, reason: string) {
  fireEvent.pointerEnter(control);
  await vi.waitFor(() => expect(holdOf(control) ?? "").toContain(reason));
}

/** Close every open menu, so the next one read is the only one open. */
function closeMenus() {
  for (const menu of screen.queryAllByRole("menu")) fireEvent.keyDown(menu, { key: "Escape" });
}

test.each(READERS)("a room's every write is drawn for $who, held with why", async (reader) => {
  const row = await mountRoom(reader.viewer);

  // THE COMPOSER: the box stays open (it is the browser that cannot send,
  // not the room that takes nothing) and Send is held with the sentence.
  const box = screen.getByRole("textbox", { name: /^Message/ }) as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "morning again" } });
  await heldWith(screen.getByRole("button", { name: /^send$/i }), reader.reason);
  expect(box.disabled).toBe(false);
  fireEvent.keyDown(box, { key: "Enter" });

  // A MESSAGE'S CHANGES: its reaction, and its edit and delete in More.
  await saysHeld(within(row).getByRole("button", { name: /:eyes:/ }), reader.reason);
  await saysHeld(moreItem(row, /^edit message/i), reader.reason);
  closeMenus();
  const remove = moreItem(row, /^delete message/i);
  await saysHeld(remove, reader.reason);

  fireEvent.click(remove);
  fireEvent.click(within(row).getByRole("button", { name: /:eyes:/ }));
  await new Promise((r) => setTimeout(r, 20));
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(presses).toHaveLength(0);
});

/**
 * The readers a thread pane is ever drawn for. A credential acting as no seat
 * gets no chat screen at all, only the sentence that it holds no seat
 * (`Chat.test.tsx`), so no room it reads is ever open to reply in.
 */
const SEATED = READERS.filter(({ viewer }) => !(viewer.login !== "" && viewer.handle === ""));

test.each(SEATED)("a thread's reply is drawn for $who, held with why", async (reader) => {
  // THE THREAD'S COMPOSER IS A COMPOSER LIKE THE ROOM'S: the box stays open,
  // since it is the browser that cannot send, and Send is held with the
  // sentence `reply_in_thread` is held with for this reader. Nothing is sent.
  mountInFrame(<ThreadPane room={ROOM} root="m-1" onClose={() => {}} />, {
    hash: `#/chatlet/${ROOM}?pane=thread:${ROOM}:m-1`,
    viewer: reader.viewer,
    answers: {
      chat_channels: { ...served, read_state: true, channels: [] },
      chat_channel: detail,
      chat_thread: { ...served, channel_id: ROOM, root: said, replies: [] },
    } as never,
  });
  const box = (await screen.findByRole("textbox", {
    name: "Reply in the thread",
  })) as HTMLTextAreaElement;
  const form = box.closest<HTMLElement>(".chat-pane-foot")!;
  fireEvent.change(box, { target: { value: "and in the thread" } });
  await heldWith(within(form).getByRole("button", { name: /^send$/i }), reader.reason);
  expect(box.disabled).toBe(false);
  fireEvent.keyDown(box, { key: "Enter" });
  await new Promise((r) => setTimeout(r, 20));
  expect(presses).toHaveLength(0);
});

test("a reader who cannot react gets the tray's reactions held, the reason in each hint", async () => {
  // THE TRAY'S REACTIONS ARE HELD AS THE CHIPS ARE: drawn, refusing the
  // press, and saying why in the hint, never left pressable for a reaction
  // the engine would refuse.
  const row = await mountRoom({ ...ADA, acts: ADA.acts.filter((t) => t !== "react_to_message") });
  const quick = within(row).getAllByRole("button", { name: /^React with/ });
  expect(quick.length).toBeGreaterThan(0);
  // THE TRAY'S AND THE REACTION BAR'S Add a reaction, both drawn and held.
  const add = within(row).getAllByRole("button", { name: "Add a reaction" });
  expect(add).toHaveLength(2);
  for (const control of [...quick, ...add]) {
    await saysHeld(control, WRITE_REASONS.not_served);
  }
  fireEvent.click(quick[0]!);
  await new Promise((r) => setTimeout(r, 20));
  expect(presses).toHaveLength(0);
});

/** A message's three changes, and how each control is found on its row. */
const MESSAGE_CHANGES: { tool: string; control: (row: HTMLElement) => HTMLElement }[] = [
  { tool: "edit_message", control: (row) => moreItem(row, /^edit message/i) },
  { tool: "delete_message", control: (row) => moreItem(row, /^delete message/i) },
  {
    tool: "react_to_message",
    control: (row) => within(row).getByRole("button", { name: /:eyes:/ }),
  },
];

test.each(MESSAGE_CHANGES)(
  "a message's $tool is held by its own tool, never by the composer's",
  async ({ tool, control }) => {
    // THE ENGINE SERVES EACH CHANGE TO A PERSON ON ITS OWN (`viewer.acts`),
    // so a person it posts for and does not make THIS change for is told so
    // on this control alone, and the others, the composer included, stay
    // theirs to press. Held by the composer's access, every control here
    // would be pressable for this person and refused by the engine.
    const row = await mountRoom({ ...ADA, acts: ADA.acts.filter((t) => t !== tool) });
    await saysHeld(control(row), WRITE_REASONS.not_served);
    closeMenus();
    for (const other of MESSAGE_CHANGES.filter((change) => change.tool !== tool)) {
      expect(holdOf(other.control(row)), other.tool).toBeNull();
      closeMenus();
    }
    const send = screen.getByRole("button", { name: /^send$/i });
    fireEvent.change(screen.getByRole("textbox", { name: /^Message/ }), {
      target: { value: "still mine to say" },
    });
    await vi.waitFor(() => expect(holdOf(send)).toBeNull());

    fireEvent.click(control(row));
    await new Promise((r) => setTimeout(r, 20));
    expect(presses).toHaveLength(0);
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
