/**
 * Making a room, from the form a person actually fills in.
 *
 * Three claims, and each of them is a different way for a dialog to lie about
 * a write:
 *
 *   - a name the engine's own grammar refuses never leaves the browser, so
 *     somebody is told what is wrong while the caret is still in the field;
 *   - a name somebody else already holds is a name to change rather than a
 *     failure, because that is the create's arbitration working;
 *   - an outcome nobody can establish does not walk the reader into a room
 *     that may not exist — even though the engine's answer carries its id.
 *
 * The dialog is `@crewlethq/ui`'s Modal, which portals its surface to
 * `document.body`, so every query here goes through `screen` rather than the
 * container `render` hands back.
 */

import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import {
  actEngine,
  applied,
  mountChat,
  pending,
  refused,
  unknown,
  type ActReply,
  type Pressed,
} from "~/test/chat.tsx";
import { MAX_TOPIC_BYTES } from "~/contract/chat.ts";
import { StartRoom } from "./StartRoom.tsx";

let reply: ActReply;
let presses: Pressed[];

beforeEach(() => {
  location.hash = "#/";
  reply = applied("create_channel", { channel: "room-9", name: "launch", kind: "public" });
  presses = actEngine(() => reply);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function mount(defaultPrivate = false) {
  const onClose = vi.fn();
  mountChat(<StartRoom people={[]} defaultPrivate={defaultPrivate} onClose={onClose} />);
  return { onClose };
}

/** The name field, by the label the form wires to it. */
function nameField(): HTMLElement {
  return screen.getByLabelText(/^Name/);
}

function create(): HTMLElement {
  return screen.getByRole("button", { name: /create/i });
}

/** Name the room, and wait for the viewer read that lets a press be sent. */
async function named(value: string) {
  fireEvent.change(nameField(), { target: { value } });
  await waitFor(() => expect(create().getAttribute("aria-disabled")).not.toBe("true"));
}

test("a topic past its cap is held here, as the room's own Topic dialog holds it", async () => {
  // THE ENGINE'S CAP, IN ITS UNIT: sent, a topic past it was refused by the
  // engine after the round trip, where the Topic dialog would have stopped it
  // first. Held, with the count on the field and the reason on Create.
  mount();
  await named("launch");
  const topic = screen.getByLabelText(/^Topic/);
  fireEvent.change(topic, { target: { value: "🙂".repeat(MAX_TOPIC_BYTES / 4) } });
  await waitFor(() => expect(create().getAttribute("aria-disabled")).not.toBe("true"));
  fireEvent.change(topic, { target: { value: "x".repeat(MAX_TOPIC_BYTES + 1) } });
  expect(screen.getByText(/257 bytes, but a topic holds at most 256/)).toBeTruthy();
  expect(create().getAttribute("aria-disabled")).toBe("true");
  expect(create().getAttribute("title")).toBe("Shorten the topic first.");
  fireEvent.click(create());
  await new Promise((r) => setTimeout(r, 20));
  expect(presses).toHaveLength(0);
});

test("a name the grammar refuses is refused here, before anything is sent", async () => {
  mount();
  fireEvent.change(nameField(), { target: { value: "Product Launch!" } });
  // The first illegal character, named — and a space names the hyphen that
  // replaces it, because "invalid name" says nothing a person can act on.
  expect(screen.getByText(/can’t hold a space/i)).toBeTruthy();
  fireEvent.click(create());
  await new Promise((r) => setTimeout(r, 20));
  expect(presses).toHaveLength(0);
});

test("what will actually be claimed is shown, because a name is normalised", () => {
  // `#Launch` and `#launch` are one room: the name is lower-cased and trimmed
  // before it is arbitrated on. Finding that out from the rail afterwards
  // reads as the engine having renamed somebody's room.
  mount();
  fireEvent.change(nameField(), { target: { value: "Launch" } });
  expect(screen.getByText("#launch")).toBeTruthy();
});

test("a name somebody else holds comes back as a name to change", async () => {
  reply = refused("create_channel", "exists", "create_channel: chatlet: name taken: launch", 409);
  const { onClose } = mount();
  await named("launch");
  fireEvent.click(create());

  await waitFor(() => expect(screen.getByText(/already has that name/i)).toBeTruthy());
  // THE DIALOG STAYS OPEN ON THE FIELD SOMEBODY HAS TO CHANGE, with what they
  // typed still in it, and nothing is navigated to.
  expect(onClose).not.toHaveBeenCalled();
  expect((nameField() as HTMLInputElement).value).toBe("launch");
  expect(location.hash).toBe("#/");
});

test("an outcome nobody can establish does not open a room that may not exist", async () => {
  // Navigating on an answer nobody can vouch for is one plausible line away,
  // and the room screen would then tell somebody their new room was not found.
  reply = unknown("create_channel");
  const { onClose } = mount();
  await named("launch");
  fireEvent.click(create());

  await waitFor(() => expect(screen.getByText(/couldn’t confirm/i)).toBeTruthy());
  expect(location.hash).toBe("#/");
  expect(onClose).not.toHaveBeenCalled();
  // And it says how to ask again: the create arbitrates on the name, so a
  // repeat that is told the name is taken has found the room it made.
  expect(screen.getByText(/same name/i)).toBeTruthy();
});

test("a record this node has not applied is not drawn as a room to walk into", async () => {
  reply = pending("create_channel", { channel: "room-9" });
  mount();
  await named("launch");
  fireEvent.click(create());

  await waitFor(() => expect(screen.getByText(/not shown here yet/i)).toBeTruthy());
  expect(location.hash).toBe("#/");
});

test("a room that applied is where the reader ends up", async () => {
  const { onClose } = mount();
  await named("launch");
  fireEvent.click(create());

  await waitFor(() => expect(location.hash).toBe("#/chatlet/room-9"));
  expect(onClose).toHaveBeenCalled();
});

test("the create carries what the form holds, normalised, as the act tool's arguments", async () => {
  mount();
  await named("  Launch  ");
  fireEvent.change(screen.getByLabelText(/^Topic/), { target: { value: " shipping it " } });
  fireEvent.click(create());

  await waitFor(() => expect(presses).toHaveLength(1));
  expect(presses[0]!.tool).toBe("create_channel");
  expect(presses[0]!.args).toEqual({ channel: "launch", kind: "public", topic: "shipping it" });
});

/**
 * THE VISIBILITY SELECTOR OPENS ON WHAT WILL ACTUALLY HAPPEN.
 *
 * `chat.chatlet.default_channel_private` is what the server gives a room created
 * without a stated visibility. This form always states one, so the policy never
 * reaches the server from here -- which means the only thing standing between a
 * person and a room they did not intend is what this selector SHOWS them.
 *
 * It was hardcoded to "public". Under a company whose default is private, that
 * told the person the opposite of the policy at the exact moment they were
 * deciding it, and a wrong answer on a privacy control is not a cosmetic one.
 *
 * BOTH DIRECTIONS, because a default that can only be one value is not a
 * default: asserting the private case alone would pass against a form that made
 * everything private regardless.
 */
/**
 * The visibility control, by the label the form wires to it.
 *
 * ASSERTED ON ITS TEXT rather than a `value`, because the design system's
 * Select is a BUTTON that displays the chosen option's label — there is no
 * form value to read, and the text is what the person actually sees, which is
 * the whole subject of these two cases.
 */
function visibility(): HTMLElement {
  return screen.getByLabelText(/who can read it/i);
}

test("the form opens on public for a company that asked for nothing", () => {
  mount(false);
  expect(visibility().textContent).toContain("Public");
});

test("the form opens on private for a company whose default is private", () => {
  mount(true);
  expect(visibility().textContent).toContain("Private");
});
