/**
 * What the chat screen says before it says anything about a conversation.
 *
 * The three shapes a reader can arrive in are not degrees of the same thing: a
 * reader without the Chatlet app, a credential acting as no seat, and a person.
 * The first is the frame's — the chat row names its app, so `app/App.test.tsx`
 * holds a room to the one refusal. The middle one is this screen's: an
 * ordinary state with a one-binding remedy, and the case this screen exists
 * to be clear about — the engine refuses every chat read to it, and if this
 * screen does not say what would change that nothing does.
 */

import {
  act as flush,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, test, vi } from "vitest";
import { ToastProvider } from "@crewlethq/ui";

import { Chat } from "./Chat.tsx";
import { READ_FLUSH_MS, useReadCursors, type ReadCursors } from "./cursor.ts";
import { LIVE_REFETCH_MS } from "./live.ts";
import { workspaceRow } from "~/app/nav.ts";
import { Router, href, samePath } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { currentReader, noteReader } from "~/lib/reader.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { QueryError, Store, type QueryName } from "~/protocol/index.ts";
import { MAX_CHAT_LIVE } from "~/protocol/store.ts";
import { ADA, ADA_SESSION, actEngine, applied, mountChat, unknown } from "~/test/chat.tsx";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionStorage.clear();
  location.hash = "#/";
});

/**
 * A socket that answers each question with a fixture and never pushes. A
 * fixture that is an Error is a read that FAILED, rejected with it.
 */
function mount(answers: Partial<Record<QueryName, unknown>>) {
  const store = new Store();
  const query = vi.fn(async (what: string) => {
    const answer = answers[what as QueryName];
    if (answer instanceof Error) throw answer;
    return answer ?? {};
  });
  const socket = { query, focus: vi.fn() };
  const view = render(
    <ClientContext.Provider value={{ store, socket } as never}>
      {/* THE FRAME'S ONE SESSION AND VIEWER READS, which the screen takes
          rather than asking for itself — the frame mounts them, so a suite
          mounting the screen alone mounts them around it. */}
      <SessionReading>
        <ViewerProvider>
          <Router>
            <Chat />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
  return { store, query, socket, view };
}

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 12 },
};

function room(over: Record<string, unknown> = {}) {
  return {
    channel: {
      v: 1,
      id: "room-1",
      kind: "public",
      name: "general",
      created_at: "2026-01-01T00:00:00Z",
    },
    revision: 3,
    unread: 0,
    ...over,
  };
}

const bound = ADA;

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

test("a credential bound to no seat is told how a binding is made, not that chat is empty", async () => {
  // THE STRICTEST READ RULE IN THE TREE, and the one a person can actually
  // fix. The engine refuses every chat read to a credential acting as no
  // seat; the screen says so from the one fact it holds independently — a
  // login with no seat — and names the binding that changes it.
  mount({ viewer: seatless });
  await waitFor(() => expect(screen.getByText(/read as the seat you hold/i)).toBeTruthy());
  // NAMED in the command that binds it, as what opens CHATLET: the shared remedy's
  // own words are about work, which is not what a binding opens here.
  // Mutation: draw the remedy with no `to` and the last two lines fail.
  const command = screen.getByText("crewlet iam bind ID SEAT");
  const said = command.parentElement?.textContent ?? "";
  expect(said).toMatch(/To read and write here, bind this account to an open seat/);
  expect(said).not.toMatch(/get work here/);
});

// A REFUSAL THE ENGINE WROTE FOR THE READER IS DRAWN IN ITS OWN WORDS. A chat
// question the engine refuses with a sentence for the caller — `bad_params`,
// the one class whose sentence survives to it — is drawn in that sentence,
// and a screen that dropped it drew "this is the screen's bug to fix" instead
// of the binding to make.
test("a refused rail says the engine's own sentence, not the generic one", async () => {
  mount({
    viewer: bound,
    chat_channels: new QueryError(
      "bad_params",
      null,
      "chat is read as a human seat: bind this credential with crewlet iam bind",
    ),
  });
  await waitFor(() => expect(screen.getByText(/crewlet iam bind/)).toBeTruthy());
  expect(screen.queryByText(/the screen.s bug to fix/)).toBeNull();
});

test("a credential bound to no seat is not asked for its rooms at all", async () => {
  // The engine would refuse it, so asking is a round trip whose only possible
  // answer is a refusal the screen has already rendered.
  const { query } = mount({ viewer: seatless });
  await waitFor(() => expect(screen.getByText(/read as the seat you hold/i)).toBeTruthy());
  expect(query.mock.calls.map(([what]) => what)).not.toContain("chat_channels");
});

// A VIEWER READ THAT FAILED IS NO ANSWER, and holding the screen on it held
// it for the five minutes until the viewer's next poll — "Working out who you
// are" over a rail nobody asked for, while every other screen worked. The
// rail is asked, and its own answer says what is true.
test("a viewer read that failed still asks for the rail, and draws it", async () => {
  const { query } = mount({
    viewer: new QueryError("timeout"),
    chat_channels: { ...served, read_state: true, channels: [room()] },
  });
  const rail = await screen.findByRole("navigation", { name: "Your rooms" });
  await waitFor(() => expect(within(rail).getByText("#general")).toBeTruthy());
  expect(query.mock.calls.map(([what]) => what)).toContain("chat_channels");
  expect(screen.queryByText(/working out who you are/i)).toBeNull();
  // AND NAMES NOBODY it does not know: no reader is drawn as the empty name.
  expect(screen.queryByText(/you are reading as/i)).toBeNull();
});

// …and when that rail is REFUSED, the refusal is the engine's own sentence
// rather than an empty list: a credential that is nobody's seat is refused
// by name, with the field to change.
test("a refused rail after a failed viewer read says the engine's own words", async () => {
  mount({
    viewer: new QueryError("timeout"),
    chat_channels: new QueryError("bad_params", null, "this credential is not bound to a seat"),
  });
  await waitFor(() => expect(screen.getByText(/not bound to a seat/i)).toBeTruthy());
  expect(screen.queryByText(/in no rooms yet/i)).toBeNull();
});

// TWO NAMES, NEVER ONE: the seat's is the chart's name for a place, and the
// reader is named as the session names them — here, before the session read
// has answered, by their login.
test("a person is told who they are and whose rooms these are", async () => {
  mount({ viewer: bound, chat_channels: { ...served, read_state: true, channels: [room()] } });
  await waitFor(() => expect(screen.getByText(/reading as the seat/i)).toBeTruthy());
  expect(screen.getByText("Ada")).toBeTruthy();
  expect(screen.getByText("ada.lovelace")).toBeTruthy();
});

test("an unread badge stops at 99+ when the engine says it stopped counting", async () => {
  // The cap is `chatlet.UnreadLimit` and the answer SAYS when it was reached, so
  // "99+" renders a stated fact. Inferred from the number instead, a room with
  // exactly a hundred unread and one with four thousand would draw the same
  // badge and neither would be true.
  mount({
    viewer: bound,
    chat_channels: {
      ...served,
      read_state: true,
      channels: [room({ unread: 100, unread_capped: true })],
    },
  });
  await waitFor(() => expect(screen.getByText("99+")).toBeTruthy());
});

test("an exact unread count is drawn as itself", async () => {
  mount({
    viewer: bound,
    chat_channels: { ...served, read_state: true, channels: [room({ unread: 7 })] },
  });
  await waitFor(() => expect(screen.getByText("7")).toBeTruthy());
});

test("a count with no read state behind it is not drawn as nothing unread", async () => {
  // The engine zeroes every count when the coordination record could not be
  // read and says so. Drawing that as a quiet rail is indistinguishable from
  // being caught up — so the screen says which one this is, and draws no
  // badge rather than a zero.
  mount({
    viewer: bound,
    chat_channels: {
      ...served,
      read_state: false,
      channels: [room({ unread: 0 })],
    },
  });
  await waitFor(() => expect(screen.getByText(/couldn’t be read/i)).toBeTruthy());
});

test("a muted room says so in the rail", async () => {
  mount({
    viewer: bound,
    chat_channels: {
      ...served,
      read_state: true,
      channels: [room({ unread: 3, muted: true })],
    },
  });
  await waitFor(() => expect(screen.getByText("muted")).toBeTruthy());
  // AND IT IS STILL BADGED. A mute suppresses NOTICE, never delivery: the
  // messages are there, and a room that hid its own count would make "muted"
  // mean "ignored".
  expect(screen.getByText("3")).toBeTruthy();
});

test("a reader in no rooms is told how a room comes to exist", async () => {
  mount({ viewer: bound, chat_channels: { ...served, read_state: true, channels: [] } });
  await waitFor(() => expect(screen.getByText(/in no rooms yet/i)).toBeTruthy());
});

// THE RAIL IS THE CHATLET ROW'S ONE SECTION RENDERER: `nav.ts` gives the row no
// header strip, so a list the row declares and the rail does not draw is a
// section nobody can reach from a room, and a name the rail spells its own way
// is a list the trail, the tab title and the palette call something else.
test("the rail draws every list the chat row declares, under the row's own names", async () => {
  location.hash = "#/chatlet/mentions";
  mount({
    viewer: bound,
    chat_channels: { ...served, read_state: true, channels: [room()] },
  });
  const rail = await screen.findByRole("navigation", { name: "Your rooms" });
  await waitFor(() => expect(within(rail).getByText("#general")).toBeTruthy());
  const row = workspaceRow("chatlet");
  const landing = row?.sections.find((s) => samePath(s.path, row.path));
  const lists = row?.sections.filter((s) => !samePath(s.path, row.path)) ?? [];
  // NOT VACUOUS: the row has lists to hold, and a landing that names the rooms.
  expect(lists.length).toBeGreaterThan(0);
  expect(within(rail).getByText(landing?.label ?? "(no landing section)")).toBeTruthy();
  for (const section of lists) {
    const link = within(rail).getByRole("link", { name: section.label });
    expect(link.getAttribute("href"), section.key).toBe(href(section.path));
    // CURRENT BY THE ADDRESS, as the header's tabs are.
    expect(link.getAttribute("aria-current"), section.key).toBe(
      samePath(section.path, ["chatlet", "mentions"]) ? "page" : null,
    );
  }
  // AND NOTHING ELSE: every other link in the rail is a room.
  expect(within(rail).getAllByRole("link")).toHaveLength(lists.length + 1);
  // A READER HOLDING CHATLET HOLDS WHAT EVERY LIST NEEDS, so no list draws the
  // lock the sidebar draws for a section the reader cannot open.
  for (const section of lists) {
    const link = within(rail).getByRole("link", { name: section.label });
    expect(link.getAttribute("title"), section.key).toBeNull();
    expect(link.textContent, section.key).not.toMatch(/needs .+ access/);
  }
});

// THE RAIL'S COUNTS AND MUTES ARE ANSWERED FROM THIS PERSON'S READ STATE, so
// whatever moves that record moves the rail: a flush or a mute this tab made,
// and another tab's, which arrives as the `chat_cursor_moved` push. Before,
// the badge and the "muted" label stayed as they were until some unrelated
// frame landed or the page was reloaded.
describe("the rail follows the reader's own read state", () => {
  const unread = {
    ...served,
    read_state: true,
    read: { handle: "ada", revision: 1, cursors: {} },
    channels: [room({ unread: 3 })],
  };

  /** How many times the rail has been asked for. */
  const railAsks = (query: ReturnType<typeof vi.fn>) =>
    query.mock.calls.filter(([what]) => what === "chat_channels").length;

  test("a newer read state pushed from another tab re-reads the rail", async () => {
    const { query, store } = mount({ viewer: bound, chat_channels: unread });
    await waitFor(() => expect(screen.getByText("3")).toBeTruthy());
    // THE RAIL'S OWN ANSWER SEEDS THE STATE, and that costs no second read.
    await new Promise((r) => setTimeout(r, 50));
    const before = railAsks(query);
    expect(before).toBe(1);
    flush(() => store.applyChatRead({ handle: "ada", revision: 2, cursors: { "room-1": 99 } }));
    await waitFor(() => expect(railAsks(query)).toBe(before + 1));
  });

  test("a frame past the bound on what the tab holds still re-reads the rail", async () => {
    // THE RAIL RE-READS ON THE TAB'S FRAME COUNT. A sum of what the rooms hold
    // stands still once a room is at its bound — the new frame and the one it
    // pushes out cancel — so the rail stopped moving exactly when the company
    // got busy.
    const { query, store } = mount({ viewer: bound, chat_channels: unread });
    await waitFor(() => expect(screen.getByText("3")).toBeTruthy());
    const frame = (seq: number) =>
      store.applyChatChange({
        channel_id: "room-9",
        op: "post",
        op_id: `op-${seq}`,
        message_id: `m-${seq}`,
        channel_seq: seq,
        at: "2026-01-01T00:01:00Z",
        position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
      } as never);
    flush(() => {
      for (let seq = 1; seq <= MAX_CHAT_LIVE; seq++) frame(seq);
    });
    await new Promise((r) => setTimeout(r, LIVE_REFETCH_MS + 50));
    const before = railAsks(query);
    flush(() => frame(MAX_CHAT_LIVE + 1));
    await waitFor(() => expect(railAsks(query)).toBe(before + 1));
  });

  test("a read state at the revision the rail already drew asks nothing", async () => {
    const { query, store } = mount({ viewer: bound, chat_channels: unread });
    await waitFor(() => expect(screen.getByText("3")).toBeTruthy());
    await new Promise((r) => setTimeout(r, 50));
    const before = railAsks(query);
    flush(() => store.applyChatRead({ handle: "ada", revision: 1, cursors: {} }));
    await new Promise((r) => setTimeout(r, 50));
    expect(railAsks(query)).toBe(before);
  });

  // A WRITE THIS TAB MADE MOVES THE RAIL ONCE. Its answer is the person's
  // new state, and the push the same write raises reaches this tab too; the
  // revision makes the two one move, whichever lands first. Asked again on
  // the answer AND on the push, the rail was counted twice per flush — every
  // fifteen seconds of reading — and the second ask threw the first away.
  describe("a write this tab made", () => {
    /** The muted state the write leaves, as the engine answers and pushes it. */
    const after = { handle: "ada", revision: 2, cursors: {}, muted: ["room-1"] };

    /** The room open beside the rail, its Mute pressable. */
    async function openRoom(answerAct: Parameters<typeof actEngine>[0]) {
      actEngine(answerAct);
      const { socket, store } = mountChat(<Chat channel="room-1" />, {
        answers: {
          chat_channels: unread,
          chat_channel: {
            ...served,
            channel: room().channel,
            revision: 3,
            message_seq: 0,
            member: true,
            members: [],
          },
          chat_messages: { ...served, channel_id: "room-1", messages: [] },
        },
      });
      const mute = await screen.findByRole("button", { name: "Mute" });
      await waitFor(() => expect(mute.getAttribute("aria-disabled")).not.toBe("true"));
      await new Promise((r) => setTimeout(r, 50));
      return { query: socket.query, store, mute, before: railAsks(socket.query) };
    }

    test("its answer re-reads the rail once, and the push it raises adds nothing", async () => {
      const { query, store, mute, before } = await openRoom((tool) =>
        applied(tool, { read: after }),
      );
      fireEvent.click(mute);
      await waitFor(() => expect(railAsks(query)).toBe(before + 1));
      flush(() => store.applyChatRead(after));
      await new Promise((r) => setTimeout(r, 100));
      expect(railAsks(query)).toBe(before + 1);
      expect(store.state.chatRead?.muted).toEqual(["room-1"]);
    });

    test("a push that lands before the answer is the same one read", async () => {
      let answer: (reply: Response) => void = () => undefined;
      const { query, store, mute, before } = await openRoom((tool) => applied(tool));
      // THE ANSWER IS HELD until the push has landed, which is the order the
      // double ask happened in.
      vi.stubGlobal(
        "fetch",
        vi.fn(
          () =>
            new Promise<Response>((resolve) => {
              answer = resolve;
            }),
        ),
      );
      fireEvent.click(mute);
      flush(() => store.applyChatRead(after));
      await waitFor(() => expect(railAsks(query)).toBe(before + 1));
      await flush(async () => {
        answer(
          new Response(
            JSON.stringify({
              tool: "mark_chat_read",
              outcome: "applied",
              receipt: { read: after },
            }),
            { status: 200, headers: { "Content-Type": "application/json" } },
          ),
        );
        await new Promise((r) => setTimeout(r, 100));
      });
      expect(railAsks(query)).toBe(before + 1);
    });
  });
});

// THE OUTBOX OUTLIVES THE ROOM. A room is remounted for every room opened, and
// an outbox kept inside it threw the unanswered rows away with it — an
// `unknown` row's Retry under its own operation key included, which is the only
// safe move for that message: typed again, it is said twice.
test("an unanswered message and its same-id retry survive a look at another room", async () => {
  const presses = actEngine((tool) => (tool === "post_message" ? unknown(tool) : applied(tool)));
  const detail = (id: string, name: string) => ({
    ...served,
    channel: { ...room().channel, id, name },
    revision: 3,
    message_seq: 0,
    member: true,
    members: [],
  });
  function Switcher() {
    const [channel, setChannel] = useState("room-1");
    return (
      <>
        <button
          type="button"
          onClick={() => setChannel((c) => (c === "room-1" ? "room-2" : "room-1"))}
        >
          switch rooms
        </button>
        <Chat channel={channel} />
      </>
    );
  }
  mountChat(<Switcher />, {
    answer: async (what, params) => {
      const id = String(params?.channel_id ?? "");
      if (what === "chat_channels") {
        return {
          ...served,
          read_state: true,
          channels: [room(), room({ channel: detail("room-2", "launch").channel })],
        };
      }
      if (what === "chat_channel") return detail(id, id === "room-1" ? "general" : "launch");
      if (what === "chat_messages") return { ...served, channel_id: id, messages: [] };
      return {};
    },
  });
  const box = (await screen.findByLabelText(/^Message #general/)) as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: "did that land?" } });
  const send = screen.getByRole("button", { name: /^send$/i });
  await waitFor(() => expect(send.getAttribute("aria-disabled")).not.toBe("true"));
  fireEvent.submit(box.closest("form")!);
  await screen.findByRole("button", { name: /send again/i });

  fireEvent.click(screen.getByRole("button", { name: /switch rooms/i }));
  await screen.findByLabelText(/^Message #launch/);
  // NOT DRAWN IN A ROOM IT WAS NOT SAID IN.
  expect(screen.queryByText("did that land?")).toBeNull();

  fireEvent.click(screen.getByRole("button", { name: /switch rooms/i }));
  await screen.findByLabelText(/^Message #general/);
  expect(screen.getByText("did that land?")).toBeTruthy();
  const retry = await screen.findByRole("button", { name: /send again/i });
  await waitFor(() => expect(retry.getAttribute("aria-disabled")).not.toBe("true"));
  fireEvent.click(retry);
  await waitFor(() => expect(presses.filter((p) => p.tool === "post_message")).toHaveLength(2));
  const [first, again] = presses.filter((p) => p.tool === "post_message");
  expect(again!.opId).toBe(first!.opId);
});

// A TAB HANDED TO ANOTHER READER, OR READING AS ANOTHER SEAT, WRITES NOTHING
// OF THE LAST ONE. A different person signing in hands the whole tab over by
// a reload (`lib/session.ts`) — its storage emptied and the new reader noted
// first, then the page goes, which fires the `pagehide` the cursor writer
// flushes on. A seat that moves under the same person remounts the screen
// (`Chat.tsx`), whose cleanup flushes too. Every write goes out as whoever
// the session is when it is sent, under the seat the engine resolves them to
// then — so either flush, sent, would write the last one's reading as the
// next one's.
describe("a tab handed to another reader, or a seat that moves", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

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

  /**
   * Mount the hook reading as `ada`, on a clock the case drives — for the
   * person the frame's session read names ([ADA_SESSION]), since a tab noted
   * for anybody else is handed over to them the moment that read lands.
   */
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

  /** The cursor writes the engine was sent, as `[tool, args]`. */
  const marks = (presses: { tool: string; args: unknown }[]) =>
    presses.filter(({ tool }) => tool === "mark_chat_read").map(({ args }) => args);

  test("a hand-over writes nothing of the last reader, not even on the way out", async () => {
    const { presses, current } = await mountReader();
    current().see("room-1", 900);
    // THE HAND-OVER'S OWN ORDER: the tab's storage goes, the new reader is
    // noted, and the page goes — `pagehide` is the last thing this tab does.
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
    // A SIGN-IN AS THE SAME PERSON notes nobody new, and moves nothing.
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
    // A TAB OPENED WITH A SESSION ALREADY IN THE BROWSER records nobody until
    // the frame's session read lands and adopts the session's principal
    // (`lib/session.ts`'s `takeSession`), and the frame may draw the screen
    // before that — the viewer can answer over the socket first. The writer
    // is then made for nobody, and the reader the tab learns is the one every
    // press it sent was already made as, not somebody else.
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
    // THE SEAT THE TAB READS AS NOW, written by the screen in the render
    // that moves it — before the old writer's cleanup or its timer.
    seat.current = "ada-2";
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(READ_FLUSH_MS * 2);
    });
    expect(marks(presses)).toEqual([]);
    // AND IT IS DONE: a writer whose seat went takes nothing more, even were
    // the seat to come back under it.
    seat.current = "ada";
    current().see("room-1", 950);
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(READ_FLUSH_MS * 2);
    });
    expect(marks(presses)).toEqual([]);
  });

  test("under StrictMode the writer set up again still takes what is read", async () => {
    // REACT SETS AN EFFECT UP, TEARS IT DOWN AND SETS IT UP AGAIN over one
    // component's state in development, which `main.tsx` renders under. The
    // teardown stops the writer, and the second setup keeps using it: stopped
    // for good, the read cursor never moved under `npm run dev`.
    const { presses, current } = await mountReader(true);
    current().see("room-1", 700);
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(READ_FLUSH_MS);
    });
    expect(marks(presses)).toEqual([{ cursors: { "room-1": 700 } }]);
  });

  test("a seat that moves remounts the screen: its unsent message and draft go", async () => {
    // AN ANSWER NOBODY CAN VOUCH FOR keeps the row on screen with a Retry —
    // which, pressed as the next seat, would say the words as it.
    actEngine((tool) => (tool === "post_message" ? unknown(tool) : applied(tool)));
    const answers: Partial<Record<QueryName, unknown>> = {
      viewer: bound,
      chat_channels: { ...served, read_state: true, channels: [room()] },
      chat_channel: {
        ...served,
        channel: room().channel,
        revision: 3,
        message_seq: 0,
        member: true,
        members: [],
      },
      chat_messages: { ...served, channel_id: "room-1", messages: [] },
    };
    const { store } = mountRoom(answers);
    const box = (await screen.findByLabelText(/^Message/)) as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: "said as ada" } });
    const send = screen.getByRole("button", { name: /^send$/i });
    await waitFor(() => expect(send.getAttribute("aria-disabled")).not.toBe("true"));
    fireEvent.submit(box.closest("form")!);
    await screen.findByRole("button", { name: /send again/i });
    fireEvent.change(box, { target: { value: "half a thought of ada's" } });

    // THE DIRECTORY BINDS THE PERSON ELSEWHERE: the engine closes the socket,
    // and the reconnect asks the viewer again.
    answers.viewer = { ...bound, owner: "ada-2", handle: "ada-2" };
    flush(() => store.setConnected(false));
    flush(() => store.setConnected(true));
    await waitFor(() => expect(screen.queryByText("said as ada")).toBeNull());
    expect(screen.queryByRole("button", { name: /send again/i })).toBeNull();
    const fresh = (await screen.findByLabelText(/^Message/)) as HTMLTextAreaElement;
    expect(fresh.value).toBe("");
  });
});

/** A room on [mount]'s socket, with the toast host and act route a press needs. */
function mountRoom(answers: Partial<Record<QueryName, unknown>>) {
  const store = new Store();
  store.applyHealth(healthFrame({ nodes: 1 }));
  const query = vi.fn(async (what: string) => {
    const answer = answers[what as QueryName];
    if (answer instanceof Error) throw answer;
    return answer ?? {};
  });
  const socket = { query, focus: vi.fn() };
  render(
    <ToastProvider>
      <ClientContext.Provider value={{ store, socket } as never}>
        <SessionReading>
          <ViewerProvider>
            <Router>
              <Chat channel="room-1" />
            </Router>
          </ViewerProvider>
        </SessionReading>
      </ClientContext.Provider>
    </ToastProvider>,
  );
  return { store, query };
}

// A DIRECT MESSAGE ASKED FOR BY ADDRESS (`#/chatlet?dm=<handle>`), which is the
// seat page's and the seat peek's door: the dialog opens ONCE with that seat
// chosen, and the address is left at once so a reload or Back does not open
// it again. It waits for the chart, since who may be named is the chart's.
// Mutation: drop the `nav.filter({ dm: null })` and the address case goes
// red; choose the handle whether or not the chart answers to it and the
// stranger case does.
describe("a direct message asked for by address", () => {
  const ORG = withDerived({
    name: "Acme",
    roles: [
      { name: "Ada", handle: "ada", kind: "human", goal: "Run it" },
      { name: "Bo Chen", handle: "bo", goal: "Ship it" },
    ],
    units: [],
  });
  const rail = { ...served, read_state: true, channels: [] };

  async function arrive(hash: string) {
    location.hash = hash;
    const presses = actEngine((tool) => applied(tool, { channel: "dm-1", kind: "dm" }));
    const { store } = mountChat(<Chat />, { answers: { chat_channels: rail } });
    flush(() => store.applyOrg(ORG as never));
    return presses;
  }

  test("opens New direct message once, with the seat chosen, and leaves the address", async () => {
    const presses = await arrive("#/chatlet?dm=bo");
    const dialog = await screen.findByRole("dialog", { name: /new direct message/i });
    await waitFor(() => expect(location.hash).toBe("#/chatlet"));
    const press = within(dialog).getByRole("button", { name: /^open$/i });
    await waitFor(() => expect(press.getAttribute("aria-disabled")).not.toBe("true"));
    fireEvent.click(press);
    await waitFor(() => expect(presses).toHaveLength(1));
    expect(presses[0]).toMatchObject({ tool: "open_dm", args: { to: ["bo"] } });
  });

  test("a handle nobody in the chart answers to opens it with nobody chosen", async () => {
    await arrive("#/chatlet?dm=stranger");
    const dialog = await screen.findByRole("dialog", { name: /new direct message/i });
    await waitFor(() => expect(location.hash).toBe("#/chatlet"));
    const press = within(dialog).getByRole("button", { name: /^open$/i });
    await new Promise((r) => setTimeout(r, 20));
    expect(press.getAttribute("aria-disabled")).toBe("true");
  });
});
