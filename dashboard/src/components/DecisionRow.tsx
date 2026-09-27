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
 *  - a CODING RUN parked on a question: whose, the question quoted, and how
 *    long its box is still held; answered by its turn (`answer_run`);
 *  - a SEAT the engine STOPPED for its token budget: which window is spent,
 *    the item it was on, and the two ways out — raise the ceiling, or hand
 *    the item to somebody else.
 *
 * Shared by Home and the Inbox, which is why it lives here: two copies of the
 * rule for which button is primary would disagree the first time it moved.
 */

import { firstLine } from "~/lib/format.ts";
import { useMemo, type ReactNode } from "react";
import { ButtonLink, EMPTY_VALUE } from "@crewlethq/ui";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { href } from "~/app/router.tsx";
import { AnswerAskButtons, AnswerRunButton, AssignButton, ReplyAskButton } from "./writes.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, type OrgIndex } from "~/lib/seats.ts";
import { PERIOD_ADJECTIVE, waitedOn } from "~/lib/budget.ts";
import { relTime } from "~/lib/format.ts";
import type {
  AgentRow,
  BudgetWindow,
  DecisionsAnswer,
  SandboxRun,
  WorkAskRow,
} from "~/protocol/index.ts";

/** A seat the engine stopped for a spent token budget, as a decision. */
export interface SeatCondition {
  row: AgentRow;
  /** The window that is spent: the seat's own, or the company's. */
  window: BudgetWindow | undefined;
  /** When it last turned a charge away, where the gate recorded it. */
  at?: string;
}

export type DecisionSubject =
  | { kind: "ask"; at: string; ask: WorkAskRow }
  | { kind: "run"; at: string; run: SandboxRun }
  | { kind: "seat"; at?: string; seat: SeatCondition };

/**
 * The Inbox's Decisions chip — `#/inbox?reason=decisions`, where Home's
 * "Review" and "Open inbox" land.
 *
 * NOT ONE OF THE EIGHTEEN NOTICE REASONS: a decision is an open ask, a parked
 * run, a stopped seat or a condition a person decides, which the `decisions`
 * read and the engine's conditions answer, so the chip narrows the list to
 * those rows rather than filtering notices on a reason none carries — which
 * once drew an empty list under a Home figure that had just said one was
 * waiting.
 */
export const DECISIONS_VIEW = "decisions";

/** Where "Review" and "Open inbox" take a reader: the view above. */
export function decisionsHref(): string {
  return href(["inbox"], { reason: DECISIONS_VIEW });
}

/**
 * The seats the engine stopped for a spent budget, each with the window that
 * is spent — the seat's own where it names one, else the company's the gate
 * refused on — and when the gate last turned a charge away.
 */
export function seatConditionsOf(agents: readonly AgentRow[]): SeatCondition[] {
  const out: SeatCondition[] = [];
  for (const row of agents) {
    if (row.activity !== "stopped" || row.stopped_reason !== "budget") continue;
    const window = waitedOn(row.budget?.windows, "refusing");
    out.push({ row, window, at: window?.refused_at });
  }
  return out;
}

/**
 * Which budget-stopped seats are THIS reader's decision, and which are only a
 * condition they can see.
 *
 * A STOPPED SEAT HAS TWO WAYS OUT, and a seat is waiting on a reader exactly
 * when they can take one of them:
 *
 *  - RAISE THE CEILING — a change to the company document, which `/config`
 *    takes from any presented token (`api.auth.tokens` gates writes and all of
 *    `/config`, with no narrower grant), so `viewer.operator`;
 *  - HAND THE ITEM ON — `update_work_item` as the reader, which needs the
 *    engine to serve that write for them (`viewer.acts`) AND an item the seat
 *    was on: a seat stopped between turns has nothing to hand on.
 *
 * Everything else a reader sees is somebody else's decision, so it is counted
 * with the conditions that "need a look", never as one waiting on them: a
 * sentence telling a person a decision waits that they cannot make is the one
 * thing this screen must not say.
 */
export function seatDecisionsFor<T extends { row: AgentRow }>(
  conditions: readonly T[],
  viewer: { operator: boolean; acts: readonly string[] },
): { mine: T[]; others: T[] } {
  const mine: T[] = [];
  const others: T[] = [];
  for (const c of conditions) {
    const item = c.row.turn?.work_item ?? c.row.live_call?.work_item ?? null;
    const reassign = item !== null && viewer.acts.includes("update_work_item");
    (viewer.operator || reassign ? mine : others).push(c);
  }
  return { mine, others };
}

/**
 * The engine's decisions and the stopped seats the reader can act on as one
 * list, newest first — the order Home's card and the Inbox's "Needs a decision" both draw.
 */
export function decisionSubjects(
  answer: DecisionsAnswer | null,
  seats: readonly SeatCondition[],
): DecisionSubject[] {
  const subjects: DecisionSubject[] = [];
  for (const item of answer?.items ?? []) {
    if (item.kind === "ask" && item.ask) subjects.push({ kind: "ask", at: item.at, ask: item.ask });
    if (item.kind === "run" && item.run) subjects.push({ kind: "run", at: item.at, run: item.run });
  }
  for (const seat of seats) subjects.push({ kind: "seat", at: seat.at, seat });
  subjects.sort((a, b) => (Date.parse(b.at ?? "") || 0) - (Date.parse(a.at ?? "") || 0));
  return subjects;
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

export function DecisionRow({
  subject,
  viewerHandle,
  now,
}: {
  subject: DecisionSubject;
  /** Whose decisions these are: a line says so when the asker reports to them. */
  viewerHandle: string;
  now: number;
}) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  switch (subject.kind) {
    case "ask":
      return <AskRow ask={subject.ask} index={index} viewerHandle={viewerHandle} now={now} />;
    case "run":
      return <RunRow run={subject.run} index={index} now={now} />;
    case "seat":
      return <SeatRow seat={subject.seat} index={index} now={now} />;
  }
}

function Row({
  who,
  ring,
  title,
  sub,
  line,
  actions,
}: {
  who: { name: string; kind: "agent" | "human" };
  ring: "warning" | "danger";
  title: ReactNode;
  sub: ReactNode;
  line?: ReactNode;
  actions: ReactNode;
}) {
  return (
    <li className="decision-row">
      <SeatAvatar name={who.name} kind={who.kind} ring={ring} size="md" />
      <div className="decision-body">
        <span className="decision-title">{title}</span>
        <span className="decision-sub">{sub}</span>
        {line && <span className="decision-line">{line}</span>}
      </div>
      <div className="decision-actions">{actions}</div>
    </li>
  );
}

function seatOf(index: OrgIndex, handle: string): { name: string; kind: "agent" | "human" } {
  const seat = index.byHandle.get(handle);
  return { name: seat?.name ?? handle, kind: seat?.kind ?? "agent" };
}

/** A key, in the mono face every key is drawn in, linking to its item. */
function Key({ value }: { value: string }) {
  return (
    <a className="decision-key mono" href={href(["work", value])}>
      {value}
    </a>
  );
}

function joined(parts: ReactNode[]): ReactNode {
  const shown = parts.filter((p) => p !== null && p !== undefined && p !== "" && p !== false);
  return shown.map((part, i) => (
    <span key={i}>
      {i > 0 && " · "}
      {part}
    </span>
  ));
}

function AskRow({
  ask,
  index,
  viewerHandle,
  now,
}: {
  ask: WorkAskRow;
  index: OrgIndex;
  viewerHandle: string;
  now: number;
}) {
  // THE PERSON, NEVER THE TOKEN: an ask a person's credential wrote is
  // authored by the token and drawn as the seat it is bound to.
  const askerHandle = ask.asked_by_seat || ask.asked_by;
  const asker = seatOf(index, askerHandle);
  const decision = ask.decision;
  const question = decision?.question || firstLine(ask.body) || ask.title;
  const recommended = decision?.options.find((o) => o.id === decision.recommended);
  // "REPORTS TO YOU" is DERIVED from the chart, never stated by the asker:
  // it is the reason the question came to this person, where the ask's
  // role says only what they are asked AS.
  const reports = index.byHandle.get(askerHandle)?.managers.some((m) => m.handle === viewerHandle);
  const sub = joined([
    <Key key="k" value={ask.key} />,
    decision ? `you are the ${decision.role}` : "asked you",
    reports ? `${asker.name} reports to you` : "",
    recommended ? (
      <span key="r">
        recommends <strong>{recommended.label}</strong>
      </span>
    ) : (
      ""
    ),
    relTime(ask.asked_at, now),
  ]);
  // WHAT THE ANSWER SETS OFF, said beside the buttons that send it: a person
  // choosing an option should know the asker is woken with it and — where the
  // ask promised one — that it is posted to a channel people read.
  const line = decision
    ? decision.inform
      ? `${asker.name} is woken with your answer and posts it to #${decision.inform.channel}`
      : `${asker.name} continues from your answer`
    : undefined;
  return (
    <Row
      who={asker}
      ring="warning"
      title={`${asker.name} asks: ${question}`}
      sub={sub}
      line={line}
      actions={
        decision && decision.options.length > 0 ? (
          <AnswerAskButtons
            item={ask.key}
            comment={ask.comment}
            options={decision.options}
            recommended={decision.recommended}
          />
        ) : (
          // AN ASK WITH NO OPTIONS IS ANSWERED IN WORDS — here, like every
          // other decision on this row: a link elsewhere sent the reader to
          // a pane with nowhere to write.
          <ReplyAskButton
            item={ask.key}
            comment={ask.comment}
            asker={asker.name}
            question={question}
          />
        )
      }
    />
  );
}

function RunRow({ run, index, now }: { run: SandboxRun; index: OrgIndex; now: number }) {
  const who = seatOf(index, run.agent_handle);
  const key = run.work_item?.key ?? "";
  const since = run.paused_at || run.updated_at;
  const sub = joined([
    key ? <Key key="k" value={key} /> : "",
    run.question ? `“${run.question}”` : "",
    relTime(since, now),
  ]);
  return (
    <Row
      who={who}
      ring="warning"
      title={`${who.name}’s coding run is parked on a question`}
      sub={sub}
      line={holdLine(run, now)}
      actions={<AnswerRunButton turnId={run.turn_id} seat={who.name} question={run.question} />}
    />
  );
}

/**
 * How long the run's box is still held — the cost of leaving the question: a
 * box past its pause window is reclaimed and the run restarts from the
 * question. Nothing where the run is held on no timer.
 */
export function holdLine(run: SandboxRun, now: number): string | undefined {
  if (run.status === "reseed")
    return "Its box was reclaimed: answering restarts the work from the question";
  const ttl = run.pause_ttl_seconds;
  const parked = Date.parse(run.paused_at);
  if (!(ttl > 0) || !Number.isFinite(parked)) return undefined;
  const left = parked + ttl * 1_000 - now;
  if (left <= 0) return "Its box is past its pause window and may be reclaimed";
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
    item ? <Key key="k" value={item.key} /> : "",
    item ? "stopped mid-turn" : "",
    seat.at ? relTime(seat.at, now) : "",
  ]);
  return (
    <Row
      who={who}
      ring="danger"
      title={`${who.name} stopped — ${period ? `${period} ` : ""}token budget exhausted`}
      sub={sub || EMPTY_VALUE}
      actions={
        <>
          {/* RAISING A CEILING IS A CHANGE TO THE COMPANY'S CONFIGURATION,
              made on the one budgets address where every window is drawn. */}
          <ButtonLink size="small" variant="secondary" href={href(["spend", "budgets"])}>
            Raise budget
          </ButtonLink>
          {item && <ReassignItem item={item.key} />}
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
  const read = useQuery("work_item", { id: item });
  const task = read.data?.task;
  if (!task) return null;
  return <AssignButton item={item} version={task.version} assignee={task.assignee ?? ""} />;
}
