/**
 * The Inbox's right-hand pane: the open row, in full, and the answer to it.
 *
 * AN ASK IS DRAWN AS THE DECISION IT IS: the question as the heading, who asked
 * and in what role, the context, what the asker recommends and why, the
 * evidence it cites, and each option as a card that SENDS that choice as the
 * answer (`comment_on_work_item{item, answers, choice}`) — the recommended one
 * marked, because the asker said so and most answers are it. "Reply with
 * instructions" turns the composer below into the answer instead, in words.
 * Beside the options the line says what the answer sets off: the asker is
 * woken with it, and posts it to the channel the ask promised (`inform`),
 * which the engine holds the asker to.
 *
 * A parked coding run, a budget-stopped seat and an engine condition each get
 * the same frame with their own body, and a notice gets its reason, what
 * changed and the conversation around it.
 */

import { useCallback, useRef, useState, type ReactNode } from "react";
import { EmptyState, FormField, IconButton, Tag, Textarea } from "@crewlethq/ui";
import {
  ActivityGlyph,
  ArrowRightGlyph,
  CheckGlyph,
  ChevronLeftGlyph,
  CornerDownLeftGlyph,
  FileTextGlyph,
  InboxGlyph,
  InfoGlyph,
  LinkGlyph,
  SquareTerminalGlyph,
  TriangleAlertGlyph,
  WandSparklesGlyph,
} from "@crewlethq/icons/glyphs";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { Mark } from "~/ui/glyph.tsx";
import { href } from "~/app/router.tsx";
import { RefusalNote, WriteButton } from "~/components/WriteButton.tsx";
import { StatusMark } from "~/components/work.tsx";
import { holdLine, ReassignItem, type SeatCondition } from "~/components/DecisionRow.tsx";
import { RaiseBudgetButton } from "~/components/budgetWrite.tsx";
import { useAct } from "~/lib/useAct.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { PERIOD_ADJECTIVE } from "~/lib/budget.ts";
import { renderMarkdown, safeHref } from "~/lib/markdown.ts";
import { fmtDateTime, humanize, relTime } from "~/lib/format.ts";
import { reasonPhrase, reasonWhy } from "~/lib/reasons.ts";
import type { OrgIndex } from "~/lib/seats.ts";
import type { Attention } from "~/lib/attention.ts";
import type {
  SandboxRun,
  WorkAskRow,
  WorkDecisionEvidence,
  WorkInboxNotice,
} from "~/protocol/index.ts";
import { noticeText, rowKey, rowWho, type Who } from "./NoticeList.tsx";
import { firstLine } from "~/lib/format.ts";
import { NoticeActions } from "./SnoozeMenu.tsx";
import { Thread, type ThreadOf } from "./Thread.tsx";
import { Composer, type ComposeMode, type ComposerHandle } from "~/components/Composer.tsx";
import type { InboxRow } from "./model.ts";

interface PaneProps {
  row: InboxRow | null;
  index: OrgIndex;
  viewerHandle: string;
  now: number;
  /** The company's clock, which the snooze presets are read on. */
  zone: string | undefined;
  maxSnoozeAhead: number | undefined;
  /** Back to the list, where the list and the pane are one column. */
  onBack?: () => void;
}

export function DecisionPane(props: PaneProps) {
  if (!props.row) {
    return (
      <div className="inbox-pane inbox-pane-empty">
        <EmptyState
          icon={<InboxGlyph size={28} />}
          title="Nothing to open"
          description="When something reaches you or waits on your decision, it opens here with what you can do about it."
        />
      </div>
    );
  }
  // REMOUNTED PER ROW, so a draft, a pressed option or a refusal drawn for
  // one row never shows under the next.
  return <OpenPane key={props.row.key} {...props} row={props.row} />;
}

/** The ask a row is, when it is one. */
function askOf(row: InboxRow): WorkAskRow | null {
  return row.kind === "decision" && row.subject.kind === "ask" ? row.subject.ask : null;
}

/** An answer in words, to the ask. */
function answerMode(ask: WorkAskRow): ComposeMode {
  return {
    kind: "answer",
    answers: ask.comment,
    ...(ask.decision?.inform ? { inform: ask.decision.inform } : {}),
  };
}

/** A reply in the thread that answers nothing. */
function replyMode(row: InboxRow): ComposeMode {
  const ask = askOf(row);
  if (ask) return { kind: "reply", replyTo: ask.comment };
  if (row.kind === "notice" && row.notice.comment_id) {
    return { kind: "reply", replyTo: row.notice.comment_id };
  }
  return { kind: "reply" };
}

/**
 * How the composer opens: a free-text ask is ANSWERED in words, since words
 * are the only answer it takes; a structured ask opens as a reply, because its
 * answer is one of the cards above and a sentence typed below them is more
 * often a question back than a decision.
 */
function initialMode(row: InboxRow): ComposeMode {
  const ask = askOf(row);
  if (ask && !(ask.decision && ask.decision.options.length > 0)) return answerMode(ask);
  return replyMode(row);
}

/** What the thread below a row is about. */
function threadOf(row: InboxRow): ThreadOf {
  const ask = askOf(row);
  if (ask) return { kind: "ask", comment: ask.comment, options: ask.decision?.options ?? [] };
  if (row.kind === "notice" && row.notice.comment_id) {
    return {
      kind: "comment",
      comment: row.notice.comment_id,
      options: row.notice.ask?.decision?.options ?? [],
    };
  }
  return { kind: "item" };
}

/** The pane's pill: what kind of thing is open. */
function paneKind(row: InboxRow): {
  label: string;
  tone: "warning" | "danger" | "neutral";
  icon: ReactNode;
} {
  switch (row.kind) {
    case "decision":
      switch (row.subject.kind) {
        case "ask":
          return row.subject.ask.decision
            ? { label: "Decision", tone: "warning", icon: <TriangleAlertGlyph size="xs" /> }
            : { label: "Question", tone: "warning", icon: <TriangleAlertGlyph size="xs" /> };
        case "run":
          return { label: "Coding run", tone: "warning", icon: <SquareTerminalGlyph size="xs" /> };
        case "seat":
          return { label: "Seat stopped", tone: "danger", icon: <TriangleAlertGlyph size="xs" /> };
      }
      break;
    case "condition":
      return {
        label: "Condition",
        tone: row.item.severity === "critical" ? "danger" : "warning",
        icon: <TriangleAlertGlyph size="xs" />,
      };
    case "notice":
      return { label: reasonPhrase(row.notice.reason), tone: "neutral", icon: null };
  }
  return { label: "", tone: "neutral", icon: null };
}

function OpenPane({
  row,
  index,
  viewerHandle,
  now,
  zone,
  maxSnoozeAhead,
  onBack,
}: PaneProps & { row: InboxRow }) {
  const itemKey = rowKey(row);
  const who = rowWho(row, index);
  const ask = askOf(row);
  const notices = row.kind === "notice" ? [row.notice] : row.kind === "decision" ? row.notices : [];
  const [title, setTitle] = useState(ask?.title ?? "");
  const onTitle = useCallback((t: string) => setTitle(t), []);
  const composer = useRef<ComposerHandle | null>(null);
  const [mode, setMode] = useState<ComposeMode>(() => initialMode(row));
  const kind = paneKind(row);
  const composes = itemKey !== "" && (ask !== null || row.kind === "notice");

  const instruct = () => {
    if (!ask) return;
    setMode(answerMode(ask));
    requestAnimationFrame(() => composer.current?.focus());
  };

  let body: ReactNode = null;
  switch (row.kind) {
    case "decision":
      switch (row.subject.kind) {
        case "ask":
          body = (
            <AskBody
              ask={row.subject.ask}
              who={who}
              index={index}
              viewerHandle={viewerHandle}
              now={now}
              answering={mode.kind === "answer"}
              onInstruct={instruct}
            />
          );
          break;
        case "run":
          body = <RunBody run={row.subject.run} who={who} now={now} />;
          break;
        case "seat":
          body = <SeatBody seat={row.subject.seat} who={who} now={now} />;
          break;
      }
      break;
    case "condition":
      body = <ConditionBody item={row.item} />;
      break;
    case "notice":
      body = <NoticeBody notice={row.notice} who={who} index={index} now={now} />;
      break;
  }

  return (
    <article className="inbox-pane" aria-label={title || kind.label}>
      <header className="inbox-pane-bar">
        {onBack && (
          <IconButton
            size="sm"
            variant="ghost"
            label="Back to the inbox"
            icon={<ChevronLeftGlyph size="sm" />}
            onClick={onBack}
          />
        )}
        <Tag variant={kind.tone} appearance="soft" leadingIcon={kind.icon ?? undefined}>
          {kind.label}
        </Tag>
        {itemKey && (
          <a className="inbox-pane-key mono" href={href(["work", itemKey])}>
            {itemKey}
          </a>
        )}
        {title && <span className="inbox-pane-title">{title}</span>}
        <span className="spacer" />
        <NoticeActions
          notices={notices}
          itemKey={itemKey}
          now={now}
          zone={zone}
          maxSnoozeAhead={maxSnoozeAhead}
        />
      </header>
      <div className="inbox-pane-scroll">
        <div className="inbox-pane-body">
          {body}
          {itemKey && (row.kind === "notice" || ask) && (
            <Thread item={itemKey} of={threadOf(row)} index={index} now={now} onTitle={onTitle} />
          )}
        </div>
      </div>
      {composes && (
        <div className="inbox-pane-compose">
          <Composer
            ref={composer}
            item={itemKey}
            mode={mode}
            to={who?.name ?? "the thread"}
            index={index}
            onReply={() => setMode(replyMode(row))}
          />
        </div>
      )}
    </article>
  );
}

/** "CTO asked you 12 minutes ago · You are the approver on LEAD-12". */
function Byline({ children, who }: { children: ReactNode; who: Who | null }) {
  return (
    <p className="inbox-pane-meta">
      {who && <SeatAvatar name={who.name} kind={who.kind} size="xs" decorative />}
      {children}
    </p>
  );
}

function AskBody({
  ask,
  who,
  index,
  viewerHandle,
  now,
  answering,
  onInstruct,
}: {
  ask: WorkAskRow;
  who: Who | null;
  index: OrgIndex;
  viewerHandle: string;
  now: number;
  answering: boolean;
  onInstruct: () => void;
}) {
  const decision = ask.decision;
  const asker = who?.name ?? ask.asked_by;
  // A FREE-TEXT ASK'S QUESTION IS ITS FIRST LINE, and the rest is its context;
  // a structured one states its question apart from the body.
  const question = decision?.question || firstLine(ask.body) || ask.title;
  const context = decision ? ask.body.trim() : ask.body.split("\n").slice(1).join("\n").trim();
  const recommended = decision?.options.find((o) => o.id === decision.recommended);
  // "REPORTS TO YOU" is derived from the chart, never stated by the asker.
  const reports = who?.handle
    ? index.byHandle.get(who.handle)?.managers.some((m) => m.handle === viewerHandle)
    : false;
  return (
    <>
      <div className="inbox-pane-head">
        <h1 className="inbox-pane-question">{question}</h1>
        <Byline who={who}>
          <span>
            <strong>{asker}</strong>
            {` asked you ${relTime(ask.asked_at, now)}`}
          </span>
          {decision && (
            <>
              <span aria-hidden="true">·</span>
              <span className="inbox-pane-role">
                <InfoGlyph size="xs" />
                {"You are the "}
                <strong>{decision.role}</strong>
                {` on ${ask.key}`}
              </span>
            </>
          )}
          {reports && (
            <>
              <span aria-hidden="true">·</span>
              <span>{`${asker} reports to you`}</span>
            </>
          )}
        </Byline>
      </div>

      {(context || recommended || (decision?.evidence?.length ?? 0) > 0) && (
        <div className="inbox-pane-card">
          {context && <div className="prose md inbox-pane-context">{renderMarkdown(context)}</div>}
          {recommended && (
            <div className="inbox-recommends">
              <span className="inbox-recommends-who">
                <WandSparklesGlyph size="sm" />
                {`${asker} recommends`}
              </span>
              <p>
                <strong>{recommended.label}.</strong>
                {decision?.rationale ? <span> {decision.rationale}</span> : null}
              </p>
            </div>
          )}
          {decision?.evidence && decision.evidence.length > 0 && (
            <div className="inbox-evidence">
              <span className="inbox-section-label">Evidence</span>
              {decision.evidence.map((ev, i) => (
                <EvidenceChip key={`${ev.kind}:${ev.ref}:${i}`} evidence={ev} />
              ))}
            </div>
          )}
        </div>
      )}

      <section className="inbox-decide" aria-label="Your decision">
        <h3 className="inbox-section-label">Your decision</h3>
        <OptionCards ask={ask} asker={asker} answering={answering} onInstruct={onInstruct} />
        {/* WHAT THE ANSWER SETS OFF where it goes further than the asker: the
            channel the ask promised to post the outcome to, which the engine
            holds the asker to. Without one, the reply card already says the
            asker continues from the answer. */}
        {decision?.inform && (
          <p className="inbox-decide-line">
            {`${asker} is woken with your answer and posts it to #${decision.inform.channel}.`}
          </p>
        )}
      </section>
    </>
  );
}

/**
 * The options as cards, and "Reply with instructions" beside them.
 *
 * ONE WRITE FOR THE ASK, never one per option: an ask takes one answer, so
 * while one choice is being sent the others refuse a press with that reason,
 * and the refusal, if there is one, is drawn once for the ask.
 */
function OptionCards({
  ask,
  asker,
  answering,
  onInstruct,
}: {
  ask: WorkAskRow;
  asker: string;
  answering: boolean;
  onInstruct: () => void;
}) {
  const write = useAct("comment_on_work_item");
  const [pressed, setPressed] = useState<string | null>(null);
  const options = ask.decision?.options ?? [];
  return (
    <>
      <div className="inbox-options" data-count={options.length + 1}>
        {options.map((option) => {
          const mine = write.busy && pressed === option.id;
          const recommended = option.id === ask.decision?.recommended;
          return (
            <WriteButton
              key={option.id}
              write={write}
              className="inbox-option"
              data-recommended={recommended || undefined}
              variant={recommended ? "primary" : "secondary"}
              pressing={mine}
              blocked={write.busy && !mine ? "Your answer to this ask is being sent" : undefined}
              showRefusal={false}
              onPress={() => {
                setPressed(option.id);
                void write.run(
                  { item: ask.key, answers: ask.comment, choice: option.id },
                  { done: `Answered ${ask.key}: ${option.label}` },
                );
              }}
            >
              <span className="inbox-option-body">
                <span className="inbox-option-label">
                  {recommended ? <CheckGlyph size="sm" /> : <ArrowRightGlyph size="sm" />}
                  {option.label}
                </span>
                {(recommended || option.detail) && (
                  <span className="inbox-option-detail">
                    {recommended ? `${asker}’s recommendation` : option.detail}
                  </span>
                )}
              </span>
            </WriteButton>
          );
        })}
        <button
          type="button"
          className="inbox-option inbox-option-reply"
          aria-pressed={answering}
          onClick={onInstruct}
        >
          <span className="inbox-option-label">
            <CornerDownLeftGlyph size="sm" />
            {options.length > 0 ? "Reply with instructions" : "Answer in words"}
          </span>
          <span className="inbox-option-detail">{`${asker} continues from your answer`}</span>
        </button>
      </div>
      <RefusalNote write={write} />
    </>
  );
}

/** One thing a decision cites, linked to where it lives. */
function EvidenceChip({ evidence }: { evidence: WorkDecisionEvidence }) {
  switch (evidence.kind) {
    case "task":
      return <TaskEvidence id={evidence.ref} label={evidence.label} />;
    case "page":
      return (
        <a className="inbox-chip" href={href(["knowledge", "pages", evidence.ref])}>
          <FileTextGlyph size="xs" />
          {evidence.label || "Page"}
        </a>
      );
    case "turn":
      return (
        <a className="inbox-chip" href={href(["live", "turns", evidence.ref])}>
          <ActivityGlyph size="xs" />
          {evidence.label || "Turn trace"}
        </a>
      );
    case "run":
      return (
        <a className="inbox-chip" href={href(["live", "runs", evidence.ref])}>
          <SquareTerminalGlyph size="xs" />
          {evidence.label || "Coding run"}
        </a>
      );
    case "url": {
      const url = safeHref(evidence.ref);
      if (!url) return <span className="inbox-chip">{evidence.label || evidence.ref}</span>;
      return (
        <a className="inbox-chip" href={url} target="_blank" rel="noreferrer noopener">
          <LinkGlyph size="xs" />
          {evidence.label || new URL(url, location.href).host}
        </a>
      );
    }
  }
  return null;
}

/**
 * A cited task, with where it stands: its key and its status as the board
 * draws them, read now rather than as the asker saw it.
 */
function TaskEvidence({ id, label }: { id: string; label?: string }) {
  const read = useQuery("work_item", { id });
  const task = read.data?.task;
  return (
    <a className="inbox-chip" href={href(["work", task?.key ?? id])}>
      {task && <StatusMark status={task.status} />}
      <span className="mono">{task?.key ?? "Task"}</span>
      <span className="truncate">{label || task?.title || ""}</span>
    </a>
  );
}

function RunBody({ run, who, now }: { run: SandboxRun; who: Who | null; now: number }) {
  const write = useAct("answer_run");
  const [answer, setAnswer] = useState("");
  const name = who?.name ?? run.agent_handle;
  const hold = holdLine(run, now);
  const send = async () => {
    const text = answer.trim();
    if (!text) return;
    const result = await write.run(
      { turn_id: run.turn_id, answer: text },
      { done: `Answered ${name}'s coding run` },
    );
    if (result && (result.kind === "applied" || result.kind === "pending")) setAnswer("");
  };
  return (
    <>
      <div className="inbox-pane-head">
        <h1 className="inbox-pane-question">{`${name}’s coding run is parked on a question`}</h1>
        <Byline who={who}>
          <span>{`Parked ${relTime(run.paused_at || run.updated_at, now)}`}</span>
          {hold && (
            <>
              <span aria-hidden="true">·</span>
              <span>{hold}</span>
            </>
          )}
        </Byline>
      </div>
      <div className="inbox-pane-card">
        <blockquote className="answer-run-question">
          {run.question || "The run paused on a clarification and cannot continue."}
        </blockquote>
        <a className="t-link" href={href(["live", "runs", run.turn_id])}>
          Open the run →
        </a>
      </div>
      <section className="inbox-decide" aria-label="Your answer">
        <FormField
          label="Your answer"
          htmlFor={`inbox-run-${run.turn_id}`}
          helper="The run resumes with this as the answer to its question, as the coding agent reads it."
        >
          <Textarea
            id={`inbox-run-${run.turn_id}`}
            rows={4}
            value={answer}
            onChange={(event) => setAnswer(event.target.value)}
          />
        </FormField>
        <div className="row gap-2">
          <WriteButton
            write={write}
            variant="primary"
            size="small"
            leadingIcon={<CornerDownLeftGlyph size="sm" />}
            blocked={answer.trim() ? undefined : "Write the answer first."}
            onPress={() => void send()}
          >
            Send answer
          </WriteButton>
        </div>
      </section>
    </>
  );
}

function SeatBody({ seat, who, now }: { seat: SeatCondition; who: Who | null; now: number }) {
  const name = who?.name ?? seat.row.role;
  const item = seat.row.turn?.work_item ?? seat.row.live_call?.work_item ?? null;
  const w = seat.window;
  return (
    <>
      <div className="inbox-pane-head">
        <h1 className="inbox-pane-question">
          {`${name} stopped — ${w ? `${PERIOD_ADJECTIVE[w.period]} ` : ""}token budget exhausted`}
        </h1>
        <Byline who={who}>
          <span>{item ? `Stopped mid-turn on ${item.key}` : "Stopped between turns"}</span>
          {seat.at && (
            <>
              <span aria-hidden="true">·</span>
              <span>{`last refused ${relTime(seat.at, now)}`}</span>
            </>
          )}
        </Byline>
      </div>
      <div className="inbox-pane-card">
        <p>
          {w
            ? `${w.used.toLocaleString()} of ${(w.limit ?? 0).toLocaleString()} tokens are spent in ${w.window}, so the engine turns this seat's charges away until ${fmtDateTime(w.resets_at)}.`
            : "The engine turns this seat's charges away until its budget window resets."}
        </p>
        <p className="t-caption">
          Two ways out: raise the ceiling, or hand the work to somebody with room.
        </p>
        <div className="row gap-2">
          <RaiseBudgetButton
            handle={seat.row.handle ?? seat.row.role}
            name={name}
            window={seat.window}
          />
          {item && <ReassignItem item={item.key} />}
        </div>
      </div>
    </>
  );
}

function ConditionBody({ item }: { item: Attention }) {
  return (
    <>
      <div className="inbox-pane-head">
        <h1 className="inbox-pane-question">{item.title}</h1>
        {item.at && <p className="inbox-pane-meta">{`Since ${fmtDateTime(item.at)}`}</p>}
      </div>
      <div className="inbox-pane-card">
        <p className="row gap-2">
          <span className="attention-icon" data-severity={item.severity}>
            <Mark name={item.icon} size="sm" />
          </span>
          <span>{item.detail}</span>
        </p>
        {item.path && (
          <a className="t-link row gap-1" href={href(item.path, item.query)}>
            Go to it <ArrowRightGlyph size="sm" />
          </a>
        )}
        <p className="t-caption">
          Computed from the live state — nothing here is stored or marked. It leaves your inbox when
          the condition clears.
        </p>
      </div>
    </>
  );
}

function NoticeBody({
  notice,
  who,
  index,
  now,
}: {
  notice: WorkInboxNotice;
  who: Who | null;
  index: OrgIndex;
  now: number;
}) {
  // THE LIST'S OWN SENTENCE, so the pane heads with what the row said.
  const said = noticeText(notice, index);
  const ask = notice.ask;
  return (
    <>
      <div className="inbox-pane-head">
        <h1 className="inbox-pane-question">{said ? firstLine(said) : humanize(notice.kind)}</h1>
        <Byline who={who}>
          <span>
            <strong>{who?.name ?? "An operator"}</strong>
            {` · ${relTime(notice.at, now)}`}
          </span>
          <span aria-hidden="true">·</span>
          <span className="inbox-pane-role">
            <InfoGlyph size="xs" />
            {`You see this because ${reasonWhy(notice.reason)}`}
          </span>
        </Byline>
      </div>
      {(said.includes("\n") || notice.fallback || ask) && (
        <div className="inbox-pane-card">
          {said.includes("\n") && <p className="inbox-pane-context">{said}</p>}
          {notice.fallback && (
            <p className="t-caption">
              You were the fallback: nobody better was found for it, so it came to you.
            </p>
          )}
          {ask && (
            <p className="t-caption">
              {ask.open
                ? "The question it is about is still open."
                : `The question it is about was ${ask.resolved ? "resolved" : "answered"}${ask.answered_at ? ` ${relTime(ask.answered_at, now)}` : ""}.`}
            </p>
          )}
        </div>
      )}
    </>
  );
}
