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

import { useState, type ReactNode } from "react";
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
  Stack,
  Text,
  safeHref,
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
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { RefusalNote, WriteButton, pressable } from "~/components/WriteButton.tsx";
import { holdLine, ReassignItem } from "~/components/DecisionRow.tsx";
import type { SeatCondition } from "~/lib/decisions.ts";
import { RaiseBudgetButton } from "~/components/budgetWrite.tsx";
import { AnswerAsk, type AskAnswer } from "~/components/writes.tsx";
import { Composer } from "~/components/Composer.tsx";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { renderMarkdown } from "~/lib/markdown.ts";
import type { OrgIndex } from "~/lib/seats.ts";
import type { Attention } from "~/lib/attention.ts";
import { detailItem, itemAddress } from "~/lib/work.ts";
import { changeLive } from "~/lib/code.ts";
import { useCodeQuery } from "~/lib/useCodeQuery.ts";
import { useLatestChecks } from "~/lib/useLatestChecks.ts";
import { useViewer } from "~/lib/viewer.ts";
import { ChangeStateTag } from "../code/Changes.tsx";
import type {
  CodeChangeState,
  CodeInboxNotice,
  SandboxRun,
  WorkAskRow,
  WorkDecisionEvidence,
  WorkInboxNotice,
} from "~/protocol/index.ts";
import {
  askContext,
  askQuestion,
  budgetLine,
  changeCard,
  changeGate,
  changeWords,
  findingsLine,
  measuredLine,
  firstSentence,
  moment,
  noticeNote,
  noticeText,
  noticeTitle,
  rowItem,
  rowKey,
  rowName,
  rowWho,
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
   * stays in the list as done; null for a notice, which leaves its group.
   */
  onSettled: (label: string | null) => void;
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
    case "change":
      return (
        <ChangeNoticePane
          notice={row.notice}
          frame={frame}
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
 * What an open change card's merge gate says ([changeGate]): what the head
 * revision's required checks measured, its change classes, and the human
 * paths that asked for this reader — each said only where it is so, so a
 * change into an ungated branch draws nothing here. Two questions, of the
 * change (its readiness, judged for this reader, and its revisions' facts)
 * and of its checks at each one's LATEST attempt, every page of them
 * (`lib/useLatestChecks.ts`): what a check stands at is the engine's to
 * answer, never a card's to rebuild from a page of the attempts' history.
 */
function GateFacts({ repo, number }: { repo: string; number: number }) {
  const { handle } = useViewer();
  const change = useCodeQuery("code_change", { name: repo, change: number });
  const checks = useLatestChecks({ repo, change: number });
  if (!change.data || !checks.rows) return null;
  const gate = changeGate(change.data, checks.rows, handle);
  if (gate.measured === null && !gate.classes && !gate.humanPaths) return null;
  return (
    <ReadingPaneSection label="Its gate">
      <Stack gap={2}>
        {gate.humanPaths && <Text>{gate.humanPaths}</Text>}
        {gate.measured !== null && (
          <ul aria-label="What its checks measured" className="col gap-1">
            {gate.measured.length === 0 ? (
              <li>
                <Text variant="caption">No required check has run on its head yet</Text>
              </li>
            ) : (
              gate.measured.map((m) => (
                <li key={m.name}>
                  <Text variant="caption">{measuredLine(m)}</Text>
                </li>
              ))
            )}
          </ul>
        )}
        {gate.classes && <Text variant="caption">{gate.classes}</Text>}
      </Stack>
    </ReadingPaneSection>
  );
}

/**
 * A notice the code host routed, drawn as an INTENT-FIRST card ([changeCard]):
 * the change it is about and why it reached this reader, then what the change
 * is FOR — the work item it implements and the description its author wrote —
 * before what happened to it, and then where it stands: its open agent
 * findings by weight and the engine's one sentence of where its merge stands.
 * The diff is one press away, on the change's own page, and the reader's own
 * marks close the card (`code_mark_inbox`).
 *
 * THE LIST ASKS NOTHING: every word above is the notice's, read with the
 * inbox page, so a list of cards costs no read per card. What the change's
 * merge gate says — its checks, its change classes, the human paths that
 * asked for this reader — is read for the ONE card open, by [GateFacts], and
 * only for a live change, which still has a gate to meet. Reviewing the
 * change is the change's own page's.
 */
function ChangeNoticePane({
  notice,
  frame,
  now,
  zone,
  maxSnoozeAhead,
  onSettled,
}: {
  notice: CodeInboxNotice;
  frame: Frame;
  now: number;
  zone: string | undefined;
  maxSnoozeAhead: number | undefined;
  onSettled: (label: string | null) => void;
}) {
  const words = changeWords(notice);
  const card = changeCard(notice);
  const state = notice.change?.state;
  const findings = findingsLine(card.findings);
  return (
    <ReadingPane>
      {frame.byline(
        <>
          <strong>{notice.change_ref}</strong> in {notice.repo}
        </>,
        card.why,
      )}
      <ReadingPaneTitle>{words.text}</ReadingPaneTitle>
      {frame.task && (
        <Text as="p" variant="caption">
          For {frame.task}
        </Text>
      )}
      {card.intent && (
        <ReadingPaneLead>
          <p>{card.intent}</p>
        </ReadingPaneLead>
      )}
      {words.excerpt && (
        <ReadingPaneQuote>
          <p>{words.excerpt}</p>
        </ReadingPaneQuote>
      )}
      {state && (
        <ReadingPaneSection label="Where it stands">
          <Stack gap={2}>
            {card.readiness ? (
              <Text>{card.readiness}</Text>
            ) : (
              <span>
                <ChangeStateTag state={state} />
              </span>
            )}
            <Text variant="caption">
              {findings ? `Open agent findings: ${findings}` : "No agent finding stands open"}
            </Text>
          </Stack>
        </ReadingPaneSection>
      )}
      {card.named && state && changeLive(state as CodeChangeState) && (
        <GateFacts repo={card.named.repo} number={card.named.number} />
      )}
      {card.diff && (
        <span>
          <ButtonLink variant="secondary" size="small" href={card.diff}>
            Open the diff
          </ButtonLink>
        </span>
      )}
      <NoticeActions
        notice={notice}
        source="code"
        now={now}
        zone={zone}
        maxSnoozeAhead={maxSnoozeAhead}
        onSettled={() => onSettled(null)}
      />
    </ReadingPane>
  );
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
  onSettled: (label: string | null) => void;
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
    onSettled("Answered");
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
          href={href(["knowledge", "pages", evidence.ref])}
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
      onClick={rowPeekHandler(() => peek.open(ref))}
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
  onSettled: (label: string | null) => void;
}) {
  const write = useAct("answer_run");
  const [text, setText] = useState("");
  const [answered, setAnswered] = useState<string | null>(null);
  const question = run.question ?? "";
  // A QUESTION THIS READER CAN'T SEE IS NOT ASKED OF THEM: the answer form
  // asked for an answer to a question it never showed.
  const hidden = questionWithheld(run);
  const heading =
    firstSentence(question) ||
    (hidden ? "This run is waiting on a question you can’t see." : "The run paused on a question");
  const hold = holdLine(run, now);
  const blocked = text.trim() ? undefined : "Write the answer first.";
  const send = async () => {
    if (!pressable(write, blocked)) return;
    const answer = text.trim();
    const result = await write.run(
      { turn_id: run.turn_id, answer },
      { done: `Answered ${frame.name}’s coding run` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) {
      setAnswered(answer);
      onSettled("Answered");
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
      {answered !== null ? (
        <ReadingPaneOutcome>
          Answered <strong>{`“${answered}”`}</strong>
        </ReadingPaneOutcome>
      ) : hidden ? (
        <Text as="p" variant="body">
          Its lead or an admin can answer it.
        </Text>
      ) : (
        <Stack gap={2}>
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
        source="tracker"
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
