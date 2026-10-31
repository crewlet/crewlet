/**
 * The chat screen's frame: who it is for, the Rooms panel, the top bar, the
 * keys, and what the screen holds above every room.
 *
 * The three shapes a reader can arrive in are not degrees of the same thing: a
 * reader without the Chatlet app, a credential acting as no seat, and a person.
 * The first is the frame's: the chat row names its app, so `app/App.test.tsx`
 * holds a room to the one refusal. The middle one is this screen's: an
 * ordinary state with a one-binding remedy, and the case this screen exists
 * to be clear about. The engine refuses every chat read to it, and if this
 * screen does not say what would change that, nothing does.
 */

import { act as flush, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import { workspaceRow } from "~/app/nav.ts";
import { MAX_READER_MARKS } from "~/contract/chat.ts";
import { href, samePath } from "~/app/router.tsx";
import { currentReader, noteReader } from "~/lib/reader.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { QueryError, type QueryName } from "~/protocol/index.ts";
import { ADA, ADA_SESSION, actEngine, applied, mountChat, unknown } from "~/test/chat.tsx";
import { fakeTransition } from "~/test/motion.ts";

import { READ_FLUSH_MS, useReadCursors, type ReadCursors } from "./cursor.ts";
import { mountScreen } from "./frameKit.tsx";
import { resetDockForTest } from "./nav.tsx";

const GENERAL = "0b0e9a8e-2f4c-4f7e-9d8a-5d1f0c6b7a10";
const LAUNCH = "6a3c1e2b-8d4f-4a9e-b7c6-1f2e3d4c5b6a";
const PLATFORM = "c1d2e3f4-a5b6-4c7d-8e9f-0a1b2c3d4e5f";
const WITH_BO = "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d";

beforeEach(() => {
  localStorage.clear();
  resetDockForTest();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  sessionStorage.clear();
  localStorage.clear();
  location.hash = "#/";
});

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 12 },
};

/** One of the reader's rooms, as `chat_channels` answers it. */
function room(id: string, name: string, over: Record<string, unknown> = {}) {
  const { channel, ...rest } = over as { channel?: Record<string, unknown> };
  return {
    channel: {
      v: 1,
      id,
      kind: "public",
      name,
      created_at: "2026-01-01T00:00:00Z",
      ...channel,
    },
    revision: 3,
    unread: 0,
    mentions: 0,
    last: {
      message_id: `${id}-last`,
      channel_seq: 4,
      at: "2026-01-01T00:04:00Z",
      position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 40 },
    },
    ...rest,
  };
}

/** The reader's rooms, measured, with these rows. */
function rail(channels: unknown[], read: Record<string, unknown> = {}) {
  return {
    ...served,
    read_state: true,
    read: { handle: "ada", revision: 1, cursors: {}, ...read },
    channels,
  };
}

/** One room's detail, as `chat_channel` answers it. */
function detail(id: string, name: string, over: Record<string, unknown> = {}) {
  return {
    ...served,
    channel: { v: 1, id, kind: "public", name, created_at: "2026-01-01T00:00:00Z" },
    revision: 3,
    message_seq: 4,
    member: true,
    members: [
      { handle: "ada", joined_at: "2026-01-01T00:00:00Z" },
      { handle: "bo", joined_at: "2026-01-01T00:00:00Z" },
    ],
    ...over,
  };
}

/** A socket answering the rail, each room's detail and an empty transcript. */
function chatAnswers(channels: unknown[], read: Record<string, unknown> = {}) {
  return async (what: string, params?: Record<string, unknown>) => {
    const id = String(params?.channel_id ?? "");
    const row = (channels as { channel: { id: string; name: string; kind: string } }[]).find(
      (r) => r.channel.id === id,
    );
    switch (what) {
      case "chat_channels":
        return rail(channels, read);
      case "chat_channel":
        return detail(id, row?.channel.name ?? id, {
          channel: { ...row?.channel, v: 1, created_at: "2026-01-01T00:00:00Z" },
        });
      case "chat_messages":
        return { ...served, channel_id: id, messages: [] };
      // ACTIVITY IS WHERE THE BARE ADDRESS OPENS, so every suite mounted there
      // is asked for its feed: nothing has named the reader yet.
      case "chat_activity":
        return { ...served, items: [], unread: { mention: 0, reply: 0 }, read_state: true };
      default:
        // A QUESTION THIS SUITE DOES NOT ANSWER, as a node that serves no such
        // question answers it: never an empty object a view would read as an
        // answer of its shape.
        throw new QueryError("unknown_query");
    }
  };
}

/** A machine credential holding Chatlet, which the directory binds to no seat. */
const seatless = {
  ...ADA,
  login: "ci:deploy",
  owner: "ci:deploy",
  handle: "",
  name: "",
  kind: "",
  acts: [],
};

/** How many times a question was asked. */
const asks = (query: ReturnType<typeof vi.fn>, what: QueryName) =>
  query.mock.calls.filter(([asked]) => asked === what).length;

/** Press a key at the window, as the screen's keys hear it. */
function press(key: string, init: KeyboardEventInit = {}): void {
  flush(() => {
    window.dispatchEvent(
      new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true, ...init }),
    );
  });
}

/** Open the Rooms panel from Menu, and hand it back. */
async function openPanel(): Promise<HTMLElement> {
  fireEvent.click(await screen.findByRole("button", { name: /^Menu/ }));
  return screen.findByRole("dialog", { name: "Rooms" });
}

describe("who the screen is for", () => {
  test("a credential bound to no seat is told how a binding is made, not that chat is empty", async () => {
    // THE STRICTEST READ RULE IN THE TREE, and the one a person can actually
    // fix. Mutation: draw the remedy with no `to` and the last two lines fail.
    mountScreen({ viewer: seatless });
    await waitFor(() => expect(screen.getByText(/read as the seat you hold/i)).toBeTruthy());
    const command = screen.getByText("crewlet iam bind ID SEAT");
    const said = command.parentElement?.textContent ?? "";
    expect(said).toMatch(/To read and write here, bind this account to an open seat/);
    expect(said).not.toMatch(/get work here/);
  });

  test("a credential bound to no seat is not asked for its rooms at all", async () => {
    const { socket } = mountScreen({ viewer: seatless });
    await waitFor(() => expect(screen.getByText(/read as the seat you hold/i)).toBeTruthy());
    expect(asks(socket.query, "chat_channels")).toBe(0);
  });

  // A REFUSAL THE ENGINE WROTE FOR THE READER IS DRAWN IN ITS OWN WORDS, where
  // the reader would otherwise see their rooms: never an empty list, and never
  // "in no rooms yet" over a list that could not be read.
  test("a refused rail says the engine's own sentence, not the generic one", async () => {
    const answers = chatAnswers([]);
    mountScreen({
      answer: async (what, params) => {
        if (what === "chat_channels") {
          throw new QueryError(
            "bad_params",
            null,
            "chat is read as a human seat: bind this credential with crewlet iam bind",
          );
        }
        return answers(what, params);
      },
    });
    const panel = await openPanel();
    await waitFor(() => expect(within(panel).getByText(/crewlet iam bind/)).toBeTruthy());
    expect(screen.queryByText(/the screen.s bug to fix/)).toBeNull();
    expect(screen.queryByText(/in no rooms yet/i)).toBeNull();
  });

  // A VIEWER READ THAT FAILED IS NO ANSWER, and holding the screen on it held
  // it for the minutes until the viewer's next poll. The rooms are asked, and
  // their own answer says what is true.
  test("a viewer read that failed still asks for the rooms, and draws them", async () => {
    const { socket } = mountScreen({
      viewer: new QueryError("timeout"),
      answer: chatAnswers([room(GENERAL, "general")]),
    });
    const panel = await openPanel();
    await waitFor(() => expect(within(panel).getByText("general")).toBeTruthy());
    expect(asks(socket.query, "chat_channels")).toBeGreaterThan(0);
    expect(screen.queryByText(/working out who you are/i)).toBeNull();
  });

  test("a reader in no rooms is told how a room comes to exist", async () => {
    mountScreen({ answer: chatAnswers([]) });
    await waitFor(() => expect(screen.getByText(/in no rooms yet/i)).toBeTruthy());
    expect(screen.getByText(/channel in the org chart/)).toBeTruthy();
  });

  // CHATLET OPENS ON ACTIVITY. The bare address is Activity said differently,
  // so it is drawn at once and the address is REPLACED: Back from Activity
  // leaves Chatlet rather than landing on a spelling the reader never chose.
  // Mutation: push instead of replace and the entry count goes red; draw
  // anything but Activity and the heading does.
  test("the bare address opens Activity, replacing the entry rather than pushing one", async () => {
    location.hash = "#/chatlet";
    const entries = history.length;
    mountScreen({ hash: "#/chatlet", answer: chatAnswers([]) });
    await waitFor(() => expect(location.hash).toBe("#/chatlet/activity"));
    expect(history.length).toBe(entries);
    expect(screen.getByRole("heading", { name: "Activity" })).toBeTruthy();
  });
});

describe("the Rooms panel", () => {
  const row = workspaceRow("chatlet");
  const views = (row?.sections ?? []).filter((s) => s.tab !== false && s.path.length > 1);

  // THE PANEL IS THE CHATLET ROW'S ONE SECTION RENDERER: `nav.ts` gives the
  // row no header strip, so a view the row declares as a tab and the panel
  // does not draw is a view nobody can reach from a room, and a name the panel
  // spells its own way is a view the trail and the palette call something else.
  test("draws every view the chat row declares, under the row's names, current by address", async () => {
    mountScreen({ hash: "#/chatlet/threads", answer: chatAnswers([room(GENERAL, "general")]) });
    const panel = await openPanel();
    // NOT VACUOUS: the row has views to hold.
    expect(views.length).toBeGreaterThanOrEqual(4);
    for (const section of views) {
      const link = within(panel).getByRole("link", { name: new RegExp(`^${section.label}`) });
      expect(link.getAttribute("href"), section.key).toBe(href(section.path));
      expect(link.getAttribute("aria-current"), section.key).toBe(
        samePath(section.path, ["chatlet", "threads"]) ? "page" : null,
      );
    }
  });

  test("lists the rooms under Starred, Channels, Team rooms and Direct messages", async () => {
    mountScreen({
      answer: chatAnswers(
        [
          room(GENERAL, "general"),
          room(LAUNCH, "launch"),
          room(PLATFORM, "platform", { channel: { kind: "unit", unit: "platform" } }),
          room(WITH_BO, "", { channel: { kind: "dm", name: "" }, participants: ["ada", "bo"] }),
        ],
        { starred: [LAUNCH] },
      ),
    });
    const panel = await openPanel();
    const section = (name: string) => {
      const heading = within(panel).getByRole("button", { name: new RegExp(`^${name}`) });
      return heading.closest("[role='group'], section, div")?.parentElement ?? panel;
    };
    await waitFor(() => expect(within(panel).getByText("launch")).toBeTruthy());
    // A STARRED ROOM IS LISTED ONCE, under Starred, whatever its kind.
    expect(within(panel).getAllByText("launch")).toHaveLength(1);
    expect(within(section("Starred")).getByText("launch")).toBeTruthy();
    expect(within(section("Channels")).getByText("general")).toBeTruthy();
    expect(within(section("Team rooms")).getByText("platform")).toBeTruthy();
    expect(within(section("Direct messages")).getByText("bo")).toBeTruthy();
  });

  test("badges what waits on the reader: a room's mentions, stopping at 99+ where the engine stopped", async () => {
    // The cap is `chatlet.UnreadLimit` and the answer SAYS when it was reached,
    // so "99+" renders a stated fact.
    mountScreen({
      answer: chatAnswers([
        room(GENERAL, "general", { unread: 100, unread_capped: true, mentions: 100 }),
        room(LAUNCH, "launch", { unread: 9, mentions: 7 }),
      ]),
    });
    const panel = await openPanel();
    await waitFor(() => expect(within(panel).getByText("99+")).toBeTruthy());
    expect(within(panel).getByText("7")).toBeTruthy();
  });

  test("a direct conversation's every unread message waits on the reader", async () => {
    mountScreen({
      answer: chatAnswers([
        room(WITH_BO, "", {
          channel: { kind: "dm", name: "" },
          participants: ["ada", "bo"],
          unread: 3,
          mentions: 0,
        }),
      ]),
    });
    const panel = await openPanel();
    await waitFor(() => expect(within(panel).getByText("3")).toBeTruthy());
  });

  test("a muted room is dimmed and badged with nothing, its messages still unread", async () => {
    // A MUTE SUPPRESSES NOTICE, never delivery: the badge is notice, so it
    // goes, and the row still says it is muted rather than looking caught up.
    mountScreen({
      answer: chatAnswers([room(GENERAL, "general", { unread: 3, mentions: 2, muted: true })]),
    });
    const panel = await openPanel();
    // THE ROW SAYS IT IS MUTED, to a screen reader as to the eye.
    const row = await within(panel).findByRole("link", { name: /general.*muted/i });
    expect(row.textContent).not.toMatch(/2/);
  });

  test("a count with no read state behind it is not drawn as nothing unread", async () => {
    // The engine zeroes every count when the coordination record could not be
    // read and says so. Drawing that as a quiet panel is indistinguishable
    // from being caught up, so the panel says which one this is.
    const answers = chatAnswers([]);
    mountScreen({
      answer: async (what, params) =>
        what === "chat_channels"
          ? { ...served, read_state: false, channels: [room(GENERAL, "general", { mentions: 0 })] }
          : answers(what, params),
    });
    const panel = await openPanel();
    await waitFor(() => expect(within(panel).getByText(/couldn’t be read/i)).toBeTruthy());
  });

  test("a filter that matches nothing says so, and a match opens every section", async () => {
    mountScreen({ answer: chatAnswers([room(GENERAL, "general"), room(LAUNCH, "launch")]) });
    const panel = await openPanel();
    await waitFor(() => expect(within(panel).getByText("general")).toBeTruthy());
    const find = within(panel).getByRole("textbox", { name: "Find a room or person" });
    fireEvent.change(find, { target: { value: "laun" } });
    expect(within(panel).getByText("launch")).toBeTruthy();
    expect(within(panel).queryByText("general")).toBeNull();
    fireEvent.change(find, { target: { value: "nothing-like-it" } });
    expect(within(panel).getByText("No matches")).toBeTruthy();
  });

  test("picking a room closes the slid-in panel and opens the room", async () => {
    mountScreen({ answer: chatAnswers([room(GENERAL, "general"), room(LAUNCH, "launch")]) });
    const panel = await openPanel();
    fireEvent.click(await within(panel).findByRole("link", { name: /^launch/ }));
    await waitFor(() => expect(location.hash).toBe(`#/chatlet/${LAUNCH}`));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Rooms" })).toBeNull());
  });

  // PINNED IS A PER-BROWSER CHOICE: the panel becomes a column beside the
  // conversation, the switchers it repeats leave the top bar, and a reload
  // keeps it. Mutation: drop the write to storage and the remount case fails.
  test("the pin docks the panel beside the conversation, and the browser keeps it", async () => {
    mountScreen({ answer: chatAnswers([room(GENERAL, "general")]) });
    const panel = await openPanel();
    fireEvent.click(within(panel).getByRole("button", { name: "Pin the panel" }));
    const dock = await screen.findByRole("navigation", { name: "Rooms" });
    // ONE LANDMARK NAMED ROOMS: the pinned column holds the panel's own
    // navigation and is no second landmark of that name.
    expect(screen.getAllByRole("navigation", { name: "Rooms" })).toHaveLength(1);
    expect(dock.closest(".chat-dock")).toBeTruthy();
    expect(screen.queryByRole("dialog", { name: "Rooms" })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Rooms|Switch room/ })).toBeNull();
    expect(localStorage.getItem(STORAGE_KEYS.chatletRoomsDocked)).toBe("1");

    cleanup();
    mountScreen({ answer: chatAnswers([room(GENERAL, "general")]) });
    expect(await screen.findByRole("navigation", { name: "Rooms" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Unpin the panel" }));
    await waitFor(() => expect(localStorage.getItem(STORAGE_KEYS.chatletRoomsDocked)).toBe("0"));
  });

  // UNPINNING EASES THE COLUMN SHUT rather than dropping it: the kit's dock
  // column keeps the panel drawn, inert, while its width eases to nothing
  // beside the conversation, and takes it away when that motion ends. Focus
  // that was in the panel goes to Menu, which brings the panel back, as the
  // panel starts to leave. jsdom runs no transitions, so the motion is the
  // column's own `getAnimations`, finished by the suite. Mutation: take the
  // panel away on the press and the first expectation goes red; hand the dock
  // no way back and the focus one does; ignore the motion and the last does.
  test("unpinning keeps the panel, inert, until its column has eased shut", async () => {
    // THE COLUMN'S OWN WIDTH TRANSITION: the kit waits on that motion and no
    // other.
    const motion = fakeTransition(".chat-dock", "width");
    try {
      localStorage.setItem(STORAGE_KEYS.chatletRoomsDocked, "1");
      mountScreen({ answer: chatAnswers([room(GENERAL, "general")]) });
      await screen.findByRole("navigation", { name: "Rooms" });
      const unpin = screen.getByRole("button", { name: "Unpin the panel" });
      unpin.focus();
      fireEvent.click(unpin);
      const leaving = document.querySelector(".chat-dock .crewlet-dock-column__panel");
      expect(leaving, "the panel is still drawn while its column eases shut").toBeTruthy();
      expect(leaving?.hasAttribute("inert")).toBe(true);
      expect(document.activeElement, "focus goes to Menu, which brings it back").toBe(
        screen.getByRole("button", { name: /^Menu/ }),
      );
      await flush(async () => motion.finish());
      await waitFor(() =>
        expect(
          document.querySelector(".chat-dock .crewlet-dock-column__panel"),
          "gone once it has closed",
        ).toBeNull(),
      );
      expect(
        document.querySelector(".chat-dock"),
        "the column stays, as wide as nothing",
      ).toBeTruthy();
    } finally {
      motion.restore();
    }
  });
});

describe("the top bar", () => {
  test("names the open room on the Rooms switcher, and Direct messages on the other", async () => {
    mountScreen({ hash: `#/chatlet/${GENERAL}`, answer: chatAnswers([room(GENERAL, "general")]) });
    expect(await screen.findByRole("button", { name: /#general/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Direct messages/ })).toBeTruthy();
  });

  test("Menu carries the sidebar's figure of what waits on the reader", async () => {
    mountScreen({
      answer: chatAnswers([
        room(GENERAL, "general", { unread: 5, mentions: 2 }),
        room(WITH_BO, "", {
          channel: { kind: "dm", name: "" },
          participants: ["ada", "bo"],
          unread: 3,
        }),
      ]),
    });
    expect(await screen.findByRole("button", { name: "Menu, 5 waiting for you" })).toBeTruthy();
  });

  // CATCH UP LEADS THE ROOM'S MORE MENU, and only the room's: Ask Crewlet is
  // the page bar's own button. With nothing unread it summarizes the room.
  test("More leads with Catch up while something is unread, and opens it as the room's pane", async () => {
    mountScreen({
      hash: `#/chatlet/${GENERAL}`,
      answer: chatAnswers([room(GENERAL, "general", { unread: 4 })]),
    });
    fireEvent.click(await screen.findByRole("button", { name: "More" }));
    const items = await screen.findAllByRole("menuitem");
    expect(items[0]?.textContent).toMatch(/Catch up with Ask Crewlet/);
    fireEvent.click(items[0]!);
    await waitFor(() =>
      expect(new URLSearchParams(location.hash.split("?")[1]).get("pane")).toBe(
        `catchup:${GENERAL}`,
      ),
    );
  });

  test("More summarizes a room read through", async () => {
    mountScreen({ hash: `#/chatlet/${GENERAL}`, answer: chatAnswers([room(GENERAL, "general")]) });
    fireEvent.click(await screen.findByRole("button", { name: "More" }));
    const items = await screen.findAllByRole("menuitem");
    expect(items[0]?.textContent).toMatch(/Summarize with Ask Crewlet/);
  });

  test("the members chip counts the room and opens its members", async () => {
    mountScreen({ hash: `#/chatlet/${GENERAL}`, answer: chatAnswers([room(GENERAL, "general")]) });
    fireEvent.click(await screen.findByRole("button", { name: "2 members" }));
    await waitFor(() =>
      expect(new URLSearchParams(location.hash.split("?")[1]).get("pane")).toBe(
        `details:${GENERAL}:members`,
      ),
    );
  });

  // A STAR PAST THE ENGINE'S CAP IS REFUSED rather than dropping an older one,
  // so the menu says so before the press. Mutation: drop the cap check and the
  // item is pressable with nothing said.
  test("Add to Starred is held, with why, once the reader keeps the engine's cap of stars", async () => {
    const full = Array.from({ length: MAX_READER_MARKS }, (_, i) => `starred-${i}`);
    mountScreen({
      hash: `#/chatlet/${GENERAL}`,
      answer: chatAnswers([room(GENERAL, "general")], { starred: full }),
    });
    fireEvent.click(await screen.findByRole("button", { name: "More" }));
    const star = await screen.findByRole("menuitem", { name: /Add to Starred/ });
    expect(star.getAttribute("aria-disabled")).toBe("true");
    expect(star.textContent).toMatch(new RegExp(`starred ${MAX_READER_MARKS} rooms`));
  });

  // A UNIT'S ROOM IS THE ORG CHART'S: no leave is offered, since the next
  // apply would put the reader back. A public room offers one.
  test("Leave is offered on a public room, and never on a team's room", async () => {
    mountScreen({ hash: `#/chatlet/${GENERAL}`, answer: chatAnswers([room(GENERAL, "general")]) });
    fireEvent.click(await screen.findByRole("button", { name: "More" }));
    expect(await screen.findByRole("menuitem", { name: /Leave #general/ })).toBeTruthy();
    cleanup();
    mountScreen({
      hash: `#/chatlet/${PLATFORM}`,
      answer: chatAnswers([
        room(PLATFORM, "platform", { channel: { kind: "unit", unit: "platform" } }),
      ]),
    });
    fireEvent.click(await screen.findByRole("button", { name: "More" }));
    await screen.findByRole("menuitem", { name: /Room details/ });
    expect(screen.queryByRole("menuitem", { name: /Leave/ })).toBeNull();
  });
});

describe("the keys", () => {
  const three = [
    room(GENERAL, "general"),
    room(LAUNCH, "launch", { unread: 2 }),
    room(PLATFORM, "platform"),
  ];

  test("Alt with an arrow moves between rooms, and with Shift between rooms with unread messages", async () => {
    mountScreen({ hash: `#/chatlet/${GENERAL}`, answer: chatAnswers(three) });
    await screen.findByRole("button", { name: /#general/ });
    press("ArrowDown", { altKey: true });
    await waitFor(() => expect(location.hash).toBe(`#/chatlet/${LAUNCH}`));
    press("ArrowDown", { altKey: true });
    await waitFor(() => expect(location.hash).toBe(`#/chatlet/${PLATFORM}`));
    // Only LAUNCH is unread, so from PLATFORM the step lands there.
    press("ArrowDown", { altKey: true, shiftKey: true });
    await waitFor(() => expect(location.hash).toBe(`#/chatlet/${LAUNCH}`));
    // AND THE BARE ARROW IS NOBODY'S KEY HERE.
    press("ArrowDown");
    await new Promise((r) => setTimeout(r, 20));
    expect(location.hash).toBe(`#/chatlet/${LAUNCH}`);
  });

  test("Escape marks the open room read through its newest message, written at once", async () => {
    const presses = actEngine((tool) => applied(tool));
    mountScreen({
      hash: `#/chatlet/${GENERAL}`,
      answer: chatAnswers([room(GENERAL, "general", { unread: 4 })]),
    });
    await screen.findByRole("button", { name: /#general/ });
    await new Promise((r) => setTimeout(r, 50));
    press("Escape");
    await waitFor(() =>
      expect(presses.filter((p) => p.tool === "mark_chat_read").map((p) => p.args)).toContainEqual({
        cursors: { [GENERAL]: expect.any(Number) },
      }),
    );
  });

  test("Escape with a pane open closes nothing of the room's read state", async () => {
    // A PANE LISTENS FOR ESCAPE ITSELF, and closing it is just closing it.
    const presses = actEngine((tool) => applied(tool));
    mountScreen({
      hash: `#/chatlet/${GENERAL}?pane=details:${GENERAL}:about`,
      answer: chatAnswers([room(GENERAL, "general", { unread: 4 })]),
    });
    await screen.findByRole("button", { name: /#general/ });
    press("Escape");
    await new Promise((r) => setTimeout(r, 50));
    expect(presses.filter((p) => p.tool === "mark_chat_read")).toEqual([]);
  });

  test("the command key with F searches the open room", async () => {
    mountScreen({ hash: `#/chatlet/${GENERAL}`, answer: chatAnswers([room(GENERAL, "general")]) });
    await screen.findByRole("button", { name: /#general/ });
    press("f", { ctrlKey: true });
    await waitFor(() => expect(location.hash).toBe(`#/chatlet/search?in=${GENERAL}`));
  });
});

// A DIRECT MESSAGE ASKED FOR BY ADDRESS (`#/chatlet?dm=<handle>`), which is the
// seat page's and the seat peek's door: it is New message with that seat
// chosen, and the address is replaced so a reload or Back does not ask again.
test("a direct message asked for by address is New message, with the address replaced", async () => {
  mountScreen({ hash: "#/chatlet?dm=bo", answer: chatAnswers([]) });
  await waitFor(() => expect(location.hash).toBe("#/chatlet/new?to=bo"));
});

// THE OUTBOX OUTLIVES THE ROOM. A room is remounted for every room opened, and
// an outbox kept inside it threw the unanswered rows away with it, an
// `unknown` row's Retry under its own operation key included, which is the only
// safe move for that message: typed again, it is said twice.
test("an unanswered message and its same-id retry survive a look at another room", async () => {
  const presses = actEngine((tool) => (tool === "post_message" ? unknown(tool) : applied(tool)));
  mountScreen({
    hash: `#/chatlet/${GENERAL}`,
    answer: chatAnswers([room(GENERAL, "general"), room(LAUNCH, "launch")]),
  });
  const box = (await screen.findByLabelText(/^Message #general/)) as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "did that land?" } });
  const send = screen.getByRole("button", { name: /^send$/i });
  await waitFor(() => expect(send.getAttribute("aria-disabled")).not.toBe("true"));
  fireEvent.click(send);
  await screen.findByRole("button", { name: /send again/i });

  flush(() => {
    location.hash = `#/chatlet/${LAUNCH}`;
  });
  await screen.findByLabelText(/^Message #launch/);
  // NOT DRAWN IN A ROOM IT WAS NOT SAID IN.
  expect(screen.queryByText("did that land?")).toBeNull();

  flush(() => {
    location.hash = `#/chatlet/${GENERAL}`;
  });
  await screen.findByLabelText(/^Message #general/);
  expect(await screen.findByText("did that land?")).toBeTruthy();
  const retry = await screen.findByRole("button", { name: /send again/i });
  await waitFor(() => expect(retry.getAttribute("aria-disabled")).not.toBe("true"));
  fireEvent.click(retry);
  await waitFor(() => expect(presses.filter((p) => p.tool === "post_message")).toHaveLength(2));
  const [first, again] = presses.filter((p) => p.tool === "post_message");
  expect(again!.opId).toBe(first!.opId);
});

// A TAB HANDED TO ANOTHER READER, OR READING AS ANOTHER SEAT, WRITES NOTHING
// OF THE LAST ONE. A different person signing in hands the whole tab over by
// a reload (`lib/session.ts`): its storage emptied and the new reader noted
// first, then the page goes, which fires the `pagehide` the cursor writer
// flushes on. A seat that moves under the same person remounts the screen,
// whose cleanup flushes too. Either flush, sent, would write the last one's
// reading as the next one's.
describe("a tab handed to another reader, or a seat that moves", () => {
  /** The tab's cursor writer, as the screen's own hook holds it, for `seat`. */
  function Reader({
    seat,
    onReader,
  }: {
    seat: { current: string };
    onReader: (cursors: ReadCursors) => void;
  }) {
    onReader(useReadCursors(seat));
    return null;
  }

  async function mountReader(strict = false) {
    const reader = ADA_SESSION.person;
    vi.useFakeTimers();
    noteReader(reader);
    const presses = actEngine();
    const seat = { current: "ada" };
    let cursors: ReadCursors | null = null;
    mountChat(<Reader seat={seat} onReader={(held) => (cursors = held)} />, { strict });
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(50);
    });
    return { presses, seat, current: () => cursors! };
  }

  const marks = (presses: { tool: string; args: unknown }[]) =>
    presses.filter(({ tool }) => tool === "mark_chat_read").map(({ args }) => args);

  test("a hand-over writes nothing of the last reader, not even on the way out", async () => {
    const { presses, current } = await mountReader();
    current().see("room-1", 900);
    sessionStorage.clear();
    noteReader("person-bo");
    flush(() => {
      window.dispatchEvent(new Event("pagehide"));
    });
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(READ_FLUSH_MS * 2);
    });
    expect(currentReader()).toBe("person-bo");
    expect(marks(presses)).toEqual([]);
  });

  test("the same reader keeps what they read, and it is written", async () => {
    const { presses, current } = await mountReader();
    current().see("room-1", 900);
    noteReader(ADA_SESSION.person);
    flush(() => {
      window.dispatchEvent(new Event("pagehide"));
    });
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(50);
    });
    expect(marks(presses)).toEqual([{ cursors: { "room-1": 900 } }]);
  });

  test("a tab that learns its reader after the writer was made still writes", async () => {
    vi.useFakeTimers();
    const presses = actEngine();
    const seat = { current: "ada" };
    let cursors: ReadCursors | null = null;
    mountChat(<Reader seat={seat} onReader={(held) => (cursors = held)} />);
    expect(currentReader()).toBeNull();
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(50);
    });
    expect(currentReader()).toBe(ADA_SESSION.person);
    cursors!.see("room-1", 900);
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(READ_FLUSH_MS * 2);
    });
    expect(marks(presses)).toEqual([{ cursors: { "room-1": 900 } }]);
  });

  test("a seat that moved under the writer drops its reading rather than write it", async () => {
    const { presses, seat, current } = await mountReader();
    current().see("room-1", 900);
    seat.current = "ada-2";
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(READ_FLUSH_MS * 2);
    });
    expect(marks(presses)).toEqual([]);
    seat.current = "ada";
    current().see("room-1", 950);
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(READ_FLUSH_MS * 2);
    });
    expect(marks(presses)).toEqual([]);
  });

  test("under StrictMode the writer set up again still takes what is read", async () => {
    const { presses, current } = await mountReader(true);
    current().see("room-1", 700);
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(READ_FLUSH_MS);
    });
    expect(marks(presses)).toEqual([{ cursors: { "room-1": 700 } }]);
  });

  test("a seat that moves remounts the screen: its unsent message and draft go", async () => {
    actEngine((tool) => (tool === "post_message" ? unknown(tool) : applied(tool)));
    // THE VIEWER THE SOCKET ANSWERS, moved in place below.
    const viewer: Record<string, unknown> = { ...ADA };
    const { store } = mountScreen({
      hash: `#/chatlet/${GENERAL}`,
      viewer,
      answer: chatAnswers([room(GENERAL, "general")]),
    });
    const box = (await screen.findByLabelText(/^Message #general/)) as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: "said as ada" } });
    const send = screen.getByRole("button", { name: /^send$/i });
    await waitFor(() => expect(send.getAttribute("aria-disabled")).not.toBe("true"));
    fireEvent.click(send);
    await screen.findByRole("button", { name: /send again/i });
    fireEvent.change(box, { target: { value: "half a thought of ada's" } });

    // THE DIRECTORY BINDS THE PERSON ELSEWHERE: the engine closes the socket,
    // and the reconnect asks the viewer again.
    Object.assign(viewer, { owner: "ada-2", handle: "ada-2" });
    flush(() => store.setConnected(false));
    flush(() => store.setConnected(true));
    await waitFor(() => expect(screen.queryByText("said as ada")).toBeNull());
    expect(screen.queryByRole("button", { name: /send again/i })).toBeNull();
    const fresh = (await screen.findByLabelText(/^Message #general/)) as HTMLTextAreaElement;
    expect(fresh.value).toBe("");
  });
});
