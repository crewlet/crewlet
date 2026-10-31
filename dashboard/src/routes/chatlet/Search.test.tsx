/**
 * Searching Chatlet: messages, people and rooms, as the reader types.
 *
 * THE INVARIANTS: messages are the engine's ranking over every room the reader
 * may read, narrowed only by the engine (In, From), and said with their
 * denominator; a partial index is never "nothing matched"; a phrase past the
 * engine's bound is never sent; a full page says there may be more; the Who
 * filter narrows the page the engine ranked and says so; a refusal is said in
 * the engine's own words.
 */

import { act, cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, test, vi } from "vitest";

import { CHAT_SEARCH_PAGE } from "~/contract/chat.ts";
import { SEARCH_QUERY_MAX } from "~/contract/wire.ts";
import { QueryError } from "~/protocol/index.ts";
import { pick } from "~/testing.tsx";
import { CHAT_ORG, SERVED } from "~/test/chatAccess.ts";

import { mountInFrame } from "./frameKit.tsx";
import { FULL_PAGE, SearchView } from "./Search.tsx";

const hit = (id: string, author: string, snippet: string) => ({
  message_id: id,
  channel_id: "room-1",
  author,
  snippet,
  at: "2026-10-30T09:00:00Z",
  score: 1,
});

const ANSWER = (hits: unknown[], extra: Record<string, unknown> = {}) => ({
  hits,
  searched: 3,
  read_level: "session",
  position: SERVED.position,
  ...extra,
});

const ROOMS = {
  ...SERVED,
  rooms: [
    {
      channel: { v: 1, id: "room-1", kind: "public", name: "launch", created_at: "" },
      members: 4,
    },
  ],
};

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function mount(q: string, answer?: (what: string, params?: Record<string, unknown>) => unknown) {
  const asked: Record<string, unknown>[] = [];
  const mounted = mountInFrame(<SearchView />, {
    hash: `#/chatlet/search?q=${encodeURIComponent(q)}`,
    answer: async (what, params) => {
      if (what === "chat_search") asked.push(params ?? {});
      if (answer) return answer(what, params);
      if (what === "chat_search") return ANSWER([hit("m-1", "bo", "the date moved to Thursday")]);
      if (what === "chat_rooms") return ROOMS;
      if (what === "chat_channels") return { ...SERVED, read_state: true, channels: [] };
      return {};
    },
  });
  act(() => mounted.store.applyOrg(CHAT_ORG as never));
  return { ...mounted, asked };
}

describe("messages", () => {
  test("a search is the engine's ranking, said with how many rooms it ran over", async () => {
    const { asked } = mount("launch");
    expect(await screen.findByText(/the date moved to Thursday/)).toBeTruthy();
    expect(asked[0]).toMatchObject({ q: "launch", limit: CHAT_SEARCH_PAGE });
    expect(screen.getByText(/Searched 3 rooms\./)).toBeTruthy();
    // A HIT IN A ROOM THE READER IS NOT IN IS NAMED from the rooms they may read.
    expect(screen.getByText(/#launch/)).toBeTruthy();
  });

  test("From narrows the search at the engine, never by filtering a page here", async () => {
    const { asked } = mount("launch");
    await screen.findByText(/the date moved to Thursday/);
    pick(screen.getByRole("combobox", { name: /^from$/i }), /From: Bo Chen/);
    await waitFor(() => expect(asked.some((p) => p.author === "bo")).toBe(true));
  });

  test("the Who filter narrows the engine's page and says so", async () => {
    mount("launch", async (what) =>
      what === "chat_search"
        ? ANSWER([hit("m-1", "bo", "from a person"), hit("m-2", "ops", "from an agent")])
        : what === "chat_rooms"
          ? ROOMS
          : {},
    );
    await screen.findByText("from an agent");
    fireEvent.click(screen.getByRole("radio", { name: /^agents$/i }));
    await waitFor(() => expect(screen.queryByText("from a person")).toBeNull());
    expect(screen.getByText(/Showing the 1 of the best 2 matches/)).toBeTruthy();
  });

  test("a full page says there may be more", async () => {
    const hits = Array.from({ length: CHAT_SEARCH_PAGE }, (_, i) =>
      hit(`m-${i}`, "bo", `hit ${i}`),
    );
    mount("launch", async (what) => (what === "chat_search" ? ANSWER(hits) : {}));
    expect(await screen.findByText(new RegExp(FULL_PAGE.slice(0, 30)))).toBeTruthy();
  });

  test("an empty ranking from an index still building says so, not that nothing matched", async () => {
    mount("launch", async (what) => (what === "chat_search" ? ANSWER([], { building: true }) : {}));
    expect(await screen.findByText(/Still indexing/)).toBeTruthy();
    expect(screen.queryByText("No messages match")).toBeNull();
  });

  test("a phrase past the search bound is not asked, and the screen says why", async () => {
    const { asked } = mount("l".repeat(SEARCH_QUERY_MAX + 1));
    expect(await screen.findByText(/a search takes at most/)).toBeTruthy();
    expect(asked).toEqual([]);
    expect(screen.queryByText("No messages match")).toBeNull();
  });

  test("a refusal is said in the engine's own words", async () => {
    const sentence = "q is too short to rank";
    mount("launch", async (what) => {
      if (what === "chat_search") throw new QueryError("bad_params", null, sentence);
      return {};
    });
    expect(await screen.findByText(new RegExp(sentence))).toBeTruthy();
  });
});

describe("people and rooms", () => {
  test("People lists the seats whose words match, and a person opens their profile", async () => {
    mount("bo");
    fireEvent.click(await screen.findByRole("tab", { name: /^people/i }));
    fireEvent.click(await screen.findByRole("button", { name: /Bo Chen/ }));
    await waitFor(() => expect(decodeURIComponent(location.hash)).toContain("pane=profile:bo"));
  });

  test("Rooms lists the rooms the reader may read whose name or topic matches", async () => {
    mount("laun");
    fireEvent.click(await screen.findByRole("tab", { name: /^rooms/i }));
    expect(await screen.findByText("launch")).toBeTruthy();
  });
});
