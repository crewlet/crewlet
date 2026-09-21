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

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { pick } from "~/testing.tsx";
import { MAX_DIRECT_PARTICIPANTS, StartDirect } from "./StartDirect.tsx";
import type { PersonOption } from "./people.ts";
import { openDirect } from "./writes.ts";
import type { WriteResult } from "./writes.ts";
import type { ChatDirectAnswer } from "~/protocol/index.ts";

vi.mock("./writes.ts", () => ({ openDirect: vi.fn() }));

const opened = vi.mocked(openDirect);
const position = { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 7 };

const person = (handle: string, name: string): PersonOption => ({
  value: handle,
  label: name,
  description: handle,
  group: "People",
});

const people = [person("bo", "Bo Chen"), person("cas", "Cas Ito"), person("dev", "Dev Rao")];

beforeEach(() => {
  opened.mockReset();
  location.hash = "#/";
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

function mount(roster: PersonOption[] = people) {
  const onClose = vi.fn();
  const onWrote = vi.fn();
  render(
    <Router>
      <StartDirect people={roster} onClose={onClose} onWrote={onWrote} />
    </Router>,
  );
  return { onClose, onWrote };
}

function field(): HTMLElement {
  return screen.getByRole("combobox", { name: /people in this conversation/i });
}

function open(): HTMLElement {
  return screen.getByRole("button", { name: /^open$/i });
}

function answered(
  outcome: "applied" | "unknown",
  over: Partial<ChatDirectAnswer> = {},
): WriteResult<ChatDirectAnswer> {
  return {
    outcome,
    answer: {
      outcome,
      op_id: "op-1",
      position,
      revision: 1,
      channel: {
        v: 1,
        id: "dm-1",
        kind: "dm",
        created_at: "2026-01-01T00:00:00Z",
      },
      ...over,
    },
    detail: "",
    code: "",
  };
}

test("a conversation that already exists is opened rather than reported", async () => {
  // `created: false` with an applied outcome is the ordinary answer: the id is
  // derived from the participants, so the loser of the race is handed the room
  // the winner made. Rendering that as a collision would make the one gesture
  // somebody repeats all day look like a failure.
  opened.mockResolvedValue(answered("applied", { created: false }));
  const { onClose, onWrote } = mount();
  pick(field(), /Bo Chen/);
  fireEvent.click(open());

  await waitFor(() => expect(location.hash).toBe("#/chat/dm-1"));
  expect(onClose).toHaveBeenCalled();
  expect(onWrote).toHaveBeenCalled();
  expect(screen.queryByText(/refused|could not/i)).toBeNull();
});

test("the people chosen are what travels, and the author is not among them", async () => {
  // The server completes the set with the caller's own seat: a body that
  // named it would be a caller naming a seat, which this surface refuses on
  // principle.
  opened.mockResolvedValue(answered("applied"));
  mount();
  pick(field(), /Bo Chen/);
  pick(field(), /Cas Ito/);
  fireEvent.click(open());

  await waitFor(() => expect(opened).toHaveBeenCalled());
  expect(opened.mock.calls[0]![0]).toEqual(["bo", "cas"]);
});

test("an outcome nobody can establish does not open a conversation", async () => {
  opened.mockResolvedValue(answered("unknown"));
  const { onClose } = mount();
  pick(field(), /Bo Chen/);
  fireEvent.click(open());

  await waitFor(() => expect(screen.getByText(/broker did not answer/i)).toBeTruthy());
  expect(location.hash).toBe("#/");
  expect(onClose).not.toHaveBeenCalled();
  // The repeat is safe and says why: the same people derive the same room.
  expect(screen.getByText(/same people/i)).toBeTruthy();
});

test("nobody chosen is nothing to open", () => {
  mount();
  expect(open().hasAttribute("disabled")).toBe(true);
  fireEvent.click(open());
  expect(opened).not.toHaveBeenCalled();
});

test("more people than a conversation holds is said before the round trip", () => {
  // Eight INCLUDING the author, so seven others. Past that a conversation is a
  // room, and a room has a name somebody can join by — which is the engine's
  // own sentence and the gesture a person should make instead.
  const crowd = Array.from({ length: MAX_DIRECT_PARTICIPANTS }, (_, n) =>
    person(`seat-${n}`, `Seat ${n}`),
  );
  mount(crowd);
  for (const option of crowd) pick(field(), new RegExp(option.label));

  expect(screen.getByText(/at most 8/i)).toBeTruthy();
  expect(open().hasAttribute("disabled")).toBe(true);
  fireEvent.click(open());
  expect(opened).not.toHaveBeenCalled();
});
