/**
 * One thing waiting on a person's decision, as a row they can answer in place.
 *
 * THREE SHAPES, ONE ROW, because they are one question to the reader — "what
 * does somebody need from me" — and a screen that drew them three ways would
 * make a person learn three rows to act on one list:
 *
 *  - an ASK on a work item: who asked, the question, what they recommend,
 *    and — for a structured ask — each option as a button that sends the
 *    choice as the answer (`comment_on_work_item{answers, choice}`), or for
 *    a free-text ask a Reply that sends words (`{answers, body}`), with the
 *    line saying what the answer sets off: the asker is woken with it, and
 *    posts it to the channel it promised where the ask carries an `inform`;
 *  - a CODING RUN parked on a question: whose, the question's title quoted,
 *    and how long its box is still held; answered by its turn (`answer_run`),
 *    with one of its options where it asked to choose;
 *  - a SEAT the engine STOPPED for its token budget: which window is spent,
 *    the item it was on, and the two ways out — raise the ceiling, or hand
 *    the item to somebody else.
 *
 * Shared by Home and My work's Asked of me, which is why it lives here: two
 * copies of the rule for which button is primary would disagree the first
 * time it moved.
 *
 * # Whose decision it is
 *
 * Usually the reader's, and the row speaks to them: "you are the approver",
 * "reports to you", "woken with your answer". My work is also read on
 * somebody ELSE's day, and there the same row is about THAT person — so the
 * caller names whose decisions these are ([Decider]), and a decider who is not
 * the reader turns every line into the third person. The controls are the
 * caller's to hold (`lib/useWriteAccess.ts`' `HoldWrites`): an ask put to Rui
 * is Rui's to answer, which the engine enforces, and the row says so rather
 * than offering a button the engine would refuse.
 */

import { firstLine } from "~/lib/format.ts";
import { useMemo, useState, type ReactNode } from "react";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import type { AgentAvatar } from "~/lib/avatar.ts";
import { href } from "~/app/router.tsx";
import { RaiseBudgetButton } from "./budgetWrite.tsx";
import { CheckGlyph } from "@crewlethq/icons/glyphs";
import { AnswerAsk, AnswerRunButton, AssignButton, type AskAnswer } from "./writes.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import type { DecisionSubject, SeatCondition } from "~/lib/decisions.ts";
import { useViewer } from "~/lib/viewer.ts";
import { indexOrg, stateWord, type OrgIndex } from "~/lib/seats.ts";
import { PERIOD_ADJECTIVE } from "~/lib/budget.ts";
import { relTime } from "~/lib/format.ts";
import type { AgentRow, BudgetWindow, SandboxRun, WorkAskRow } from "~/protocol/index.ts";
import { itemAddress, itemPath, type ItemRef } from "~/lib/work.ts";
import { QUESTION_WITHHELD, questionWithheld } from "~/lib/access.ts";
import { RunQuestionLine } from "./RunQuestion.tsx";

/**
 * Where Home's "Review" and "Open inbox" take a reader: the Inbox, whose list
 * opens with Needs you, every decision this one counted. There is no narrower
 * view to land on: the Inbox has no filter bar, and a decision is never
 * listed below an update.
 */
export function decisionsHref(): string {
  return href(["inbox"]);
}

/**
 * Which budget-stopped seats are THIS reader's decision, and which are only a
 * condition they can see.
 *
 * A STOPPED SEAT HAS TWO WAYS OUT, and a seat is waiting on a reader exactly
 * when they can take one of them:
 *
 *  - RAISE THE CEILING — a change to the company document, which `/config`
 *    takes from an admin, and a SEAT's ceiling also from whoever leads it
 *    (`lib/leadScope.ts`) — so the reader's access or the units they lead
 *    decide it, never a binding of their own. The company's ceiling, which
 *    stops a seat whose own window has room, is Admin's alone. Where another system manages the document (ADR-0030),
 *    only its writers raise anything: `configWrite` is `mayChangeConfig`
 *    (`lib/useWriteAccess.ts`), the same answer the raise control is disabled
 *    by, and the caller passes no leads — so a seat is never counted as a
 *    decision whose control then refuses the reader;
 *  - HAND THE ITEM ON — `update_work_item` as the reader, which needs the
 *    engine to serve that write for them (`viewer.acts`) AND an item the seat
 *    was on: a seat stopped between turns has nothing to hand on.
 *
 * Everything else a reader sees is somebody else's decision, so it is counted
 * with the conditions that "need a look", never as one waiting on them: a
 * sentence telling a person a decision waits that they cannot make is the one
 * thing this screen must not say.
 */
export function seatDecisionsFor<T extends { row: AgentRow; window?: BudgetWindow }>(
  conditions: readonly T[],
  reader: {
    /** Whether the reader may change the company's configuration: `mayChangeConfig`. */
    configWrite: boolean;
    acts: readonly string[];
  },
  /**
   * The handles of the seats the reader leads ([LeadScope.seats]) — EMPTY
   * on a managed document, where a lead's raise is refused like anybody's.
   */
  leads: { has(handle: string): boolean } = new Set(),
): { mine: T[]; others: T[] } {
  const mine: T[] = [];
  const others: T[] = [];
  for (const c of conditions) {
    const raises = reader.configWrite || (c.window !== undefined && leads.has(c.row.handle ?? ""));
    const item = c.row.turn?.work_item ?? c.row.live_call?.work_item ?? null;
    const reassign = item !== null && reader.acts.includes("update_work_item");
    (raises || reassign ? mine : others).push(c);
  }
  return { mine, others };
}

/** A stable key for one subject, whichever list draws it. */
export function subjectKey(s: DecisionSubject): string {
  switch (s.kind) {
    case "ask":
      return `ask:${s.ask.comment}`;
    case "run":
      return `run:${s.run.turn_id}`;
    case "seat":
      return `seat:${s.seat.row.id}`;
  }
}

/**
 * Whose decisions a row is about.
 *
 * `name` is set exactly when they are NOT the reader: the row is then written
 * in the third person under that name. Absent, the decider is the reader and
 * the row says "you".
 */
export interface Decider {
  handle: string;
  name?: string;
}

/** A decision that is not an ask: a parked run or a stopped seat, one row each. */
export type RowSubject = Exclude<DecisionSubject, { kind: "ask" }>;

/**
 * One parked run or one stopped seat, as a row with the buttons that settle
 * it. An ask is drawn by [DecisionList] instead, under the person who asked.
 */
export function DecisionRow({ subject, now }: { subject: RowSubject; now: number }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  switch (subject.kind) {
    case "run":
      return <RunRow run={subject.run} index={index} now={now} />;
    case "seat":
      return <SeatRow seat={subject.seat} index={index} now={now} />;
  }
}

/**
 * The asks put to one person, answerable where they stand.
 *
 * THE LIST MY WORK'S "ASKED OF ME" AND A SEAT'S PAGE BOTH DRAW, newest first
 * as the engine answers them (`work_my_work.asked_of_me`). Nothing when there
 * are none: what an empty one says is the caller's.
 */
export function AskList({
  rows,
  decider,
  now,
}: {
  rows: readonly WorkAskRow[];
  decider: Decider;
  now: number;
}) {
  const subjects = useMemo(
    () => rows.map((ask): DecisionSubject => ({ kind: "ask", at: ask.asked_at, ask })),
    [rows],
  );
  if (rows.length === 0) return null;
  return <DecisionList subjects={subjects} decider={decider} now={now} />;
}

/** One block of a decision list: the asks one person put, or a single row. */
type Block =
  | { kind: "asks"; key: string; asker: string; asks: WorkAskRow[] }
  | { kind: "row"; key: string; subject: RowSubject };

/**
 * Asks grouped under the person who put them, in the order the first of each
 * person's asks came; a parked run or a stopped seat stays a row of its own.
 */
export function blocksOf(subjects: readonly DecisionSubject[]): Block[] {
  const blocks: Block[] = [];
  const byAsker = new Map<string, Block & { kind: "asks" }>();
  for (const subject of subjects) {
    if (subject.kind !== "ask") {
      blocks.push({ kind: "row", key: subjectKey(subject), subject });
      continue;
    }
    // THE AUTHOR IS THE SEAT: a person the identity directory binds to a seat
    // writes AS it, so the asker is that seat, and a principal bound to none
    // draws as its own login.
    const asker = subject.ask.asked_by;
    const group = byAsker.get(asker);
    if (group) {
      group.asks.push(subject.ask);
      continue;
    }
    const created = { kind: "asks" as const, key: `asker:${asker}`, asker, asks: [subject.ask] };
    byAsker.set(asker, created);
    blocks.push(created);
  }
  return blocks;
}

/**
 * The decisions a card or a section lists, each answerable in place.
 *
 * GROUPED BY WHO IS ASKING: who asked, the item and the role they are asked in
 * are said once over that person's questions, rather than once on every row.
 *
 * ONE ASK OPEN AT A TIME: opening one ask's options closes whichever was open,
 * so a card never holds two half-made answers.
 *
 * AN ANSWERED ASK STAYS, as the line saying what was answered. The engine
 * closes it and the next read drops it, so the list keeps the answered ask
 * itself until the list is drawn afresh; `limit` counts only what still
 * waits, so answering one does not pull the next into its place mid-read.
 */
export function DecisionList({
  subjects,
  decider,
  now,
  limit,
}: {
  subjects: readonly DecisionSubject[];
  decider: Decider;
  now: number;
  /** How many waiting decisions to draw; every one when absent. */
  limit?: number;
}) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const [openAsk, setOpenAsk] = useState<string | null>(null);
  const [answered, setAnswered] = useState<
    ReadonlyMap<string, { subject: DecisionSubject; answer: AskAnswer }>
  >(new Map());
  const shown = useMemo(() => {
    const waiting = subjects.filter((s) => !answered.has(subjectKey(s))).slice(0, limit);
    const kept = [...answered.values()].map((a) => a.subject);
    return [...waiting, ...kept].sort(
      (a, b) => (Date.parse(b.at ?? "") || 0) - (Date.parse(a.at ?? "") || 0),
    );
  }, [subjects, answered, limit]);
  const answer = (subject: DecisionSubject, given: AskAnswer) =>
    setAnswered((prev) => new Map(prev).set(subjectKey(subject), { subject, answer: given }));
  return (
    <ul className="decision-list">
      {blocksOf(shown).map((block) =>
        block.kind === "row" ? (
          <DecisionRow key={block.key} subject={block.subject} now={now} />
        ) : (
          <AskGroup
            key={block.key}
            asks={block.asks}
            asker={block.asker}
            index={index}
            decider={decider}
            now={now}
            openAsk={openAsk}
            onOpenAsk={setOpenAsk}
            answeredOf={(ask) => answered.get(`ask:${ask.comment}`)?.answer}
            onAnswered={(ask, given) => {
              setOpenAsk(null);
              answer({ kind: "ask", at: ask.asked_at, ask }, given);
            }}
          />
        ),
      )}
    </ul>
  );
}

/**
 * One decision: the seat it is about, what it is, and the buttons that make it.
 *
 * THE ROW'S STATE TWICE, in two forms: the glyph on the seat's badge, and the
 * same state as the first words of the line under the title. An ask and a
 * parked run wait on a person (`needs`); a seat refused by its budget has
 * stopped. The glyph is what a reader scanning the column of badges sees, and
 * the word is what one reading the row, or one who cannot tell the fills
 * apart, is told.
 */
function Row({
  who,
  status,
  title,
  sub,
  line,
  actions,
}: {
  who: { name: string; kind: "agent" | "human"; avatar?: AgentAvatar | null };
  status: "needs" | "stopped";
  title: ReactNode;
  sub: ReactNode[];
  line?: ReactNode;
  actions: ReactNode;
}) {
  return (
    <li className="decision-row">
      <SeatAvatar name={who.name} kind={who.kind} avatar={who.avatar} status={status} size="md" />
      <div className="decision-body">
        <span className="decision-title">{title}</span>
        <span className="decision-sub">
          <span className="decision-state" data-state={status}>
            {stateWord(status)}
          </span>
          {sub.length > 0 && " · "}
          {sub}
        </span>
        {line && <span className="decision-line">{line}</span>}
      </div>
      <div className="decision-actions">{actions}</div>
    </li>
  );
}

function seatOf(
  index: OrgIndex,
  handle: string,
): { name: string; kind: "agent" | "human"; avatar: AgentAvatar | null } {
  const seat = index.byHandle.get(handle);
  return { name: seat?.name ?? handle, kind: seat?.kind ?? "agent", avatar: seat?.avatar ?? null };
}

/**
 * A key, in the mono face every key is drawn in, linking to its item by its
 * ADDRESS (`itemAddress`): a key another task claimed first opens that task.
 */
function Key({ item }: { item: ItemRef }) {
  return (
    <a className="decision-key mono" href={href(itemPath(item))}>
      {item.key || item.id}
    </a>
  );
}

function joined(parts: ReactNode[]): ReactNode[] {
  const shown = parts.filter((p) => p !== null && p !== undefined && p !== "" && p !== false);
  return shown.map((part, i) => (
    <span key={i}>
      {i > 0 && " · "}
      {part}
    </span>
  ));
}

/**
 * The asks one person put, under one head: who asked, and the item and role
 * every one of them shares; what differs between them is said on the ask.
 */
function AskGroup({
  asks,
  asker: askerHandle,
  index,
  decider,
  now,
  openAsk,
  onOpenAsk,
  answeredOf,
  onAnswered,
}: {
  asks: readonly WorkAskRow[];
  asker: string;
  index: OrgIndex;
  decider: Decider;
  now: number;
  openAsk: string | null;
  onOpenAsk: (comment: string | null) => void;
  answeredOf: (ask: WorkAskRow) => AskAnswer | undefined;
  onAnswered: (ask: WorkAskRow, answer: AskAnswer) => void;
}) {
  const asker = seatOf(index, askerHandle);
  // WHO THE LINES ARE ABOUT: "you" on the reader's own decisions, the
  // decider's name on anybody else's — see [Decider].
  const them = decider.name;
  const roleOf = (ask: WorkAskRow) =>
    ask.decision
      ? `${them ? `${them} is` : "you are"} the ${ask.decision.role}`
      : `asked ${them ?? "you"}`;
  // WHAT EVERY ASK SHARES IS SAID ONCE, on the head; what differs, on the ask.
  // ONE ITEM IS ONE ID: two tasks may show one key (`itemAddress`), and asks
  // on both are two items, not one.
  const sharedItem = asks.every((a) => a.id === asks[0]!.id) ? asks[0]! : null;
  const sharedRole = asks.every((a) => roleOf(a) === roleOf(asks[0]!)) ? roleOf(asks[0]!) : "";
  const newest = asks.reduce(
    (at, a) => (Date.parse(a.asked_at) > Date.parse(at) ? a.asked_at : at),
    asks[0]!.asked_at,
  );
  const head = joined([
    sharedItem ? (
      <span key="k">
        on <Key item={sharedItem} />
      </span>
    ) : (
      ""
    ),
    sharedRole,
    relTime(newest, now),
  ]);
  return (
    <li className="decision-group">
      <div className="decision-group-head">
        <SeatAvatar
          name={asker.name}
          kind={asker.kind}
          avatar={asker.avatar}
          status="needs"
          size="md"
        />
        <span className="decision-group-who">{asker.name}</span>
        <span className="decision-sub">{head}</span>
      </div>
      <ul className="decision-questions">
        {asks.map((ask) => {
          const decision = ask.decision;
          const question = decision?.question || firstLine(ask.body) || ask.title;
          const given = answeredOf(ask);
          const own = joined([
            sharedItem ? "" : <Key key="k" item={ask} />,
            sharedRole ? "" : roleOf(ask),
            decision?.inform ? `answer posted to #${decision.inform.channel}` : "",
          ]);
          return (
            <li key={ask.comment} className="decision-question">
              <span className="decision-title">{question}</span>
              {own.length > 0 && <span className="decision-sub">{own}</span>}
              {given ? (
                <AnsweredLine answer={given} />
              ) : (
                <AnswerAsk
                  item={itemAddress(ask)}
                  label={ask.key}
                  comment={ask.comment}
                  asker={asker.name}
                  options={decision?.options ?? []}
                  recommended={decision?.recommended}
                  open={openAsk === ask.comment}
                  onOpenChange={(open) => onOpenAsk(open ? ask.comment : null)}
                  onAnswered={(answer) => onAnswered(ask, answer)}
                />
              )}
            </li>
          );
        })}
      </ul>
    </li>
  );
}

/** What an ask was answered with, where its controls were. */
function AnsweredLine({ answer }: { answer: AskAnswer }) {
  return (
    <span className="decision-answered" role="status">
      <CheckGlyph size="sm" aria-hidden="true" />
      <span>
        Answered{" "}
        {answer.kind === "reply" ? (
          <strong>{`“${answer.text}”`}</strong>
        ) : (
          <>
            <strong>{answer.label}</strong>
            {answer.recommended && <span className="decision-answered-rec"> (Recommended)</span>}
          </>
        )}
      </span>
    </span>
  );
}

function RunRow({ run, index, now }: { run: SandboxRun; index: OrgIndex; now: number }) {
  const who = seatOf(index, run.agent_handle);
  const item = run.work_item ?? null;
  const since = run.paused_at || run.updated_at;
  const question = (run.question ?? "").trim();
  const sub = joined([
    item ? <Key key="k" item={item} /> : "",
    // THE QUESTION, never its context: the pane and the answer dialog read
    // that.
    question ? (
      <>
        “<RunQuestionLine question={question} />”
      </>
    ) : questionWithheld(run) ? (
      QUESTION_WITHHELD
    ) : (
      ""
    ),
    audienceOf(run, index),
    relTime(since, now),
  ]);
  return (
    <Row
      who={who}
      status="needs"
      title={`${who.name}’s coding run is parked on a question`}
      sub={sub}
      line={holdLine(run, now)}
      actions={<AnswerRunButton run={run} seat={who.name} />}
    />
  );
}

/**
 * WHO THE QUESTION IS PUT TO, as the chart names them: the handles the engine
 * resolved when the run parked — the seat's lead chain, where the audience
 * the run wrote named nobody the chart has. Where that resolved to nobody
 * either (a seat with no lead chain, a park on a node holding no company), or
 * the row reached this screen before its durable record was read, the
 * audience as the run wrote it. Nothing where it named no audience at all.
 */
export function audienceOf(run: SandboxRun, index: OrgIndex): string {
  const handles = run.audience_handles ?? [];
  if (handles.length > 0) {
    return `asks ${handles.map((h) => index.byHandle.get(h)?.name ?? h).join(", ")}`;
  }
  return run.audience ? `asks ${run.audience}` : "";
}

/**
 * How long the run's box is still held — the cost of leaving the question: a
 * box past its pause window is reclaimed and the run restarts from the
 * question. Nothing where the run is held on no timer.
 */
export function holdLine(run: SandboxRun, now: number): string | undefined {
  if (run.status === "reseed") return "Answering restarts the work";
  const ttl = run.pause_ttl_seconds;
  const parked = Date.parse(run.paused_at);
  if (!(ttl > 0) || !Number.isFinite(parked)) return undefined;
  const left = parked + ttl * 1_000 - now;
  if (left <= 0) return "Answer soon or the work restarts";
  const minutes = Math.round(left / 60_000);
  const hours = Math.floor(minutes / 60);
  return `Its box is held for ${hours > 0 ? `${hours}h ${minutes % 60}m` : `${minutes}m`} more`;
}

function SeatRow({ seat, index, now }: { seat: SeatCondition; index: OrgIndex; now: number }) {
  const handle = seat.row.handle ?? seat.row.role;
  const who = seatOf(index, handle);
  const item = seat.row.turn?.work_item ?? seat.row.live_call?.work_item ?? null;
  const period = seat.window ? PERIOD_ADJECTIVE[seat.window.period] : "";
  const sub = joined([
    item ? <Key key="k" item={item} /> : "",
    item ? "stopped mid-turn" : "",
    seat.at ? relTime(seat.at, now) : "",
  ]);
  return (
    <Row
      who={who}
      status="stopped"
      title={`${who.name} stopped: ${period ? `${period} ` : ""}token budget exhausted`}
      sub={sub}
      actions={
        <>
          {/* RAISED WHERE IT IS REPORTED: the dialog opens on the scope
              that is refusing this seat, the stopped window focused. */}
          <RaiseBudgetButton handle={handle} name={who.name} window={seat.window} />
          {item && <ReassignItem item={itemAddress(item)} />}
        </>
      }
    />
  );
}

/**
 * Hand the item a stopped seat was on to somebody else — conditional on the
 * version this reader saw, like every assignment (`AssignButton`).
 */
export function ReassignItem({ item }: { item: string }) {
  // A HAND-OFF IS A WORKLET WRITE, so the version it is conditional on is
  // read only for a reader holding Worklet; without it there is no button.
  const { worklet } = useViewer();
  const read = useQuery("work_item", { id: item }, { enabled: worklet });
  const task = read.data?.task;
  if (!task) return null;
  return (
    <AssignButton
      item={item}
      label={task.key}
      version={task.version}
      assignee={task.assignee ?? ""}
    />
  );
}
