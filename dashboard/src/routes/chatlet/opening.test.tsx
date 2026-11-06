/**
 * A room opens on its messages, never on an empty transcript.
 *
 * THE INVARIANT: the transcript is drawn only from a tail that has been read.
 * The first page answers one commit before the room merges it into the tail
 * it draws, and a transcript drawn in that commit was an empty list: on
 * screen, a frame of nothing under a lone day pill before the messages came,
 * since the transcript measured itself over no rows and windowed its first
 * real frame from the top while the list rested at its end.
 *
 * Each draw of the transcript is recorded at the module, so the count is the
 * room's own: what it handed the transcript, every time it drew one.
 */

import { cleanup, screen } from "@testing-library/react";
import { createElement, forwardRef } from "react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import type { QueryName } from "~/protocol/index.ts";
import { actEngine, applied, mountChat } from "~/test/chat.tsx";

import { ReadCursors } from "./cursor.ts";
import { LoneRoom } from "./testkit.tsx";
import type { TranscriptHandle, TranscriptProps } from "./Transcript.tsx";

const drawn = vi.hoisted(() => ({ tails: [] as { loaded: boolean; rows: number }[] }));

vi.mock("./Transcript.tsx", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./Transcript.tsx")>();
  const Recorded = forwardRef<TranscriptHandle, TranscriptProps>(function Recorded(props, ref) {
    drawn.tails.push({ loaded: props.tail.loaded, rows: props.tail.messages.length });
    return createElement(actual.Transcript, { ...props, ref });
  });
  return { ...actual, Transcript: Recorded };
});

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 40 },
};

function message(seq: number) {
  return {
    message: {
      v: 1,
      id: `m-${seq}`,
      channel_id: "room-1",
      author: "bo",
      author_kind: "human",
      body: `line ${seq}`,
      created_at: "2026-01-01T00:00:00Z",
    },
    channel_seq: seq,
    position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq },
  };
}

let answers: Partial<Record<QueryName, unknown>>;

beforeEach(() => {
  drawn.tails = [];
  actEngine((tool) => applied(tool, { channel: "room-1" }));
  answers = {
    chat_channel: {
      ...served,
      channel: {
        v: 1,
        id: "room-1",
        kind: "public",
        name: "general",
        created_at: "2026-01-01T00:00:00Z",
      },
      revision: 3,
      message_seq: 2,
      member: true,
      members: [],
    },
    chat_messages: { ...served, channel_id: "room-1", messages: [message(2), message(1)] },
  };
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

function mount() {
  const cursors = new ReadCursors({ flush: async () => undefined });
  return mountChat(
    <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
    { answers },
  );
}

// Mutation: gate the transcript on the query's loading flag again and the
// first draw is of the unread, empty tail.
test("the transcript is drawn only from a tail that has been read", async () => {
  mount();
  await screen.findByText("line 2");
  expect(drawn.tails.length).toBeGreaterThan(0);
  expect(drawn.tails.filter((tail) => !tail.loaded)).toEqual([]);
  expect(drawn.tails[0]).toEqual({ loaded: true, rows: 2 });
});

test("a room whose first page could not be read says so, and draws no transcript", async () => {
  const cursors = new ReadCursors({ flush: async () => undefined });
  mountChat(
    <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
    {
      answer: async (what) => {
        if (what === "chat_messages") throw new Error("bad_params");
        return answers[what as QueryName] ?? {};
      },
    },
  );
  expect(await screen.findByText("The engine refused this request.")).toBeTruthy();
  expect(drawn.tails).toEqual([]);
  expect(screen.queryByLabelText("Reading the room")).toBeNull();
});
