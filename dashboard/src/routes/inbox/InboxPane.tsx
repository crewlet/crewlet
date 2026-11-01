/**
 * The Inbox's pane: the open row, read in full in one column, and the answer
 * to it.
 *
 * TOP TO BOTTOM, NOTHING TO OPEN AND NOTHING SAID TWICE. One sentence says who
 * is asking, what is asked of the reader and on which task; the question
 * follows in large type, then its context, then the asker's reasoning with the
 * evidence it cites, then the answer, and last the conversation with the reply
 * box at its end. The time is on the row in the list, so it is not repeated
 * here.
 *
 * AN ASK IS ANSWERED THROUGH `AnswerAsk`, the one write every surface answers
 * an ask with, in its `buttons` layout: each option sends that choice with one
 * press (`comment_on_work_item{item, answers, choice}`), the recommended one
 * first and filled, and "Something else" answers in words instead. Answering
 * also marks the notices that asked it read, so an answered question does not
 * come back as an update.
 *
 * WHAT WAS DONE STAYS ON SCREEN FOR A MOMENT: an answer, an executor's answer, a read
 * or a snooze tells the screen (`onSettled`), which shows the outcome here and
 * then opens the next row.
 */

import { useEffect, useRef, useState, type ReactNode } from "react";
import {
  Button,
  ButtonLink,
  EmptyState,
  IconButton,
  Composer as Field,
  Link,
  ReadingPane,
  ReadingPaneByline,
  ReadingPaneLead,
  ReadingPaneOutcome,
  ReadingPaneQuote,
  ReadingPaneReasoning,
  ReadingPaneSection,
  ReadingPaneTitle,
  RefusalStrip,
  Stack,
  Stepper,
  InlineRef,
  Text,
  peekClickHandler,
  type StepperStep,
} from "@crewlethq/ui";
import {
  ActivityGlyph,
  ChevronLeftGlyph,
  CircleCheckGlyph,
  FileTextGlyph,
  SquareKanbanGlyph,
  SquareTerminalGlyph,
} from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { describeLink } from "~/lib/inlineTokens.ts";
import { useLinkHosts } from "~/lib/linkHosts.ts";
import { peekHref, usePeekControls } from "~/app/frame/peekAddress.ts";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { holdLine, ReassignItem } from "~/components/DecisionRow.tsx";
import type { SeatCondition } from "~/lib/decisions.ts";
import { RaiseBudgetButton } from "~/components/budgetWrite.tsx";
import { LimitRemedy } from "~/components/LimitRemedy.tsx";
import { AnswerAsk, type AskAnswer } from "~/components/writes.tsx";
import { AnswerChoices, AnsweredChoices } from "~/components/AnswerChoices.tsx";
import { TechnicalDetails } from "~/components/common.tsx";
import { Composer } from "~/components/Composer.tsx";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { RUNS_POLL_MS, runAskTexts, unanswerable } from "~/lib/runs.ts";
import { useFocusLanding } from "~/lib/focusLanding.ts";
import { RunQuestion } from "~/components/RunQuestion.tsx";
import { answerFate, type GivenAnswer } from "./answerFate.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { addressMark, InertAddress, TokenScope } from "~/components/InlineToken.tsx";
import { CHECKED_WORDS, renderMarkdown, renderTokens } from "~/lib/markdown.ts";
import { nameOfIn, type OrgIndex } from "~/lib/seats.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useAgents } from "~/lib/store-hooks.ts";
import type { Attention } from "~/lib/attention.ts";
import { describeFollowUp, detailItem, itemAddress, labelAfterKey } from "~/lib/work.ts";
import type {
  AnsweredDecision,
  AnsweredWorkAsk,
  ExecutorRow,
  WorkAskRow,
  WorkDecisionEvidence,
  WorkItem,
  WorkInboxNotice,
} from "~/protocol/index.ts";
import {
  answerWords,
  answeredQuestion,
  answeredTexts,
  askContext,
  askTexts,
  askQuestion,
  followThrough,
  budgetLine,
  moment,
  noticeNote,
  noticeText,
  noticeTitle,
  rowItem,
  rowKey,
  rowName,
  rowWho,
  type AnswerWords,
  type FollowThrough,
  type InboxRow,
  type Who,
} from "./model.ts";
import { Badge } from "./InboxColumn.tsx";
import { NoticeActions } from "./NoticeActions.tsx";
import { TaskLink } from "./TaskLink.tsx";
import { Thread, type ThreadOf } from "./Thread.tsx";
import { questionWithheld } from "~/lib/access.ts";

interface PaneProps {
  row: InboxRow | null;
  /**
   * What the pane says with no row open, where the default ("New updates
   * open here") would promise what the company's services cannot send.
   */
  empty?: { title: string; description: string } | undefined;
  /**
   * The list beside the pane is empty and says so in its own empty state. The
   * pane then says no second "All clear": it holds one quiet line, the place a
   * reader's next update will open.
   */
  listEmpty?: boolean | undefined;
  index: OrgIndex;
  now: number;
  /** The company's clock, which the snooze presets are read on. */
  zone: string | undefined;
  maxSnoozeAhead: number | undefined;
  /** The title of the task the row is on, where the list resolved one. */
  title: string | undefined;
  /** Back to the list, where the list and the pane are one column. */
  onBack?: (() => void) | undefined;
  /**
   * The answer given here to the row, which has moved to Answered and which
   * the engine does not list as answered yet. The pane draws it, read-only,
   * where the controls that gave it were: drawn again, they would answer the
   * question a second time. Undefined for every other row.
   */
  answered?: AskAnswer | undefined;
  /**
   * The row was dealt with here. `label` says how ("Answered") for a row that
   * stays in the list as done, with the answer that was given; null for a
   * notice, which leaves its group.
   *
   * `hold` keeps the row open rather than moving on to the next one: an
   * answer the engine has accepted and not yet settled is shown as sent, and
   * the row stays where the person can see what became of it. Null with
   * `hold` takes back a label given earlier, for an answer the engine could
   * not use.
   */
  onSettled: (label: string | null, answer?: AskAnswer, hold?: boolean) => void;
}

export function InboxPane(props: PaneProps) {
  if (!props.row && props.listEmpty && !props.empty) {
    return (
      <div className="inbox-pane inbox-pane-empty">
        <Text variant="caption">New updates open here.</Text>
      </div>
    );
  }
  if (!props.row) {
    return (
      <div className="inbox-pane inbox-pane-empty">
        <EmptyState
          icon={<CircleCheckGlyph size={28} />}
          title={props.empty?.title ?? "All clear"}
          description={props.empty?.description ?? "New updates open here."}
        />
      </div>
    );
  }
  // ONE READING PANE FOR EVERY ROW, told which row it reads (`contentKey`):
  // a new row plays in where the last one stood and takes focus to its title,
  // so after an answer the reader's focus lands on the next item rather than
  // on the page. What it holds is REMOUNTED PER ROW, so a draft, a pressed
  // option or an outcome drawn for one row never shows under the next.
  return (
    <div className="inbox-pane">
      <ReadingPane contentKey={props.row.key}>
        <OpenPane key={props.row.key} {...props} row={props.row} />
      </ReadingPane>
    </div>
  );
}

/** What each role is asked for, as the byline says it. */
const ASKED: Readonly<Record<string, string>> = {
  approver: "asks for your approval",
  contributor: "asks for your input",
};

/** The pieces every kind of row shares: who, the task, and the line over the item. */
interface Frame {
  who: Who | null;
  name: string;
  task: ReactNode;
  /**
   * The task's ADDRESS ([itemAddress]), or "": what the pane threads, replies,
   * answers and reassigns by. Its key is only what it prints, since a key
   * another task claimed first would read and write the claimant's
   * conversation from a duplicate's notice.
   */
  address: string;
  /** The key it prints — what a reply's or an answer's toast names it by — or "". */
  itemKey: string;
  byline: (sentence: ReactNode, note?: string) => ReactNode;
}

function OpenPane({
  row,
  index,
  now,
  zone,
  maxSnoozeAhead,
  title,
  onBack,
  answered,
  onSettled,
}: PaneProps & { row: InboxRow }) {
  const who = rowWho(row, index);
  const item = rowItem(row);
  const back = onBack ? (
    <IconButton
      size="sm"
      variant="ghost"
      label="Back to the inbox"
      icon={<ChevronLeftGlyph size="sm" />}
      onClick={onBack}
    />
  ) : undefined;
  const frame: Frame = {
    who,
    name: rowName(row, who),
    task: item ? <TaskLink item={item} label={rowKey(row)} title={title} /> : null,
    address: item ? itemAddress(item) : "",
    itemKey: rowKey(row),
    byline: (sentence, note) => (
      <ReadingPaneByline
        start={back}
        leading={<Badge row={row} who={who} size={20} />}
        note={note || undefined}
      >
        {sentence}
      </ReadingPaneByline>
    ),
  };

  switch (row.kind) {
    case "decision":
      switch (row.subject.kind) {
        case "ask":
          return (
            <AskPane
              ask={row.subject.ask}
              notices={row.notices}
              frame={frame}
              index={index}
              now={now}
              answered={answered}
              onSettled={onSettled}
            />
          );
        case "run":
          return (
            <RunPane
              run={row.subject.run}
              frame={frame}
              now={now}
              answered={answered}
              onSettled={onSettled}
            />
          );
        case "seat":
          return <SeatPane seat={row.subject.seat} frame={frame} />;
      }
      break;
    case "condition":
      return <ConditionPane item={row.item} frame={frame} now={now} />;
    case "answered":
      return <AnsweredPane answered={row.answered} frame={frame} index={index} now={now} />;
    case "notice":
      return (
        <NoticePane
          notice={row.notice}
          frame={frame}
          index={index}
          now={now}
          zone={zone}
          maxSnoozeAhead={maxSnoozeAhead}
          onSettled={onSettled}
        />
      );
  }
  return null;
}

/**
 * "Answered Exact search (Recommended)", where the controls that answered it
 * were: an ask's, or an executor's once it took the answer.
 */
function answeredWith(answer: AskAnswer): ReactNode {
  return (
    <>
      Answered <strong>{answer.kind === "choice" ? answer.label : `“${answer.text}”`}</strong>
      {answer.kind === "choice" && answer.recommended && " (Recommended)"}
    </>
  );
}

function AskPane({
  ask,
  notices,
  frame,
  index,
  now,
  answered,
  onSettled,
}: {
  ask: WorkAskRow;
  notices: readonly WorkInboxNotice[];
  frame: Frame;
  index: OrgIndex;
  now: number;
  /** The answer given here before, drawn in place of the controls ([PaneProps.answered]). */
  answered: AskAnswer | undefined;
  onSettled: (label: string | null, answer?: AskAnswer) => void;
}) {
  const decision = ask.decision;
  const options = decision?.options ?? [];
  const [open, setOpen] = useState(false);
  // THE ANSWER THE PANE OPENED ON, given before: the pane is drawn afresh
  // for each row, so this is the row's as it was opened.
  const [before] = useState<AskAnswer | null>(answered ?? null);
  const [answer, setAnswer] = useState<AskAnswer | null>(before);
  const mark = useAct("mark_inbox");
  // THE OUTCOME TAKES THE FOCUS from the option that sent it, which it
  // replaces: an answer given here, never the one the pane opened on.
  const landing = useFocusLanding(answer !== null && answer !== before);
  const recommended = options.find((o) => o.id === decision?.recommended);
  const context = askContext(ask);
  // THE ASK'S OWN TASK IS NOT EVIDENCE of anything: the byline names it.
  const evidence = (decision?.evidence ?? []).filter(
    (ev) => !(ev.kind === "task" && (ev.ref === ask.id || ev.ref === ask.key)),
  );
  const chips = evidence.length > 0 ? <Evidence list={evidence} /> : null;
  // THE REASONING SITS WITH WHAT IT RECOMMENDS, and goes once the ask is
  // answered: what is left to read then is the answer.
  const reasoning =
    !answer && recommended && decision?.rationale
      ? { label: recommended.label, text: decision.rationale }
      : null;
  // A QUESTION ANSWERED IN WORDS is answered in its own field above, so the
  // conversation carries no second box.
  const inWords = options.length === 0 || open;

  const onAnswered = (given: AskAnswer) => {
    setAnswer(given);
    const unread = notices.filter((n) => !n.read).map((n) => n.record_id);
    if (unread.length > 0) {
      void mark.run({ read: unread }, { done: "Marked read", quiet: true });
    }
    onSettled("Answered", given);
  };

  return (
    <TokenScope texts={askTexts(ask)}>
      <>
        {frame.byline(
          <>
            <strong>{frame.name}</strong>
            {` ${(decision && ASKED[decision.role]) || "asks you"}`}
            {frame.task && <> on {frame.task}</>}
          </>,
        )}
        <ReadingPaneTitle>
          {renderTokens(askQuestion(ask), "question", "text", true)}
        </ReadingPaneTitle>
        {context && (
          <ReadingPaneLead>
            {renderMarkdown(context, { ...CHECKED_WORDS, scoped: true })}
          </ReadingPaneLead>
        )}
        {reasoning ? (
          <ReadingPaneReasoning
            title={
              <>
                {`${frame.name} recommends `}
                <strong>{renderTokens(reasoning.label, "recommended", "text", true)}</strong>
              </>
            }
            footer={chips}
          >
            <p>{renderTokens(reasoning.text, "reasoning", "checked", true)}</p>
          </ReadingPaneReasoning>
        ) : (
          chips
        )}
        {answer ? (
          <ReadingPaneOutcome {...landing}>{answeredWith(answer)}</ReadingPaneOutcome>
        ) : (
          <AnswerAsk
            layout="buttons"
            item={frame.address}
            label={frame.itemKey}
            comment={ask.comment}
            asker={frame.name}
            options={options}
            recommended={decision?.recommended}
            open={open}
            onOpenChange={setOpen}
            onAnswered={onAnswered}
          />
        )}
        {decision?.inform && !answer && (
          <Text as="p" variant="caption">
            {`Your answer is also posted to #${decision.inform.channel}.`}
          </Text>
        )}
        <RefusalNote write={mark} />
        <Conversation
          item={frame.address}
          itemKey={frame.itemKey}
          of={{ kind: "ask", comment: ask.comment, options }}
          label="Replies to this question"
          to={frame.name}
          replyTo={inWords && !answer ? null : ask.comment}
          placeholder="Reply without answering. @mention to bring someone in."
          index={index}
          now={now}
        />
      </>
    </TokenScope>
  );
}

/** What each role was asked for, as an answered ask's byline says it. */
const WAS_ASKED: Readonly<Record<string, string>> = {
  approver: "asked for your approval",
  contributor: "asked for your input",
};

/**
 * An answered question, read in full: the page it was answered on with the
 * buttons replaced by the answer, an ask's or an executor's alike.
 *
 * WHO ANSWERED IS SAID, because it is not always the reader: a colleague may
 * answer a question put to you, and the line under the answer says so and
 * where to disagree.
 *
 * AN ASK'S goes on to what the asker has done with it since, then the
 * conversation, whose first entry is the answer with the option it chose.
 * WHAT HAPPENED NEXT IS FOUR STEPS (asked, answered, picked up, acted on),
 * the last two read from the asker's seat as it is now and from its first
 * record on the task since (`followed_up`). A person who asked has no seat
 * state to read, so their steps stop at the answer.
 *
 * AN EXECUTOR'S has no thread and no record on a task to follow, so the
 * reading is its ask itself: its context, and the options it offered with the
 * chosen one marked, as `RunPane` draws them once it is answered there, then
 * the way to the executor.
 */
function AnsweredPane({
  answered,
  frame,
  index,
  now,
}: {
  answered: AnsweredDecision;
  frame: Frame;
  index: OrgIndex;
  now: number;
}) {
  const viewer = useViewer();
  const words = answerWords(answered, viewer.owner, index);
  const run = "run" in answered ? answered.run : null;
  const ask = "run" in answered ? null : answered;
  const decision = run ? run.decision : ask?.ask.decision;
  const asked = (decision && WAS_ASKED[decision.role]) || "asked you";
  const said =
    words.kind === "resolved" ? null : words.kind === "choice" ? words.said : `“${words.said}”`;
  const context = run ? run.question_context.trim() : "";
  return (
    <TokenScope texts={answeredTexts(answered)}>
      <>
        {frame.byline(
          <>
            <strong>{frame.name}</strong>
            {run ? `’s executor ${asked}` : ` ${asked}`}
            {frame.task && <> on {frame.task}</>}
          </>,
        )}
        {run ? (
          <RunQuestion question={answeredQuestion(answered)} />
        ) : (
          <ReadingPaneTitle>
            {renderTokens(answeredQuestion(answered), "question", "text", true)}
          </ReadingPaneTitle>
        )}
        {context && (
          <ReadingPaneLead>
            {renderMarkdown(context, { ...CHECKED_WORDS, scoped: true })}
          </ReadingPaneLead>
        )}
        {run && decision && (
          <AnsweredChoices
            options={decision.options}
            recommended={decision.recommended}
            chosen={words.kind === "choice" ? (answered.answer.choice ?? null) : null}
          />
        )}
        <ReadingPaneOutcome>
          {`${words.who} ${words.verb}`}
          {said && (
            <>
              {" "}
              <strong>{said}</strong>
            </>
          )}
          {words.recommended && " (Recommended)"}
          {words.note && (
            <>
              {" and wrote "}
              <strong>{`“${words.note}”`}</strong>
            </>
          )}
        </ReadingPaneOutcome>
        {words.who !== "You" && words.kind !== "resolved" && (
          <Text as="p" variant="caption">
            {`${words.who} answered for you at ${moment(answered.answer.at, now)}. Reply below if you see it differently.`}
          </Text>
        )}
        {run && <RunLink turnId={run.turn_id} />}
        {ask && (
          <AskFollowThrough answered={ask} frame={frame} words={words} index={index} now={now} />
        )}
      </>
    </TokenScope>
  );
}

/** What the asker of an answered ask has done with it since, and the conversation. */
function AskFollowThrough({
  answered,
  frame,
  words,
  index,
  now,
}: {
  answered: AnsweredWorkAsk;
  frame: Frame;
  words: AnswerWords;
  index: OrgIndex;
  now: number;
}) {
  const agents = useAgents();
  const ask = answered.ask;
  const seat = agents.find((a) => a.handle === ask.asked_by);
  const follow = followThrough(answered, frame.who?.kind === "agent" ? seat : undefined);
  return (
    <>
      {follow ? (
        <ReadingPaneSection label="What happened next">
          <Stepper
            label={`What ${frame.name} did with the answer`}
            orientation="vertical"
            steps={followSteps(answered, follow, frame.name, words, now, index)}
            current={
              follow.state === "acted" ? null : follow.state === "acting" ? "done" : "picked"
            }
            tone={follow.state === "held" ? "warning" : "info"}
            pulse={follow.state === "acting"}
          />
        </ReadingPaneSection>
      ) : null}
      <Conversation
        item={frame.address}
        itemKey={frame.itemKey}
        of={{ kind: "ask", comment: ask.comment, options: ask.decision?.options ?? [] }}
        label="Replies to this question"
        to={frame.name}
        replyTo={ask.comment}
        placeholder={`Add to your answer for ${frame.name}. @mention to bring someone in.`}
        index={index}
        now={now}
      />
    </>
  );
}

/** The way from an executor's question to its page, after what is said of its hold. */
function RunLink({ turnId, hold = "" }: { turnId: string; hold?: string }) {
  return (
    <Text as="p" variant="caption">
      {hold ? `${hold}. ` : ""}
      <Link href={href(["live", "executors", turnId])} size="inherit">
        Open the executor
      </Link>
    </Text>
  );
}

/** The four steps from the question to the work, as the pane draws them. */
function followSteps(
  answered: AnsweredWorkAsk,
  follow: FollowThrough,
  asker: string,
  words: AnswerWords,
  now: number,
  index: OrgIndex,
): StepperStep[] {
  const done = answered.followed_up;
  const picked: StepperStep = {
    id: "picked",
    label:
      follow.state === "waiting" || follow.state === "held"
        ? `${asker} picks it up`
        : `${asker} picked it up`,
  };
  if (follow.state === "held") {
    picked.status = "attention";
    picked.description =
      follow.reason === "paused"
        ? "Paused. It continues once resumed."
        : "Stopped. It continues once it can run.";
  } else if (follow.state === "waiting") {
    picked.description = "Not yet";
  } else if (follow.at) {
    picked.description = moment(follow.at, now);
  }
  return [
    { id: "asked", label: `${asker} asked`, description: moment(answered.ask.asked_at, now) },
    {
      id: "answered",
      label: words.kind === "resolved" ? `${words.who} closed it` : `${words.who} answered`,
      description: moment(answered.answer.at, now),
    },
    picked,
    {
      id: "done",
      label: follow.state === "acted" ? `${asker} acted on it` : `${asker} acts on it`,
      description: done
        ? describeFollowUp(done, { seatName: nameOfIn(index) })
        : follow.state === "acting"
          ? "Working on it now"
          : undefined,
    },
  ];
}

/** What a decision cites, each linked to where it lives. */
function Evidence({ list }: { list: readonly WorkDecisionEvidence[] }) {
  return (
    <Stack gap={2} align="start" wrap className="inbox-evidence">
      {list.map((ev, i) => (
        <EvidenceLink key={`${ev.kind}:${ev.ref}:${i}`} evidence={ev} />
      ))}
    </Stack>
  );
}

function EvidenceLink({ evidence }: { evidence: WorkDecisionEvidence }) {
  const look = { variant: "secondary", size: "small" } as const;
  switch (evidence.kind) {
    case "task":
      return <TaskEvidence id={evidence.ref} label={evidence.label} />;
    case "page":
      return (
        <ButtonLink
          {...look}
          href={href(["doclet", "pages", evidence.ref])}
          leadingIcon={<FileTextGlyph size="sm" />}
        >
          {evidence.label || "Page"}
        </ButtonLink>
      );
    case "turn":
      return (
        <ButtonLink
          {...look}
          href={href(["live", "turns", evidence.ref])}
          leadingIcon={<ActivityGlyph size="sm" />}
        >
          {evidence.label || "Turn trace"}
        </ButtonLink>
      );
    case "run":
      return (
        <ButtonLink
          {...look}
          href={href(["live", "executors", evidence.ref])}
          leadingIcon={<SquareTerminalGlyph size="sm" />}
        >
          {evidence.label || "Executor"}
        </ButtonLink>
      );
    case "url":
      return <UrlEvidence raw={evidence.ref} />;
  }
  return null;
}

/**
 * An address the asker cited, by the Inbox's `checked` rule
 * (`LinkMode` in `lib/inlineTokens.ts`): a button only when the dashboard
 * builds it, on a host the company configured and rebuilt from what it names
 * ([LinkDescription.canonical]), labelled by the dashboard (the reference it
 * is, or its host and path) and marked with its app's mark.
 *
 * NEVER THE ASKER'S LABEL AND NEVER ITS ADDRESS AS WRITTEN: both are an
 * agent's words, and a button reading "Sign in to GitHub" that opens
 * `https://evil.example.net/?q=<what it read>` is the one press an injected
 * instruction needs. Any other address is drawn inert, as written, with its
 * mark, so the reader sees and can copy where it would go.
 */
function UrlEvidence({ raw }: { raw: string }) {
  const hosts = useLinkHosts();
  const said = describeLink(raw, hosts);
  if (said === null) return <Text variant="caption">{raw}</Text>;
  if (said.canonical) {
    // WHAT ITS MARK MEANS IS SAID before its words, and an identifier is set
    // in mono, as the same address reads in a sentence.
    return (
      <ButtonLink
        variant="secondary"
        size="small"
        href={said.canonical}
        external
        leadingIcon={addressMark(said, "sm")}
      >
        <InlineRef kind={said.kind ?? undefined} code={said.code}>
          {said.label}
        </InlineRef>
      </ButtonLink>
    );
  }
  return (
    <Text variant="caption">
      <InertAddress said={said} raw={raw} written={raw} />
    </Text>
  );
}

/**
 * A cited task, by its key and title as they are now, opened beside the Inbox
 * at its ADDRESS ([itemAddress]) once the read says which task it is.
 */
function TaskEvidence({ id, label }: { id: string; label?: string }) {
  const read = useQuery("work_item", { id });
  const task = read.data?.task;
  const peek = usePeekControls();
  const ref = { kind: "item" as const, id: read.data ? itemAddress(detailItem(read.data)) : id };
  return (
    <ButtonLink
      variant="secondary"
      size="small"
      href={peekHref(ref)}
      leadingIcon={<SquareKanbanGlyph size="sm" />}
      onClick={peekClickHandler(() => peek.open(ref))}
    >
      {task ? `${task.key} ${citedLabel(task, label)}` : label || "Task"}
    </ButtonLink>
  );
}

/**
 * What a cited task's button says after its key: the asker's label without a
 * copy of the key it already follows ([labelAfterKey]), or the task's title.
 */
function citedLabel(task: WorkItem, label: string | undefined): string {
  return labelAfterKey(label ?? "", [task.key, ...(task.former_keys ?? [])]) || task.title;
}

/** The conversation under the answer, with the reply box at its end. */
function Conversation({
  item,
  itemKey,
  of,
  label,
  to,
  replyTo,
  placeholder,
  index,
  now,
}: {
  /** The task's address ([itemAddress]), or "" for no conversation. */
  item: string;
  /** The key the reply's toast names the task by, or "". */
  itemKey: string;
  of: ThreadOf;
  /** What is below it, said exactly. */
  label: string;
  /** Who the reader is replying to, by name. */
  to: string;
  /** The comment a reply answers; undefined for a comment on the task; null for no box. */
  replyTo: string | null | undefined;
  placeholder: string;
  index: OrgIndex;
  now: number;
}) {
  if (!item) return null;
  return (
    <ReadingPaneSection label={label}>
      <Thread item={item} of={of} index={index} now={now} />
      {replyTo !== null && (
        <Composer
          item={item}
          label={itemKey}
          mode={replyTo ? { kind: "reply", replyTo } : { kind: "reply" }}
          to={to}
          index={index}
          placeholder={placeholder}
        />
      )}
    </ReadingPaneSection>
  );
}

/**
 * An executor parked on a question, read as an ask is: its question as the
 * title (`RunQuestion`), its context as Markdown, the executor's
 * recommendation with the evidence it cites, then the answer. A question that
 * asked to choose is answered with one press of an option (`answer_executor{turn_id, launch_id,
 * choice}`), the recommended one first and filled, through the same buttons
 * an ask's options are (`AnswerChoices`); "Something else", and an open
 * question from the start, answers in words (`answer_executor{turn_id, launch_id,
 * answer}`). Every answer names the question this pane drew by its launch.
 */
function RunPane({
  run,
  frame,
  now,
  answered,
  onSettled,
}: {
  run: ExecutorRow;
  frame: Frame;
  now: number;
  /**
   * The answer given here before, which the executor took ([PaneProps.answered]):
   * drawn as taken, with no controls and nothing left to follow.
   */
  answered: AskAnswer | undefined;
  onSettled: (label: string | null, answer?: AskAnswer, hold?: boolean) => void;
}) {
  const write = useAct("answer_executor");
  const [text, setText] = useState("");
  // "SOMETHING ELSE" OPENED THE FIELD in the options' place.
  const [open, setOpen] = useState(false);
  // THE OPTION BEING SENT, which its button says while the write is out.
  const [chosen, setChosen] = useState<string | null>(null);
  // THE ANSWER THE ENGINE ACCEPTED, and what it said back about it: the
  // question it answers and the answer's own id. `pending` is all the press
  // can say; the executor's own record says the rest (see `answerFate`).
  const [given, setGiven] = useState<GivenAnswer | null>(null);
  // WHY THE LAST ANSWER WAS NOT USED, said until the person answers again.
  const [unused, setUnused] = useState<string | null>(null);
  // ANSWERED BY SOMEBODY ELSE FIRST: nothing is left to answer here.
  const [superseded, setSuperseded] = useState(false);
  // THE ENGINE REFUSED THIS ANSWER because the question was answered, and
  // whether it said so in its own sentence (`worded`), the refusal's
  // `detail`, which names who answered. An older node may send none, and
  // the dashboard's sentence for the code, which names nobody, is drawn.
  // When it was answered is said beside it off the executor's record, on the
  // reader's own clock.
  const [refusedAnswered, setRefusedAnswered] = useState<{ worded: boolean } | null>(null);
  const runs = useQuery("executors", undefined, { pollMs: RUNS_POLL_MS });
  const record = runs.data?.runs.find((r) => r.turn_id === run.turn_id);
  // WHETHER THIS ANSWER WAS SEEN ON THE EXECUTOR'S RECORD, so a record that
  // then goes away is read as the executor finishing with it rather than as
  // its ending.
  const seenTaken = useRef(false);
  // WHAT THE PANE SAYS WAS ANSWERED: the answer sent from it, or the one the
  // executor took before it opened, which has no fate left to follow.
  const said = given?.answer ?? answered ?? null;
  const fate = given
    ? answerFate(given, record, runs.data !== undefined, seenTaken.current)
    : answered
      ? "taken"
      : null;
  if (fate === "recorded" || fate === "taken") seenTaken.current = true;
  // THE OUTCOME TAKES THE FOCUS from the control that sent the answer, which
  // it replaces.
  const landing = useFocusLanding(given !== null);
  const question = (run.question ?? "").trim();
  const context = run.question_context.trim();
  const decision = run.decision;
  const options = decision?.options ?? [];
  const recommended = options.find((o) => o.id === decision?.recommended);
  // A QUESTION THIS READER CAN'T SEE IS NOT ASKED OF THEM: the answer form
  // asked for an answer to a question it never showed.
  const hidden = questionWithheld(run);
  // THE EXECUTOR'S OWN TASK AND THE EXECUTOR ITSELF ARE NOT EVIDENCE of
  // anything: the byline names the one and the line at the end opens the other.
  const evidence = (decision?.evidence ?? []).filter(
    (ev) =>
      !(ev.kind === "task" && (ev.ref === run.work_item?.id || ev.ref === run.work_item?.key)) &&
      !(ev.kind === "run" && ev.ref === run.turn_id),
  );
  const chips = evidence.length > 0 ? <Evidence list={evidence} /> : null;
  // THE REASONING SITS WITH WHAT IT RECOMMENDS, and goes once an answer is
  // sent: what is left to read then is the answer.
  const reasoning =
    !said && recommended && decision?.rationale
      ? { label: recommended.label, text: decision.rationale }
      : null;
  const inWords = options.length === 0 || open;
  const hold = holdLine(run, now);
  const notYet = unanswerable(run);
  const blocked = notYet ?? (text.trim() ? undefined : "Write the answer first.");
  // ONCE PER ANSWER: telling the screen redraws it, and the screen's
  // callback is a new one on every draw, so the fate is said once.
  const told = useRef<string | null>(null);

  useEffect(() => {
    if (!given || fate === null || fate === "sent") return;
    if (told.current === given.answerId + fate) return;
    told.current = given.answerId + fate;
    if (fate === "recorded") {
      // RECORDED ON THE EXECUTOR, its resume waiting for the seat: answered, and
      // kept open, saying what the seat is doing first.
      onSettled("Answered", given.answer, true);
      return;
    }
    if (fate === "taken") {
      onSettled("Answered", given.answer);
      return;
    }
    if (fate === "ended") {
      // NEITHER ANSWERED NOR REFUSED: the executor is no longer running, and
      // that is all the record says. Kept open, as sent, with that said
      // beside it.
      return;
    }
    // NOT USED, so the person is told so and why, never left looking at
    // "Sent" for an answer the executor will not read.
    if (fate === "superseded") {
      setUnused("Your answer was not used: another answer was recorded first.");
      setSuperseded(true);
    } else {
      setUnused(
        fate === "declined"
          ? `${frame.name}’s executor could not use your answer and is waiting on its question again. Answer it again.`
          : `${frame.name}’s executor moved on to a new question before your answer reached it. Answer the question above.`,
      );
    }
    setGiven(null);
    onSettled(null, undefined, true);
  }, [fate, given, frame.name, onSettled, record, now]);

  // THE SEAT'S OTHER WORK, while this answer waits for it: its executor on
  // another turn holding the seat.
  const busyOn = runs.data?.runs.find(
    (r) =>
      r.agent_handle === run.agent_handle &&
      r.turn_id !== run.turn_id &&
      (r.status === "launching" || r.status === "running" || r.status === "resumed"),
  );

  // ONE SEND for a choice and for words: the press differs only in what it
  // carries, and the answer's fate is read the same way after it.
  const send = async (args: { choice: string } | { answer: string }, said: AskAnswer) => {
    // THE QUESTION THIS PANE DREW rides the answer, so an executor that moved
    // on to another one refuses it rather than taking it as that one's answer.
    const result = await write.run(
      { turn_id: run.turn_id, launch_id: run.launch_id, ...args },
      { done: `Sent your answer to ${frame.name}’s executor` },
    );
    // THE RECORD IS ASKED AGAIN by the refusal itself: already_answered and
    // conflict (the executor asks another question now) are codes that look
    // again at what the press would have moved (protocol/writeOutcome.ts),
    // executors and decisions among it. Who answered and when arrive
    // without a read of this pane's own, and so does the question the
    // executor asks now, which this pane then draws under the engine's sentence.
    setRefusedAnswered(
      result?.kind === "refused" && result.code === "already_answered"
        ? { worded: result.detail !== "" }
        : null,
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) {
      const receipt = (result.receipt ?? {}) as { launch_id?: unknown; answer_id?: unknown };
      setUnused(null);
      seenTaken.current = false;
      setGiven({
        answer: said,
        launchId: typeof receipt.launch_id === "string" ? receipt.launch_id : (run.launch_id ?? ""),
        answerId: typeof receipt.answer_id === "string" ? receipt.answer_id : "",
      });
      // SENT, NOT ANSWERED: the row stays open, marked as sent, until the
      // executor's record says the answer was taken or why it was not.
      onSettled("Sent", said, true);
    }
  };
  const sendWords = () => {
    if (!pressable(write, blocked)) return;
    const answer = text.trim();
    void send({ answer }, { kind: "reply", text: answer });
  };
  const choose = (choice: string) => {
    const option = options.find((o) => o.id === choice);
    if (!option) return;
    setChosen(choice);
    void send(
      { choice },
      {
        kind: "choice",
        id: option.id,
        label: option.label,
        recommended: option.id === decision?.recommended,
      },
    );
  };
  // WHO ANSWERED IS SAID ONCE. The engine's sentence for the refusal names
  // them while it is drawn, so the line beside it says only when; where the
  // strip is the dashboard's own sentence, or the reader dismissed it,
  // nothing else on the pane names them, and the line does.
  const answererNamed = refusedAnswered?.worded === true && write.refusal !== null;
  return (
    <TokenScope texts={runAskTexts(run)}>
      <>
        {frame.byline(
          <>
            <strong>{frame.name}</strong>
            {"’s executor"}
            {decision && ASKED[decision.role] ? ` ${ASKED[decision.role]}` : ""}
            {frame.task && <> on {frame.task}</>}
          </>,
        )}
        {question ? (
          <RunQuestion question={question} />
        ) : (
          <ReadingPaneTitle>
            {hidden
              ? "This executor is waiting on a question you can’t see."
              : "The executor paused on a question"}
          </ReadingPaneTitle>
        )}
        {context && (
          <ReadingPaneLead>
            {renderMarkdown(context, { ...CHECKED_WORDS, scoped: true })}
          </ReadingPaneLead>
        )}
        {reasoning ? (
          <ReadingPaneReasoning
            title={
              <>
                {`${frame.name} recommends `}
                <strong>{renderTokens(reasoning.label, "recommended", "text", true)}</strong>
              </>
            }
            footer={chips}
          >
            <p>{renderTokens(reasoning.text, "reasoning", "checked", true)}</p>
          </ReadingPaneReasoning>
        ) : (
          chips
        )}
        {said !== null && decision && (
          // THE OPTIONS STAY, THE ONE CHOSEN MARKED, as an answered
          // executor's pane reads them once it has left Needs you.
          <AnsweredChoices
            options={options}
            recommended={decision.recommended}
            chosen={said.kind === "choice" ? said.id : null}
          />
        )}
        {said !== null ? (
          // PENDING UNTIL THE EXECUTOR TOOK IT: sent, recorded or ended is
          // never the success check a used answer gets.
          <ReadingPaneOutcome {...landing} tone={fate === "taken" ? "done" : "pending"}>
            {fate === "taken" ? (
              answeredWith(said)
            ) : fate === "recorded" ? (
              <>
                Answer recorded.{" "}
                {busyOn
                  ? busyLine(busyOn, frame.name)
                  : `${frame.name}’s executor picks it up next.`}
              </>
            ) : fate === "ended" ? (
              <>Answer sent. {frame.name}’s executor is no longer running.</>
            ) : (
              <>Answer sent. Waiting for {frame.name}’s executor to take it.</>
            )}
          </ReadingPaneOutcome>
        ) : superseded ? (
          // THE PANE'S OWN SENTENCE names nobody, so the line beside it says
          // whose answer was used as well as when.
          <Stack gap={2}>
            {unused && <RefusalStrip onDismiss={() => setUnused(null)}>{unused}</RefusalStrip>}
            {record?.answered_at && (
              <Text as="p" variant="caption">
                {answeredAt(record, now, true)}
              </Text>
            )}
          </Stack>
        ) : hidden ? (
          <Text as="p" variant="body">
            Its lead or an admin can answer it.
          </Text>
        ) : (
          <Stack gap={2}>
            {unused && <RefusalStrip onDismiss={() => setUnused(null)}>{unused}</RefusalStrip>}
            {inWords ? (
              <>
                {/* THE FIELD TAKES THE FOCUS only when a press opened it:
                    stepping onto a question with j or k must not drop the
                    reader into a field. */}
                <Field
                  label="Your answer to the executor"
                  value={text}
                  onChange={setText}
                  onSubmit={sendWords}
                  placeholder="Answer as the harness will read it"
                  disabled={!write.access.can || write.busy}
                  autoFocus={open}
                  actions={
                    <>
                      {options.length > 0 && (
                        <Button
                          size="small"
                          variant="ghost"
                          onClick={() => {
                            write.dismiss();
                            setOpen(false);
                          }}
                          disabled={write.busy}
                        >
                          Cancel
                        </Button>
                      )}
                      <WriteButton
                        write={write}
                        size="small"
                        variant="primary"
                        blocked={blocked}
                        showRefusal={false}
                        onPress={sendWords}
                      >
                        Send answer
                      </WriteButton>
                    </>
                  }
                />
                {/* THE REFUSAL'S SENTENCE, drawn here under the field and by
                    `AnswerChoices` under the options. */}
                <RefusalNote write={write} />
              </>
            ) : (
              <AnswerChoices
                write={write}
                options={options}
                recommended={decision?.recommended}
                pending={write.busy ? chosen : null}
                blocked={notYet}
                onChoose={choose}
                onSomethingElse={() => setOpen(true)}
              />
            )}
            {/* BESIDE THE REFUSAL'S SENTENCE, when the question was
                answered, on the reader's clock, off the executor's record, naming
                who answered only where nothing above it does ([answererNamed]). */}
            {refusedAnswered && record?.answered_at && (
              <Text as="p" variant="caption">
                {answeredAt(record, now, !answererNamed)}
              </Text>
            )}
          </Stack>
        )}
        <RunLink turnId={run.turn_id} hold={hold} />
      </>
    </TokenScope>
  );
}

/**
 * When the answer that was used was recorded, on the READER's clock
 * ([moment]), and by whom where `who`: a value beside the sentence drawn
 * above it, never a second wording of it. Who answered is left to that
 * sentence where it names them, as the engine's sentence for a question
 * already answered does (`caveat.AnsweredAlready`, which leaves the time to
 * the screen), so a person is told it once.
 */
function answeredAt(record: ExecutorRow, now: number, who: boolean): string {
  const by = who && record.answered_by ? ` by @${record.answered_by}` : "";
  return `Answered${by} at ${moment(record.answered_at ?? "", now)}.`;
}

/**
 * What a seat whose executor is busy on another turn is doing, for an answer
 * waiting on it: "The Senior Backend Engineer is busy executing GP-4 and picks
 * this up next."
 */
function busyLine(busy: ExecutorRow, name: string): string {
  const who = busy.role ? `The ${busy.role}` : name;
  const on = busy.work_item?.key ? `executing ${busy.work_item.key}` : "executing another turn";
  return `${who} is busy ${on} and picks this up next.`;
}

function SeatPane({ seat, frame }: { seat: SeatCondition; frame: Frame }) {
  const w = seat.window;
  return (
    <>
      {frame.byline(
        <>
          <strong>{frame.name}</strong>
          {" stopped"}
          {frame.task && <> on {frame.task}</>}
        </>,
      )}
      <ReadingPaneTitle>{budgetLine(w?.period)}</ReadingPaneTitle>
      <ReadingPaneLead>
        <p>
          {w
            ? `${w.used.toLocaleString()} of ${(w.limit ?? 0).toLocaleString()} tokens spent. Resets ${fmtDateTime(w.resets_at)}.`
            : "The engine refuses this seat's charges until its budget window resets."}{" "}
          Raise the limit or reassign the work.
        </p>
      </ReadingPaneLead>
      <Stack gap={2} align="center" wrap>
        <RaiseBudgetButton
          handle={seat.row.handle ?? seat.row.role}
          name={frame.name}
          window={seat.window}
        />
        {frame.address && <ReassignItem item={frame.address} />}
      </Stack>
    </>
  );
}

function ConditionPane({ item, frame, now }: { item: Attention; frame: Frame; now: number }) {
  return (
    <>
      {frame.byline(
        <>
          <strong>{frame.name}</strong>
          {item.at ? ` since ${moment(item.at, now)}` : ""}
        </>,
      )}
      <ReadingPaneTitle>{item.title}</ReadingPaneTitle>
      <ReadingPaneLead>
        <p>{item.detail}</p>
      </ReadingPaneLead>
      <TechnicalDetails text={item.technical} />
      {item.remedy ? (
        <Stack gap={2} align="start">
          <LimitRemedy remedy={item.remedy} />
        </Stack>
      ) : (
        item.path && (
          <Stack gap={2} align="start">
            <ButtonLink variant="secondary" size="small" href={href(item.path, item.query)}>
              Go to it
            </ButtonLink>
          </Stack>
        )
      )}
      <Text as="p" variant="caption">
        Clears on its own when the condition ends.
      </Text>
    </>
  );
}

function NoticePane({
  notice,
  frame,
  index,
  now,
  zone,
  maxSnoozeAhead,
  onSettled,
}: {
  notice: WorkInboxNotice;
  frame: Frame;
  index: OrgIndex;
  now: number;
  zone: string | undefined;
  maxSnoozeAhead: number | undefined;
  onSettled: (label: string | null) => void;
}) {
  const said = noticeText(notice, index);
  const comment = notice.comment_id ?? "";
  const ask = notice.ask;
  // A COMMENT QUOTED WHOLE ABOVE is not listed again below it; one the notice
  // carries only a preview of (cut at the engine's excerpt bound, and marked
  // so) stays in the conversation, where it is read in full.
  const whole = !said.endsWith("…");
  return (
    <>
      {frame.byline(
        <>
          <strong>{frame.name}</strong>
          {frame.task && <> on {frame.task}</>}
        </>,
        noticeNote(notice, (at) => moment(at, now)),
      )}
      <ReadingPaneTitle>{noticeTitle(notice)}</ReadingPaneTitle>
      {said &&
        (comment ? (
          <ReadingPaneQuote>
            <p>{renderTokens(said, "said", "checked")}</p>
          </ReadingPaneQuote>
        ) : (
          <ReadingPaneLead>
            <p>{renderTokens(said, "said", "checked")}</p>
          </ReadingPaneLead>
        ))}
      {ask && (
        <Text as="p" variant="caption">
          {ask.open
            ? "The question it is about is still open."
            : `The question it is about was ${ask.resolved ? "resolved" : "answered"}${ask.answered_at ? ` at ${moment(ask.answered_at, now)}` : ""}.`}
        </Text>
      )}
      <NoticeActions
        notice={notice}
        now={now}
        zone={zone}
        maxSnoozeAhead={maxSnoozeAhead}
        onSettled={() => onSettled(null)}
      />
      <Conversation
        item={frame.address}
        itemKey={frame.itemKey}
        of={comment ? { kind: "comment", comment, whole } : { kind: "item" }}
        label={comment ? "Replies to this comment" : "Conversation on this task"}
        to={frame.name}
        replyTo={comment || undefined}
        placeholder={`Reply to ${frame.name}. @mention to bring someone in.`}
        index={index}
        now={now}
      />
    </>
  );
}
