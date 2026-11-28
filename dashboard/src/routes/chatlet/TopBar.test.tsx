/**
 * The top bar's room actions, for every reader: what each offers, and what
 * each holds and why.
 *
 * EVERY CHAT WRITE IS DRAWN FOR A READER WHO CANNOT MAKE IT, AND SAYS WHY:
 * this is the suite `app/writeGate.test.tsx` names for the room's More menu
 * (Leave, the star, Archive room) and the frame's star. Each is drawn for nobody signed in
 * (`WRITE_REASONS.anonymous`), a person without the Chatlet app, a person the
 * engine makes no chat change for and a credential acting as no seat
 * (`WRITE_REASONS.not_served`), and held with the sentence that says what
 * would change that, never hidden.
 */

import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";
import { useToast } from "@crewlethq/ui";

import { useOwnStar } from "~/app/Shell.tsx";
import { InsetToastProvider } from "~/app/toastInset.tsx";
import { WRITE_REASONS } from "~/lib/useWriteAccess.ts";
import { ADA, actEngine, applied } from "~/test/chat.tsx";
import { CHAT_ORG, SERVED, WRITE_READERS } from "~/test/chatAccess.ts";
import { ARCHIVE_HELD } from "./roomLifecycle.ts";

import { mountInFrame, mountScreen } from "./frameKit.tsx";
import { RoomScopeProvider } from "./scope.tsx";
import { ChatTopBar } from "./TopBar.tsx";

// THE FRAME'S OWN HOOK, watched: whether the bar tells the frame that the
// room's own menu carries the star (and so the page bar draws none).
vi.mock("~/app/Shell.tsx", async (actual) => ({
  ...(await actual<typeof import("~/app/Shell.tsx")>()),
  useOwnStar: vi.fn(),
}));

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
    waiting: [],
    waiting_capped: false,
    read: { handle: "ada", revision: 1, cursors: {} },
    channels: [{ channel, revision: 1, unread: 0 }],
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
    test("Leave, the star and Archive room are drawn and held with why", async () => {
      await moreFor(viewer);
      await heldWithin(await item(/^Leave #general/), reason);
      await heldWithin(await item(/^Add to Starred/), reason);
      // ARCHIVING IS ITS CREATOR'S, its team's lead's or an admin's, so the
      // room's own rule outranks the browser's for anybody else.
      await heldWithin(await item(/^Archive room/), ARCHIVE_HELD);
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

// ITS CREATOR ARCHIVES IT, asked first in a dialog; a reopen is the same
// press on an archived room. Mutation: send the press from the menu itself,
// or let any member archive, and this goes red.
describe("archiving the room", () => {
  test("its creator is offered Archive room, which asks before it archives", async () => {
    const presses = actEngine((tool) => applied(tool, { channel: ROOM }));
    const mine = { ...channel, created_by: "ada" };
    const mounted = mountInFrame(
      <RoomScopeProvider channelID={ROOM}>
        <ChatTopBar />
      </RoomScopeProvider>,
      {
        hash: `#/chatlet/${ROOM}`,
        viewer: ADA,
        answers: { ...answers, chat_channel: { ...answers.chat_channel, channel: mine } },
      },
    );
    mounted.store.applyOrg(CHAT_ORG as never);
    fireEvent.click(await screen.findByRole("button", { name: "More" }));
    const archive = await item(/^Archive room/);
    await waitFor(() => expect(archive.getAttribute("aria-disabled")).not.toBe("true"));
    fireEvent.click(archive);
    expect(presses).toHaveLength(0);
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

// MENU'S COUNT IS THE KIT BUTTON'S TRAILING PART, after the word and a gap,
// never inside the word's own box, where it was drawn flush against it
// ("Menu3"). Mutation: pass the count as a child again and this goes red.
test("Menu draws what waits as a count apart from its word", async () => {
  mountInFrame(<ChatTopBar />, {
    hash: "#/chatlet/activity",
    answers: {
      ...answers,
      chat_channels: {
        ...answers.chat_channels,
        channels: [{ channel, revision: 1, unread: 3 }],
        waiting: [{ channel_id: channel.id, root_id: "", count: 3, capped: false }],
      },
    },
  });
  const menu = await screen.findByRole("button", { name: "Menu, 3 waiting for you" });
  const label = menu.querySelector(".crewlet-btn__label");
  expect(label?.textContent).toBe("Menu");
  expect(label?.nextElementSibling?.textContent).toBe("3");
});

test("the Messages switcher's foot leads to the Direct messages page", async () => {
  mountInFrame(<ChatTopBar />, { hash: "#/chatlet/activity", answers });
  fireEvent.click(
    await screen.findByRole("button", { name: /^Direct messages|Switch conversation/ }),
  );
  const page = await screen.findByRole("option", { name: /All direct messages/ });
  fireEvent.click(page);
  await waitFor(() => expect(location.hash).toBe("#/chatlet/dms"));
});

// THE SWITCHER NAMES THE ROOM THE READER IS IN, whether or not it is one of
// the rooms it lists: those are the reader's memberships, unarchived, and a
// team's room read from outside the team or an archived room is neither. Read
// off that list, the trigger fell back to "Rooms" in exactly those rooms.
describe("the Rooms switcher names the open room", () => {
  const TEAM = "5a1c9e2b-7d3f-4c8e-a1b2-c3d4e5f60718";
  const ARCHIVED = "6b2d0f3c-8e4a-4d9f-b2c3-d4e5f6071829";
  const team = {
    v: 1,
    id: TEAM,
    kind: "unit",
    name: "platform",
    unit: "Platform",
    created_at: "2026-01-01T00:00:00Z",
  };
  const archived = {
    ...channel,
    id: ARCHIVED,
    name: "old-launch",
    archived_at: "2026-02-01T00:00:00Z",
  };
  const opened = (room: typeof channel, member: boolean, rail: unknown[]) => ({
    ...answers,
    chat_channels: { ...answers.chat_channels, channels: rail },
    chat_channel: {
      ...answers.chat_channel,
      channel: room,
      member,
      members: member ? answers.chat_channel.members : [],
    },
  });
  // THE FIRST SWITCHER IN THE BAR is Rooms; its trigger's words are the name.
  const trigger = async () =>
    waitFor(() => {
      const button = document.querySelector<HTMLElement>(".chat-switch button");
      expect(button).not.toBeNull();
      return button!;
    });

  test("a team's room the reader is outside of, by its team's name", async () => {
    mountInFrame(
      <RoomScopeProvider channelID={TEAM}>
        <ChatTopBar />
      </RoomScopeProvider>,
      {
        hash: `#/chatlet/${TEAM}`,
        answers: opened(team as never, false, [{ channel, revision: 1, unread: 0 }]),
      },
    );
    await waitFor(async () => expect((await trigger()).textContent).toMatch(/platform/i));
    expect((await trigger()).textContent).not.toMatch(/^Rooms/);
  });

  test("an archived room, by its name", async () => {
    mountInFrame(
      <RoomScopeProvider channelID={ARCHIVED}>
        <ChatTopBar />
      </RoomScopeProvider>,
      {
        hash: `#/chatlet/${ARCHIVED}`,
        answers: opened(archived, true, [
          { channel: archived, revision: 1, unread: 0 },
          { channel, revision: 1, unread: 0 },
        ]),
      },
    );
    await waitFor(async () => expect((await trigger()).textContent).toMatch(/#old-launch/));
  });
});

// ONE STAR, AND SO ONE "…", ON A ROOM: its own More carries the room's star, so
// the page bar draws none, which on a phone was a "…" of its own stacked over
// the room's and a page bar of two rows. A view beside the rooms has no More,
// and keeps the frame's star.
test("on a room the bar tells the frame its own More carries the star; on a view it does not", async () => {
  const owns = vi.mocked(useOwnStar);
  owns.mockClear();
  mountInFrame(
    <RoomScopeProvider channelID={ROOM}>
      <ChatTopBar />
    </RoomScopeProvider>,
    { hash: `#/chatlet/${ROOM}`, answers },
  );
  await screen.findByRole("button", { name: "More" });
  expect(owns).toHaveBeenCalled();
  cleanup();
  owns.mockClear();
  mountInFrame(<ChatTopBar />, { hash: "#/chatlet/activity", answers });
  await screen.findByRole("button", { name: "Search Chatlet" });
  expect(owns).not.toHaveBeenCalled();
});

// ONE PLACE FOR A TOAST ON EVERY SCREEN, the window's foot: the bar stood the
// stack at the top right under itself, on Chatlet alone, where it lay over an
// open pane. Mutation: claim the top again, and the stack's edge is the top.
test("the bar leaves the toast stack at the window's foot, where every screen has it", async () => {
  function Raise() {
    const toast = useToast();
    return (
      <button type="button" onClick={() => toast.show({ variant: "success", title: "Starred" })}>
        raise
      </button>
    );
  }
  mountInFrame(
    <InsetToastProvider>
      <ChatTopBar />
      <Raise />
    </InsetToastProvider>,
    { hash: "#/chatlet/activity", answers },
  );
  await screen.findByRole("button", { name: "Search Chatlet" });
  fireEvent.click(screen.getByRole("button", { name: "raise" }));
  await screen.findByText("Starred");
  const stack = screen.getByText("Starred").closest<HTMLElement>(".crewlet-toaster")!;
  expect(stack.className).toMatch(/crewlet-toaster--bottom-right/);
});
