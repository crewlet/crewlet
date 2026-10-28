/**
 * A conversation as the drawer draws it, and the one function that moves it.
 *
 * A PURE REDUCER: every way a turn moves (the person sends, the engine
 * accepts, a frame arrives, the stream drops, the person stops or approves)
 * is an event, and [reduce] is the only thing that turns events into state.
 * So the state machine is a table a suite reads row by row (`turn.test.ts`),
 * and the drawer is a function of the state rather than of the order its
 * effects happened to run in.
 *
 * # The state machine for one turn
 *
 *     sending            --accepted-->            thinking
 *     sending            --refused-->             refused
 *     thinking, working  --step-->                working
 *     thinking, working  --proposal card-->       awaiting_approval
 *     thinking, working  --action, confirm none-> applying
 *     awaiting_approval  --approve-->             applying
 *     applying           --page applied it-->     working, its card applied
 *     awaiting_approval  --page moved-->          awaiting_approval, the card marked stale
 *     applying           --settled-->             working
 *     applying           --unknown-->             unconfirmed
 *     any open           --done-->                the done's state
 *     any open           --stopped-->             stopped
 *
 * A FRAME AT OR BELOW THE SEQ A TURN HAS IS DROPPED, so a resumed stream that
 * replays what the drawer already drew never draws a delta twice, and a
 * `snapshot` (what a resume answers when the frame log cannot fill the gap)
 * replaces the turn's steps, replies and cards whole.
 *
 * NOTHING HERE IS A MODEL'S WORDS DECIDING A STATE. A phase moves on the
 * engine's own frames and the person's own gestures; reply text is carried
 * as text and drawn as prose, never read.
 */

import type {
  AssistantActionEvent,
  AssistantCardEvent,
  AssistantFileRef,
  AssistantTurnMode,
  AssistantDoneEvent,
  AssistantErrorEvent,
  AssistantPageContextEvent,
  AssistantProposalCard,
  AssistantStatusEvent,
  AssistantStepEvent,
  AssistantTranscript,
} from "~/contract/assistant.ts";
import { ASSISTANT_ACTION_CONTEXT_INVALIDATE } from "~/contract/assistant.ts";
import type { AssistantNarrowFrame } from "~/protocol/assistantWire.ts";
import type { StreamState } from "~/protocol/assistant.ts";
import type { ActionLocal } from "./pageActions.ts";
import type { Remedy } from "~/protocol/remedy.ts";

/** The page a message was sent with, as its chip shows it. */
export type PageChip = NonNullable<AssistantPageContextEvent["page"]>;

/** A wire turn state, the closed set a `done` frame ends a segment in. */
type WireTurnState = AssistantDoneEvent["state"];

/** Where one turn stands, as the drawer draws it. */
export type TurnPhase =
  /** The question is on its way to the engine. */
  | "sending"
  /** Accepted, and no tool has been called yet. */
  | "thinking"
  /** A tool is running, or the answer is streaming. */
  | "working"
  /** A proposed change waits on the person. */
  | "awaiting_approval"
  /** An approved change is being applied. */
  | "applying"
  /** An approved change was sent and nobody can say whether it landed. */
  | "unconfirmed"
  /** The engine refused the question before any turn began. */
  | "refused"
  /** Ended: the turn's own final states. */
  | "completed"
  | "stopped"
  | "failed"
  | "interrupted"
  | "expired"
  | "budget_exhausted";

/** The phases a turn never leaves except by a new turn. */
export const ENDED_PHASES: ReadonlySet<TurnPhase> = new Set([
  "refused",
  "completed",
  "stopped",
  "failed",
  "interrupted",
  "expired",
  "budget_exhausted",
]);

/** Whether a turn is still moving: a question in flight, a run, or a change waiting. */
export function isOpen(phase: TurnPhase): boolean {
  return !ENDED_PHASES.has(phase);
}

/** A question the engine refused before it became a turn. */
export interface SendRefusal {
  /** The engine's code: `assistant_busy`, `assistant_turn_running`, `assistant_protocol_mismatch`. */
  code: string;
  /** The sentence a person is shown. */
  sentence: string;
  /** What to do, where the engine said: a model that is not ready names its fix. */
  hint: string | null;
  /** Seconds the engine said to wait, where it said. */
  retryAfterSeconds: number | null;
  /** Where the limit that refused it is raised, for a refusal a limit caused (a spent budget). */
  remedy: Remedy | null;
}

/** A proposal's state as the drawer holds it, with what the person's gesture added. */
export interface ProposalLocal {
  /** The page under the drawer moved since the change was prepared. */
  pageMoved: boolean;
  /** Why a page change failed on this screen, in the screen's words. */
  failure: string | null;
  /**
   * Why the person's last decision was refused, or went unanswered, while
   * the card still waits on one; cleared by the next press.
   */
  refusal: DecisionRefusal | null;
}

/** A decision the engine refused, or nobody answered, in the drawer's words. */
export interface DecisionRefusal {
  sentence: string;
  hint: string | null;
}

/** One turn, and everything the drawer draws for it. */
export interface TurnView {
  /** The drawer's own key for it, fixed from the moment it is sent. */
  key: string;
  /** The engine's turn id; empty while the question is on its way. */
  id: string;
  phase: TurnPhase;
  /** The last frame applied. */
  seq: number;
  /** The person's question, as typed. */
  question: string;
  /** The page it was sent with, or null where the person removed it. */
  page: PageChip | null;
  /** The images the person attached to the question. */
  attachments: AssistantFileRef[];
  /**
   * What the question asked for, where this tab sent it: a brief's Try again
   * asks for a brief again. Absent is an ordinary question, and a turn read
   * back from the transcript, which does not say.
   */
  mode?: AssistantTurnMode;
  /** When the person asked, RFC 3339. */
  askedAt: string;
  /**
   * When the turn last answered, RFC 3339: its done frame, or, in a reloaded
   * transcript, its last reply. Null until it has. What the turn's foot says,
   * since a turn that waited minutes on an approval was answered long after
   * it was asked.
   */
  answeredAt: string | null;
  /** The engine's step count while a segment runs. */
  stepsDone: number;
  steps: AssistantStepEvent[];
  /** The assistant's messages in this turn, in order. */
  replies: { id: string; text: string }[];
  cards: AssistantCardEvent[];
  /** What the person's own gestures added to each proposal, by its id. */
  proposals: Record<string, ProposalLocal>;
  /** The page actions the engine sent, for the page to claim and apply. */
  actions: AssistantActionEvent[];
  /** What became of each page action in this tab, by its id. */
  actionStates: Record<string, ActionLocal>;
  done: AssistantDoneEvent | null;
  error: AssistantErrorEvent | null;
  refusal: SendRefusal | null;
  /** A stop is on its way. */
  stopping: boolean;
}

/** Why the conversation's stream stopped for good, as the drawer says it. */
export type ConversationFault = "protocol_mismatch" | "malformed" | "refused" | null;

/** One administrator's compliance read of the conversation (D06). */
export type AuditRead = AssistantTranscript["audit_reads"][number];

/** A conversation as the drawer draws it. */
export interface ConversationView {
  /** The engine's conversation id, or null for a conversation not yet created. */
  id: string | null;
  title: string;
  /** Who read this conversation for compliance, and why, as the engine records it (D06). */
  auditReads: readonly AuditRead[];
  turns: TurnView[];
  stream: StreamState;
  /** When the current outage began (ms), while reconnecting. */
  reconnectingSince: number | null;
  fault: ConversationFault;
}

/** A conversation nobody has started: the drawer's home. */
export const EMPTY: ConversationView = {
  id: null,
  title: "",
  auditReads: [],
  turns: [],
  stream: "closed",
  reconnectingSince: null,
  fault: null,
};

/** Every way a conversation moves. */
export type TurnEvent =
  | {
      type: "send";
      key: string;
      text: string;
      page: PageChip | null;
      attachments: AssistantFileRef[];
      mode: AssistantTurnMode;
      at: string;
    }
  | { type: "created"; id: string; title: string }
  | { type: "accepted"; key: string; turnId: string; afterSeq: number }
  | { type: "refused"; key: string; refusal: SendRefusal }
  | { type: "frame"; frame: AssistantNarrowFrame }
  | { type: "stream"; state: StreamState; since: number | null }
  | { type: "fault"; fault: Exclude<ConversationFault, null> }
  | { type: "stopping"; turnId: string }
  | { type: "stop_failed"; turnId: string }
  | { type: "stopped"; turnId: string; state: WireTurnState }
  | { type: "approve"; proposalId: string }
  | { type: "approval_settled"; proposalId: string; state: AssistantProposalCard["state"] }
  | {
      type: "approval_failed";
      proposalId: string;
      refusal: DecisionRefusal;
      unknown: boolean;
    }
  | { type: "page_moved"; revision: string; revisions: Readonly<Record<string, string>> }
  | { type: "action_local"; actionId: string; local: ActionLocal }
  | { type: "transcript"; transcript: AssistantTranscript }
  | { type: "reset" };

/** The phase a turn's wire state names. */
export function phaseOf(state: WireTurnState): TurnPhase {
  switch (state) {
    case "running":
      return "working";
    case "waiting_for_approval":
      return "awaiting_approval";
    case "superseded":
      // THE PERSON ASKED THE NEXT QUESTION while this turn waited on a card:
      // its loop is over, and its cards keep their own states (a proposal
      // stays approvable until it expires).
      return "completed";
    default:
      return state;
  }
}

/** The phase a status frame names. */
function statusPhase(state: AssistantStatusEvent["state"]): TurnPhase {
  return state === "waiting_for_approval" ? "awaiting_approval" : state;
}

/** Replace the turn `match` picks with what `change` makes of it. */
function withTurn(
  view: ConversationView,
  match: (turn: TurnView) => boolean,
  change: (turn: TurnView) => TurnView,
): ConversationView {
  let changed = false;
  const turns = view.turns.map((turn) => {
    if (changed || !match(turn)) return turn;
    changed = true;
    return change(turn);
  });
  return changed ? { ...view, turns } : view;
}

/** A proposal card's body, where a card event is one. */
export function proposalOf(card: AssistantCardEvent): AssistantProposalCard | null {
  return card.card.type === "proposal" ? (card.card.proposal ?? null) : null;
}

/** Whether any proposal in the turn still waits on the person. */
function waitsOnPerson(turn: TurnView): boolean {
  return turn.cards.some((card) => proposalOf(card)?.state === "proposed");
}

/** Insert or replace by key, keeping arrival order for a new one. */
function upsert<T>(list: readonly T[], item: T, same: (a: T) => boolean): T[] {
  const at = list.findIndex(same);
  if (at < 0) return [...list, item];
  const next = [...list];
  next[at] = item;
  return next;
}

/** One frame applied to its turn; see the file's doc for what each kind does. */
function applyFrame(turn: TurnView, frame: AssistantNarrowFrame): TurnView {
  if (frame.seq <= turn.seq) return turn;
  const next: TurnView = { ...turn, seq: frame.seq };
  const open = isOpen(turn.phase);
  switch (frame.kind) {
    case "conversation":
      return next;
    case "page_context": {
      const used = frame.page_context;
      return { ...next, page: used.used ? (used.page ?? turn.page) : null };
    }
    case "status": {
      const phase = open ? statusPhase(frame.status.state) : turn.phase;
      return { ...next, phase, stepsDone: frame.status.steps_done };
    }
    case "delta": {
      const { message_id, text, restart } = frame.delta;
      const at = next.replies.findIndex((r) => r.id === message_id);
      const replies = [...next.replies];
      if (at < 0) replies.push({ id: message_id, text });
      else replies[at] = { id: message_id, text: restart ? text : replies[at]!.text + text };
      return {
        ...next,
        replies,
        phase: open && turn.phase === "thinking" ? "working" : next.phase,
      };
    }
    case "step": {
      const steps = upsert(next.steps, frame.step, (s) => s.step_id === frame.step.step_id).sort(
        (a, b) => a.index - b.index,
      );
      return {
        ...next,
        steps,
        phase: open && turn.phase === "thinking" ? "working" : next.phase,
      };
    }
    case "card": {
      const cards = upsert(next.cards, frame.card, (c) => c.card_id === frame.card.card_id);
      const proposal = proposalOf(frame.card);
      const waiting =
        open && proposal?.state === "proposed" && turn.phase !== "applying"
          ? "awaiting_approval"
          : next.phase;
      return { ...next, cards, phase: waiting };
    }
    case "action": {
      const actions = [...next.actions, frame.action];
      const applies =
        open &&
        frame.action.name !== ASSISTANT_ACTION_CONTEXT_INVALIDATE &&
        frame.action.confirm === "none";
      return { ...next, actions, phase: applies ? "applying" : next.phase };
    }
    case "done":
      return {
        ...next,
        done: frame.done,
        phase: phaseOf(frame.done.state),
        stopping: false,
        answeredAt: frame.at,
      };
    case "error":
      return {
        ...next,
        error: frame.error,
        phase: open ? "failed" : next.phase,
        answeredAt: open ? frame.at : next.answeredAt,
      };
    case "snapshot": {
      const { messages, steps, cards } = frame.snapshot;
      const asked = messages.find((m) => m.role === "user");
      return {
        ...next,
        attachments: asked?.attachments ?? next.attachments,
        steps: [...steps].sort((a, b) => a.index - b.index),
        cards: [...cards],
        replies: messages
          .filter((m) => m.role === "assistant")
          .map((m) => ({ id: m.id, text: m.text })),
      };
    }
  }
}

/** A turn as a reloaded transcript describes it. */
function fromTranscript(transcript: AssistantTranscript): TurnView[] {
  const order: string[] = [];
  const turns = new Map<string, TurnView>();
  const replyTurn = new Map<string, string>();
  const open = transcript.open_turn;
  // HOW EACH TURN ENDED, as the engine recorded it: a turn stopped or failed
  // before it wrote a word is drawn with its outcome line, as it was live,
  // rather than read as completed and drawn as nothing at all.
  const outcomes = new Map(transcript.turns.map((t) => [t.turn_id, t]));
  for (const message of transcript.messages) {
    let turn = turns.get(message.turn_id);
    if (!turn) {
      order.push(message.turn_id);
      const ended = outcomes.get(message.turn_id);
      turn = {
        key: message.turn_id,
        id: message.turn_id,
        phase:
          open?.turn_id === message.turn_id
            ? phaseOf(open.state)
            : ended
              ? phaseOf(ended.state)
              : "completed",
        seq: open?.turn_id === message.turn_id ? open.after_seq : 0,
        question: "",
        page: null,
        attachments: [],
        askedAt: message.created_at,
        answeredAt: null,
        stepsDone: 0,
        steps: [],
        replies: [],
        cards: [],
        proposals: {},
        actions: [],
        actionStates: {},
        done: null,
        error: open?.turn_id === message.turn_id ? null : (ended?.error ?? null),
        refusal: null,
        stopping: false,
      };
      turns.set(message.turn_id, turn);
    }
    if (message.role === "user") {
      turns.set(message.turn_id, {
        ...turn,
        question: message.text,
        page: message.page ?? null,
        attachments: message.attachments ?? [],
        askedAt: message.created_at,
      });
    } else {
      replyTurn.set(message.id, message.turn_id);
      turns.set(message.turn_id, {
        ...turn,
        replies: [...turn.replies, { id: message.id, text: message.text }],
        answeredAt: message.created_at,
      });
    }
  }
  for (const card of transcript.cards) {
    const id = replyTurn.get(card.message_id);
    const turn = id === undefined ? undefined : turns.get(id);
    if (turn) turns.set(turn.id, { ...turn, cards: [...turn.cards, card] });
  }
  // A STEP NAMES ITS TURN where the engine says so; one that does not is not
  // placed under a turn it may not belong to.
  for (const step of transcript.steps) {
    const id = step.turn_id;
    const turn = id === undefined ? undefined : turns.get(id);
    if (turn) {
      turns.set(turn.id, {
        ...turn,
        steps: [...turn.steps, step].sort((a, b) => a.index - b.index),
      });
    }
  }
  return order.map((id) => turns.get(id)!);
}

/** The conversation after `event`. */
export function reduce(view: ConversationView, event: TurnEvent): ConversationView {
  switch (event.type) {
    case "reset":
      return EMPTY;
    case "created":
      return { ...view, id: event.id, title: event.title };
    case "send":
      return {
        ...view,
        fault: null,
        turns: [
          ...view.turns,
          {
            key: event.key,
            id: "",
            phase: "sending",
            seq: 0,
            question: event.text,
            page: event.page,
            attachments: event.attachments,
            ...(event.mode === "ask" ? {} : { mode: event.mode }),
            askedAt: event.at,
            answeredAt: null,
            stepsDone: 0,
            steps: [],
            replies: [],
            cards: [],
            proposals: {},
            actions: [],
            actionStates: {},
            done: null,
            error: null,
            refusal: null,
            stopping: false,
          },
        ],
      };
    case "accepted": {
      const accepted = withTurn(
        view,
        (t) => t.key === event.key && t.phase === "sending",
        (t) => ({ ...t, id: event.turnId, seq: event.afterSeq, phase: "thinking" }),
      );
      // THE NEXT QUESTION SUPERSEDES A TURN WAITING ON A CARD: its loop is
      // over (the engine records it `superseded`, which this tab hears only
      // if it follows that turn again), and its cards keep their own states,
      // so a proposal in it stays approvable until it expires.
      if (accepted === view) return view;
      const turns = accepted.turns.map((t) =>
        t.key !== event.key && t.phase === "awaiting_approval"
          ? { ...t, phase: "completed" as const }
          : t,
      );
      return { ...accepted, turns };
    }
    case "refused":
      return withTurn(
        view,
        (t) => t.key === event.key && t.phase === "sending",
        (t) => ({ ...t, phase: "refused", refusal: event.refusal }),
      );
    case "frame": {
      const { frame } = event;
      const withTitle =
        frame.kind === "conversation"
          ? { ...view, id: view.id ?? frame.conversation_id, title: frame.conversation.title }
          : view;
      return withTurn(
        withTitle,
        (t) => t.id === frame.turn_id,
        (t) => applyFrame(t, frame),
      );
    }
    case "stream":
      return {
        ...view,
        stream: event.state,
        reconnectingSince: event.state === "reconnecting" ? event.since : null,
      };
    case "fault":
      return { ...view, fault: event.fault };
    case "stopping":
      return withTurn(
        view,
        (t) => t.id === event.turnId && isOpen(t.phase),
        (t) => ({ ...t, stopping: true }),
      );
    case "stop_failed":
      return withTurn(
        view,
        (t) => t.id === event.turnId,
        (t) => ({ ...t, stopping: false }),
      );
    case "stopped":
      return withTurn(
        view,
        (t) => t.id === event.turnId && isOpen(t.phase),
        (t) => ({ ...t, phase: phaseOf(event.state), stopping: false }),
      );
    case "approve":
      return withProposal(view, event.proposalId, (turn) => {
        // THE NEXT PRESS CLEARS THE LAST REFUSAL: it is about the decision
        // before this one.
        const local = turn.proposals[event.proposalId];
        const proposals = local
          ? { ...turn.proposals, [event.proposalId]: { ...local, refusal: null } }
          : turn.proposals;
        return turn.phase === "awaiting_approval"
          ? { ...turn, proposals, phase: "applying" }
          : { ...turn, proposals };
      });
    case "approval_settled":
      return withProposal(view, event.proposalId, (turn) => {
        const cards = turn.cards.map((card) => {
          const proposal = proposalOf(card);
          if (proposal?.proposal_id !== event.proposalId) return card;
          return { ...card, card: { ...card.card, proposal: { ...proposal, state: event.state } } };
        });
        const settled = { ...turn, cards };
        if (!isOpen(turn.phase)) return settled;
        return { ...settled, phase: waitsOnPerson(settled) ? "awaiting_approval" : "working" };
      });
    case "approval_failed":
      return withProposal(view, event.proposalId, (turn) => ({
        ...turn,
        proposals: {
          ...turn.proposals,
          [event.proposalId]: {
            pageMoved: turn.proposals[event.proposalId]?.pageMoved ?? false,
            failure: turn.proposals[event.proposalId]?.failure ?? null,
            refusal: event.refusal,
          },
        },
        phase: !isOpen(turn.phase)
          ? turn.phase
          : event.unknown
            ? "unconfirmed"
            : waitsOnPerson(turn)
              ? "awaiting_approval"
              : "working",
      }));
    case "action_local":
      return withTurn(
        view,
        (t) => t.actions.some((a) => a.action_id === event.actionId),
        (turn) => actionSettled(turn, event.actionId, event.local),
      );
    case "page_moved":
      return {
        ...view,
        turns: view.turns.map((turn) => {
          if (turn.phase !== "awaiting_approval") return turn;
          const proposals = { ...turn.proposals };
          let moved = false;
          for (const card of turn.cards) {
            const proposal = proposalOf(card);
            // EACH AGAINST ITS ACTION'S OWN REVISION, where the page states
            // one: a change goes stale only when what it depends on moved.
            const now =
              (proposal?.page_action && event.revisions[proposal.page_action.name]) ||
              event.revision;
            if (
              proposal?.scope === "page" &&
              proposal.state === "proposed" &&
              proposal.source_revision !== undefined &&
              proposal.source_revision !== now
            ) {
              proposals[proposal.proposal_id] = {
                failure: proposals[proposal.proposal_id]?.failure ?? null,
                refusal: proposals[proposal.proposal_id]?.refusal ?? null,
                pageMoved: true,
              };
              moved = true;
            }
          }
          return moved ? { ...turn, proposals } : turn;
        }),
      };
    case "transcript":
      return {
        id: event.transcript.conversation.id,
        title: event.transcript.conversation.title,
        auditReads: event.transcript.audit_reads,
        turns: fromTranscript(event.transcript),
        stream: "closed",
        reconnectingSince: null,
        fault: null,
      };
  }
}

/** A proposal's state once its page action settled in this tab, or null where the card is the engine's to move. */
const CARD_AFTER: Record<ActionLocal["state"], AssistantProposalCard["state"] | null> = {
  applied: "applied",
  undone: "applied",
  stale: "stale",
  blocked: "failed",
  invalid: "failed",
  no_handler: "failed",
  unconfirmed: "failed",
  // ANOTHER TAB APPLIES IT: the engine's own card frames say what came of it.
  elsewhere: null,
};

/**
 * A turn once one of its page actions settled in this tab: the outcome kept
 * for its receipt, the proposal it applies drawn at its outcome (applied with
 * the receipt, or refused with the screen's sentence), and the turn back to
 * work once nothing is being applied.
 */
function actionSettled(turn: TurnView, actionId: string, local: ActionLocal): TurnView {
  const action = turn.actions.find((a) => a.action_id === actionId);
  const after = CARD_AFTER[local.state];
  const proposalId = action?.proposal_id;
  let next: TurnView = { ...turn, actionStates: { ...turn.actionStates, [actionId]: local } };
  if (proposalId !== undefined && after !== null) {
    const failed = after === "failed";
    next = {
      ...next,
      cards: next.cards.map((card) => {
        const proposal = proposalOf(card);
        if (proposal?.proposal_id !== proposalId) return card;
        const receipt = after === "applied" ? { summary: local.words } : proposal.receipt;
        return {
          ...card,
          card: {
            ...card.card,
            proposal: { ...proposal, state: after, ...(receipt ? { receipt } : {}) },
          },
        };
      }),
      proposals: failed
        ? {
            ...next.proposals,
            [proposalId]: {
              pageMoved: next.proposals[proposalId]?.pageMoved ?? false,
              failure: local.words,
              refusal: null,
            },
          }
        : next.proposals,
    };
  }
  if (next.phase !== "applying" || local.state === "elsewhere") return next;
  return { ...next, phase: waitsOnPerson(next) ? "awaiting_approval" : "working" };
}

/** Change the turn holding a proposal. */
function withProposal(
  view: ConversationView,
  proposalId: string,
  change: (turn: TurnView) => TurnView,
): ConversationView {
  return withTurn(
    view,
    (t) => t.cards.some((card) => proposalOf(card)?.proposal_id === proposalId),
    change,
  );
}

/** The turn that is still moving, if any: the last one, while it is open. */
export function openTurn(view: ConversationView): TurnView | null {
  const last = view.turns.at(-1);
  return last && isOpen(last.phase) ? last : null;
}

/** Where the conversation stands, as the Ask button says it while the drawer is closed. */
export function activityOf(view: ConversationView): "idle" | "working" | "waiting" {
  const turn = openTurn(view);
  if (!turn) return "idle";
  return turn.phase === "awaiting_approval" ? "waiting" : "working";
}
