/**
 * Whom a person's message reached and who answered, under its words: the
 * engine's own routing as it kept it, worded here and drawn with uilet's
 * `DeliveryReceipt`.
 */

import { cleanup, screen } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";

import type { ChatMessage, QueryName } from "~/protocol/index.ts";
import { ADA, mountChat } from "~/test/chat.tsx";

import { ReadCursors } from "./cursor.ts";
import { quietHint, reachRecipients, showsReceipt } from "./receipts.ts";
import { LoneRoom } from "./testkit.tsx";

const served = {
  read_level: "session",
  complete: true,
  position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 40 },
};

const channel = {
  v: 1,
  id: "room-1",
  kind: "public",
  name: "general",
  created_at: "2026-01-01T00:00:00Z",
};

function said(over: Partial<ChatMessage> = {}): ChatMessage {
  return {
    v: 1,
    id: "m-1",
    channel_id: "room-1",
    author: "ada",
    author_kind: "human",
    body: "where are we on the release?",
    created_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

afterEach(() => {
  cleanup();
  localStorage.clear();
  location.hash = "#/";
});

describe("the words", () => {
  test("note a reason a person would not guess, and mark who answered", () => {
    const got = reachRecipients(
      {
        recipients: [
          { handle: "pm", reason: "answerer" },
          { handle: "qa", reason: "follow_all" },
          { handle: "sre", reason: "mention" },
        ],
      },
      [{ handle: "pm", message_id: "m-2", at: "2026-01-01T00:01:00Z" }],
      (handle) => handle.toUpperCase(),
    );
    expect(got).toEqual([
      { id: "pm", name: "PM", reason: undefined, answered: true },
      { id: "qa", name: "QA", reason: "every message", answered: false },
      { id: "sre", name: "SRE", reason: undefined, answered: false },
    ]);
  });

  // "REACHED NO AGENT" ONLY WHERE THE WORDS ASKED ONE. Mutation: show it for
  // every empty reach in a named room and a room of people warns under every
  // line.
  test("warn of no agent only where the words named one", () => {
    const none = { recipients: [] };
    const agent = (handle: string) => handle === "pm";
    expect(showsReceipt(said({ reach: none }), agent)).toBe(false);
    expect(showsReceipt(said({ reach: none, mentions: ["bob"] }), agent)).toBe(false);
    expect(showsReceipt(said({ reach: none, mentions: ["bob", "pm"] }), agent)).toBe(true);
    expect(
      showsReceipt(said({ reach: { recipients: [{ handle: "pm", reason: "dm" }] } }), agent),
    ).toBe(true);
    expect(showsReceipt(said(), agent)).toBe(false);
    expect(showsReceipt(said({ reach: none, author_kind: "agent" }), agent)).toBe(false);
    expect(
      showsReceipt(
        said({ reach: none, deleted_at: "2026-01-01T00:02:00Z", mentions: ["pm"] }),
        agent,
      ),
    ).toBe(false);
  });

  // THE ROOM'S STANDING AUDIENCE IS SAID BY THE COMPOSER, not under every
  // line. Mutation: return true for any recipient and every message in a room
  // whose agents hear everything takes a receipt line.
  test("draw no receipt for agents that hear every message, unless one answered", () => {
    const agent = (handle: string) => handle === "pm" || handle === "qa";
    const everyone = {
      recipients: [
        { handle: "pm", reason: "follow_all" },
        { handle: "qa", reason: "follow_all" },
      ],
    };
    expect(showsReceipt(said({ reach: everyone }), agent)).toBe(false);
    expect(showsReceipt(said({ reach: everyone }), agent, [])).toBe(false);
    expect(
      showsReceipt(said({ reach: everyone }), agent, [
        { handle: "pm", message_id: "m-2", at: "2026-01-01T00:01:00Z" },
      ]),
    ).toBe(true);
    const named = {
      recipients: [
        { handle: "pm", reason: "mention" },
        { handle: "qa", reason: "follow_all" },
      ],
    };
    expect(showsReceipt(said({ reach: named, mentions: ["pm"] }), agent)).toBe(true);
    expect(
      showsReceipt(said({ reach: { recipients: [{ handle: "qa", reason: "follow" }] } }), agent),
    ).toBe(true);
  });

  test("say why a draft reaches no agent", () => {
    expect(quietHint("no_agent")).toMatch(/Mention one/);
    expect(quietHint("addressed_to_people")).toMatch(/only people/);
    expect(quietHint(undefined)).toBeUndefined();
  });
});

describe("under a person's message", () => {
  function mount(view: Record<string, unknown>) {
    const answers: Partial<Record<QueryName, unknown>> = {
      chat_channel: { ...served, channel, revision: 3, message_seq: 1, member: true, members: [] },
      chat_messages: { ...served, channel_id: "room-1", messages: [view] },
    };
    const cursors = new ReadCursors({ flush: async () => undefined });
    return mountChat(
      <LoneRoom channelID="room-1" viewer="ada" nameOf={(handle) => handle} cursors={cursors} />,
      { answers, viewer: ADA },
    );
  }

  test("says whom it reached, then who answered", async () => {
    mount({
      message: said({ reach: { recipients: [{ handle: "pm", reason: "answerer" }] } }),
      channel_seq: 1,
      position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 1 },
      answered: [{ handle: "pm", message_id: "m-2", at: "2026-01-01T00:01:00Z" }],
      delivery: "answered",
    });
    expect(await screen.findByText("Reached pm")).toBeTruthy();
    expect(screen.getByText("pm answered")).toBeTruthy();
  });

  // A MESSAGE STORED BEFORE MESSAGES RECORDED THEIR REACH says nothing, rather
  // than claim it reached no agent. Mutation: read a missing reach as an
  // empty one in showsReceipt and this draws "Reached no agent".
  test("draws no receipt where the reach was never recorded", async () => {
    mount({
      message: said({ mentions: ["pm"] }),
      channel_seq: 1,
      position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 1 },
      delivery: "unknown",
    });
    expect(await screen.findByText("where are we on the release?")).toBeTruthy();
    expect(document.querySelector(".crewlet-delivery-receipt")).toBeNull();
  });

  test("says when the agent it named was not reached", async () => {
    mount({
      message: said({ reach: {}, mentions: ["pm"] }),
      channel_seq: 1,
      position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 1 },
      delivery: "no_agent",
    });
    expect(await screen.findByText("Reached no agent")).toBeTruthy();
  });

  test("draws nothing under a post in a room no agent answers", async () => {
    mount({
      message: said({ reach: {} }),
      channel_seq: 1,
      position: { stream: "CREWLET_CHAT_LOG", generation: 1, seq: 1 },
      delivery: "no_agent",
    });
    expect(await screen.findByText("where are we on the release?")).toBeTruthy();
    expect(document.querySelector(".crewlet-delivery-receipt")).toBeNull();
  });
});
