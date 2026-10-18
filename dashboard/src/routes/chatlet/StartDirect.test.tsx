/**
 * Opening a direct conversation.
 *
 * THE CASE THIS SUITE IS FOR is the one that looks like a collision and is
 * not: a conversation's id is DERIVED from the people in it, so opening one
 * that already exists is that room — not an error, not a second room, and not
 * something to tell somebody about. The screen goes to it.
 *
 * Beside it is the same refusal-before-the-round-trip rule the name grammar
 * has: a conversation holds eight people counting the author, and the ninth is
 * said here rather than after a request.
 */

import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { pick } from "~/testing.tsx";
import { MAX_DIRECT_PARTICIPANTS } from "~/contract/chat.ts";
import {
  actEngine,
  applied,
  mountChat,
  unknown,
  type ActReply,
  type Pressed,
} from "~/test/chat.tsx";
import { StartDirect } from "./StartDirect.tsx";
import type { PersonOption } from "./people.ts";

const person = (handle: string, name: string): PersonOption => ({
  value: handle,
  label: name,
  description: handle,
  group: "People",
});

const people = [person("bo", "Bo Chen"), person("cas", "Cas Ito"), person("dev", "Dev Rao")];

let reply: ActReply;
let presses: Pressed[];

beforeEach(() => {
  location.hash = "#/";
  reply = applied("open_dm", { channel: "dm-1", kind: "dm", created: true });
  presses = actEngine(() => reply);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function mount(roster: PersonOption[] = people) {
  const onClose = vi.fn();
  mountChat(<StartDirect people={roster} onClose={onClose} />);
  return { onClose };
}

function field(): HTMLElement {
  return screen.getByRole("combobox", { name: /people in this conversation/i });
}

function open(): HTMLElement {
  return screen.getByRole("button", { name: /^open$/i });
}

/** Wait for the viewer read, which is what lets a press be sent at all. */
async function ready() {
  await waitFor(() => expect(open().getAttribute("aria-disabled")).not.toBe("true"));
}

test("a conversation that already exists is opened rather than reported", async () => {
  // `created: false` with an applied outcome is the ordinary answer: the id is
  // derived from the participants, so the loser of the race is handed the room
  // the winner made. Rendering that as a collision would make the one gesture
  // somebody repeats all day look like a failure.
  reply = applied("open_dm", { channel: "dm-1", kind: "dm", created: false });
  const { onClose } = mount();
  pick(field(), /Bo Chen/);
  await ready();
  fireEvent.click(open());

  await waitFor(() => expect(location.hash).toBe("#/chatlet/dm-1"));
  expect(onClose).toHaveBeenCalled();
  expect(screen.queryByText(/refused|could not/i)).toBeNull();
});

test("the people chosen are what travels, and the author is not among them", async () => {
  // The server completes the set with the caller's own seat: a press that
  // named it would be a caller naming a seat, which this surface refuses on
  // principle.
  mount();
  pick(field(), /Bo Chen/);
  pick(field(), /Cas Ito/);
  await ready();
  fireEvent.click(open());

  await waitFor(() => expect(presses).toHaveLength(1));
  expect(presses[0]).toMatchObject({ tool: "open_dm", args: { to: ["bo", "cas"] } });
});

test("an outcome nobody can establish does not open a conversation", async () => {
  reply = unknown("open_dm");
  const { onClose } = mount();
  pick(field(), /Bo Chen/);
  await ready();
  fireEvent.click(open());

  await waitFor(() => expect(screen.getByText(/nobody can say whether/i)).toBeTruthy());
  expect(location.hash).toBe("#/");
  expect(onClose).not.toHaveBeenCalled();
  // The repeat is safe and says why: the same people derive the same room.
  expect(screen.getByText(/same people/i)).toBeTruthy();
});

test("nobody chosen is nothing to open", async () => {
  mount();
  await waitFor(() => expect(open().getAttribute("aria-disabled")).toBe("true"));
  fireEvent.click(open());
  await new Promise((r) => setTimeout(r, 20));
  expect(presses).toHaveLength(0);
});

test("more people than a conversation holds is said before the round trip", async () => {
  // Eight INCLUDING the author, so seven others. Past that a conversation is a
  // room, and a room has a name somebody can join by — which is the engine's
  // own sentence and the gesture a person should make instead.
  const crowd = Array.from({ length: MAX_DIRECT_PARTICIPANTS }, (_, n) =>
    person(`seat-${n}`, `Seat ${n}`),
  );
  mount(crowd);
  for (const option of crowd) pick(field(), new RegExp(option.label));

  expect(screen.getByText(/at most 8/i)).toBeTruthy();
  await waitFor(() => expect(open().getAttribute("aria-disabled")).toBe("true"));
  fireEvent.click(open());
  await new Promise((r) => setTimeout(r, 20));
  expect(presses).toHaveLength(0);
});
