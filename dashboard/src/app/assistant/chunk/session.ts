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
 * `context.invalidate` action names the domain and the position its write
 * landed at, where its answer named one, and arrives for every write that
 * landed. The tab's read floor rises to the position, exactly as a button's
 * own write raises it, and every live read asks again
 * (`protocol/session.ts`'s EVERY_QUESTION), so the page under the drawer
 * reads its own change whichever route made it.
 *
 * A PAGE ACTION the stream carries is taken here, once per tab, by the
 * claim, apply and report flow (`pageActions.ts`): this module knows which
 * changes this tab approved and which questions it asked, which is what
 * decides whether this tab is the one to apply it.
 */

import { useSyncExternalStore } from "react";
import {
  ASSISTANT_ACTION_CONTEXT_INVALIDATE,
  type AssistantErrorCode,
  type AssistantFileRef,
  type AssistantProposalAnswer,
  type AssistantProposalCard,
  type AssistantProposalDecision,
  type AssistantTurnMode,
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
import { rereadViewer } from "~/lib/viewer.ts";
import { EVERY_QUESTION, session as floors, type SessionDomain } from "~/protocol/session.ts";
import { writeFailed } from "~/protocol/writeOutcome.ts";
import { setAssistantActivity } from "../status.ts";
import { writeTab } from "../tab.ts";
import { pageSnapshot, undoPageAction, type PageSnapshot } from "../context.ts";
import { takeAction } from "./pageActions.ts";
import {
  EMPTY,
  activityOf,
  isOpen,
  openTurn,
  reduce,
  type ConversationView,
  type DecisionRefusal,
  type SendRefusal,
  type TurnEvent,
} from "./turn.ts";

let view: ConversationView = EMPTY;
let stream: TurnStream | null = null;

/**
 * The streams of earlier turns followed beside the conversation's own: a
 * decision on a card of a turn the next question superseded resumes nothing,
 * and its receipt arrives on that turn's stream, which is read here rather
 * than in place of the turn now running.
 */
const asides = new Set<TurnStream>();

/** End every stream this tab reads: the conversation's and the asides. */
function closeStreams(): void {
  stream?.close();
  stream = null;
  for (const aside of asides) aside.close();
  asides.clear();
}
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
    hint: err.code === "assistant_model_not_ready" && err.hint !== "" ? err.hint : null,
    retryAfterSeconds: typeof retry === "number" && retry > 0 ? Math.ceil(retry) : null,
    remedy: err.code === "budget_exhausted" ? err.remedy : null,
  };
}

/** The sentence for a question the engine would not take. */
function refusalWords(err: RestError): string {
  switch (err.code) {
    case "assistant_busy":
      return "Question limit reached.";
    case "assistant_turn_running":
      return "An answer is still running in this conversation. Wait for it to finish.";
    case "assistant_protocol_mismatch":
      return "Crewlet was updated. Reload the page to keep asking.";
    case "assistant_unavailable":
      return "The assistant cannot answer right now. Try again in a moment.";
    case "assistant_model_not_ready":
      // THE ENGINE'S OWN SENTENCE for the model's state (signed out, its plan
      // spent), the one a failed answer would have carried.
      return err.detail || err.sentence;
    case "budget_exhausted":
      // WHOSE CEILING, by the remedy the engine named: a person's own is
      // theirs to have raised, and the company's binds everybody.
      return err.remedy?.setting === "assistant_token_budget"
        ? "Your own assistant budget for this window is spent."
        : "The company's token budget for this window is spent.";
    default:
      if (err.unanswered)
        return "The question could not be sent. Check your connection and try again.";
      return err.sentence || err.detail || "The engine refused the question.";
  }
}

/** The turn error codes a model failure ends a turn with. */
const MODEL_FAILURES: ReadonlySet<AssistantErrorCode> = new Set([
  "model_signed_out",
  "model_busy",
  "model_timeout",
  "model_unavailable",
]);

/**
 * A turn the model failed asks the viewer again, so the drawer's notice (a
 * model signed out, a plan spent) is drawn before the reader asks again.
 */
function modelFailed(frame: AssistantNarrowFrame): void {
  if (frame.kind === "error" && MODEL_FAILURES.has(frame.error.code)) rereadViewer();
}

/**
 * A write the engine says landed: the tab's read floor raised to it where it
 * names a domain a read waits on, and every live read asked again.
 */
function invalidated(frame: AssistantNarrowFrame): void {
  if (frame.kind !== "action" || frame.action.name !== ASSISTANT_ACTION_CONTEXT_INVALIDATE) return;
  const write = frame.action.invalidate;
  if (!write) return;
  const domain = Object.hasOwn(SESSION_QUERIES, write.domain)
    ? (write.domain as SessionDomain)
    : null;
  floors.written(domain, write.position === "" ? null : write.position, [EVERY_QUESTION]);
}

/** The turns this tab asked, so a page action answering one of them is this tab's to apply. */
const asked = new Set<string>();

/** Claim, apply and report a page action the stream carried, if this tab is the one to. */
function pageAction(frame: AssistantNarrowFrame): void {
  if (frame.kind !== "action" || frame.action.name === ASSISTANT_ACTION_CONTEXT_INVALIDATE) return;
  const action = frame.action;
  const mine =
    asked.has(frame.turn_id) ||
    (action.proposal_id !== undefined && approvals.has(action.proposal_id));
  const at = generation;
  void takeAction(action, mine, {
    settle: (local) => dispatch({ type: "action_local", actionId: action.action_id, local }),
  }).then((reported) => {
    // THE TURN RESUMES ONLY ONCE THE PAGE HAS REPORTED, and the stream that
    // carried the action may already have closed on the parked turn, so the
    // tab that reported it reads the turn again from where it was. Only that
    // tab, once: a replayed frame of an action already taken reads nothing.
    if (reported && at === generation) rejoin(frame.turn_id);
  });
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
  stream = read(streamUrl, lastEventId, true);
}

/**
 * Read an earlier turn's stream beside the conversation's own, until it ends.
 * Its frames reduce as any turn's do; the connection state the drawer shows
 * stays the running turn's.
 */
function followAside(streamUrl: string, lastEventId: string | undefined): void {
  const aside = read(streamUrl, lastEventId, false, () => asides.delete(aside));
  asides.add(aside);
}

/** One turn stream, its frames dispatched while this conversation is open. */
function read(
  streamUrl: string,
  lastEventId: string | undefined,
  own: boolean,
  ended?: () => void,
): TurnStream {
  const mine = generation;
  const live = (event: TurnEvent) => {
    if (mine === generation) dispatch(event);
  };
  return openTurnStream(
    streamUrl,
    {
      frame: (frame) => {
        if (mine !== generation) return;
        invalidated(frame);
        dispatch({ type: "frame", frame });
        pageAction(frame);
        modelFailed(frame);
      },
      state: (state, since) => {
        if (own) live({ type: "stream", state, since });
        if (state === "closed") ended?.();
      },
      stop: (reason) => {
        if (own) live(stopped(reason));
      },
    },
    lastEventId === undefined ? {} : { lastEventId },
  );
}

/**
 * The conversation being created, shared by everything that needs one at
 * once: an image attached and a question sent before the first answer came
 * back are one new conversation, never two.
 */
let creating: { generation: number; id: Promise<string> } | null = null;

/** Make the conversation a question will be asked in, if there is none yet. */
async function ensureConversation(): Promise<string> {
  if (view.id !== null) return view.id;
  if (creating !== null && creating.generation === generation) return creating.id;
  const mine = generation;
  const id = (async () => {
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
  })();
  creating = { generation: mine, id };
  try {
    return await id;
  } finally {
    if (creating?.id === id) creating = null;
  }
}

/** An image uploaded for the next question, and the conversation it was stored in. */
export interface UploadedAttachment {
  conversationId: string;
  file: AssistantFileRef;
}

/**
 * Upload an image the person attached, at once, to the conversation the next
 * question is asked in (made now if there is none yet). Sent under its own
 * operation key through the same request path as every other assistant
 * write, so a step-up, a retry and a refusal are handled as theirs are; a
 * refusal rejects with the engine's `RestError`.
 */
export async function uploadAttachment(
  file: File,
  signal?: AbortSignal,
): Promise<UploadedAttachment> {
  const conversationId = await ensureConversation();
  const stored = await call("POST /assistant/conversations/{id}/files", file, {
    id: conversationId,
    query: { name: file.name },
    opKey: newActOpID(),
    ...(signal ? { signal } : {}),
  });
  return { conversationId, file: stored.file };
}

/**
 * Ask `text`, with the page attached where the person left it attached.
 * Each send is a new question under its own operation key, minted here and
 * kept as the turn's key in the drawer: "Try again" after a failure asks
 * again rather than replaying the turn that failed, which the same key would
 * answer. A question whose answer was lost and is sent again cannot run
 * twice, since a conversation holds one running turn (409
 * `assistant_turn_running`).
 */
export async function send(
  text: string,
  page: PageSnapshot | null,
  attachments: readonly AssistantFileRef[] = [],
  mode: AssistantTurnMode = "ask",
): Promise<void> {
  const key = newActOpID();
  const mine = generation;
  dispatch({
    type: "send",
    key,
    text,
    page: page ? { title: page.title, screen: page.context.screen } : null,
    attachments: [...attachments],
    mode,
    at: new Date().toISOString(),
  });
  try {
    const id = await ensureConversation();
    const accepted = await call(
      "POST /assistant/conversations/{id}/turns",
      {
        ...versioned,
        input: { text },
        page_context: page ? page.context : null,
        ...(mode === "ask" ? {} : { mode }),
        ...(attachments.length > 0 ? { attachments: attachments.map((a) => a.file_id) } : {}),
      },
      { id, opKey: key },
    );
    if (mine !== generation) return;
    asked.add(accepted.turn_id);
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
        : {
            code: "",
            sentence: "The question could not be sent.",
            hint: null,
            retryAfterSeconds: null,
            remedy: null,
          };
    dispatch({ type: "refused", key, refusal });
    // A MODEL THE ENGINE KNOWS IS NOT READY: the drawer's notice is drawn now,
    // not at the next poll.
    if (refusal.code === "assistant_model_not_ready") rereadViewer();
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
  closeStreams();
  dispatch({ type: "reset" });
  writeTab({ conversation: null });
}

/** Open a conversation from the engine's transcript, and rejoin a turn still running in it. */
export async function openConversation(id: string): Promise<void> {
  generation += 1;
  const mine = generation;
  closeStreams();
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
  rejoin();
}

/** Read the open turn again from its last frame; only `turnId`'s, where one is named. */
function rejoin(turnId?: string): void {
  const turn = openTurn(view);
  if (!turn || turn.id === "" || view.id === null) return;
  if (turnId !== undefined && turn.id !== turnId) return;
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
 * The operation each decision is sent under, by proposal: minted at the first
 * press and sent again on a retry, so a retry after an answer nobody heard is
 * the same decision to the engine, which it collapses (ADR-0051). Approvals
 * and declines keep separate keys, because each is its own operation.
 */
const approvals = new Map<string, string>();
const declines = new Map<string, string>();

/** The operation `keys` holds for proposal `id`, minted on its first use. */
function decisionKey(keys: Map<string, string>, id: string): string {
  let key = keys.get(id);
  if (key === undefined) {
    key = newActOpID();
    keys.set(id, key);
  }
  return key;
}

/** A decision's answer: the proposal at its state now, and the resumed turn's stream. */
function decided(proposal: AssistantProposalCard, answer: AssistantProposalAnswer): void {
  dispatch({
    type: "approval_settled",
    proposalId: proposal.proposal_id,
    state: answer.proposal.state,
  });
  if (answer.stream_url) {
    const after = answer.after_seq ? assistantEventId(answer.turn_id, answer.after_seq) : undefined;
    // A CARD OF AN EARLIER TURN, superseded by the question now running:
    // its receipt is read beside that turn, never in place of it.
    const current = view.turns.at(-1);
    if (current && current.id !== "" && current.id !== answer.turn_id && isOpen(current.phase)) {
      followAside(answer.stream_url, after);
    } else {
      follow(answer.stream_url, after);
    }
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
    } else {
      dispatch({ type: "approval_failed", proposalId, ...decisionFailure(err) });
    }
    // THE ENGINE ANSWERED, AND A REFUSED DECISION RESUMES THE TURN with that
    // result (an expired or moved change is told to the model), but no stream
    // comes with a refusal: the turn is read again, or the drawer would show
    // it working until the page is reloaded.
    rejoin();
    return;
  }
  dispatch({ type: "approval_failed", proposalId, ...decisionFailure(err) });
}

/**
 * What a decision the engine refused, or nobody answered, says on its card
 * while the card still waits on one (ProposalCard's `decisionRefusal`): the
 * refusal's own sentence and the route's next step, never "Nothing changed.",
 * which is the card's to say, and only of a failed change.
 */
export function decisionFailure(err: unknown): { refusal: DecisionRefusal; unknown: boolean } {
  if (err instanceof RestError) {
    const failed = writeFailed(err);
    if (failed.kind !== "unknown") {
      return { refusal: { sentence: failed.sentence, hint: failed.hint || null }, unknown: false };
    }
  }
  return {
    refusal: {
      sentence: "Could not confirm the approval. It may have landed.",
      hint: "Approve again: it sends the same change, never a second one.",
    },
    unknown: true,
  };
}

/**
 * Approve a change the assistant prepared, exactly as the card showed it: the
 * digest it was drawn from travels with the approval, and the engine refuses
 * one that moved. A step-up the engine asks for is confirmed by the
 * dashboard's own dialog and the approval sent again (`protocol/rest.ts`).
 */
export async function approve(proposal: AssistantProposalCard): Promise<void> {
  const id = proposal.proposal_id;
  const opKey = decisionKey(approvals, id);
  dispatch({ type: "approve", proposalId: id });
  try {
    const answer = await call("POST /assistant/proposals/{id}/approve", decisionBody(proposal), {
      id,
      opKey,
    });
    decided(proposal, answer);
  } catch (err) {
    if (isAbort(err)) return;
    undecided(proposal, err);
  }
}

/**
 * A decision's body: the digest the card showed, and the screen the person
 * decided from as it is now, which the turn the decision resumes runs on (the
 * engine keeps a turn asked without a page without one).
 */
function decisionBody(proposal: AssistantProposalCard): AssistantProposalDecision {
  const screen = pageSnapshot();
  return {
    ...versioned,
    digest: proposal.digest,
    ...(screen === null ? {} : { page_context: screen.context }),
  };
}

/** Decline a change; nothing is changed, and the turn hears that it was declined. */
export async function decline(proposal: AssistantProposalCard): Promise<void> {
  const id = proposal.proposal_id;
  const opKey = decisionKey(declines, id);
  try {
    const answer = await call("POST /assistant/proposals/{id}/decline", decisionBody(proposal), {
      id,
      opKey,
    });
    decided(proposal, answer);
  } catch (err) {
    if (isAbort(err)) return;
    undecided(proposal, err);
  }
}

/**
 * Undo a page change from its receipt, through the screen that applied it.
 * The receipt then says it was undone, or, where the page has moved on since
 * (or the screen has gone), that it can no longer be undone from here.
 */
export function undoAction(actionId: string): void {
  const answer = undoPageAction(actionId);
  dispatch({
    type: "action_local",
    actionId,
    local:
      answer === "undone"
        ? { state: "undone", words: "Undone. The page is back as it was.", undoable: false }
        : {
            state: "applied",
            words:
              "The page has changed since, so this can no longer be undone from here. Use the page's own controls.",
            undoable: false,
          },
  });
}

/** Tell the conversation the page under the drawer moved, so a prepared page change is stale. */
export function pageMoved(revision: string, revisions: Readonly<Record<string, string>>): void {
  dispatch({ type: "page_moved", revision, revisions });
}

/** Test seam: a fresh store, as a fresh page has. */
export function resetSessionForTest(): void {
  generation += 1;
  creating = null;
  asked.clear();
  approvals.clear();
  declines.clear();
  closeStreams();
  view = EMPTY;
  setAssistantActivity("idle");
}
