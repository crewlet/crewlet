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
 * WHAT WAS DONE STAYS ON SCREEN FOR A MOMENT: an answer, a run's answer, a read
 * or a snooze tells the screen (`onSettled`), which shows the outcome here and
 * then opens the next row.
 */

import { useEffect, useRef, useState, type ReactNode } from "react";
import {
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
  Text,
  safeHref,
  peekClickHandler,
  type StepperStep,
} from "@crewlethq/ui";
import {
  ActivityGlyph,
  ChevronLeftGlyph,
  CircleCheckGlyph,
  FileTextGlyph,
  LinkGlyph,
  SquareKanbanGlyph,
  SquareTerminalGlyph,
} from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { peekHref, usePeekControls } from "~/app/frame/peekAddress.ts";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { holdLine, ReassignItem } from "~/components/DecisionRow.tsx";
import type { SeatCondition } from "~/lib/decisions.ts";
import { RaiseBudgetButton } from "~/components/budgetWrite.tsx";
import { AnswerAsk, type AskAnswer } from "~/components/writes.tsx";
import { TechnicalDetails } from "~/components/common.tsx";
import { Composer } from "~/components/Composer.tsx";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { RUNS_POLL_MS, answerFate, type GivenAnswer } from "~/lib/runs.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import { nameOfIn, type OrgIndex } from "~/lib/seats.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useAgents } from "~/lib/store-hooks.ts";
import type { Attention } from "~/lib/attention.ts";
import { describeFollowUp, detailItem, itemAddress } from "~/lib/work.ts";
import type {
  AnsweredDecision,
  SandboxRun,
  WorkAskRow,
  WorkDecisionEvidence,
  WorkInboxNotice,
} from "~/protocol/index.ts";
import {
  answerWords,
  askContext,
  askQuestion,
  followThrough,
  budgetLine,
  firstSentence,
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
  // REMOUNTED PER ROW, so a draft, a pressed option or an outcome drawn for one
  // row never shows under the next.
  return (
    <div className="inbox-pane">
      <OpenPane key={props.row.key} {...props} row={props.row} />
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
              onSettled={onSettled}
            />
          );
        case "run":
          return <RunPane run={row.subject.run} frame={frame} now={now} onSettled={onSettled} />;
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

/** "Answered Exact search", where the controls that answered it were. */
function answeredWith(answer: AskAnswer): ReactNode {
  return (
    <>
      Answered <strong>{answer.kind === "choice" ? answer.label : `“${answer.text}”`}</strong>
    </>
  );
}

function AskPane({
  ask,
  notices,
  frame,
  index,
  now,
  onSettled,
}: {
  ask: WorkAskRow;
  notices: readonly WorkInboxNotice[];
  frame: Frame;
  index: OrgIndex;
  now: number;
  onSettled: (label: string | null, answer?: AskAnswer) => void;
}) {
  const decision = ask.decision;
  const options = decision?.options ?? [];
  const [open, setOpen] = useState(false);
  const [answer, setAnswer] = useState<AskAnswer | null>(null);
  const mark = useAct("mark_inbox");
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

  const answered = (given: AskAnswer) => {
    setAnswer(given);
    const unread = notices.filter((n) => !n.read).map((n) => n.record_id);
    if (unread.length > 0) {
      void mark.run({ read: unread }, { done: "Marked read", quiet: true });
    }
    onSettled("Answered", given);
  };

  return (
    <ReadingPane>
      {frame.byline(
        <>
          <strong>{frame.name}</strong>
          {` ${(decision && ASKED[decision.role]) || "asks you"}`}
          {frame.task && <> on {frame.task}</>}
        </>,
      )}
      <ReadingPaneTitle>{askQuestion(ask)}</ReadingPaneTitle>
      {context && <ReadingPaneLead>{renderMarkdown(context)}</ReadingPaneLead>}
      {reasoning ? (
        <ReadingPaneReasoning
          title={
            <>
              {`${frame.name} recommends `}
              <strong>{reasoning.label}</strong>
            </>
          }
          footer={chips}
        >
          <p>{reasoning.text}</p>
        </ReadingPaneReasoning>
      ) : (
        chips
      )}
      {answer ? (
        <ReadingPaneOutcome>{answeredWith(answer)}</ReadingPaneOutcome>
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
          onAnswered={answered}
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
    </ReadingPane>
  );
}

/** What each role was asked for, as an answered ask's byline says it. */
const WAS_ASKED: Readonly<Record<string, string>> = {
  approver: "asked for your approval",
  contributor: "asked for your input",
};

/**
 * An answered ask, read in full: the page it was answered on with the buttons
 * replaced by the answer, then what the asker has done with it since, then
 * the conversation.
 *
 * WHO ANSWERED IS SAID, because it is not always the reader: a colleague may
 * answer a question put to you, and the line under the answer says so and
 * where to disagree.
 *
 * WHAT HAPPENED NEXT IS FOUR STEPS (asked, answered, picked up, acted on),
 * the last two read from the asker's seat as it is now and from its first
 * record on the task since (`followed_up`). A person who asked has no seat
 * state to read, so their steps stop at the answer.
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
  const agents = useAgents();
  const ask = answered.ask;
  const words = answerWords(answered, viewer.owner, index);
  const seat = agents.find((a) => a.handle === ask.asked_by);
  const follow = followThrough(answered, frame.who?.kind === "agent" ? seat : undefined);
  const said =
    words.kind === "resolved" ? null : words.kind === "choice" ? words.said : `“${words.said}”`;
  return (
    <ReadingPane>
      {frame.byline(
        <>
          <strong>{frame.name}</strong>
          {` ${(ask.decision && WAS_ASKED[ask.decision.role]) || "asked you"}`}
          {frame.task && <> on {frame.task}</>}
        </>,
      )}
      <ReadingPaneTitle>{askQuestion(ask)}</ReadingPaneTitle>
      <ReadingPaneOutcome>
        {`${words.who} ${words.verb}`}
        {said && (
          <>
            {" "}
            <strong>{said}</strong>
          </>
        )}
        {words.recommended && " (Recommended)"}
      </ReadingPaneOutcome>
      {words.who !== "You" && words.kind !== "resolved" && (
        <Text as="p" variant="caption">
          {`${words.who} answered for you at ${moment(answered.answer.at, now)}. Reply below if you see it differently.`}
        </Text>
      )}
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
    </ReadingPane>
  );
}

/** The four steps from the question to the work, as the pane draws them. */
function followSteps(
  answered: AnsweredDecision,
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
          href={href(["live", "runs", evidence.ref])}
          leadingIcon={<SquareTerminalGlyph size="sm" />}
        >
          {evidence.label || "Coding run"}
        </ButtonLink>
      );
    case "url": {
      // A LINK THE ASKER WROTE is followed only through `safeHref`, and one it
      // refuses is shown as words rather than dropped.
      const url = safeHref(evidence.ref);
      if (!url) return <Text variant="caption">{evidence.label || evidence.ref}</Text>;
      return (
        <ButtonLink {...look} href={url} external leadingIcon={<LinkGlyph size="sm" />}>
          {evidence.label || new URL(url, location.href).host}
        </ButtonLink>
      );
    }
  }
  return null;
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
      {task ? `${task.key} ${label || task.title}` : label || "Task"}
    </ButtonLink>
  );
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

function RunPane({
  run,
  frame,
  now,
  onSettled,
}: {
  run: SandboxRun;
  frame: Frame;
  now: number;
  onSettled: (label: string | null, answer?: AskAnswer, hold?: boolean) => void;
}) {
  const write = useAct("answer_run");
  const [text, setText] = useState("");
  // THE ANSWER THE ENGINE ACCEPTED, and what it said back about it: the
  // question it answers and the answer's own id. `pending` is all the press
  // can say; the run's own record says the rest (see `answerFate`).
  const [given, setGiven] = useState<GivenAnswer | null>(null);
  // WHY THE LAST ANSWER WAS NOT USED, said until the person answers again.
  const [unused, setUnused] = useState<string | null>(null);
  // ANSWERED BY SOMEBODY ELSE FIRST: nothing is left to answer here.
  const [superseded, setSuperseded] = useState(false);
  const runs = useQuery("sandbox_runs", undefined, { pollMs: RUNS_POLL_MS });
  const record = runs.data?.runs.find((r) => r.turn_id === run.turn_id);
  const fate = given ? answerFate(given, record, runs.data !== undefined) : null;
  const question = run.question ?? "";
  // A QUESTION THIS READER CAN'T SEE IS NOT ASKED OF THEM: the answer form
  // asked for an answer to a question it never showed.
  const hidden = questionWithheld(run);
  const heading =
    firstSentence(question) ||
    (hidden ? "This run is waiting on a question you can’t see." : "The run paused on a question");
  const hold = holdLine(run, now);
  const blocked = text.trim() ? undefined : "Write the answer first.";
  // ONCE PER ANSWER: telling the screen redraws it, and the screen's
  // callback is a new one on every draw, so the fate is said once.
  const told = useRef<string | null>(null);

  useEffect(() => {
    if (!given || fate === null || fate === "sent") return;
    if (told.current === given.answerId + fate) return;
    told.current = given.answerId + fate;
    const answer: AskAnswer = { kind: "reply", text: given.text };
    if (fate === "recorded") {
      // RECORDED ON THE RUN, its resume waiting for the seat: answered, and
      // kept open, saying what the seat is doing first.
      onSettled("Answered", answer, true);
      return;
    }
    if (fate === "taken") {
      onSettled("Answered", answer);
      return;
    }
    // NOT USED, so the person is told so and why, never left looking at
    // "Sent" for an answer the run will not read.
    if (fate === "superseded") {
      const by = record?.answered_by ? ` by ${record.answered_by}` : "";
      setUnused(`This question was already answered${by}, so your answer was not used.`);
      setSuperseded(true);
    } else {
      setUnused(
        fate === "declined"
          ? `${frame.name}’s run could not use your answer and is waiting on its question again. Answer it again.`
          : `${frame.name}’s run moved on to a new question before your answer reached it. Answer the question above.`,
      );
    }
    setGiven(null);
    onSettled(null, undefined, true);
  }, [fate, given, frame.name, onSettled, record?.answered_by]);

  // THE SEAT'S OTHER JOB, while this answer waits for it: another of the
  // seat's coding runs holding the seat.
  const busyOn = runs.data?.runs.find(
    (r) =>
      r.agent_handle === run.agent_handle &&
      r.turn_id !== run.turn_id &&
      (r.status === "launching" || r.status === "running" || r.status === "resumed"),
  );

  const send = async () => {
    if (!pressable(write, blocked)) return;
    const answer = text.trim();
    const result = await write.run(
      { turn_id: run.turn_id, answer },
      { done: `Sent your answer to ${frame.name}’s coding run` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) {
      const receipt = (result.receipt ?? {}) as { launch_id?: unknown; answer_id?: unknown };
      setUnused(null);
      setGiven({
        text: answer,
        launchId: typeof receipt.launch_id === "string" ? receipt.launch_id : (run.launch_id ?? ""),
        answerId: typeof receipt.answer_id === "string" ? receipt.answer_id : "",
      });
      // SENT, NOT ANSWERED: the row stays open, marked as sent, until the
      // run's record says the answer was taken or why it was not.
      onSettled("Sent", { kind: "reply", text: answer }, true);
    }
  };
  return (
    <ReadingPane>
      {frame.byline(
        <>
          <strong>{frame.name}</strong>
          {"’s coding run"}
          {frame.task && <> on {frame.task}</>}
        </>,
      )}
      <ReadingPaneTitle>{heading}</ReadingPaneTitle>
      {/* THE RUN'S OWN WORDS, verbatim, where the heading is only their start. */}
      {question.trim() !== heading && <ReadingPaneQuote verbatim>{question}</ReadingPaneQuote>}
      {given !== null ? (
        <ReadingPaneOutcome>
          {fate === "taken" ? (
            <>
              Answered <strong>{`“${given.text}”`}</strong>
            </>
          ) : fate === "recorded" ? (
            <>
              Answer recorded: <strong>{`“${given.text}”`}</strong>.{" "}
              {busyOn ? busyLine(busyOn, frame.name) : `${frame.name}’s run picks it up next.`}
            </>
          ) : (
            <>
              Sent <strong>{`“${given.text}”`}</strong>. Waiting for {frame.name}’s run to take it.
            </>
          )}
        </ReadingPaneOutcome>
      ) : superseded ? (
        unused && <RefusalStrip onDismiss={() => setUnused(null)}>{unused}</RefusalStrip>
      ) : hidden ? (
        <Text as="p" variant="body">
          Its lead or an admin can answer it.
        </Text>
      ) : (
        <Stack gap={2}>
          {unused && <RefusalStrip onDismiss={() => setUnused(null)}>{unused}</RefusalStrip>}
          <Field
            label="Your answer to the run"
            value={text}
            onChange={setText}
            onSubmit={() => void send()}
            placeholder="Answer as the coding agent will read it"
            disabled={!write.access.can || write.busy}
            actions={
              <WriteButton
                write={write}
                size="small"
                variant="primary"
                blocked={blocked}
                showRefusal={false}
                onPress={() => void send()}
              >
                Send answer
              </WriteButton>
            }
          />
          <RefusalNote write={write} />
        </Stack>
      )}
      <Text as="p" variant="caption">
        {hold ? `${hold}. ` : ""}
        <Link href={href(["live", "runs", run.turn_id])} size="inherit">
          Open the run
        </Link>
      </Text>
    </ReadingPane>
  );
}

/**
 * What a seat busy on another coding run is doing, for an answer waiting on it:
 * "The Senior Backend Engineer is busy on GP-4’s run and picks this up next."
 */
function busyLine(busy: SandboxRun, name: string): string {
  const who = busy.role ? `The ${busy.role}` : name;
  const on = busy.work_item?.key ? `on ${busy.work_item.key}’s run` : "on another coding run";
  return `${who} is busy ${on} and picks this up next.`;
}

function SeatPane({ seat, frame }: { seat: SeatCondition; frame: Frame }) {
  const w = seat.window;
  return (
    <ReadingPane>
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
    </ReadingPane>
  );
}

function ConditionPane({ item, frame, now }: { item: Attention; frame: Frame; now: number }) {
  return (
    <ReadingPane>
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
      {item.path && (
        <Stack gap={2} align="start">
          <ButtonLink variant="secondary" size="small" href={href(item.path, item.query)}>
            Go to it
          </ButtonLink>
        </Stack>
      )}
      <Text as="p" variant="caption">
        Clears on its own when the condition ends.
      </Text>
    </ReadingPane>
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
    <ReadingPane>
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
            <p>{said}</p>
          </ReadingPaneQuote>
        ) : (
          <ReadingPaneLead>
            <p>{said}</p>
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
    </ReadingPane>
  );
}
