/**
 * A message between being typed and being a message.
 *
 * Optimistic send is allowed here and A LIE IS NOT, which is the whole of this
 * file. A post arbitrates nothing at the broker, so it is fast and it almost
 * always lands — but "almost always" is three outcomes, not two, and the
 * engine answers all three:
 *
 *   - **applied** — the record is on the log and this node has written it.
 *     The row is real; it is drawn from the transcript the moment the re-read
 *     brings it back, and until then it is still drawn as pending, because
 *     what is on screen is not yet the message the company holds.
 *   - **pending** — the record is published and this node has not applied it.
 *     Nothing is wrong. It is still not a sent message on this screen.
 *   - **unknown** — the broker did not say. It may be in the room already and
 *     it may never arrive, and the ONLY safe move is to say so and offer the
 *     retry, under the same operation id: a fresh one would derive a second
 *     message and say it twice.
 *
 * A refusal is the fourth state and the only one that is not ambiguous: the
 * engine read the request and would not take it.
 *
 * THE ROWS LEAVE BY BEING SUPERSEDED, never by a timer. An outgoing message is
 * dropped when the transcript carries its id — which is the same event that
 * makes the real row appear — so the two can never be on screen at once and a
 * row can never quietly become "sent" without the message existing.
 */

import { useCallback, useEffect, useState } from "react";

import { operationID, postMessage, replyToMessage, type WriteResult } from "./writes.ts";

/** Where an outgoing message has got to. */
export type SendState = "sending" | "accepted" | "unknown" | "refused";

/** One message this tab is trying to say. */
export interface Outgoing {
  /** The idempotency key, minted once and REUSED by every retry. */
  operationID: string;
  channelID: string;
  /** The message being answered, empty for a room post. */
  threadRoot: string;
  body: string;
  mentions: string[];
  collective: boolean;
  state: SendState;
  /**
   * The id the engine derived, once it has answered. Empty while in flight and
   * on a refusal — and it is what settles this row, because the transcript
   * carrying that id IS the message existing.
   */
  messageID: string;
  /** The engine's own sentence about a refusal, written for the person. */
  detail: string;
  /** When this tab started trying, which is the only instant this row has. It
   *  is NOT the message's: a message is stamped with the broker's time, and
   *  that is what the transcript will render. */
  startedAt: number;
}

/** Whether this row is still waiting on the company — the state a composer
 *  draws as pending and never as sent. */
export function inFlight(row: Outgoing): boolean {
  return row.state === "sending" || row.state === "accepted";
}

/** What one answer does to the row that caused it. */
export function applyResult(row: Outgoing, result: WriteResult): Outgoing {
  switch (result.outcome) {
    case "applied":
    case "pending":
      // ACCEPTED, NOT SENT. The record exists; the message on this screen is
      // still the draft until the room hands it back, which is a round trip
      // away and carries the broker's own instant, the sequence, and whatever
      // else has happened since.
      return { ...row, state: "accepted", messageID: result.answer?.message?.id ?? "", detail: "" };
    case "unknown":
      return {
        ...row,
        state: "unknown",
        // NOT the message id: there may be no message. What the answer does
        // carry is the op id to retry under, which this row already holds.
        messageID: "",
        detail: "",
      };
    default:
      return { ...row, state: "refused", messageID: "", detail: result.detail };
  }
}

/** What a composer hands over. */
export interface Draft {
  channelID: string;
  /** The message being answered, empty for a room post. */
  threadRoot?: string;
  body: string;
  mentions?: string[];
  collective?: boolean;
}

export interface OutboxHandle {
  /** Everything this tab is still trying to say, oldest first. */
  rows: Outgoing[];
  /** Say it. Resolves when the engine has answered, whatever it answered. */
  send: (draft: Draft) => Promise<void>;
  /** Try again under the SAME operation id. */
  retry: (id: string) => Promise<void>;
  /** Take a refused or unknown row off the screen. It does not unsay
   *  anything — on an unknown row the message may well be in the room. */
  discard: (id: string) => void;
}

/**
 * The outgoing messages of one tab.
 *
 * `settled` is the set of message ids the transcript now carries; rows that
 * reach it are dropped. It is passed in rather than read here because this
 * hook has no view of the conversation, and a second read of it would be a
 * second answer to "is this message in the room".
 */
export function useOutbox(settled: ReadonlySet<string>): OutboxHandle {
  const [rows, setRows] = useState<Outgoing[]>([]);

  // SUPERSEDED, NOT EXPIRED. The row goes when the real one arrives, so the
  // two are never both on screen — and a message that never arrives stays
  // visible and says what it is, which is the point.
  useEffect(() => {
    setRows((held) => {
      const keep = held.filter((row) => !(row.messageID && settled.has(row.messageID)));
      // Same array when nothing settled: React bails out of the render, so
      // this effect running on every commit costs nothing.
      return keep.length === held.length ? held : keep;
    });
  }, [settled]);

  const run = useCallback(async (row: Outgoing): Promise<void> => {
    setRows((held) =>
      held.some((r) => r.operationID === row.operationID)
        ? held.map((r) => (r.operationID === row.operationID ? row : r))
        : [...held, row],
    );
    const body = {
      body: row.body,
      mentions: row.mentions,
      collective: row.collective,
      operation_id: row.operationID,
    };
    const result = row.threadRoot
      ? await replyToMessage(row.channelID, row.threadRoot, body)
      : await postMessage(row.channelID, body);
    setRows((held) =>
      held.map((r) => (r.operationID === row.operationID ? applyResult(r, result) : r)),
    );
  }, []);

  const send = useCallback(
    (draft: Draft) =>
      run({
        operationID: operationID(),
        channelID: draft.channelID,
        threadRoot: draft.threadRoot ?? "",
        body: draft.body,
        mentions: draft.mentions ?? [],
        collective: draft.collective ?? false,
        state: "sending",
        messageID: "",
        detail: "",
        startedAt: Date.now(),
      }),
    [run],
  );

  const retry = useCallback(
    async (id: string): Promise<void> => {
      const row = rows.find((r) => r.operationID === id);
      if (!row) return;
      await run({ ...row, state: "sending", detail: "" });
    },
    [rows, run],
  );

  const discard = useCallback((id: string) => {
    setRows((held) => held.filter((row) => row.operationID !== id));
  }, []);

  return { rows, send, retry, discard };
}
