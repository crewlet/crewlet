/**
 * A room's details pane: About, Members and Notifications.
 *
 * THE INVARIANTS: Edit is offered beside a topic and a description only where
 * the engine would take the change, and opens the room's own dialog; Members
 * is the room's answer, people before agents, with Add people only where the
 * engine adds anybody; Leave and the notification levels are drawn for every
 * reader and held with why (`app/writeGate.test.tsx` names this suite for
 * `Details.tsx` and `notify.tsx`); a level is the reader's own mark, written
 * through `mark_chat_read`.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { ADA, actEngine, applied, type Pressed } from "~/test/chat.tsx";
import { CHAT_ORG, SERVED, WRITE_READERS, heldWith } from "~/test/chatAccess.ts";

import { DetailsPane, keptFor, withFollowing } from "./Details.tsx";
import { ChatDialogHost } from "./Dialogs.tsx";
import { mountInFrame } from "./frameKit.tsx";
import { useChatNav, type DetailsTab } from "./nav.tsx";
import { levelOf } from "./notify.tsx";

const ROOM = "room-1";

const detail = (channel: Record<string, unknown> = {}, extra: Record<string, unknown> = {}) => ({
  ...SERVED,
  channel: {
    v: 1,
    id: ROOM,
    kind: "public",
    name: "launch",
    created_at: "2026-10-01T00:00:00Z",
    created_by: "ada",
    ...channel,
  },
  revision: 1,
  message_seq: 3,
  member: true,
  members: [
    { handle: "ops", joined_at: "2026-10-01T00:00:00Z" },
    { handle: "bo", joined_at: "2026-10-01T00:00:00Z" },
    { handle: "ada", joined_at: "2026-10-01T00:00:00Z" },
  ],
  ...extra,
});

const rail = {
  ...SERVED,
  read_state: true,
  read: { handle: "ada", revision: 2, cursors: {}, every: [], muted: [] },
  channels: [],
};

/** The open dialog, as the screen draws it from the navigation. */
function Dialog() {
  const nav = useChatNav();
  return <ChatDialogHost dialog={nav.dialog} onClose={nav.closeDialog} />;
}

let presses: Pressed[];

beforeEach(() => {
  presses = actEngine((tool) => applied(tool, { channel: ROOM, read: rail.read }));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function open(
  tab: DetailsTab,
  {
    viewer = ADA as Record<string, unknown>,
    room = detail(),
  }: { viewer?: Record<string, unknown>; room?: ReturnType<typeof detail> } = {},
) {
  const mounted = mountInFrame(
    <>
      <DetailsPane room={ROOM} tab={tab} onClose={() => {}} />
      <Dialog />
    </>,
    {
      hash: `#/chatlet/${ROOM}?pane=details:${ROOM}:${tab}`,
      viewer,
      answers: {
        chat_channel: room,
        chat_channels: rail,
        chat_links: { channel_id: ROOM, links: [] },
      },
    },
  );
  act(() => mounted.store.applyOrg(CHAT_ORG as never));
  return mounted;
}

describe("About", () => {
  test("how long a room keeps what is said: for ever, a count, or the company's", () => {
    expect(keptFor(0)).toBe("For ever");
    expect(keptFor(30)).toBe("30 days");
    expect(keptFor(null)).toBe("As long as the company default");
    expect(keptFor(undefined)).toBe("As long as the company default");
  });

  test("a member is offered Add a topic, which opens the room's own dialog", async () => {
    open("about");
    fireEvent.click(await screen.findByRole("button", { name: /add a topic/i }));
    expect(await screen.findByRole("dialog", { name: /add a topic/i })).toBeTruthy();
  });

  test("a reader outside a private room is told there is no topic, and offered no Edit", async () => {
    open("about", { room: detail({ kind: "private" }, { member: false }) });
    expect(await screen.findByText("No topic yet")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /add a topic|edit topic/i })).toBeNull();
  });

  test.each(WRITE_READERS)("Leave is drawn for $who, held with why", async (reader) => {
    open("about", { viewer: reader.viewer });
    await heldWith(await screen.findByRole("button", { name: /leave room/i }), reader.reason);
  });

  test("Leave leaves, and takes the reader out of the room", async () => {
    open("about");
    fireEvent.click(await screen.findByRole("button", { name: /leave room/i }));
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]).toMatchObject({ tool: "leave_channel", args: { channel: ROOM } });
    await waitFor(() => expect(location.hash).not.toContain(ROOM));
  });

  test("a team room offers no Leave: its members are the org chart's", async () => {
    open("about", { room: detail({ kind: "unit", unit: "Platform" }) });
    expect(await screen.findByText(/Membership follows the org chart/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /leave room/i })).toBeNull();
  });
});

describe("Members", () => {
  test("people come before agents, the reader is marked, and a member opens their profile", async () => {
    open("members");
    const list = await screen.findByRole("list");
    const names = within(list)
      .getAllByRole("button")
      .map((row) => row.textContent ?? "");
    expect(names[0]).toMatch(/Add people/);
    expect(names[1]).toMatch(/Ada/);
    expect(names[2]).toMatch(/Bo Chen/);
    expect(names[3]).toMatch(/Ops Agent/);
    expect(within(list).getByText("(you)")).toBeTruthy();
    fireEvent.click(within(list).getByRole("button", { name: /Bo Chen/ }));
    await waitFor(() => expect(decodeURIComponent(location.hash)).toContain("pane=profile:bo"));
  });

  test("Add people opens the room's dialog", async () => {
    open("members");
    fireEvent.click(await screen.findByRole("button", { name: /add people/i }));
    expect(await screen.findByRole("dialog", { name: /add people to #launch/i })).toBeTruthy();
  });

  test("a team room offers nobody to add", async () => {
    open("members", { room: detail({ kind: "unit", unit: "Platform" }) });
    await screen.findByText("Bo Chen");
    expect(screen.queryByRole("button", { name: /add people/i })).toBeNull();
  });

  test("an agent says how it follows the room, and its switch states the membership against the read", async () => {
    open("members", {
      room: detail(
        {},
        {
          members: [
            { handle: "ops", follow_all: false, joined_at: "2026-10-01T00:00:00Z" },
            { handle: "bo", joined_at: "2026-10-01T00:00:00Z" },
            { handle: "ada", joined_at: "2026-10-01T00:00:00Z" },
          ],
        },
      ),
    });
    const toggle = await screen.findByRole("switch", { name: "Ops Agent gets every message" });
    expect(screen.getByText(/Answers when named/)).toBeTruthy();
    // A PERSON IS NEVER WOKEN by a message, so a person's row has no switch.
    expect(screen.getAllByRole("switch")).toHaveLength(1);
    // HELD UNTIL THE ROOM'S OWN ANSWER SAYS IT TAKES THE CHANGE.
    await waitFor(() => expect(toggle.getAttribute("aria-disabled")).toBeNull());
    fireEvent.click(toggle);
    await waitFor(() => expect(presses.map((p) => p.tool)).toContain("set_channel_members"));
    const sent = presses.find((p) => p.tool === "set_channel_members")!.args;
    const read = [
      { handle: "ops", follow_all: false },
      { handle: "bo", follow_all: false },
      { handle: "ada", follow_all: false },
    ];
    expect(sent).toEqual({
      channel: ROOM,
      members: [{ handle: "ops", follow_all: true }, read[1], read[2]],
      expected_members: read,
    });
  });

  test("a team room's agents follow it as the org chart says: the switch shows it, stays put and says why", async () => {
    open("members", {
      room: detail(
        { kind: "unit", unit: "Platform" },
        {
          members: [
            { handle: "ops", source: "org", follow_all: true, joined_at: "2026-10-01T00:00:00Z" },
            { handle: "ada", source: "org", joined_at: "2026-10-01T00:00:00Z" },
          ],
        },
      ),
    });
    const toggle = (await screen.findByRole("switch", {
      name: "Ops Agent gets every message",
    })) as HTMLInputElement;
    expect(toggle.checked).toBe(true);
    expect(toggle.getAttribute("aria-disabled")).toBe("true");
    const reason = document.getElementById(toggle.getAttribute("aria-describedby") ?? "");
    expect(reason?.textContent).toMatch(/The org chart sets this/);
    fireEvent.click(toggle);
    expect(presses.map((p) => p.tool)).not.toContain("set_channel_members");
  });

  test.each(WRITE_READERS)("an agent's switch is drawn for $who, held with why", async (reader) => {
    open("members", { viewer: reader.viewer });
    const toggle = await screen.findByRole("switch", { name: "Ops Agent gets every message" });
    const reason = document.getElementById(toggle.getAttribute("aria-describedby") ?? "");
    await waitFor(() => expect(reason?.textContent).toBe(reader.reason));
  });

  test("a following switch restates every member as read, and moves only the one", () => {
    expect(
      withFollowing([{ handle: "pm", follow_all: true }, { handle: "qa" }], "qa", true),
    ).toEqual({
      members: [
        { handle: "pm", follow_all: true },
        { handle: "qa", follow_all: true },
      ],
      expected_members: [
        { handle: "pm", follow_all: true },
        { handle: "qa", follow_all: false },
      ],
    });
  });

  test("finding a member narrows the list, and says so when nobody matches", async () => {
    open("members");
    const find = await screen.findByRole("textbox", { name: /find members/i });
    fireEvent.change(find, { target: { value: "zz" } });
    expect(await screen.findByText(/Nobody in #launch matches/)).toBeTruthy();
  });
});

describe("Notifications", () => {
  test("a level is read off the reader's own marks", () => {
    expect(levelOf({ muted: ["r"], every: [] } as never, "r")).toBe("muted");
    expect(levelOf({ every: ["r"] } as never, "r")).toBe("all");
    expect(levelOf({} as never, "r")).toBe("mentions");
    expect(levelOf(null, "r")).toBe("mentions");
  });

  test("Every message is the reader's mark, written through mark_chat_read", async () => {
    open("notifications");
    fireEvent.click(await screen.findByRole("radio", { name: /every message/i }));
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]).toMatchObject({
      tool: "mark_chat_read",
      args: { notify: { [ROOM]: "all" } },
    });
  });

  test.each(WRITE_READERS)("the levels are drawn for $who, held with why", async (reader) => {
    open("notifications", { viewer: reader.viewer });
    await heldWith(await screen.findByRole("radio", { name: /every message/i }), reader.reason);
  });
});
