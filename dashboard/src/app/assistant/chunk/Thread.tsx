/**
 * A conversation's turns, drawn from the reducer's state and nothing else.
 *
 * ONE TURN IS: the person's question in a bubble, with the page it carried
 * under it; the run chip, which says the turn's state in words and opens on
 * the steps it took (each with the ordinary request it made, in mono); the
 * answer as prose with no container round it; the engine's cards; and, once
 * the turn has ended, a foot with when it was and Copy. No answer rating: in
 * a self-hosted engine nothing would read one.
 *
 * THE THREAD IS A LOG WITH ITS LIVE REGION OFF: a streaming answer is not
 * read out token by token. What is announced, once, through the one polite
 * announcer, is each transition a reader acts on: an answer ready, a change
 * waiting on them, a turn stopped.
 */

import { useEffect, useRef } from "react";
import {
  Callout,
  CopyButton,
  MessageBubble,
  RelativeTime,
  RunChip,
  Skeleton,
  StepList,
  Tag,
  VisuallyHidden,
  Button,
  Inline,
  Stack,
  Text,
  announce,
} from "@crewlethq/ui";
import { LayoutDashboardGlyph } from "@crewlethq/icons/glyphs";
import { fmtDateTime } from "~/lib/format.ts";
import {
  ActionReceipt,
  CardBlock,
  ReplyProse,
  runChipOf,
  stepsOf,
  type CardHandlers,
} from "./blocks.tsx";
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
  /** Send a question again, for a turn that failed on something that may pass. */
  onRetry: (text: string) => void;
  /** Why a question cannot be sent now (Send's own reason), which holds Try again off too. */
  retryDisabledReason?: string | undefined;
  /** Dial the running turn's stream again. */
  onReconnect: () => void;
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

/** One turn. */
function Turn({
  turn,
  now,
  on,
  opened,
  onRetry,
  retryDisabledReason,
  last,
}: {
  turn: TurnView;
  now: number;
  on: CardHandlers;
  opened: ReadonlySet<string>;
  onRetry: (text: string) => void;
  retryDisabledReason: string | undefined;
  last: boolean;
}) {
  const open = isOpen(turn.phase);
  const chip = runChipOf(turn, now);
  const steps = stepsOf(turn);
  // THE SHIMMER STANDS IN FOR AN ANSWER NOT YET DRAWN, so it goes as soon as
  // anything of the answer is on screen: a reply, or a card a read built.
  const thinking =
    (turn.phase === "sending" || turn.phase === "thinking") &&
    turn.replies.length === 0 &&
    turn.cards.length === 0;
  const placed = new Set(turn.replies.map((r) => r.id));
  const cardsOf = (messageId: string) => turn.cards.filter((c) => c.message_id === messageId);
  const loose = turn.cards.filter((c) => !placed.has(c.message_id));
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
        attachment={turn.page ? <SentWith title={turn.page.title} /> : undefined}
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
              ? `You can ask again in ${turn.refusal.retryAfterSeconds} seconds, and your question is back in the box below.`
              : "Nothing changed.")}
        </Callout>
      ) : (
        <RunChip state={chip.state} title={chip.title} meta={chip.meta} defaultOpen={open}>
          {steps.length > 0 ? <StepList label="Steps" steps={steps} /> : undefined}
        </RunChip>
      )}
      {thinking ? <Skeleton variant="text" rows={3} label="Thinking" /> : null}
      {turn.replies.map((reply, i) => (
        <Stack key={reply.id} gap={3}>
          <ReplyProse
            text={reply.text}
            streaming={open && last && i === turn.replies.length - 1 && turn.phase === "working"}
            dimmed={turn.phase === "stopped"}
          />
          {cardsOf(reply.id).map(card)}
        </Stack>
      ))}
      {loose.map(card)}
      {receipts.map((a) => (
        <ActionReceipt
          key={a.action_id}
          actionId={a.action_id}
          local={turn.actionStates[a.action_id]!}
          on={on}
        />
      ))}
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
                onClick={() => onRetry(turn.question)}
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
            <RelativeTime value={turn.askedAt} now={now} />
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
}: ThreadProps) {
  useAnnouncements(view);
  const reconnecting = view.stream === "reconnecting";
  const patient =
    view.reconnectingSince !== null && now - view.reconnectingSince > RECONNECT_PATIENCE_MS;
  return (
    <>
      <AuditReads reads={view.auditReads} />
      {view.turns.map((turn, i) => (
        <Turn
          key={turn.key}
          turn={turn}
          now={now}
          on={on}
          opened={opened}
          onRetry={onRetry}
          retryDisabledReason={retryDisabledReason}
          last={i === view.turns.length - 1}
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
