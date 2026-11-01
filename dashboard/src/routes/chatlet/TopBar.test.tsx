/**
 * The top bar's room actions, for every reader: what each offers, and what
 * each holds and why.
 *
 * EVERY CHAT WRITE IS DRAWN FOR A READER WHO CANNOT MAKE IT, AND SAYS WHY:
 * this is the suite `app/writeGate.test.tsx` names for the room's More menu
 * (Leave, the star) and the frame's star. Each is drawn for nobody signed in
 * (`WRITE_REASONS.anonymous`), a person without the Chatlet app, a person the
 * engine makes no chat change for and a credential acting as no seat
 * (`WRITE_REASONS.not_served`), and held with the sentence that says what
 * would change that, never hidden.
 */

import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";

import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { ADA, actEngine, applied } from "~/test/chat.tsx";
import { CHAT_ORG, SERVED, WRITE_READERS } from "~/test/chatAccess.ts";

import { mountInFrame, mountScreen } from "./frameKit.tsx";
import { RoomScopeProvider } from "./scope.tsx";
import { ChatTopBar } from "./TopBar.tsx";

const ROOM = "0b0e9a8e-2f4c-4f7e-9d8a-5d1f0c6b7a10";

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

const channel = {
  v: 1,
  id: ROOM,
  kind: "public",
  name: "general",
  created_at: "2026-01-01T00:00:00Z",
};

const answers = {
  chat_channels: {
    ...SERVED,
    read_state: true,
    read: { handle: "ada", revision: 1, cursors: {} },
    channels: [{ channel, revision: 1, unread: 0, mentions: 0 }],
  },
  chat_channel: {
    ...SERVED,
    channel,
    revision: 1,
    message_seq: 1,
    member: true,
    members: [{ handle: "ada", joined_at: "2026-01-01T00:00:00Z" }],
  },
};

/** The bar over the open room, as the frame draws it, for `viewer`. */
async function moreFor(viewer: Record<string, unknown>) {
  const mounted = mountInFrame(
    <RoomScopeProvider channelID={ROOM}>
      <ChatTopBar />
    </RoomScopeProvider>,
    { hash: `#/chatlet/${ROOM}`, viewer, answers },
  );
  mounted.store.applyOrg(CHAT_ORG as never);
  fireEvent.click(await screen.findByRole("button", { name: "More" }));
  return mounted;
}

/** A menu item, once the room's detail has decided what the menu holds. */
async function item(name: RegExp): Promise<HTMLElement> {
  return screen.findByRole("menuitem", { name });
}

/** An item held with `reason`: refusing the press, and saying why under its label. */
async function heldWithin(control: HTMLElement, reason: string): Promise<void> {
  await waitFor(() => expect(control.getAttribute("aria-disabled")).toBe("true"));
  expect(within(control).getByText(reason)).toBeTruthy();
}

test("the readers held are the ones the write gate names", () => {
  // NOT VACUOUS: the list draws for nobody signed in and for a reader the
  // engine makes no chat change for.
  const reasons = WRITE_READERS.map((r) => r.reason);
  expect(reasons).toContain(WRITE_REASONS.anonymous);
  expect(reasons).toContain(WRITE_REASONS.not_served);
});

/**
 * A credential acting as no seat is never shown a room at all: the screen is
 * its own sentence about the binding that would give it one, which the engine
 * would otherwise refuse on every read the bar is drawn from.
 */
const seatless = (viewer: Record<string, unknown>) => viewer.login !== "" && viewer.handle === "";

describe.each(WRITE_READERS.filter((r) => !seatless(r.viewer)))(
  "for $who",
  ({ viewer, reason }) => {
    test("Leave and the star are drawn and held with why", async () => {
      await moreFor(viewer);
      await heldWithin(await item(/^Leave #general/), reason);
      await heldWithin(await item(/^Add to Starred/), reason);
    });
  },
);

describe.each(WRITE_READERS.filter((r) => seatless(r.viewer)))("for $who", ({ viewer }) => {
  test("the screen draws the binding it needs, and no room action", async () => {
    mountScreen({ hash: `#/chatlet/${ROOM}`, viewer, answers });
    expect(await screen.findByText(/read as the seat you hold/i)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "More" })).toBeNull();
  });
});

describe("for a person in the room", () => {
  test("Leave and the star are offered, and the star is the frame's one write", async () => {
    const presses = actEngine((tool) => applied(tool));
    await moreFor(ADA);
    const leave = await item(/^Leave #general/);
    await waitFor(() => expect(leave.getAttribute("aria-disabled")).not.toBe("true"));
    fireEvent.click(await item(/^Add to Starred/));
    await waitFor(() =>
      expect(presses).toContainEqual(
        expect.objectContaining({ tool: "mark_chat_read", args: { star: [ROOM] } }),
      ),
    );
  });
});
