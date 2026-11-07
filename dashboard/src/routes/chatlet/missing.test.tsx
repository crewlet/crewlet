/**
 * A room address that names no room the reader can see.
 *
 * THE INVARIANT: a room the engine answers `not_found` for (removed, never
 * made, a mistyped or stale link, a private room the reader is not in) is ONE
 * sentence with a way on. Drawn as a room, it said "Not found" twice (once for
 * its detail, once for its messages), titled the page with the raw id, put a
 * "couldn't be read" bar where the box goes, and kept the room's own top bar
 * actions for a room that is not there.
 *
 * The page's labels are recorded at the frame's hook, which the app's shell
 * turns into the trail and the tab's title.
 */

import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { QueryError } from "~/protocol/index.ts";

import { mountScreen } from "./frameKit.tsx";

const labels = vi.hoisted(() => ({ published: [] as Record<string, string>[] }));

vi.mock("~/app/Shell.tsx", async (importOriginal) => {
  const actual = await importOriginal<typeof import("~/app/Shell.tsx")>();
  return {
    ...actual,
    usePageLabels: (published: Record<string, string>) => {
      labels.published.push(published);
      actual.usePageLabels(published);
    },
  };
});

const GONE = "00000000-0000-0000-0000-000000000000";

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 12 },
};

/** The reader's rooms (none), and `not_found` for anything about the room. */
async function answer(what: string) {
  switch (what) {
    case "chat_channels":
      return {
        ...served,
        read_state: true,
        read: { handle: "ada", revision: 1, cursors: {} },
        channels: [],
      };
    case "chat_activity":
      return { ...served, items: [], unread: { mention: 0, reply: 0 }, read_state: true };
    case "chat_channel":
    case "chat_messages":
      throw new QueryError("not_found");
    default:
      throw new QueryError("unknown_query");
  }
}

beforeEach(() => {
  labels.published = [];
  localStorage.clear();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
  location.hash = "#/";
});

// Mutation: drop the missing room's own view and the refusal is said twice
// over a "couldn't be read" bar; drop the top bar's check and Search this
// room, Notifications and More are back.
test("a room that is not there is one sentence with a way on, and no room controls", async () => {
  mountScreen({ hash: `#/chatlet/${GONE}`, answer });
  expect(
    await screen.findByRole("heading", { name: "This room doesn’t exist, or you can’t see it" }),
  ).toBeTruthy();
  expect(screen.queryAllByText(/Not found\. It may have been removed/)).toHaveLength(0);
  expect(screen.queryByText(/couldn’t be read/)).toBeNull();
  expect(screen.queryByRole("textbox")).toBeNull();
  for (const control of ["Search this room", "Notifications", "More"]) {
    expect(screen.queryByRole("button", { name: control })).toBeNull();
  }
  expect(screen.getByRole("button", { name: /Back to Activity/ })).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: /Browse rooms/ }));
  await waitFor(() => expect(location.hash).toBe("#/chatlet/browse"));
});

// Mutation: withdraw the label for a room with no channel, as before, and the
// trail and the tab read the id somebody followed.
test("a room that is not there titles the page as not found, never by its id", async () => {
  mountScreen({ hash: `#/chatlet/${GONE}`, answer });
  await waitFor(() => expect(labels.published.at(-1)).toEqual({ [GONE]: "Room not found" }));
});
