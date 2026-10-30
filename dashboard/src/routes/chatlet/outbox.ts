/**
 * A message between being typed and being a message.
 *
 * Optimistic send is allowed here and A LIE IS NOT, which is the whole of this
 * file. A post arbitrates nothing at the broker, so it is fast and it almost
 * always lands — but "almost always" is three outcomes, not two, and the act
 * transport (`protocol/act.ts`, ADR-0024) answers all three:
 *
 *   - **applied** — the record is on the log and this node has written it.
 *     The row is real; it is drawn from the transcript the moment the re-read
 *     brings it back, and until then it is still drawn as pending, because
 *     what is on screen is not yet the message the company holds.
 *   - **pending** — the record is published and this node has not applied it.
 *     Nothing is wrong. It is still not a sent message on this screen — and
 *     it is the one answer whose verdict is still to come, because a message
 *     is DECIDED where it is applied (ADR-0046): the room may have been
 *     archived, or its author taken out of it, by a record just ahead. So a
 *     row answered pending is ASKED AGAIN, under its own operation key, once
 *     the answering node has had the wait a read is promised
 *     ([PENDING_REASK_MS]) and then less and less often: the operation ledger
 *     answers the same key with what the applier decided, so the repeat
 *     writes nothing twice and comes back applied, still pending, or the
 *     refusal — which the row then says. Left alone, a refused post stayed
 *     "accepted" for ever, because the transcript that settles a row never
 *     carries a message that is in no room.
 *   - **unknown** — nobody can say whether it landed. It may be in the room
 *     already and it may never arrive, and the ONLY safe move is to say so and
 *     offer the retry, under the same operation key — the press's
 *     `Idempotency-Key`: the engine derives the message's own id from it, so a
 *     fresh one would say it twice.
 *
 * A refusal is the fourth state and the only one that is not ambiguous: the
 * engine read the request and would not take it.
 *
 * THE ROWS LEAVE BY BEING SUPERSEDED, never by a timer. An outgoing message is
 * dropped when the transcript carries its id — which is the same event that
 * makes the real row appear — so the two can never be on screen at once and a
 * row can never quietly become "sent" without the message existing.
 *
 * ONE ROW PER PRESS, AND THE ROW KEEPS ITS PRESS. `useAct` holds one refusal
 * and retries one press; a room holds as many unanswered messages as somebody
 * typed, each with a Retry of its own — so each row carries the operation key
 * its first send went under and every retry of it sends that key again.
 *
 * AND THE OUTBOX OUTLIVES THE ROOM. It is the whole screen's, held above every
 * room (`Chat.tsx`), because a room is remounted for each room the person
 * opens: kept inside it, switching rooms threw away every unanswered row of
 * the room being left — an `unknown` row's same-id Retry included, which is
 * the only safe move for that message, so a person who came back and typed it
 * again said it twice. Each room settles ITS OWN rows against what its
 * transcript carries ([OutboxHandle.settle]); a room that is not open settles
 * nothing, so its rows wait for the person to come back to them. It is still
 * ONE SEAT'S: the screen remounts it under each seat the reader acts as
 * (`viewer.owner`), and a different person signing in reloads the tab
 * (`lib/session.ts`), which takes it with them — the engine scopes a key by
 * the principal that sent it, so a row retried as somebody else would be
 * their message rather than the first one again.
 */

import { useCallback, useEffect, useRef, useState } from "react";

import { PENDING_REASK_MS } from "~/contract/chat.ts";
import { RETRY_AFTER_MAX_MS } from "~/contract/retry.ts";
import { useAct } from "~/lib/useAct.ts";
import type { WriteAccess } from "~/lib/useWriteAccess.ts";
import { newActOpID, type ActResult } from "~/protocol/act.ts";
import type { ChatReceipt } from "~/protocol/index.ts";

/** Where an outgoing message has got to. */
export type SendState = "sending" | "accepted" | "unknown" | "refused";

/** One message this tab is trying to say. */
export interface Outgoing {
  /** The press's operation key, minted once and REUSED by every retry — the
   *  engine derives the message's id from it. */
  opId: string;
  channelID: string;
  /** The message being answered, empty for a room post. */
  threadRoot: string;
  /** What was typed. The handles it names and whether it addresses the room
   *  are the engine's reading of it, never a field this tab resolves. */
  body: string;
  state: SendState;
  /**
   * The id the engine derived, once it has answered. Empty while in flight and
   * on a refusal — and it is what settles this row, because the transcript
   * carrying that id IS the message existing.
   */
  messageID: string;
  /** The engine's own sentence about a refusal, written for the person. */
  detail: string;
  /** Whether a refusal was the node rather than the message — draining, or
   *  unable to answer just now — so the same press may land a moment later. */
  retryable: boolean;
  /** When this tab started trying, which is the only instant this row has. It
   *  is NOT the message's: a message is stamped with the broker's time, and
   *  that is what the transcript will render. */
  startedAt: number;
  /**
   * What the engine said of an ACCEPTED row: `applied` — this node decided
   * it and it landed — or `pending` — published and not decided here yet,
   * which is the answer a row is asked again for. Empty on every other
   * state.
   */
  answer: "applied" | "pending" | "";
  /** How often a pending row has been asked again, which spaces the next. */
  reasks: number;
  /** When a pending row is next asked again (ms since the epoch); 0 for a
   *  row that is not waiting on one. */
  reaskAt: number;
}

/**
 * How long after its last answer a row answered `pending` is asked again: the
 * wait a read is promised the answering node's applier reaches what it waits
 * for ([PENDING_REASK_MS]) first, doubling with every answer that is still
 * pending, and never more than the dashboard's ceiling on waiting for an
 * engine ([RETRY_AFTER_MAX_MS]) — a node that has not decided a post in that
 * long is one a person stops believing will.
 */
export function reaskDelay(reasks: number): number {
  return Math.min(PENDING_REASK_MS * 2 ** reasks, RETRY_AFTER_MAX_MS);
}

/** The row asked again `reasks` times, due its next ask at the delay that
 *  count earns, from `now`. */
function reaskLater(row: Outgoing, reasks: number, now: number): Outgoing {
  return { ...row, reasks, reaskAt: now + reaskDelay(reasks) };
}

/** Whether this row is still waiting on the company — the state a composer
 *  draws as pending and never as sent. */
export function inFlight(row: Outgoing): boolean {
  return row.state === "sending" || row.state === "accepted";
}

/**
 * What one answer does to the row that caused it — or, for a press that was
 * never sent (null: this browser could not act when it was made), what that
 * leaves the row as.
 */
function applyResult(row: Outgoing, result: ActResult | null, blocked = ""): Outgoing {
  if (result === null) {
    // NOTHING WAS SENT, so nothing about the message changed: an unknown row
    // stays unknown — it may still be in the room — and a first send that
    // never left is a refusal this browser made, with the reason it made it.
    if (row.state === "unknown") return row;
    return {
      ...row,
      ...unanswered,
      state: "refused",
      messageID: "",
      detail: blocked || "Nothing was sent.",
      retryable: true,
    };
  }
  switch (result.kind) {
    case "applied":
    case "pending": {
      // ACCEPTED, NOT SENT. The record exists; the message on this screen is
      // still the draft until the room hands it back, which is a round trip
      // away and carries the broker's own instant, the sequence, and whatever
      // else has happened since.
      const id = (result.receipt as Partial<ChatReceipt> | null)?.message_id;
      const accepted: Outgoing = {
        ...row,
        state: "accepted",
        messageID: typeof id === "string" ? id : "",
        detail: "",
        retryable: false,
        answer: result.kind,
        reasks: 0,
        reaskAt: 0,
      };
      return result.kind === "pending" ? reaskLater(accepted, 0, Date.now()) : accepted;
    }
    case "unknown":
      // NOT a message id: there may be no message. The operation key to
      // retry under is the one this row already holds.
      return { ...row, ...unanswered, state: "unknown", messageID: "", detail: "" };
    case "refused":
      return refusedRow(row, result);
  }
}

/** What a row waiting on no answer carries. */
const unanswered = { retryable: false, answer: "", reasks: 0, reaskAt: 0 } as const;

/** The row a refusal leaves, in the engine's own words. */
function refusedRow(row: Outgoing, result: Extract<ActResult, { kind: "refused" }>): Outgoing {
  return {
    ...row,
    ...unanswered,
    state: "refused",
    messageID: "",
    detail: [result.sentence, result.hint].filter((part) => part.trim() !== "").join(" "),
    retryable: result.retryable,
  };
}

/**
 * What asking again does to a row answered `pending`.
 *
 * THE RECORD IS DURABLE — the first answer said so — so only a DECISION moves
 * the row: `applied` is the post having landed (the transcript then settles
 * it), and a refusal of the message itself is what the applier decided, drawn
 * as every refusal is. Anything that is not a decision — still pending, an
 * answer nobody can vouch for, a node that refused for now, a browser that
 * could not send — leaves it accepted and asks again later, because none of
 * them says anything about the message, and drawing one as "it may not have
 * been said" would take back what the first answer established.
 */
export function applyReask(row: Outgoing, result: ActResult | null, now: number): Outgoing {
  if (result?.kind === "applied") {
    const id = (result.receipt as Partial<ChatReceipt> | null)?.message_id;
    return {
      ...row,
      answer: "applied",
      messageID: typeof id === "string" && id !== "" ? id : row.messageID,
      reaskAt: 0,
    };
  }
  if (result?.kind === "refused" && !result.retryable) return refusedRow(row, result);
  return reaskLater(row, row.reasks + 1, now);
}

/** What a composer hands over. */
export interface Draft {
  channelID: string;
  /** The message being answered, empty for a room post. */
  threadRoot?: string;
  body: string;
}

export interface OutboxHandle {
  /** Everything this tab is still trying to say, in every room, oldest first. */
  rows: Outgoing[];
  /**
   * Drop ONE ROOM's rows its transcript now carries: `settled` is the set of
   * message ids the open room and its thread hold. Rows of every other room
   * are left alone — that room's transcript is not on screen to say.
   *
   * Passed in rather than read here because this hook has no view of the
   * conversation, and a second read of it would be a second answer to "is
   * this message in the room". Stable across renders, so a room can settle
   * from an effect.
   */
  settle: (channelID: string, settled: ReadonlySet<string>) => void;
  /** Say it. Resolves when the engine has answered, whatever it answered. */
  send: (draft: Draft) => Promise<void>;
  /** Try again under the SAME operation key. */
  retry: (opId: string) => Promise<void>;
  /** Take a refused or unknown row off the screen. It does not unsay
   *  anything — on an unknown row the message may well be in the room. */
  discard: (opId: string) => void;
  /** Whether this browser may say something in the room, and why not. */
  post: WriteAccess;
  /** Whether it may answer in a thread, and why not. */
  reply: WriteAccess;
}

/** The outgoing messages of one person's chat screen, in every room. */
export function useOutbox(): OutboxHandle {
  const [rows, setRows] = useState<Outgoing[]>([]);
  // QUIET, both of them: a toast per message would say "sent" about a row the
  // transcript is still drawing as pending — the row is where the outcome is
  // drawn, and that is the one place it can be drawn honestly.
  const post = useAct("post_message");
  const reply = useAct("reply_in_thread");

  // SUPERSEDED, NOT EXPIRED. The row goes when the real one arrives, so the
  // two are never both on screen — and a message that never arrives stays
  // visible and says what it is, which is the point.
  const settle = useCallback((channelID: string, settled: ReadonlySet<string>) => {
    setRows((held) => {
      const keep = held.filter(
        (row) => !(row.channelID === channelID && row.messageID && settled.has(row.messageID)),
      );
      // Same array when nothing settled: React bails out of the render, so
      // a room settling on every commit costs nothing.
      return keep.length === held.length ? held : keep;
    });
  }, []);

  // ONE PRESS'S CALL, the first send, a retry and a re-ask alike: the same
  // tool, the same arguments and the row's own operation key every time.
  const ask = useCallback(
    (row: Outgoing): Promise<ActResult | null> => {
      const label = { done: "Sent", quiet: true };
      const options = { opId: row.opId };
      return row.threadRoot
        ? reply.run(
            { channel: row.channelID, reply_to: row.threadRoot, body: row.body },
            label,
            options,
          )
        : post.run({ channel: row.channelID, body: row.body }, label, options);
    },
    [post, reply],
  );

  const run = useCallback(
    async (row: Outgoing): Promise<void> => {
      setRows((held) =>
        held.some((r) => r.opId === row.opId)
          ? held.map((r) => (r.opId === row.opId ? row : r))
          : [...held, row],
      );
      const result = await ask(row);
      const blocked = row.threadRoot ? reply.access : post.access;
      setRows((held) =>
        held.map((r) =>
          r.opId === row.opId ? applyResult(r, result, blocked.can ? "" : blocked.reason) : r,
        ),
      );
    },
    [ask, post.access, reply.access],
  );

  // A ROW ANSWERED PENDING IS ASKED AGAIN when it is due, and only one ask at
  // a time per row: the timers are re-armed on every change to the rows, but
  // each against the row's own due instant, so a room busy with other presses
  // never pushes a waiting row's ask back.
  const reasking = useRef(new Set<string>());
  useEffect(() => {
    const due = rows.filter(
      (row) =>
        row.state === "accepted" && row.answer === "pending" && !reasking.current.has(row.opId),
    );
    const timers = due.map((row) =>
      setTimeout(
        () => {
          reasking.current.add(row.opId);
          void ask(row).then(
            (result) => {
              reasking.current.delete(row.opId);
              setRows((held) =>
                held.map((r) =>
                  r.opId === row.opId && r.state === "accepted" && r.answer === "pending"
                    ? applyReask(r, result, Date.now())
                    : r,
                ),
              );
            },
            () => {
              reasking.current.delete(row.opId);
              setRows((held) =>
                held.map((r) =>
                  r.opId === row.opId && r.state === "accepted" && r.answer === "pending"
                    ? applyReask(r, null, Date.now())
                    : r,
                ),
              );
            },
          );
        },
        Math.max(0, row.reaskAt - Date.now()),
      ),
    );
    return () => timers.forEach(clearTimeout);
  }, [rows, ask]);

  const send = useCallback(
    (draft: Draft) =>
      run({
        // MINTED HERE, ONCE PER MESSAGE, rather than by the hook per call:
        // the row has to hold the key before the answer arrives, because the
        // answer it needs it for may never arrive.
        opId: newActOpID(),
        channelID: draft.channelID,
        threadRoot: draft.threadRoot ?? "",
        body: draft.body,
        state: "sending",
        messageID: "",
        detail: "",
        startedAt: Date.now(),
        ...unanswered,
      }),
    [run],
  );

  const retry = useCallback(
    async (opId: string): Promise<void> => {
      const row = rows.find((r) => r.opId === opId);
      if (!row) return;
      await run({ ...row, ...unanswered, state: "sending", detail: "" });
    },
    [rows, run],
  );

  const discard = useCallback((opId: string) => {
    setRows((held) => held.filter((row) => row.opId !== opId));
  }, []);

  return { rows, settle, send, retry, discard, post: post.access, reply: reply.access };
}
