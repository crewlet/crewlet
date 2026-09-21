/**
 * The other list, from the panel a person actually uses.
 *
 * THE CLAIM: what this offers is the rooms the viewer is NOT in. The engine
 * serves no directory read, so the candidates are derived — from a keyword
 * search over every room this person may read, and from the live frames every
 * readable room produces — and the derivation is worthless if it hands back
 * the rooms they are already in: the rows would render, the names would be
 * right, the Join button would write nothing, and nobody could tell.
 *
 * Beside it, the two rooms the screen must treat differently: a unit's room,
 * which can be joined like any public one, and a room whose kind this build
 * cannot classify, which the engine refuses every write to.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { Store } from "~/protocol/index.ts";
import { Browse } from "./Browse.tsx";
import { joinRoom } from "./writes.ts";
import type { WriteResult } from "./writes.ts";

vi.mock("./writes.ts", () => ({ joinRoom: vi.fn() }));

const joined = vi.mocked(joinRoom);
const position = { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 12 };
const served = { read_level: "session", complete: true, position };

function room(id: string, name: string, kind: string, member: boolean) {
  return {
    ...served,
    channel: { v: 1, id, kind, name, created_at: "2026-01-01T00:00:00Z" },
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
  unit: room("unit", "engineering", "unit", false),
  odd: room("odd", "future", "broadcast", false),
};

beforeEach(() => {
  joined.mockReset();
  location.hash = "#/";
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

/** What the next search answers with, mutable so a second one can differ. */
let hits: string[] = [];

function mount(found: string[]) {
  hits = [...found];
  const store = new Store();
  const query = vi.fn(async (what: string, params?: Record<string, unknown>) => {
    if (what === "chat_channels") return { ...served, read_state: true, channels: [rooms.mine] };
    if (what === "chat_search") {
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
      const found = rooms[String(params?.channel_id)];
      // A ROOM THIS VIEWER MAY NOT READ ANSWERS EXACTLY AS ONE THAT DOES NOT
      // EXIST — the engine's own rule, because a private room's existence is
      // itself something to know.
      if (!found) throw new Error("not_found");
      return found;
    }
    return {};
  });
  const onClose = vi.fn();
  const onWrote = vi.fn();
  render(
    <ClientContext.Provider value={{ store, socket: { query, focus: vi.fn() } } as never}>
      <Router>
        <Browse onClose={onClose} onWrote={onWrote} />
      </Router>
    </ClientContext.Provider>,
  );
  return { store, query, onClose, onWrote };
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

test("a room the engine will not describe leaves the list rather than erroring", async () => {
  // "No such room" and "not one you may read" are one answer by design, so a
  // candidate that comes back refused is simply not offered — it is not a
  // failure to report to somebody.
  mount(["open-1", "secret"]);
  search("launch");
  await waitFor(() => expect(screen.getByText("#launch")).toBeTruthy());
  expect(screen.queryByText(/secret/)).toBeNull();
});

test("a unit's room can be joined like any room the company can read", async () => {
  mount(["unit"]);
  search("engineering");
  await waitFor(() => expect(screen.getByText("#engineering")).toBeTruthy());
  expect(within(rowFor("engineering")).getByRole("button", { name: /join/i })).toBeTruthy();
});

test("a room this build cannot classify offers no join, and says why", async () => {
  // An open enum read two-valued falls out as "not private". The engine
  // refuses every write to such a room; a Join button here would be a control
  // whose only outcome is a refusal.
  mount(["odd"]);
  search("future");
  await waitFor(() => expect(screen.getByText("#future")).toBeTruthy());
  expect(within(rowFor("future")).queryByRole("button", { name: /join/i })).toBeNull();
  expect(screen.getByText(/newer build/i)).toBeTruthy();
});

test("joining takes the reader into the room", async () => {
  joined.mockResolvedValue({
    outcome: "applied",
    answer: {
      outcome: "applied",
      op_id: "op-1",
      position,
      revision: 2,
      channel: rooms["open-1"]!.channel,
    },
    detail: "",
    code: "",
  } as WriteResult);
  const { onClose, onWrote } = mount(["open-1"]);
  search("launch");
  await waitFor(() => expect(screen.getByText("#launch")).toBeTruthy());
  fireEvent.click(within(rowFor("launch")).getByRole("button", { name: /join/i }));

  await waitFor(() => expect(location.hash).toBe("#/chat/open-1"));
  expect(joined).toHaveBeenCalledWith("open-1");
  expect(onWrote).toHaveBeenCalled();
  expect(onClose).toHaveBeenCalled();
});

test("a join nobody can establish does not walk into the room", async () => {
  joined.mockResolvedValue({
    outcome: "unknown",
    answer: {
      outcome: "unknown",
      op_id: "op-1",
      position,
      revision: 0,
      channel: rooms["open-1"]!.channel,
    },
    detail: "",
    code: "",
  } as WriteResult);
  mount(["open-1"]);
  search("launch");
  await waitFor(() => expect(screen.getByText("#launch")).toBeTruthy());
  fireEvent.click(within(rowFor("launch")).getByRole("button", { name: /join/i }));

  await waitFor(() => expect(screen.getByText(/broker did not answer/i)).toBeTruthy());
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

  expect(screen.queryByText(/No room to join came back/i)).toBeNull();

  // AND THE VERDICT ARRIVES once there is nothing left out there. The second
  // search matches only a room this person is already in, and the lookup the
  // first one left in flight must not hold the panel open for ever.
  hits.length = 0;
  hits.push("mine");
  search("second");
  await waitFor(() => expect(screen.getByText(/No room to join came back/i)).toBeTruthy());
});
