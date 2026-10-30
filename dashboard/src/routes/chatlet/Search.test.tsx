/**
 * Searching the company's conversation, and the one bound the screen keeps.
 *
 * CHATLET'S SEARCH IS A RANKED SEARCH, so it takes the bound the other two take
 * (`SEARCH_QUERY_MAX`, the engine's `knowledge.MaxQueryBytes`): the engine
 * refuses a longer phrase as `bad_params` rather than cutting it, and a refusal
 * this screen drew as its results read as "Nothing matched" — a company that
 * never said the thing, rather than a phrase too long to ask about. So a phrase
 * past the bound is not sent at all, and the screen says why in place of the
 * results, exactly as the work search and the knowledge screen do.
 */

import { act, cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { focusSearchTarget } from "~/app/searchTarget.ts";
import { CHAT_SEARCH_PAGE } from "~/contract/chat.ts";
import { SEARCH_QUERY_MAX } from "~/contract/wire.ts";
import { mountChat } from "~/test/chat.tsx";
import { FULL_PAGE, Search } from "./Search.tsx";

const position = { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 9 };

const general = {
  channel: { v: 1, id: "room-1", kind: "public", name: "general", created_at: "" },
  participants: [],
  unread: 0,
  mentions: 0,
};

beforeEach(() => {
  location.hash = "#/chatlet/search";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function mount(q: string) {
  location.hash = `#/chatlet/search?q=${encodeURIComponent(q)}`;
  const { socket } = mountChat(
    <Search channels={[general as never]} viewer="ada" nameOf={(handle) => handle} />,
    {
      answer: async (what) =>
        what === "chat_search"
          ? {
              hits: [
                {
                  message_id: "m-1",
                  channel_id: "room-1",
                  author: "bo",
                  snippet: "the launch moved to Thursday",
                  at: "2026-01-01T00:00:00Z",
                  score: 1,
                },
              ],
              searched: 3,
              read_level: "session",
              position,
            }
          : {},
    },
  );
  const asked = () =>
    socket.query.mock.calls
      .filter(([what]) => what === "chat_search")
      .map(([, params]) => String((params as Record<string, unknown> | undefined)?.q));
  return { asked };
}

test("a hit whose room could not be read just now says so, and asks again", async () => {
  // A ROOM OUTSIDE THE RAIL IS NAMED BY ITS OWN READ, and one that could not
  // be read just now is said rather than titled "a room" as if that were the
  // answer — and asked for again, rather than never.
  location.hash = `#/chatlet/search?q=${encodeURIComponent("launch")}`;
  let fails = true;
  mountChat(<Search channels={[]} viewer="ada" nameOf={(handle) => handle} />, {
    answer: async (what) => {
      if (what === "chat_search") {
        return {
          hits: [
            {
              message_id: "m-9",
              channel_id: "room-9",
              author: "bo",
              snippet: "the launch moved",
              at: "2026-01-01T00:00:00Z",
              score: 1,
            },
          ],
          searched: 3,
          read_level: "session",
          position,
        };
      }
      if (what === "chat_channel") {
        if (fails) {
          fails = false;
          throw new Error("query_failed");
        }
        return {
          channel: { v: 1, id: "room-9", kind: "public", name: "launch", created_at: "" },
          member: false,
          members: [],
          read_level: "session",
          position,
        };
      }
      return {};
    },
  });
  expect(await screen.findByText(/couldn’t be read just now/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: /ask again/i }));
  await waitFor(() => expect(screen.getByText("#launch")).toBeTruthy());
  expect(screen.queryByText(/couldn’t be read just now/)).toBeNull();
});

test("a re-search that keeps a room still being read names it when the read lands", async () => {
  // ONE READ PER ROOM, AND IT OUTLIVES THE SEARCH THAT ASKED IT. The second
  // search keeps room-9, whose name the first one is still reading, so it is
  // not asked about again — which makes that first read the only one it gets.
  // Its answer used to be filed and drawn by nothing, and the hit stayed
  // titled "a room" with no word that its name could not be read.
  location.hash = `#/chatlet/search?q=${encodeURIComponent("launch")}`;
  const hit = (id: string) => ({
    message_id: `m-${id}`,
    channel_id: id,
    author: "bo",
    snippet: `said in ${id}`,
    at: "2026-01-01T00:00:00Z",
    score: 1,
  });
  // THE NAMES ARE HELD until the second search has been drawn, so the reads
  // the first one started are still out when it lands.
  let release!: () => void;
  const held = new Promise<void>((resolve) => (release = resolve));
  const { socket } = mountChat(<Search channels={[]} viewer="ada" nameOf={(handle) => handle} />, {
    answer: async (what, params) => {
      if (what === "chat_search") {
        return {
          hits: params?.q === "launch" ? [hit("room-9"), hit("room-8")] : [hit("room-9")],
          searched: 3,
          read_level: "session",
          position,
        };
      }
      if (what === "chat_channel") {
        const id = String(params?.channel_id);
        await held;
        return {
          channel: {
            v: 1,
            id,
            kind: "public",
            name: id === "room-9" ? "launch" : "ops",
            created_at: "",
          },
          member: false,
          members: [],
          read_level: "session",
          position,
        };
      }
      return {};
    },
  });
  // THE FIRST SEARCH'S READS ARE OUT before the second search replaces it.
  await waitFor(() =>
    expect(socket.query.mock.calls.filter(([what]) => what === "chat_channel")).toHaveLength(2),
  );
  fireEvent.change(screen.getByRole("searchbox", { name: /search the company's chat/i }), {
    target: { value: "moved" },
  });
  fireEvent.click(screen.getByRole("button", { name: /^search$/i }));
  await waitFor(() => expect(screen.queryByText(/said in room-8/)).toBeNull());
  expect(screen.getByText(/said in room-9/)).toBeTruthy();
  await act(async () => release());
  await waitFor(() => expect(screen.getByText("#launch")).toBeTruthy());
  expect(screen.queryByText("a room")).toBeNull();
  expect(socket.query.mock.calls.filter(([what]) => what === "chat_channel")).toHaveLength(2);
});

test("a phrase at the search bound is asked, and its hits are drawn", async () => {
  const phrase = "l".repeat(SEARCH_QUERY_MAX);
  const { asked } = mount(phrase);
  await waitFor(() => expect(asked()).toEqual([phrase]));
  expect(await screen.findByText(/the launch moved to Thursday/)).toBeTruthy();
});

test("a phrase past the search bound is not asked, and the screen says why", async () => {
  // SENT, IT IS REFUSED, and the refusal is not a verdict on the company's
  // conversation: "Nothing matched" here would be a claim about a search that
  // never ran.
  const { asked } = mount("l".repeat(SEARCH_QUERY_MAX + 1));
  expect(
    await screen.findByText(
      `This is ${SEARCH_QUERY_MAX + 1} bytes, and a search takes at most ${SEARCH_QUERY_MAX}. Search on a few keywords or a phrase, not a pasted passage.`,
    ),
  ).toBeTruthy();
  expect(asked()).toEqual([]);
  expect(screen.queryByText(/Nothing matched/)).toBeNull();
  expect(screen.queryByText(/the launch moved to Thursday/)).toBeNull();
});

test("the bound is the engine's measure — bytes, not characters", async () => {
  // A MULTI-BYTE PHRASE UNDER THE BOUND IN CHARACTERS AND OVER IT IN BYTES is
  // exactly the one a screen counting `length` would send and have refused.
  const phrase = "é".repeat(SEARCH_QUERY_MAX / 2 + 1);
  expect(phrase.length).toBeLessThanOrEqual(SEARCH_QUERY_MAX);
  const { asked } = mount(phrase);
  expect(await screen.findByText(/a search takes at most/)).toBeTruthy();
  expect(asked()).toEqual([]);
});

test("`/` focuses this screen's search rather than opening the palette", async () => {
  // THE KEY SEARCHES WHAT THE READER IS LOOKING AT (`app/searchTarget.ts`):
  // on chat's search screen that is its own box, with the phrase selected to
  // type over.
  mount("launch");
  const box = (await screen.findByRole("searchbox", {
    name: /search the company's chat/i,
  })) as HTMLInputElement;
  expect(focusSearchTarget()).toBe(true);
  expect(document.activeElement).toBe(box);
  expect(box.selectionStart).toBe(0);
  expect(box.selectionEnd).toBe("launch".length);
});

// A PARTIAL INDEX IS NOT "NOBODY SAID THAT": the answering node's chat index
// had not caught up with its transcript (`building`), so an empty ranking is
// one over part of what was said — said so, and never drawn as "Nothing
// matched". Mutation: drop the `building` guard on the empty verdict and the
// second assertion goes red; drop the callout and the first does.
test("an empty ranking from an index still building says so, not that nothing matched", async () => {
  location.hash = `#/chatlet/search?q=${encodeURIComponent("launch")}`;
  mountChat(<Search channels={[general as never]} viewer="ada" nameOf={(handle) => handle} />, {
    answer: async (what) =>
      what === "chat_search"
        ? { hits: [], searched: 3, building: true, read_level: "session", position }
        : {},
  });
  expect(await screen.findByText(/Still indexing/)).toBeTruthy();
  expect(screen.queryByText(/Nothing matched/)).toBeNull();
});

// THE PAGE IS THE ENGINE'S, AND A FULL ONE SAYS SO. The engine answers at most
// `CHAT_SEARCH_PAGE` hits and has no next page; asked for more, it clamped
// without a word, and a full page read as everything that matched. So the
// screen asks for exactly that page, and under a full one says there may be
// more and how to narrow it — and under a page that is not full, says nothing
// of the kind. Mutation: ask with the transcript's PAGE again and the first
// assertion goes red; drop the full-page sentence and the second does.
test("a search asks for the engine's page, and a full page says there may be more", async () => {
  const full = Array.from({ length: CHAT_SEARCH_PAGE }, (_, i) => ({
    message_id: `m-${i}`,
    channel_id: "room-1",
    author: "bo",
    snippet: `launch note ${i}`,
    at: "2026-01-01T00:00:00Z",
    score: 1,
  }));
  location.hash = `#/chatlet/search?q=${encodeURIComponent("launch")}`;
  const { socket } = mountChat(
    <Search channels={[general as never]} viewer="ada" nameOf={(handle) => handle} />,
    {
      answer: async (what) =>
        what === "chat_search" ? { hits: full, searched: 3, read_level: "session", position } : {},
    },
  );
  await screen.findByText(/launch note 0$/);
  const limits = socket.query.mock.calls
    .filter(([what]) => what === "chat_search")
    .map(([, params]) => (params as Record<string, unknown> | undefined)?.limit);
  expect(limits).toEqual([CHAT_SEARCH_PAGE]);
  expect(screen.getByText(new RegExp(FULL_PAGE.slice(0, 30)))).toBeTruthy();
});

test("a page that is not full says nothing of more matches", async () => {
  mount("launch");
  await screen.findByText(/the launch moved to Thursday/);
  expect(screen.queryByText(new RegExp(FULL_PAGE.slice(0, 30)))).toBeNull();
});
