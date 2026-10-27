/**
 * A conversation's turns, drawn from the reducer's state and nothing else.
 *
 * ONE TURN IS: the person's question in a bubble, with the page and the
 * images it carried under it; the run chip, once the turn has taken a step, which says the
 * turn's state in words and opens on the steps it took (each with the
 * ordinary request it made, in mono); each deep run the assistant switched
 * to, as its own card with the steps it made nested under it; the answer as prose with no container
 * round it; the engine's cards; while the assistant works, the thinking
 * indicator after all of it; and, once the turn has ended, a foot with when
 * it was and Copy. No answer rating: in a self-hosted engine nothing would
 * read one.
 *
 * THE INDICATOR IS THE LIVE LINE, AS IN CLAUDE: the Assistant's mark turning,
 * with "Thinking" or the running step's sentence under a shimmer, and the
 * mark alone while the answer's words arrive above it. It replaces both the
 * skeleton that stood in for the answer (three lines that jumped to one when
 * the first words came) and the caret, which stood on a line of its own. So
 * the run chip says nothing the indicator already says: it is drawn from the
 * first step, and stays closed while the turn runs, its steps one press away.
 *
 * THE THREAD IS A LOG WITH ITS LIVE REGION OFF: a streaming answer is not
 * read out token by token. What is announced, once, through the one polite
 * announcer, is each transition a reader acts on: an answer ready, a change
 * waiting on them, a turn stopped.
 */

import { useEffect, useMemo, useRef } from "react";
import {
  Callout,
  CopyButton,
  MessageBubble,
  RelativeTime,
  RunChip,
  StepList,
  Tag,
  ThinkingIndicator,
  VisuallyHidden,
  Button,
  Inline,
  Stack,
  Text,
  announce,
} from "@crewlethq/ui";
import { LayoutDashboardGlyph } from "@crewlethq/icons/glyphs";
import type {
  AssistantCardEvent,
  AssistantFileRef,
  AssistantTurnMode,
} from "~/contract/assistant.ts";
import { fmtDateTime, fmtWait } from "~/lib/format.ts";
import {
  ActionReceipt,
  CardBlock,
  DeepRunBlock,
  ReplyProse,
  deepRunsOf,
  runChipOf,
  stepsOf,
  thinkingOf,
  type CardHandlers,
} from "./blocks.tsx";
import { SentAttachments } from "./files.tsx";
import { Answer } from "./reads.tsx";
import { changedNothing } from "./pageActions.ts";
import { isOpen, proposalOf, type ConversationView, type TurnView } from "./turn.ts";

/** How long a reconnect may run before the drawer offers to try again by hand. */
export const RECONNECT_PATIENCE_MS = 30_000;

export interface ThreadProps {
  view: ConversationView;
  now: number;
  on: CardHandlers;
  /** The cards whose place the reader opened beside the conversation. */
  opened: ReadonlySet<string>;
  /** Send a question again, with the images it carried, for a turn that failed on something that may pass. */
  onRetry: (
    text: string,
    attachments: readonly AssistantFileRef[],
    mode: AssistantTurnMode,
  ) => void;
  /** Why a question cannot be sent now (Send's own reason), which holds Try again off too. */
  retryDisabledReason?: string | undefined;
  /** Dial the running turn's stream again. */
  onReconnect: () => void;
  /**
   * Whether the reader administers the company, and so is shown each step's
   * request beside its sentence (`stepDetail`). Optional only for a suite
   * that draws a thread by itself; the drawer always says.
   */
  technical?: boolean;
}

/** The page a question carried, as the read-only chip under its bubble. */
function SentWith({ title }: { title: string }) {
  return (
    <Tag size="xs" appearance="outline" leadingIcon={<LayoutDashboardGlyph size="xs" />}>
      <VisuallyHidden>Sent with </VisuallyHidden>
      {title}
    </Tag>
  );
}

/** The proposal states whose write is on its way, or landed without anyone hearing. */
const MAY_HAVE_LANDED: ReadonlySet<string> = new Set([
  "approved",
  "executing",
  "applying",
  "unknown",
]);

/**
 * What a turn that stopped or failed left changed, in the words its end is
 * said with. A STOP OR A FAILURE ENDS WHAT IS LEFT, NOT WHAT WAS DONE: a
 * change approved earlier in the same turn stays applied, and one whose
 * outcome nobody heard may have landed, so "Nothing changed." is said only
 * where neither is true.
 */
export function consequenceOf(turn: TurnView): string {
  let uncertain = false;
  for (const event of turn.cards) {
    const state = proposalOf(event)?.state;
    if (state === "applied") return "What was already applied stays applied.";
    if (state !== undefined && MAY_HAVE_LANDED.has(state)) uncertain = true;
  }
  for (const local of Object.values(turn.actionStates)) {
    if (!changedNothing(local.state)) return "What was already applied stays applied.";
  }
  return uncertain
    ? "A change may have landed. Its card says where it stands."
    : "Nothing changed.";
}

/** What an ended turn's foot copies: the answer as Markdown. */
function answerText(turn: TurnView): string {
  return turn.replies.map((r) => r.text).join("\n\n");
}

/** The cards no answer of the turn holds: none. */
const NO_CARDS: readonly AssistantCardEvent[] = [];

/**
 * A turn's cards by the message each belongs to, and those whose message the
 * turn holds no answer for, each in the order the turn sent them.
 */
function cardsByMessage(
  cards: readonly AssistantCardEvent[],
  replies: ReadonlySet<string>,
): { cardsOf: ReadonlyMap<string, readonly AssistantCardEvent[]>; loose: AssistantCardEvent[] } {
  const cardsOf = new Map<string, AssistantCardEvent[]>();
  const loose: AssistantCardEvent[] = [];
  for (const card of cards) {
    if (!replies.has(card.message_id)) {
      loose.push(card);
      continue;
    }
    const list = cardsOf.get(card.message_id);
    if (list) list.push(card);
    else cardsOf.set(card.message_id, [card]);
  }
  return { cardsOf, loose };
}

/** One turn. */
function Turn({
  turn,
  now,
  on,
  opened,
  onRetry,
  retryDisabledReason,
  technical,
}: {
  turn: TurnView;
  now: number;
  on: CardHandlers;
  opened: ReadonlySet<string>;
  onRetry: (
    text: string,
    attachments: readonly AssistantFileRef[],
    mode: AssistantTurnMode,
  ) => void;
  retryDisabledReason: string | undefined;
  technical: boolean;
}) {
  const open = isOpen(turn.phase);
  const chip = runChipOf(turn, now);
  const steps = stepsOf(turn, technical);
  const deepRuns = deepRunsOf(turn);
  const thinking = thinkingOf(turn);
  // EACH ANSWER'S CARDS ARE ONE ARRAY while the turn's cards are, so an
  // answer works out its reads and its mentions once per change of its cards,
  // never on every render (a running turn's clock renders it every second).
  const replyIds = turn.replies.map((r) => r.id).join("\n");
  const { cardsOf, loose } = useMemo(
    () => cardsByMessage(turn.cards, new Set(replyIds.split("\n"))),
    [turn.cards, replyIds],
  );
  // A PAGE CHANGE WITH NO CARD of its own (a pure view change, D11) is drawn
  // as its receipt; one a card proposed is drawn on that card.
  const carded = new Set(
    turn.cards.flatMap((c) =>
      c.card.type === "proposal" && c.card.proposal ? [c.card.proposal.proposal_id] : [],
    ),
  );
  const receipts = turn.actions.filter(
    (a) =>
      turn.actionStates[a.action_id] !== undefined &&
      (a.proposal_id === undefined || !carded.has(a.proposal_id)),
  );
  const card = (event: (typeof turn.cards)[number]) => {
    const proposal = event.card.type === "proposal" ? event.card.proposal : undefined;
    return (
      <CardBlock
        key={event.card_id}
        event={event}
        local={proposal ? turn.proposals[proposal.proposal_id] : undefined}
        on={on}
        opened={opened.has(event.card_id)}
      />
    );
  };
  return (
    <>
      <MessageBubble
        from="self"
        attachment={
          turn.page || turn.attachments.length > 0 ? (
            <Inline gap={1} wrap>
              {turn.page ? <SentWith title={turn.page.title} /> : null}
              <SentAttachments files={turn.attachments} />
            </Inline>
          ) : undefined
        }
      >
        {turn.question}
      </MessageBubble>
      {turn.refusal ? (
        <Callout
          variant={turn.refusal.code === "assistant_busy" ? "warning" : "neutral"}
          role="status"
          title={turn.refusal.sentence}
        >
          {turn.refusal.hint ??
            (turn.refusal.retryAfterSeconds !== null
              ? `You can ask again ${fmtWait(turn.refusal.retryAfterSeconds)}, and your question is back in the box below.`
              : "Nothing changed.")}
        </Callout>
      ) : turn.steps.length > 0 ? (
        <RunChip state={chip.state} title={chip.title} meta={chip.meta} defaultOpen={false}>
          {steps.length > 0 ? <StepList label="Steps" steps={steps} /> : undefined}
        </RunChip>
      ) : null}
      {deepRuns.map((run) => (
        <DeepRunBlock key={run.step.step_id} run={run} now={now} technical={technical} />
      ))}
      {turn.replies.map((reply) => (
        <Answer
          key={reply.id}
          text={reply.text}
          dimmed={turn.phase === "stopped"}
          cards={cardsOf.get(reply.id) ?? NO_CARDS}
          card={card}
          on={on}
        />
      ))}
      {loose.length > 0 ? (
        <Answer text="" dimmed={false} cards={loose} card={card} on={on} />
      ) : null}
      {receipts.map((a) => (
        <ActionReceipt
          key={a.action_id}
          actionId={a.action_id}
          local={turn.actionStates[a.action_id]!}
          on={on}
        />
      ))}
      {/* ONE ELEMENT FOR THE WHOLE RUN, in a slot of its own, so its status
          region is announced once and only its words change. */}
      {thinking ? <ThinkingIndicator status={thinking.status} /> : null}
      {turn.phase === "stopped" ? (
        <ReplyProse text={`Stopped. ${consequenceOf(turn)}`} dimmed />
      ) : null}
      {turn.error ? (
        <Callout
          variant="danger"
          role="alert"
          title={turn.error.message}
          action={
            turn.error.retryable ? (
              <Button
                size="small"
                variant="secondary"
                disabledReason={retryDisabledReason}
                onClick={() => onRetry(turn.question, turn.attachments, turn.mode ?? "ask")}
              >
                Try again
              </Button>
            ) : undefined
          }
        >
          {turn.error.hint ? `${turn.error.hint} ` : ""}
          {consequenceOf(turn)}
        </Callout>
      ) : null}
      {!open && !turn.refusal ? (
        <Inline gap={2} align="center">
          {/* THE FOOT IS METADATA, set at the caption step beside Copy. */}
          <Text as="span" variant="caption" tone="secondary">
            <RelativeTime value={turn.answeredAt ?? turn.askedAt} now={now} />
          </Text>
          {turn.replies.length > 0 ? (
            <CopyButton size="small" variant="ghost" text={() => answerText(turn)} />
          ) : null}
        </Inline>
      ) : null}
    </>
  );
}

/** What a turn moving into `phase` announces, once, or null for a move nobody acts on. */
function announcement(turn: TurnView): string | null {
  switch (turn.phase) {
    case "awaiting_approval": {
      const waiting = turn.cards.find(
        (c) => c.card.type === "proposal" && c.card.proposal?.state === "proposed",
      );
      return `Needs your approval: ${waiting?.card.proposal?.title ?? "a change"}`;
    }
    case "completed":
      return "Answer ready";
    case "stopped":
      return `Stopped. ${consequenceOf(turn)}`;
    case "failed":
      return turn.error?.message ?? `The answer failed. ${consequenceOf(turn)}`;
    default:
      return null;
  }
}

/** Announce each transition of the last turn once. */
function useAnnouncements(view: ConversationView): void {
  const said = useRef<string>("");
  const last = view.turns.at(-1);
  const key = last ? `${last.key}:${last.phase}` : "";
  useEffect(() => {
    if (!last || said.current === key) return;
    const first = said.current === "";
    said.current = key;
    // A CONVERSATION OPENED FROM HISTORY is not news: nothing is said for
    // the turn it was opened on.
    if (first) return;
    const words = announcement(last);
    if (words) announce(words);
  }, [key, last]);
}

/**
 * WHO READ THIS CONVERSATION FOR COMPLIANCE, and why, at the top of it (D06):
 * an administrator may read somebody's conversation only with a reason, and
 * the engine records each read on the conversation itself, so its owner sees
 * every one of them where they read their own words.
 */
function AuditReads({ reads }: { reads: ConversationView["auditReads"] }) {
  if (reads.length === 0) return null;
  return (
    <Callout
      variant="info"
      title={
        reads.length === 1
          ? "An administrator read this conversation"
          : `Administrators read this conversation ${reads.length} times`
      }
    >
      <Stack gap={1}>
        {reads.map((read) => (
          <span key={`${read.at}/${read.reader}`}>
            {`${read.reader}, ${fmtDateTime(read.at)}: “${read.reason}”`}
          </span>
        ))}
      </Stack>
    </Callout>
  );
}

export function Thread({
  view,
  now,
  on,
  opened,
  onRetry,
  retryDisabledReason,
  onReconnect,
  technical = false,
}: ThreadProps) {
  useAnnouncements(view);
  const reconnecting = view.stream === "reconnecting";
  const patient =
    view.reconnectingSince !== null && now - view.reconnectingSince > RECONNECT_PATIENCE_MS;
  return (
    <>
      <AuditReads reads={view.auditReads} />
      {view.turns.map((turn) => (
        <Turn
          key={turn.key}
          turn={turn}
          now={now}
          on={on}
          opened={opened}
          onRetry={onRetry}
          retryDisabledReason={retryDisabledReason}
          technical={technical}
        />
      ))}
      {reconnecting ? (
        <Callout
          variant="info"
          role="status"
          title="Reconnecting"
          action={
            patient ? (
              <Button size="small" variant="secondary" onClick={onReconnect}>
                Try again
              </Button>
            ) : undefined
          }
        >
          The connection dropped. Your question keeps running in Crewlet, and the answer continues
          here once you are back online.
        </Callout>
      ) : null}
      {view.fault === "protocol_mismatch" ? (
        <Callout
          variant="warning"
          role="alert"
          title="Crewlet was updated"
          action={
            <Button size="small" variant="primary" onClick={() => location.reload()}>
              Reload
            </Button>
          }
        >
          Reload the page to keep asking. Your conversation is kept.
        </Callout>
      ) : view.fault !== null ? (
        <Callout variant="warning" role="alert" title="The answer could not be followed">
          Open the conversation again from Conversations to see where it stands.
        </Callout>
      ) : null}
    </>
  );
}
