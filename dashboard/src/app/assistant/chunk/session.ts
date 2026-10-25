/**
 * The open conversation, and everything that moves it: the one store the
 * drawer draws from.
 *
 * MODULE STATE, NOT THE DRAWER'S: a reader who closes the drawer mid turn has
 * an answer still coming, and the Ask button says so (`../status.ts`). So the
 * conversation, its stream and its turn outlive the drawer's component, and
 * the drawer is a view of this store. A page reload starts it again from the
 * tab's record (`../tab.ts`) and the engine's transcript.
 *
 * EVERY MOVE IS A [reduce] EVENT (`turn.ts`): the person's gestures dispatch
 * one, and so does every answer and frame, so the state is the reducer's and
 * this module only runs its effects: a request, a stream, a floor raised.
 *
 * WHAT A WRITE THE ASSISTANT MADE MOVED reaches every screen: the engine's
 * `context.invalidate` action names the log and the position its write
 * landed at, and the tab's read floors rise to it (`protocol/session.ts`),
 * exactly as a button's own write raises them, so the page under the drawer
 * reads its own change.
 */

import { useSyncExternalStore } from "react";
import {
  ASSISTANT_ACTION_CONTEXT_INVALIDATE,
  type AssistantProposalAnswer,
  type AssistantProposalCard,
} from "~/contract/assistant.ts";
import { SESSION_QUERIES } from "~/contract/domains.ts";
import {
  call,
  openTurnStream,
  versioned,
  type StreamStop,
  type TurnStream,
} from "~/protocol/assistant.ts";
import { assistantEventId, type AssistantNarrowFrame } from "~/protocol/assistantWire.ts";
import { newActOpID } from "~/protocol/act.ts";
import { RestError, isAbort } from "~/protocol/rest.ts";
import { session as floors, type SessionDomain } from "~/protocol/session.ts";
import { writeFailed } from "~/protocol/writeOutcome.ts";
import { setAssistantActivity } from "../status.ts";
import { writeTab } from "../tab.ts";
import type { PageSnapshot } from "../context.ts";
import {
  EMPTY,
  activityOf,
  openTurn,
  reduce,
  type ConversationView,
  type SendRefusal,
  type TurnEvent,
} from "./turn.ts";

let view: ConversationView = EMPTY;
let stream: TurnStream | null = null;
/** Bumped by every gesture that replaces the conversation, so a late answer for the last one is dropped. */
let generation = 0;
const listeners = new Set<() => void>();

function dispatch(event: TurnEvent): void {
  view = reduce(view, event);
  setAssistantActivity(activityOf(view));
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** The conversation now. */
export function conversation(): ConversationView {
  return view;
}

/** The conversation, as a value the drawer re-renders on. */
export function useConversation(): ConversationView {
  return useSyncExternalStore(subscribe, conversation, () => EMPTY);
}

/** What a refusal of a question says to the person, from the engine's answer. */
export function sendRefusalOf(err: RestError): SendRefusal {
  // THE WAIT THE ENGINE NAMED, on a 429 (the person's question limit) or a
  // 503 (no model answering): a countdown the person can read, never a guess.
  const retry = err.retryAfterSeconds;
  return {
    code: err.code,
    sentence: refusalWords(err),
    retryAfterSeconds: typeof retry === "number" && retry > 0 ? Math.ceil(retry) : null,
  };
}

/** The sentence for a question the engine would not take. */
function refusalWords(err: RestError): string {
  switch (err.code) {
    case "assistant_busy":
      return "Question limit reached.";
    case "assistant_turn_running":
      return "An answer is still running in this conversation. Stop it, or wait for it to finish.";
    case "assistant_protocol_mismatch":
      return "Crewlet was updated. Reload the page to keep asking.";
    case "assistant_unavailable":
      return "The assistant cannot answer right now. Try again in a moment.";
    case "budget_exhausted":
      return "The assistant's budget for now is spent. Try again when it resets.";
    default:
      if (err.unanswered)
        return "The question could not be sent. Check your connection and try again.";
      return err.sentence || err.detail || "The engine refused the question.";
  }
}

/** Raise the tab's read floors to a write the engine says landed. */
function invalidated(frame: AssistantNarrowFrame): void {
  if (frame.kind !== "action" || frame.action.name !== ASSISTANT_ACTION_CONTEXT_INVALIDATE) return;
  const write = frame.action.invalidate;
  if (!write || !Object.hasOwn(SESSION_QUERIES, write.domain)) return;
  floors.written(write.domain as SessionDomain, write.position, []);
}

/** The stream stopped for good: what the drawer says about it. */
function stopped(reason: StreamStop): TurnEvent {
  switch (reason.code) {
    case "protocol_mismatch":
      return { type: "fault", fault: "protocol_mismatch" };
    case "refused":
      return {
        type: "fault",
        fault:
          reason.error.code === "assistant_protocol_mismatch" ? "protocol_mismatch" : "refused",
      };
    default:
      return { type: "fault", fault: "malformed" };
  }
}

/** Read the turn at `streamUrl`, resuming after `lastEventId` where one is held. */
function follow(streamUrl: string, lastEventId: string | undefined): void {
  stream?.close();
  const mine = generation;
  const live = (event: TurnEvent) => {
    if (mine === generation) dispatch(event);
  };
  stream = openTurnStream(
    streamUrl,
    {
      frame: (frame) => {
        if (mine !== generation) return;
        invalidated(frame);
        dispatch({ type: "frame", frame });
      },
      state: (state, since) => live({ type: "stream", state, since }),
      stop: (reason) => live(stopped(reason)),
    },
    lastEventId === undefined ? {} : { lastEventId },
  );
}

/** Make the conversation a question will be asked in, if there is none yet. */
async function ensureConversation(): Promise<string> {
  if (view.id !== null) return view.id;
  const mine = generation;
  const created = await call("POST /assistant/conversations", versioned);
  if (mine === generation) {
    dispatch({
      type: "created",
      id: created.conversation.id,
      title: created.conversation.title,
    });
    writeTab({ conversation: created.conversation.id });
  }
  return created.conversation.id;
}

/**
 * Ask `text`, with the page attached where the person left it attached.
 * The question is sent under one operation key, minted here, so a retry the
 * person makes is the same question to the engine.
 */
export async function send(text: string, page: PageSnapshot | null): Promise<void> {
  const opKey = newActOpID();
  const key = opKey;
  const mine = generation;
  dispatch({
    type: "send",
    key,
    opKey,
    text,
    page: page ? { title: page.title, screen: page.context.screen } : null,
    at: new Date().toISOString(),
  });
  try {
    const id = await ensureConversation();
    const accepted = await call(
      "POST /assistant/conversations/{id}/turns",
      { ...versioned, input: { text }, page_context: page ? page.context : null },
      { id, opKey },
    );
    if (mine !== generation) return;
    dispatch({ type: "accepted", key, turnId: accepted.turn_id, afterSeq: accepted.after_seq });
    follow(
      accepted.stream_url,
      accepted.after_seq > 0 ? assistantEventId(accepted.turn_id, accepted.after_seq) : undefined,
    );
  } catch (err) {
    if (mine !== generation || isAbort(err)) return;
    const refusal =
      err instanceof RestError
        ? sendRefusalOf(err)
        : { code: "", sentence: "The question could not be sent.", retryAfterSeconds: null };
    dispatch({ type: "refused", key, refusal });
  }
}

/** Stop the running turn; any change it is waiting on is declined by the engine. */
export async function stop(): Promise<void> {
  const turn = openTurn(view);
  if (!turn || turn.id === "") return;
  dispatch({ type: "stopping", turnId: turn.id });
  try {
    const answer = await call("POST /assistant/turns/{id}/stop", versioned, { id: turn.id });
    dispatch({ type: "stopped", turnId: turn.id, state: answer.state });
  } catch {
    dispatch({ type: "stop_failed", turnId: turn.id });
  }
}

/** Leave the conversation for the home; it stays in Conversations. */
export function newConversation(): void {
  generation += 1;
  stream?.close();
  stream = null;
  dispatch({ type: "reset" });
  writeTab({ conversation: null });
}

/** Open a conversation from the engine's transcript, and rejoin a turn still running in it. */
export async function openConversation(id: string): Promise<void> {
  generation += 1;
  const mine = generation;
  stream?.close();
  stream = null;
  const transcript = await call("GET /assistant/conversations/{id}", null, { id });
  if (mine !== generation) return;
  dispatch({ type: "transcript", transcript });
  writeTab({ conversation: transcript.conversation.id });
  const open = transcript.open_turn;
  if (open) {
    follow(
      open.stream_url,
      open.after_seq > 0 ? assistantEventId(open.turn_id, open.after_seq) : undefined,
    );
  }
}

/** Remove the open conversation from the person's history, and go home. */
export async function clearConversation(): Promise<void> {
  const id = view.id;
  if (id === null) return;
  await call("DELETE /assistant/conversations/{id}", null, { id });
  newConversation();
}

/** Dial the running turn's stream again, after the drawer's Try again. */
export function reconnect(): void {
  const turn = openTurn(view);
  if (!turn || turn.id === "" || view.id === null) return;
  follow(
    `/assistant/conversations/${encodeURIComponent(view.id)}/stream?turn=${encodeURIComponent(turn.id)}`,
    turn.seq > 0 ? assistantEventId(turn.id, turn.seq) : undefined,
  );
}

/** The person's recent conversations, newest first. */
export async function recentConversations(limit = 4) {
  return (await call("GET /assistant/conversations", null, { query: { limit } })).conversations;
}

/**
 * The operation each approval is sent under, by proposal: minted at the first
 * press and sent again on a retry, so a retry after an answer nobody heard is
 * the same write to the engine, which it collapses (ADR-0047).
 */
const approvals = new Map<string, string>();

/** A decision's answer: the proposal at its state now, and the resumed turn's stream. */
function decided(proposal: AssistantProposalCard, answer: AssistantProposalAnswer): void {
  dispatch({
    type: "approval_settled",
    proposalId: proposal.proposal_id,
    state: answer.proposal.state,
  });
  if (answer.stream_url) {
    follow(
      answer.stream_url,
      answer.after_seq ? assistantEventId(answer.turn_id, answer.after_seq) : undefined,
    );
  }
}

/** What a refused or unanswered decision came to, on its card. */
function undecided(proposal: AssistantProposalCard, err: unknown): void {
  const proposalId = proposal.proposal_id;
  if (err instanceof RestError) {
    // THE ENGINE SAYS THE CARD IS AT ANOTHER STATE NOW: it expired, or the
    // record it was prepared against moved. The card says so in its own words.
    if (err.code === "proposal_expired" || err.code === "proposal_stale") {
      dispatch({
        type: "approval_settled",
        proposalId,
        state: err.code === "proposal_expired" ? "expired" : "stale",
      });
      return;
    }
    const failed = writeFailed(err);
    dispatch({
      type: "approval_failed",
      proposalId,
      failure:
        failed.kind === "unknown"
          ? "Could not confirm. It may have landed."
          : `${failed.sentence} Nothing changed.`,
      unknown: failed.kind === "unknown",
    });
    return;
  }
  dispatch({
    type: "approval_failed",
    proposalId,
    failure: "Could not confirm. It may have landed.",
    unknown: true,
  });
}

/**
 * Approve a change the assistant prepared, exactly as the card showed it: the
 * digest it was drawn from travels with the approval, and the engine refuses
 * one that moved. A step-up the engine asks for is confirmed by the
 * dashboard's own dialog and the approval sent again (`protocol/rest.ts`).
 */
export async function approve(proposal: AssistantProposalCard): Promise<void> {
  const id = proposal.proposal_id;
  const opKey = approvals.get(id) ?? newActOpID();
  approvals.set(id, opKey);
  dispatch({ type: "approve", proposalId: id });
  try {
    const answer = await call(
      "POST /assistant/proposals/{id}/approve",
      { ...versioned, digest: proposal.digest },
      { id, opKey },
    );
    decided(proposal, answer);
  } catch (err) {
    if (isAbort(err)) return;
    undecided(proposal, err);
  }
}

/** Decline a change; nothing is changed, and the turn hears that it was declined. */
export async function decline(proposal: AssistantProposalCard): Promise<void> {
  try {
    const answer = await call(
      "POST /assistant/proposals/{id}/decline",
      { ...versioned, digest: proposal.digest },
      { id: proposal.proposal_id },
    );
    decided(proposal, answer);
  } catch (err) {
    if (isAbort(err)) return;
    undecided(proposal, err);
  }
}

/** Tell the conversation the page under the drawer moved, so a prepared page change is stale. */
export function pageMoved(revision: string): void {
  dispatch({ type: "page_moved", revision });
}

/** Test seam: a fresh store, as a fresh page has. */
export function resetSessionForTest(): void {
  generation += 1;
  stream?.close();
  stream = null;
  view = EMPTY;
  setAssistantActivity("idle");
}
