/**
 * What the chat screen says before it says anything about a conversation.
 *
 * The three shapes a reader can arrive in are not degrees of the same thing: a
 * missing token, a token no seat names, and a person. The first is the
 * frame's — every chat section is guarded, so `app/App.test.tsx`'s guarded
 * walk holds it to the one refusal. The middle one is this screen's: an
 * ordinary state with a one-line remedy, and the case this screen exists to be
 * clear about — the socket reduces the engine's own refusal to a CODE, so if
 * this screen does not name the field nothing does.
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

import { Chat } from "./Chat.tsx";
import { READ_FLUSH_MS, useReadCursors, type ChatReader } from "./cursor.ts";
import { LIVE_REFETCH_MS } from "./live.ts";
import { workspaceRow } from "~/app/nav.ts";
import { Router, href, samePath } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { clearToken, QueryError, Store, storeToken, type QueryName } from "~/protocol/index.ts";
import { MAX_CHAT_LIVE } from "~/protocol/store.ts";
import { actEngine, applied, mountChat, unknown } from "~/test/chat.tsx";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
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
      {/* THE FRAME'S ONE VIEWER READ, which the screen takes rather than
          asking for itself — the frame mounts it, so a suite mounting the
          screen alone mounts it around it. */}
      <ViewerProvider>
        <Router>
          <Chat />
        </Router>
      </ViewerProvider>
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

const bound = {
  operator_id: "U0FOUNDER",
  operator: true,
  handle: "ada",
  name: "Ada",
  kind: "human",
};

test("a token no seat names is told which field to change, not that chat is empty", async () => {
  // THE STRICTEST READ RULE IN THE TREE, and the one a person can actually
  // fix. The engine refuses with a sentence naming the field; the socket
  // carries only the code, so the screen says it from the one fact it holds
  // independently — an operator id with no handle.
  mount({ viewer: { operator_id: "U0CI", operator: true, handle: "", name: "", kind: "" } });
  await waitFor(() => expect(screen.getByText(/not bound to a seat/i)).toBeTruthy());
  expect(screen.getByText(/contact\.crewlet_operator_id/)).toBeTruthy();
});

// A REFUSAL THE ENGINE WROTE FOR THE READER IS DRAWN IN ITS OWN WORDS. Every
// chat question refuses a credential that is nobody's seat as `bad_params`
// precisely because that is the one class whose sentence survives to the
// caller — and a screen that dropped the sentence drew "this is the screen's
// bug to fix" instead of the field to change.
test("a refused rail says the engine's own sentence, not the generic one", async () => {
  mount({
    viewer: bound,
    chat_channels: new QueryError("bad_params", null, "give a seat contact.crewlet_operator_id"),
  });
  await waitFor(() => expect(screen.getByText(/contact\.crewlet_operator_id/)).toBeTruthy());
  expect(screen.queryByText(/the screen.s bug to fix/)).toBeNull();
});

test("an unbound reader is not asked for their rooms at all", async () => {
  // The engine would refuse it, so asking is a round trip whose only possible
  // answer is a refusal the screen has already rendered.
  const { query } = mount({
    viewer: { operator_id: "U0CI", operator: true, handle: "", name: "", kind: "" },
  });
  await waitFor(() => expect(screen.getByText(/not bound to a seat/i)).toBeTruthy());
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

test("a person is told whose rooms these are", async () => {
  mount({ viewer: bound, chat_channels: { ...served, read_state: true, channels: [room()] } });
  await waitFor(() => expect(screen.getByText(/you are reading as/i)).toBeTruthy());
  expect(screen.getByText("Ada")).toBeTruthy();
});

test("an unread badge stops at 99+ when the engine says it stopped counting", async () => {
  // The cap is `chat.UnreadLimit` and the answer SAYS when it was reached, so
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
  await waitFor(() => expect(screen.getByText(/could not be read/i)).toBeTruthy());
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

// THE RAIL IS THE CHAT ROW'S ONE SECTION RENDERER: `nav.ts` gives the row no
// header strip, so a list the row declares and the rail does not draw is a
// section nobody can reach from a room, and a name the rail spells its own way
// is a list the trail, the tab title and the palette call something else.
test("the rail draws every list the chat row declares, under the row's own names", async () => {
  location.hash = "#/chat/mentions";
  mount({
    viewer: bound,
    chat_channels: { ...served, read_state: true, channels: [room()] },
  });
  const rail = await screen.findByRole("navigation", { name: "Your rooms" });
  await waitFor(() => expect(within(rail).getByText("#general")).toBeTruthy());
  const row = workspaceRow("chat");
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
      samePath(section.path, ["chat", "mentions"]) ? "page" : null,
    );
  }
  // AND NOTHING ELSE: every other link in the rail is a room.
  expect(within(rail).getAllByRole("link")).toHaveLength(lists.length + 1);
  // A PERSON HOLDS THE CREDENTIAL every guarded list needs, so no list says
  // it needs one.
  expect(rail.textContent).not.toMatch(/needs an operator credential/);
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

// A COMPANY WHOSE CHAT IS SOMEBODY ELSE'S IS NOT A NODE WITH CHAT DOWN. The
// engine serves no chat question there, and each one asked came back
// `unknown_query` — "the subsystem behind it is not running on this node",
// which sends an operator to debug a node over a line of company
// configuration. The screen says which chat the company runs, asks nothing,
// and offers to start nothing it could not hold.
describe("a company whose chat is not the engine's own", () => {
  test("on a vendor's chat, the screen names it and asks nothing", async () => {
    const { query } = mount({
      viewer: { ...bound, chat_backend: "vendor", chat_vendors: ["mattermost"] },
    });
    await waitFor(() => expect(screen.getByText(/chat is on Mattermost/)).toBeTruthy());
    expect(screen.getByText("chat.backend")).toBeTruthy();
    expect(query.mock.calls.map(([what]) => what)).not.toContain("chat_channels");
    expect(screen.queryByRole("button", { name: /new room/i })).toBeNull();
    expect(screen.queryByRole("navigation", { name: "Your rooms" })).toBeNull();
  });

  test("with both vendors, it names both", async () => {
    mount({
      viewer: { ...bound, chat_backend: "vendor", chat_vendors: ["mattermost", "slack"] },
    });
    await waitFor(() => expect(screen.getByText(/on Mattermost and Slack/)).toBeTruthy());
  });

  test("with no chat at all, it says so", async () => {
    const { query } = mount({ viewer: { ...bound, chat_backend: "none", chat_vendors: [] } });
    await waitFor(() => expect(screen.getByText(/runs no chat/)).toBeTruthy());
    expect(query.mock.calls.map(([what]) => what)).not.toContain("chat_channels");
  });

  test("on the engine's own, the rooms are asked for as ever", async () => {
    const { query } = mount({
      viewer: { ...bound, chat_backend: "native", chat_vendors: [] },
      chat_channels: { ...served, read_state: true, channels: [room()] },
    });
    const rail = await screen.findByRole("navigation", { name: "Your rooms" });
    // WAITED FOR, NOT READ AT ONCE: the rail is drawn in the same commit
    // that arms its question, and the question goes out a beat later, so a
    // loaded runner can show the frame before the socket was asked anything.
    // The room the answer names, drawn in the rail, is the proof it was both
    // asked and answered.
    await waitFor(() => expect(query.mock.calls.map(([what]) => what)).toContain("chat_channels"));
    expect(await within(rail).findByText(/general/)).toBeTruthy();
  });
});

// THE OUTBOX OUTLIVES THE ROOM. A room is remounted for every room opened, and
// an outbox kept inside it threw the unanswered rows away with it — an
// `unknown` row's Retry under its own request id included, which is the only
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
  await screen.findByRole("button", { name: /same id/i });

  fireEvent.click(screen.getByRole("button", { name: /switch rooms/i }));
  await screen.findByLabelText(/^Message #launch/);
  // NOT DRAWN IN A ROOM IT WAS NOT SAID IN.
  expect(screen.queryByText("did that land?")).toBeNull();

  fireEvent.click(screen.getByRole("button", { name: /switch rooms/i }));
  await screen.findByLabelText(/^Message #general/);
  expect(screen.getByText("did that land?")).toBeTruthy();
  const retry = await screen.findByRole("button", { name: /same id/i });
  await waitFor(() => expect(retry.getAttribute("aria-disabled")).not.toBe("true"));
  fireEvent.click(retry);
  await waitFor(() => expect(presses.filter((p) => p.tool === "post_message")).toHaveLength(2));
  const [first, again] = presses.filter((p) => p.tool === "post_message");
  expect(again!.request_id).toBe(first!.request_id);
});

// A NEW CREDENTIAL IS A NEW PERSON, and nothing the screen holds for the last
// one may outlive it. The viewer read keeps its last answer on screen while it
// asks again, so nothing else remounts this screen; and every write here goes
// out as whoever the token names WHEN it is sent — so the last person's
// reading, typing and unsent messages would all be written as the next one.
describe("a new credential is a new person", () => {
  afterEach(() => {
    clearToken();
    vi.useRealTimers();
  });

  /** The tab's reader, as the screen's own hook holds it. */
  function Reader({ onReader }: { onReader: (reader: ChatReader) => void }) {
    onReader(useReadCursors());
    return null;
  }

  /** The credential each request to the engine carried, in order. */
  const credentials = () =>
    vi
      .mocked(fetch)
      .mock.calls.map(
        ([, init]) => (init?.headers as Record<string, string> | undefined)?.Authorization,
      );

  /** Mount the hook and let the viewer read land, on a clock the case drives. */
  async function mountReader(token: string) {
    vi.useFakeTimers();
    storeToken(token);
    const presses = actEngine();
    let reader: ChatReader | null = null;
    mountChat(<Reader onReader={(held) => (reader = held)} />);
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(50);
    });
    return { presses, current: () => reader! };
  }

  test("what the last person read is never written as the next one", async () => {
    const { presses, current } = await mountReader("token-ada");
    const ada = current();
    ada.cursors.see("room-1", 900);

    flush(() => {
      storeToken("token-bo");
    });
    // THE FLUSH THAT WAS DUE, and the one leaving would make: neither goes.
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(READ_FLUSH_MS * 2);
    });
    expect(presses).toHaveLength(0);

    // AND THE NEXT PERSON'S OWN READING IS THEIRS TO WRITE — below the last
    // person's position, which a writer that remembered it would suppress.
    const bo = current();
    expect(bo.reader).not.toBe(ada.reader);
    bo.cursors.see("room-1", 400);
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(READ_FLUSH_MS);
    });
    expect(presses.map(({ tool, args }) => [tool, args])).toEqual([
      ["mark_chat_read", { cursors: { "room-1": 400 } }],
    ]);
    expect(credentials()).toEqual(["Bearer token-bo"]);
  });

  /** Another tab of the dashboard saving `token`: the stored token every tab
   *  shares changes at once, and this document hears of it when the returned
   *  function delivers the `storage` event — a task later, in a browser. */
  function anotherTabSaves(token: string): () => void {
    localStorage.setItem(STORAGE_KEYS.apiToken, token);
    return () =>
      window.dispatchEvent(
        new StorageEvent("storage", { key: STORAGE_KEYS.apiToken, newValue: token }),
      );
  }

  test("a token another tab saved is a new person here too", async () => {
    // EVERY TAB SHARES THE STORED TOKEN, and every write here goes out as
    // whoever it names when it is sent.
    const { presses, current } = await mountReader("token-ada");
    const ada = current();
    ada.cursors.see("room-1", 900);
    const heard = anotherTabSaves("token-bo");

    // THE FLUSH THAT COMES DUE BEFORE THIS TAB HEARS: the token is already
    // the next person's, so the last person's reading is not written at all.
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(READ_FLUSH_MS * 2);
    });
    expect(presses).toHaveLength(0);

    flush(() => heard());
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(READ_FLUSH_MS * 2);
    });
    expect(presses).toHaveLength(0);
    // AND THE NEXT PERSON READS ON AS THEMSELVES, below the last person's
    // position, which a writer that remembered it would suppress.
    const bo = current();
    expect(bo.reader).not.toBe(ada.reader);
    bo.cursors.see("room-1", 400);
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(READ_FLUSH_MS);
    });
    expect(presses.map(({ args }) => args)).toEqual([{ cursors: { "room-1": 400 } }]);
    expect(credentials()).toEqual(["Bearer token-bo"]);
  });

  test("under StrictMode the writer set up again still takes what is read", async () => {
    // REACT SETS AN EFFECT UP, TEARS IT DOWN AND SETS IT UP AGAIN over one
    // component's state in development, which `main.tsx` renders under. The
    // teardown stops the writer, and the second setup keeps using it: stopped
    // for good, the read cursor never moved under `npm run dev`.
    vi.useFakeTimers();
    storeToken("token-ada");
    const presses = actEngine();
    let reader: ChatReader | null = null;
    mountChat(<Reader onReader={(held) => (reader = held)} />, { strict: true });
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(50);
    });
    reader!.cursors.see("room-1", 700);
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(READ_FLUSH_MS);
    });
    expect(presses.map(({ tool, args }) => [tool, args])).toEqual([
      ["mark_chat_read", { cursors: { "room-1": 700 } }],
    ]);
  });

  test("the same token saved again is the same person, and keeps what they read", async () => {
    const { presses, current } = await mountReader("token-ada");
    const ada = current();
    ada.cursors.see("room-1", 900);
    flush(() => {
      storeToken("token-ada");
    });
    await flush(async () => {
      await vi.advanceTimersByTimeAsync(READ_FLUSH_MS);
    });
    expect(current().reader).toBe(ada.reader);
    expect(presses.map(({ args }) => args)).toEqual([{ cursors: { "room-1": 900 } }]);
  });

  test("a room's unsent message and draft are not handed to the next person", async () => {
    storeToken("token-ada");
    // AN ANSWER NOBODY CAN VOUCH FOR keeps the row on screen with a Retry —
    // which, pressed by the next person, would say the words as them.
    actEngine((tool) => (tool === "post_message" ? unknown(tool) : applied(tool)));
    mountChat(<Chat channel="room-1" />, {
      answers: {
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
      },
    });
    const box = (await screen.findByLabelText(/^Message/)) as HTMLTextAreaElement;
    fireEvent.change(box, { target: { value: "said as Ada" } });
    const send = screen.getByRole("button", { name: /^send$/i });
    await waitFor(() => expect(send.getAttribute("aria-disabled")).not.toBe("true"));
    fireEvent.submit(box.closest("form")!);
    await screen.findByRole("button", { name: /same id/i });
    fireEvent.change(box, { target: { value: "half a thought of Ada's" } });

    flush(() => {
      storeToken("token-bo");
    });
    await waitFor(() => expect(screen.queryByText("said as Ada")).toBeNull());
    expect(screen.queryByRole("button", { name: /same id/i })).toBeNull();
    const fresh = (await screen.findByLabelText(/^Message/)) as HTMLTextAreaElement;
    expect(fresh.value).toBe("");
  });
});
