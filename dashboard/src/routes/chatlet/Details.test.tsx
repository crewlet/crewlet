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

import { ADA, ADA_SESSION, actEngine, applied, type Pressed } from "~/test/chat.tsx";
import { ENGINE_HEADERS } from "~/test/engineAnswer.ts";
import { CHAT_ORG, SERVED, WRITE_READERS, heldWith } from "~/test/chatAccess.ts";

import { DetailsPane, removalWords, withFollowing, withoutMember } from "./Details.tsx";
import { ChatDialogHost } from "./Dialogs.tsx";
import { mountInFrame } from "./frameKit.tsx";
import { useChatNav, type DetailsTab } from "./nav.tsx";
import { levelOf } from "./notify.tsx";
import { ARCHIVE_HELD, keptFor } from "./roomLifecycle.ts";

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
  waiting: [],
  waiting_capped: false,
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
    // EACH ROW'S OWN BUTTON, never the Remove beside it.
    const names = within(list)
      .getAllByRole("button")
      .filter((row) => !/^Remove /.test(row.getAttribute("aria-label") ?? ""))
      .map((row) => row.textContent ?? "");
    expect(names[0]).toMatch(/Add people/);
    expect(names[1]).toMatch(/Ada/);
    expect(names[2]).toMatch(/Bo Chen/);
    expect(names[3]).toMatch(/Ops Agent/);
    expect(within(list).getByText("(you)")).toBeTruthy();
    fireEvent.click(within(list).getByRole("button", { name: /^Bo Chen/ }));
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
    const toggle = await screen.findByRole("switch", { name: "Every message to Ops Agent" });
    // The follow rule leads the line, so a narrow row cuts the team instead.
    expect(screen.getByText(/^Answers when named · /)).toBeTruthy();
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
      name: "Every message to Ops Agent",
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
    const toggle = await screen.findByRole("switch", { name: "Every message to Ops Agent" });
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

  test("another member's row offers Remove, which asks first and states the membership without them", async () => {
    open("members");
    // THE READER'S OWN ROW HAS NONE: leaving is About's Leave.
    await screen.findByText("Bo Chen");
    expect(screen.queryByRole("button", { name: /^Remove Ada/ })).toBeNull();
    const remove = await screen.findByRole("button", { name: "Remove Bo Chen from #launch" });
    // HELD UNTIL THE ROOM'S OWN ANSWER SAYS IT TAKES THE CHANGE.
    await waitFor(() => expect(remove.getAttribute("aria-disabled")).toBeNull());
    fireEvent.click(remove);
    const dialog = await screen.findByRole("alertdialog", { name: "Remove Bo Chen from #launch?" });
    expect(within(dialog).getByText(/stop getting its messages/)).toBeTruthy();
    expect(presses.map((p) => p.tool)).not.toContain("set_channel_members");
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(presses.map((p) => p.tool)).toContain("set_channel_members"));
    const read = [
      { handle: "ops", follow_all: false },
      { handle: "bo", follow_all: false },
      { handle: "ada", follow_all: false },
    ];
    expect(presses.find((p) => p.tool === "set_channel_members")!.args).toEqual({
      channel: ROOM,
      members: [read[0], read[2]],
      expected_members: read,
    });
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
  });

  test("a team room's Remove is drawn and held, with why; a direct conversation draws none", async () => {
    open("members", { room: detail({ kind: "unit", unit: "Platform" }) });
    const remove = await screen.findByRole("button", { name: /^Remove Bo Chen from / });
    expect(remove.getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(remove);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    cleanup();
    open("members", { room: detail({ kind: "dm", name: undefined }) });
    await screen.findByText("Bo Chen");
    expect(screen.queryByRole("button", { name: /^Remove / })).toBeNull();
  });

  test.each(WRITE_READERS)("Remove is drawn for $who, held with why", async (reader) => {
    open("members", { viewer: reader.viewer });
    const remove = await screen.findByRole("button", { name: /^Remove Bo Chen/ });
    await waitFor(() => expect(remove.getAttribute("aria-disabled")).toBe("true"));
  });

  test("a removal restates every other member as read, and says what it costs by the room's kind", () => {
    expect(withoutMember([{ handle: "pm", follow_all: true }, { handle: "qa" }], "qa")).toEqual({
      members: [{ handle: "pm", follow_all: true }],
      expected_members: [
        { handle: "pm", follow_all: true },
        { handle: "qa", follow_all: false },
      ],
    });
    const bo = { kind: "human", name: "Bo Chen" } as Parameters<typeof removalWords>[1];
    expect(removalWords("private", bo)).toMatch(/^They can no longer read this room/);
    expect(removalWords("public", bo)).toMatch(/^They stop getting its messages/);
    const ops = { kind: "agent", name: "Ops Agent" } as Parameters<typeof removalWords>[1];
    expect(removalWords("public", ops)).toMatch(
      /^It stops getting its messages, and it leaves its room list/,
    );
    expect(removalWords("private", ops)).toMatch(/add it back\.$/);
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

// A ROOM'S LIFECYCLE AND RECORD (`RoomAdmin.tsx`): Archive is drawn for every
// reader of a named room and held, in the engine's own words, for anybody but
// its creator, its team's lead or an admin; how long it keeps messages and
// deleting older ones are an administrator's alone, each asked in the page
// and sent with its confirmation repeated. Mutation: let a member archive and
// the held case fails; send a retention without `?confirm=` and the
// retention case does; draw the destructions for everybody and the reader
// case does.
describe("its lifecycle and record", () => {
  /** An administrator: the Chatlet app and Admin. */
  const ADMIN = { ...ADA, grants: ["chatlet", "admin"] };

  /** The engine's REST answers for the room's own routes, recorded. */
  function restEngine() {
    const sent: { method: string; url: string; body: unknown }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init: RequestInit = {}) => {
        const headers = { ...ENGINE_HEADERS, "Content-Type": "application/json" };
        if (String(url).endsWith("/auth/session")) {
          return new Response(JSON.stringify(ADA_SESSION), { status: 200, headers });
        }
        sent.push({
          method: init.method ?? "GET",
          url: String(url),
          body: init.body ? JSON.parse(String(init.body)) : undefined,
        });
        return new Response(JSON.stringify({ channel_id: ROOM, outcome: "applied" }), {
          status: 200,
          headers,
        });
      }),
    );
    return sent;
  }

  test("its creator archives it, asked once, through update_channel", async () => {
    open("about");
    expect(await screen.findByText("No. It takes new messages.")).toBeTruthy();
    fireEvent.click(await screen.findByRole("button", { name: "Archive room" }));
    const dialog = await screen.findByRole("alertdialog", { name: /archive #launch/i });
    expect(within(dialog).getByText(/takes no new messages from anybody/)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Archive" }));
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]).toMatchObject({
      tool: "update_channel",
      args: { channel: ROOM, archived: true },
    });
  });

  test("an archived room says since when, and offers Reopen", async () => {
    open("about", { room: detail({ archived_at: "2026-11-02T10:00:00Z" }) });
    expect(await screen.findByText(/^Since .*takes no new messages\.$/)).toBeTruthy();
    fireEvent.click(await screen.findByRole("button", { name: "Reopen room" }));
    fireEvent.click(
      within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Reopen" }),
    );
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]).toMatchObject({ args: { channel: ROOM, archived: false } });
  });

  test("anybody but its creator, its team's lead or an admin is held, with why", async () => {
    open("about", { room: detail({ created_by: "bo" }) });
    await heldWith(await screen.findByRole("button", { name: "Archive room" }), ARCHIVE_HELD);
  });

  test("a reader who is not an admin is offered no destruction", async () => {
    open("about");
    await screen.findByText("Messages are kept");
    expect(screen.queryByRole("button", { name: /edit messages are kept/i })).toBeNull();
    expect(screen.queryByRole("button", { name: "Delete older messages" })).toBeNull();
  });

  test("an admin sets how long it keeps messages, confirmed as the engine checks it", async () => {
    open("about", { viewer: ADMIN, room: detail({ created_by: "bo" }) });
    const sent = restEngine();
    // AN ADMIN ARCHIVES ANY ROOM.
    await waitFor(async () =>
      expect(
        (await screen.findByRole("button", { name: "Archive room" })).getAttribute("aria-disabled"),
      ).not.toBe("true"),
    );
    fireEvent.click(await screen.findByRole("button", { name: /edit messages are kept/i }));
    const dialog = await screen.findByRole("dialog", { name: /how long #launch keeps/i });
    fireEvent.click(within(dialog).getByText("A number of days"));
    fireEvent.change(within(dialog).getByRole("textbox", { name: "Days" }), {
      target: { value: "90" },
    });
    const save = within(dialog).getByRole("button", { name: "Save" });
    // NOT BEFORE THE DELETION IT MAY CAUSE IS CONFIRMED.
    expect((save as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(within(dialog).getByRole("textbox", { name: /delete/i }), {
      target: { value: "delete" },
    });
    fireEvent.click(save);
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]!.method).toBe("PUT");
    expect(sent[0]!.url).toContain(`/chatlet/channels/${ROOM}/retention?confirm=90`);
    expect(sent[0]!.body).toEqual({ retention_days: 90 });
  });

  test("an admin deletes older messages once the room's name is typed back", async () => {
    open("about", { viewer: ADMIN });
    const sent = restEngine();
    fireEvent.click(await screen.findByRole("button", { name: "Delete older messages" }));
    const dialog = await screen.findByRole("alertdialog", { name: /delete older messages/i });
    fireEvent.change(within(dialog).getByLabelText("Delete messages before"), {
      target: { value: "2026-11-01" },
    });
    const press = within(dialog).getByRole("button", { name: "Delete" });
    expect((press as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(within(dialog).getByRole("textbox", { name: /launch/ }), {
      target: { value: "launch" },
    });
    fireEvent.click(press);
    await waitFor(() => expect(sent).toHaveLength(1));
    const url = new URL(sent[0]!.url, "http://engine");
    expect(url.pathname).toBe(`/chatlet/channels/${ROOM}/prune`);
    expect(url.searchParams.get("confirm")).toBe(url.searchParams.get("cutoff"));
    expect(Number.isNaN(Date.parse(url.searchParams.get("cutoff") ?? ""))).toBe(false);
  });
});
