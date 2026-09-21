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

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { StartRoom } from "./StartRoom.tsx";
import { createChannel } from "./writes.ts";
import type { WriteResult } from "./writes.ts";

vi.mock("./writes.ts", () => ({ createChannel: vi.fn() }));

const made = vi.mocked(createChannel);
const position = { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 42 };

beforeEach(() => {
  made.mockReset();
  location.hash = "#/";
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

function mount(defaultPrivate = false) {
  const onClose = vi.fn();
  const onWrote = vi.fn();
  render(
    <Router>
      <StartRoom people={[]} defaultPrivate={defaultPrivate} onClose={onClose} onWrote={onWrote} />
    </Router>,
  );
  return { onClose, onWrote };
}

/** The name field, by the label the form wires to it. */
function nameField(): HTMLElement {
  return screen.getByLabelText(/^Name/);
}

function create(): HTMLElement {
  return screen.getByRole("button", { name: /create/i });
}

function answered(outcome: "applied" | "pending" | "unknown"): WriteResult {
  return {
    outcome,
    answer: {
      outcome,
      op_id: "op-1",
      position,
      revision: 1,
      channel: {
        v: 1,
        id: "room-9",
        kind: "public",
        name: "launch",
        created_at: "2026-01-01T00:00:00Z",
      },
    },
    detail: "",
    code: "",
  };
}

test("a name the grammar refuses is refused here, before anything is sent", () => {
  mount();
  fireEvent.change(nameField(), { target: { value: "Product Launch!" } });
  // The first illegal character, named — and a space names the hyphen that
  // replaces it, because "invalid name" says nothing a person can act on.
  expect(screen.getByText(/space is not part of an address/i)).toBeTruthy();
  fireEvent.click(create());
  expect(made).not.toHaveBeenCalled();
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
  made.mockResolvedValue({
    outcome: "refused",
    answer: null,
    detail: "chat: invalid: name: #launch is already a room",
    code: "name_taken",
  });
  const { onClose } = mount();
  fireEvent.change(nameField(), { target: { value: "launch" } });
  fireEvent.click(create());

  await waitFor(() => expect(screen.getByText(/already a room/i)).toBeTruthy());
  // THE DIALOG STAYS OPEN ON THE FIELD SOMEBODY HAS TO CHANGE, with what they
  // typed still in it, and nothing is navigated to.
  expect(onClose).not.toHaveBeenCalled();
  expect((nameField() as HTMLInputElement).value).toBe("launch");
  expect(location.hash).toBe("#/");
});

test("an outcome nobody can establish does not open a room that may not exist", async () => {
  // The 504 carries the record the engine tried to write, id and all. Reading
  // that id off the answer is one plausible line away, and the room screen
  // would then tell somebody their new room was not found.
  made.mockResolvedValue(answered("unknown"));
  const { onClose } = mount();
  fireEvent.change(nameField(), { target: { value: "launch" } });
  fireEvent.click(create());

  await waitFor(() => expect(screen.getByText(/broker did not answer/i)).toBeTruthy());
  expect(location.hash).toBe("#/");
  expect(onClose).not.toHaveBeenCalled();
  // And it says how to ask again: the create arbitrates on the name, so a
  // repeat that is told the name is taken has found the room it made.
  expect(screen.getByText(/same name/i)).toBeTruthy();
});

test("a record this node has not applied is not drawn as a room to walk into", async () => {
  made.mockResolvedValue(answered("pending"));
  mount();
  fireEvent.change(nameField(), { target: { value: "launch" } });
  fireEvent.click(create());

  await waitFor(() => expect(screen.getByText(/has not applied it yet/i)).toBeTruthy());
  expect(location.hash).toBe("#/");
});

test("a room that applied is where the reader ends up", async () => {
  made.mockResolvedValue(answered("applied"));
  const { onClose, onWrote } = mount();
  fireEvent.change(nameField(), { target: { value: "launch" } });
  fireEvent.click(create());

  await waitFor(() => expect(location.hash).toBe("#/chat/room-9"));
  expect(onClose).toHaveBeenCalled();
  // AND THE RAIL IS ASKED AGAIN: the applier's own frame will say so a moment
  // later, but the person is looking at the list now.
  expect(onWrote).toHaveBeenCalled();
});

test("the create carries what the form holds, normalised", async () => {
  made.mockResolvedValue(answered("applied"));
  mount();
  fireEvent.change(nameField(), { target: { value: "  Launch  " } });
  fireEvent.change(screen.getByLabelText(/^Topic/), { target: { value: " shipping it " } });
  fireEvent.click(create());

  await waitFor(() => expect(made).toHaveBeenCalled());
  expect(made.mock.calls[0]![0]).toEqual({ name: "launch", kind: "public", topic: "shipping it" });
});

/**
 * THE VISIBILITY SELECTOR OPENS ON WHAT WILL ACTUALLY HAPPEN.
 *
 * `chat.native.default_channel_private` is what the server gives a room created
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
