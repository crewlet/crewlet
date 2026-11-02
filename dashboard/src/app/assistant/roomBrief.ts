/**
 * A chat room's catch up, asked of Ask Crewlet from the room's own pane.
 *
 * IT IS A BRIEF TURN OF THE ROOM, nothing of its own: a new conversation, one
 * question in `brief` mode with the room as the page's selection, read off the
 * turn's stream until its one brief card arrives. The engine reads the room as
 * the person, summarises what they have not read (or the latest when nothing
 * is unread) as points that each cite the message they came from, and keeps a
 * point only for a message it read back for them; `needs` is what waits on
 * them. So the pane never posts or reads anything itself, and what it shows is
 * exactly what the drawer would show for the same brief.
 *
 * ONE TURN PER MOUNT, aborted on unmount: closing the pane stops reading the
 * stream (the turn itself finishes on the engine and stays in the person's
 * conversations). `again()` asks a new one, and `openInAssistant()` opens the
 * same conversation in the panel, where the person can go on asking.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import type {
  AssistantBriefCard,
  AssistantBriefPoint,
  AssistantPageContext,
} from "~/contract/assistant.ts";
import { newActOpID } from "~/protocol/act.ts";
import { call, openTurnStream, versioned, type StreamStop } from "~/protocol/assistant.ts";
import { assistantEventId, type AssistantNarrowFrame } from "~/protocol/assistantWire.ts";
import { isAbort, RestError } from "~/protocol/rest.ts";
import { pageSnapshot } from "./context.ts";
import { queueConversation, useAssistantControl } from "./control.ts";

/** Where a room's catch up stands. */
export type RoomBriefPhase = "asking" | "done" | "failed";

/** A room's catch up as the pane draws it. */
export interface RoomBrief {
  /** What the room is about right now, in one line. */
  about: string;
  /** What was said, oldest first, each point with the message it came from. */
  points: AssistantBriefPoint[];
  /** What waits on the person in the room, most pressing first. */
  needs: AssistantBriefCard["needs"];
  /** Questions to ask next, which the panel asks in the same conversation. */
  followUps: string[];
  /** When the engine finished reading for it. */
  asOf: string;
}

export interface RoomBriefState {
  phase: RoomBriefPhase;
  /** The turn's live status line while it reads ("Reading #engineering"). */
  status: string;
  brief: RoomBrief | null;
  /** Why it failed, in a sentence; empty unless `phase` is `failed`. */
  error: string;
  /** Ask for a new catch up. */
  again: () => void;
  /** Open this catch up's conversation in Ask Crewlet, to go on asking. */
  openInAssistant: () => void;
}

/** The status line before the engine has said what it is reading. */
const READING = "Reading the room";

/** What a failure says when the engine gave no words of its own. */
const FAILED = "The catch up could not be written. Try again.";

/** The question the turn is asked with, as the drawer's brief row words it. */
const QUESTION = "Catch me up on this room";

/**
 * The page the turn is asked about: the room screen's own context when the
 * screen has this room open, which carries everything the screen reports, and
 * otherwise the room alone, as the chat screen would report it.
 */
export function roomPageContext(channelID: string): AssistantPageContext {
  const page = pageSnapshot();
  const selection = page?.context.selection;
  if (page && selection && selection.kind === "room" && selection.id === channelID) {
    return page.context;
  }
  return {
    screen: "chatlet",
    title: "Chatlet",
    params: { id: channelID },
    query: {},
    resources: [],
    selection: { kind: "room", id: channelID, noun: "room" },
    source_revision: "",
    accepts: [],
  };
}

/** The brief a card frame carries, as the pane draws it. */
function briefOf(card: AssistantBriefCard): RoomBrief {
  return {
    about: card.about,
    points: card.points ?? [],
    needs: card.needs,
    followUps: card.follow_ups,
    asOf: card.as_of,
  };
}

/** What a stream that stopped for good says. */
function stopWords(reason: StreamStop): string {
  if (reason.code === "refused") return reason.error.message || FAILED;
  return FAILED;
}

/**
 * Ask Ask Crewlet to catch the person up on `channelID`, once per mount and
 * once per `again()`.
 */
export function useRoomBrief(channelID: string): RoomBriefState {
  const control = useAssistantControl();
  const [asked, setAsked] = useState(0);
  const [phase, setPhase] = useState<RoomBriefPhase>("asking");
  const [status, setStatus] = useState(READING);
  const [brief, setBrief] = useState<RoomBrief | null>(null);
  const [error, setError] = useState("");
  const conversation = useRef<string | null>(null);

  useEffect(() => {
    const abort = new AbortController();
    let stream: { close: () => void } | null = null;
    let card: RoomBrief | null = null;
    setPhase("asking");
    setStatus(READING);
    setBrief(null);
    setError("");
    conversation.current = null;

    const fail = (sentence: string) => {
      if (abort.signal.aborted) return;
      stream?.close();
      setError(sentence);
      setPhase("failed");
    };
    const frame = (f: AssistantNarrowFrame) => {
      if (abort.signal.aborted) return;
      switch (f.kind) {
        case "step":
          if (f.step.state === "running" && f.step.label) setStatus(f.step.label);
          return;
        case "card":
          if (f.card.card.type === "brief" && f.card.card.brief) {
            card = briefOf(f.card.card.brief);
            setBrief(card);
          }
          return;
        case "error":
          fail(f.error.message || FAILED);
          return;
        case "done":
          stream?.close();
          if (card) setPhase("done");
          else fail(FAILED);
          return;
      }
    };

    void (async () => {
      try {
        const created = await call("POST /assistant/conversations", versioned, {
          signal: abort.signal,
        });
        conversation.current = created.conversation.id;
        const accepted = await call(
          "POST /assistant/conversations/{id}/turns",
          {
            ...versioned,
            input: { text: QUESTION },
            page_context: roomPageContext(channelID),
            mode: "brief",
          },
          { id: created.conversation.id, opKey: newActOpID(), signal: abort.signal },
        );
        if (abort.signal.aborted) return;
        stream = openTurnStream(
          accepted.stream_url,
          { frame, state: () => {}, stop: (reason) => fail(stopWords(reason)) },
          accepted.after_seq > 0
            ? { lastEventId: assistantEventId(accepted.turn_id, accepted.after_seq) }
            : {},
        );
      } catch (err) {
        if (isAbort(err)) return;
        fail(err instanceof RestError && err.message ? err.message : FAILED);
      }
    })();
    return () => {
      abort.abort();
      stream?.close();
    };
  }, [channelID, asked]);

  const again = useCallback(() => setAsked((n) => n + 1), []);
  const openInAssistant = useCallback(() => {
    const id = conversation.current;
    if (id === null) return;
    queueConversation(id);
    if (!control.open) control.toggle("heading");
  }, [control]);

  return { phase, status, brief, error, again, openInAssistant };
}
