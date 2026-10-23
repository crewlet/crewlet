/**
 * The other list, from the panel a person actually uses.
 *
 * THE CLAIM: what this offers is the rooms the viewer is NOT in. The
 * candidates are derived — from a keyword search over every room this person
 * may read, from the live frames every readable room produces, and from the
 * names of every room they may read, which the frame reads once — and the
 * derivation is worthless if it hands back the rooms they are already in: the
 * rows would render, the names would be right, the Join button would write
 * nothing, and nobody could tell.
 *
 * Beside it, the two rooms the screen must treat differently: a unit's room,
 * which every seat may read and nobody may join — its membership is the org
 * chart's — and a room whose kind this build cannot classify, which the engine
 * refuses every write to.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { CHAT_SEARCH_PAGE } from "~/contract/chat.ts";
import { MAX_IN_FLIGHT_QUERIES, SEARCH_QUERY_MAX } from "~/contract/wire.ts";
import { QueryError } from "~/protocol/index.ts";
import {
  actEngine,
  applied,
  mountChat,
  NATIVE_SERVICES,
  unknown,
  type ActReply,
  type Pressed,
} from "~/test/chat.tsx";
import { session } from "~/protocol/session.ts";
import { RoomNamesProvider } from "~/lib/roomNames.ts";
import { Browse, FULL_PAGE_ROOMS } from "./Browse.tsx";

const position = { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 12 };
const served = { read_level: "session", complete: true, position };

function room(id: string, name: string, kind: string, member: boolean, unit?: string) {
  return {
    ...served,
    channel: { v: 1, id, kind, name, created_at: "2026-01-01T00:00:00Z", ...(unit && { unit }) },
    revision: 1,
    message_seq: 3,
    member,
    members: [{ handle: "bo", joined_at: "2026-01-01T00:00:00Z" }],
  };
}

/** Every room this node would describe, and the ones it refuses. */
const rooms: Record<string, ReturnType<typeof room>> = {
  mine: room("mine", "general", "public", true),
  "open-1": room("open-1", "launch", "public", false),
  unit: room("unit", "engineering", "unit", false, "engineering"),
  odd: room("odd", "future", "broadcast", false),
  quiet: room("quiet", "quiet", "public", false),
};

let reply: ActReply;
let presses: Pressed[];

beforeEach(() => {
  location.hash = "#/";
  searchFails = null;
  roomFails = {};
  slower = {};
  reply = applied("join_channel", { channel: "open-1", name: "launch", joined: true });
  presses = actEngine(() => reply);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/** What the next search answers with, mutable so a second one can differ. */
let hits: string[] = [];

/** How the next search FAILS, when a case wants it to: the error it rejects
 *  with. */
let searchFails: Error | null = null;

/** Per room, how its next description fails — consumed by the read it fails,
 *  so the read after it is answered. */
let roomFails: Record<string, Error> = {};

/** Per question, how many milliseconds its answer takes, for a case that
 *  needs one answer to land before another. */
let slower: Record<string, number> = {};

const after = (ms: number | undefined) =>
  ms ? new Promise((resolve) => setTimeout(resolve, ms)) : Promise.resolve();

/**
 * The panel, inside the frame's reading of the rooms' names where a case
 * names them (`named`): every room the viewer may read, by name.
 */
function mount(found: string[], named?: { id: string; name: string; kind: string }[]) {
  hits = [...found];
  const answer = async (what: string, params?: Record<string, unknown>) => {
    await after(slower[what]);
    if (what === "chat_room_names") return { ...served, rooms: named ?? [] };
    if (what === "chat_channels") return { ...served, read_state: true, channels: [rooms.mine] };
    if (what === "chat_search") {
      if (searchFails) throw searchFails;
      return {
        hits: hits.map((id) => ({ message_id: `m-${id}`, channel_id: id, at: "", score: 1 })),
        searched: 4,
        read_level: "session",
        position,
      };
    }
    if (what === "chat_channel") {
      // A NODE THAT NEVER ANSWERS about one room. Not an error — a read still
      // out there — which is a different state from a refusal and the one a
      // superseded search leaves behind.
      if (params?.channel_id === "slow") return new Promise(() => {});
      const failing = roomFails[String(params?.channel_id)];
      if (failing) {
        delete roomFails[String(params?.channel_id)];
        throw failing;
      }
      const found = rooms[String(params?.channel_id)];
      // A ROOM THIS VIEWER MAY NOT READ ANSWERS EXACTLY AS ONE THAT DOES NOT
      // EXIST — the engine's own rule, because a private room's existence is
      // itself something to know.
      if (!found) throw new Error("not_found");
      return found;
    }
    return {};
  };
  const onClose = vi.fn();
  const panel = <Browse onClose={onClose} />;
  const { store, socket } = mountChat(
    named ? <RoomNamesProvider>{panel}</RoomNamesProvider> : panel,
    // THE NAMES ARE ASKED once the engine has said the company runs Chatlet.
    named ? { answer, services: NATIVE_SERVICES } : { answer },
  );
  return { store, query: socket.query, onClose };
}

/** Run a search, which is how a candidate gets into the list. */
function search(text: string) {
  fireEvent.change(screen.getByRole("searchbox", { name: /find a room/i }), {
    target: { value: text },
  });
  fireEvent.click(screen.getByRole("button", { name: /^search$/i }));
}

function rowFor(name: string): HTMLElement {
  const title = screen.getByText(`#${name}`);
  const row = title.closest(".list-row");
  if (!row) throw new Error(`no row around #${name}`);
  return row as HTMLElement;
}

test("a room the viewer is already in is not offered as one to join", async () => {
  mount(["open-1", "mine"]);
  search("launch");
  await waitFor(() => expect(screen.getByText("#launch")).toBeTruthy());
  // THE EXCLUSION. `mine` matched the search and this person is in it, so it
  // belongs to the rail rather than to this list.
  expect(screen.queryByText("#general")).toBeNull();
});

// A ROOM NOBODY HAS SPOKEN IN IS OFFERED BY NAME: neither a search nor a live
// frame finds it, and the names of the rooms the viewer may read do. Only
// while nothing is searched for, since a search narrows the list. Mutation:
// leave the names out of the offer, or keep them under a search, and this
// fails.
test("a room nobody has spoken in is offered by its name, until something is searched for", async () => {
  mount(
    ["open-1"],
    [
      { id: "mine", name: "general", kind: "public" },
      { id: "quiet", name: "quiet", kind: "public" },
    ],
  );
  expect(await screen.findByText("#quiet")).toBeTruthy();
  expect(screen.queryByText("#general")).toBeNull();
  search("launch");
  await waitFor(() => expect(screen.getByText("#launch")).toBeTruthy());
  expect(screen.queryByText("#quiet")).toBeNull();
});

test("a room is described at this tab's read floor", async () => {
  // EVERY ASK NAMES THE FLOOR (`withFloor`): a room this tab has just joined,
  // described by a node that has not applied the join yet, would be offered
  // back to it as a room to join.
  session.written("chat", "CREWLET_CHAT_LOG@1:90", []);
  const { query } = mount(["open-1"]);
  search("launch");
  await waitFor(() => expect(screen.getByText("#launch")).toBeTruthy());
  expect(query).toHaveBeenCalledWith("chat_channel", {
    channel_id: "open-1",
    read_level: "session",
    min_position: "CREWLET_CHAT_LOG@1:90",
  });
});

test("a room the engine will not describe leaves the list rather than erroring", async () => {
  // "No such room" and "not one you may read" are one answer by design, so a
  // candidate that comes back refused is simply not offered — it is not a
  // failure to report to somebody.
  mount(["open-1", "secret"]);
  search("launch");
  await waitFor(() => expect(screen.getByText("#launch")).toBeTruthy());
  expect(screen.queryByText(/secret/)).toBeNull();
  expect(screen.queryByText(/couldn’t be read/)).toBeNull();
});

test("a search that failed says why, never that no room came back", async () => {
  // AN UNKNOWN ANSWER IS NOT A DEFINITE ONE. A node that could not rank the
  // phrase has said nothing about the company's rooms, and "every room that
  // matched is one you are already in, or nothing matched at all" would be a
  // verdict nobody reached.
  searchFails = new QueryError("unavailable");
  mount(["open-1"]);
  search("launch");
  expect(await screen.findByText(/still catching up/i)).toBeTruthy();
  expect(screen.queryByText(/No room to join/i)).toBeNull();
});

test("a room that could not be read just now is said, and offered once it can be", async () => {
  // "NOT THERE OR NOT YOURS" AND "COULD NOT SAY" ARE DIFFERENT ANSWERS. One
  // blip used to drop the room from the list for the life of the dialog, with
  // nothing on screen to say a room was missing.
  roomFails = { "open-1": new QueryError("unavailable") };
  const { query } = mount(["open-1"]);
  search("launch");
  expect(await screen.findByText(/1 room couldn’t be read just now/)).toBeTruthy();
  expect(screen.queryByText("#launch")).toBeNull();
  expect(screen.queryByText(/No room to join/i)).toBeNull();

  fireEvent.click(screen.getByRole("button", { name: /ask again/i }));
  await waitFor(() => expect(screen.getByText("#launch")).toBeTruthy());
  expect(screen.queryByText(/couldn’t be read/)).toBeNull();
  expect(
    query.mock.calls.filter(
      ([what, params]) =>
        what === "chat_channel" &&
        (params as Record<string, unknown> | undefined)?.channel_id === "open-1",
    ),
  ).toHaveLength(2);
});

test("a room the engine said to ask about again is asked again after its wait", async () => {
  // THE ENGINE'S OWN WAIT, as every socket question honours it: no press is
  // needed for a node that said when it would be ready.
  roomFails = {
    "open-1": new QueryError("unavailable", { code: "behind", detail: null, retryAfter: 1 }),
  };
  mount(["open-1"]);
  search("launch");
  expect(await screen.findByText(/1 room couldn’t be read just now/)).toBeTruthy();
  await waitFor(() => expect(screen.getByText("#launch")).toBeTruthy(), { timeout: 3_000 });
});

test("a unit's room offers no join, and says the org chart is the way in", async () => {
  // The engine refuses a join to a unit's room — its membership is the
  // chart's alone — so a Join here would be a control whose only outcome is a
  // refusal.
  mount(["unit"]);
  search("engineering");
  await waitFor(() => expect(screen.getByText("#engineering")).toBeTruthy());
  expect(within(rowFor("engineering")).queryByRole("button", { name: /join/i })).toBeNull();
  expect(
    within(rowFor("engineering")).getByText(/engineering team’s own room.*org chart/i),
  ).toBeTruthy();
});

test("a room this build cannot classify offers no join, and says why", async () => {
  // An open enum read two-valued falls out as "not private". The engine
  // refuses every write to such a room; a Join button here would be a control
  // whose only outcome is a refusal.
  mount(["odd"]);
  search("future");
  await waitFor(() => expect(screen.getByText("#future")).toBeTruthy());
  expect(within(rowFor("future")).queryByRole("button", { name: /join/i })).toBeNull();
  expect(screen.getByText(/newer version of the engine/i)).toBeTruthy();
});

/** The row's Join, once the viewer read has made it pressable. */
async function joinButton(name: string): Promise<HTMLElement> {
  const button = within(rowFor(name)).getByRole("button", { name: /join/i });
  await waitFor(() => expect(button.getAttribute("aria-disabled")).not.toBe("true"));
  return button;
}

test("joining takes the reader into the room", async () => {
  const { onClose } = mount(["open-1"]);
  search("launch");
  await waitFor(() => expect(screen.getByText("#launch")).toBeTruthy());
  fireEvent.click(await joinButton("launch"));

  await waitFor(() => expect(location.hash).toBe("#/chatlet/open-1"));
  expect(presses).toHaveLength(1);
  expect(presses[0]).toMatchObject({ tool: "join_channel", args: { channel: "open-1" } });
  expect(onClose).toHaveBeenCalled();
});

test("a join nobody can establish does not walk into the room", async () => {
  reply = unknown("join_channel");
  mount(["open-1"]);
  search("launch");
  await waitFor(() => expect(screen.getByText("#launch")).toBeTruthy());
  fireEvent.click(await joinButton("launch"));

  await waitFor(() => expect(screen.getByText(/couldn’t confirm/i)).toBeTruthy());
  expect(location.hash).toBe("#/");
});

test("a room still being read is not reported as no room at all", async () => {
  // WAITING AND EMPTY ARE DIFFERENT ANSWERS, and the panel has both: a room
  // whose lookup is still out there is not "nothing matched". Told apart by a
  // counter the lookups increment, the panel draws its verdict on the render
  // BEFORE the first request is even made — so a search with one slow room in
  // it says "no room came back" and then never says anything else, because
  // nothing that follows re-renders it. Derived from the rows, an id that is
  // neither held nor refused is waiting by construction, from the first
  // render onwards.
  const { query } = mount(["slow"]);
  search("first");
  // THE LOOKUP HAS TO BE OUT THERE before the second search replaces it —
  // waiting for the search itself would let this pass on a lookup that never
  // started.
  await waitFor(() =>
    expect(
      query.mock.calls.some(
        ([what, params]) =>
          what === "chat_channel" &&
          (params as Record<string, unknown> | undefined)?.channel_id === "slow",
      ),
    ).toBe(true),
  );

  expect(screen.queryByText(/No room to join/i)).toBeNull();

  // AND THE VERDICT ARRIVES once there is nothing left out there. The second
  // search matches only a room this person is already in, and the lookup the
  // first one left in flight must not hold the panel open for ever.
  hits.length = 0;
  hits.push("mine");
  search("second");
  await waitFor(() => expect(screen.getByText(/No room to join/i)).toBeTruthy());
});

test("a room seen live is offered once it is read, whenever the viewer's own rooms answered", async () => {
  // THE ROOMS SEEN LIVE ARE LOOKED UP AT ONCE, before the viewer's own rooms
  // have answered — and that answer then takes the room they are in out of
  // the list, which is a new pass over a room still being read. That read is
  // the only one the room gets, so its answer has to draw it: it used to be
  // filed and drawn by nothing, and the dialog said "Reading the rooms" for
  // as long as it was open, offering nothing on an ordinary open.
  slower = { chat_channels: 5, chat_channel: 30 };
  const { store } = mount([]);
  const frame = (id: string, name: string, seq: number) =>
    store.applyChatChange({
      channel_id: id,
      op: "post",
      op_id: `op-${id}`,
      message_id: `m-${id}`,
      channel_seq: 1,
      at: "2026-01-01T00:00:00Z",
      position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
      channel_name: name,
      channel_kind: "public",
    });
  act(() => {
    frame("open-1", "launch", 20);
    frame("mine", "general", 10);
  });
  await waitFor(() => expect(screen.getByText("#launch")).toBeTruthy());
  expect(screen.queryByText("#general")).toBeNull();
  expect(screen.queryByLabelText("Reading the rooms")).toBeNull();
});

/** Every `chat_search` this panel asked, by its phrase. */
function searched(query: ReturnType<typeof mount>["query"]): string[] {
  return query.mock.calls
    .filter(([what]) => what === "chat_search")
    .map(([, params]) => String((params as Record<string, unknown> | undefined)?.q));
}

// THE SEARCH BOUND IS THE ENGINE'S, AND THIS PANEL SENDS NOTHING PAST IT. The
// engine refuses a phrase past `SEARCH_QUERY_MAX` as `bad_params`; sent, the
// refusal reached this panel as an empty search and it said "No room to join
// came back" about a search that never ran.
test("a phrase at the search bound is asked", async () => {
  const { query } = mount(["open-1"]);
  const phrase = "l".repeat(SEARCH_QUERY_MAX);
  search(phrase);
  await waitFor(() => expect(searched(query)).toEqual([phrase]));
  await waitFor(() => expect(screen.getByText("#launch")).toBeTruthy());
});

test("a phrase past the search bound is not asked, and the panel says why", async () => {
  const { query } = mount(["open-1"]);
  search("l".repeat(SEARCH_QUERY_MAX + 1));
  expect(
    await screen.findByText(
      `This is ${SEARCH_QUERY_MAX + 1} bytes, and a search takes at most ${SEARCH_QUERY_MAX}. Search on a few keywords or a phrase, not a pasted passage.`,
    ),
  ).toBeTruthy();
  expect(searched(query)).toEqual([]);
  // NOT A VERDICT ON THE COMPANY'S ROOMS: nothing was asked.
  expect(screen.queryByText(/No room to join/i)).toBeNull();
});

// ONE WAVE OF LOOKUPS, AND THE REST SAID. Every candidate row is a
// `chat_channel` read, and the socket runs `MAX_IN_FLIGHT_QUERIES` of them at
// once: a search that matched more rooms than that asks about one wave and
// says how many more matched, rather than queueing the screen behind its own
// lookups or presenting the wave as everything.
test("a search that matched many rooms looks up one wave of them and says how many more", async () => {
  const many = Array.from({ length: MAX_IN_FLIGHT_QUERIES + 3 }, (_, n) => `far-${n}`);
  const { query } = mount(many);
  search("launch");
  const more = await screen.findByText(/3 more rooms matched than fit here/);
  expect(more).toBeTruthy();
  // COUNTED ONCE THE WAVE HAS SETTLED, not when the caption appears: the
  // caption renders with the search's answer and the lookups are issued by an
  // effect after that render commits, so a count taken there can read none of
  // them. Every `far-` room is refused, so the empty answer is the panel
  // having heard back from each lookup it asked.
  expect(await screen.findByText("No room to join")).toBeTruthy();
  const looked = query.mock.calls.filter(([what]) => what === "chat_channel");
  expect(looked).toHaveLength(MAX_IN_FLIGHT_QUERIES);
});

// THE PAGE IS THE ENGINE'S, AND A FULL ONE SAYS MORE MAY MATCH. The rooms this
// panel offers are the rooms its hits are in; asked for more than the engine
// answers, a clamped page offered a partial set of rooms as every room that
// matched. Mutation: ask with the transcript's PAGE again and the limit goes
// red; drop the note and the full page's assertion does.
test("the finder asks for the engine's page, and a full one says more may match", async () => {
  const { query } = mount(Array.from({ length: CHAT_SEARCH_PAGE }, () => "open-1"));
  search("launch");
  await waitFor(() =>
    expect(screen.getByText(new RegExp(FULL_PAGE_ROOMS.slice(0, 30)))).toBeTruthy(),
  );
  const limits = query.mock.calls
    .filter(([what]) => what === "chat_search")
    .map(([, params]) => (params as Record<string, unknown> | undefined)?.limit);
  expect(limits).toEqual([CHAT_SEARCH_PAGE]);
});
