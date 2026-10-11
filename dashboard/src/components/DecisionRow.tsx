/**
 * One thing waiting on a person's decision, as a row they can answer in place.
 *
 * TWO SHAPES, ONE ROW, because they are one question to the reader — "what
 * does somebody need from me" — and a screen that drew them two ways would
 * make a person learn two rows to act on one list:
 *
 *  - an ASK on a work item: who asked, the question, what they recommend,
 *    and — for a structured ask — each option as a button that sends the
 *    choice as the answer (`comment_on_work_item{answers, choice}`), or for
 *    a free-text ask a Reply that sends words (`{answers, body}`), with the
 *    line saying what the answer sets off: the asker is woken with it, and
 *    posts it to the channel it promised where the ask carries an `inform`;
 *  - a CODING RUN parked on a question: whose, the question quoted, and how
 *    long its box is still held; answered by its turn (`answer_run`).
 *
 * Shared by Today, the Inbox, My work's Asked of me and the console's
 * Overview, which is why it lives here: two copies of the rule for which
 * button is primary would disagree the first time it moved.
 *
 * # A stopped seat is the console's
 *
 * A seat the engine STOPPED for its token budget is a decision too, but only
 * an admin's: its way out is a ceiling raised through `/config`, and what it
 * says is the budget's window — spend, which a member is not served
 * (ADR-0031). So the row for it, and the reading of which seats are stopped,
 * are the console's (`routes/admin/overview/seatDecisions.tsx`), drawn with
 * the same row ([DecisionLine]) beside these two on the Overview and nowhere
 * in the person shell — whose screens ask only what a member is served
 * (`app/shell.test.ts`).
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
import { useMemo, type ReactNode } from "react";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { href } from "~/app/router.tsx";
import { AnswerAskButtons, AnswerRunButton, ReplyAskButton } from "./writes.tsx";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, type OrgIndex } from "~/lib/seats.ts";
import { relTime } from "~/lib/format.ts";
import type { DecisionRun, DecisionsAnswer, SandboxRun, WorkAskRow } from "~/protocol/index.ts";

export type DecisionSubject =
  { kind: "ask"; at: string; ask: WorkAskRow } | { kind: "run"; at: string; run: DecisionRun };

/**
 * The Inbox's Decisions chip — `#/inbox?reason=decisions`, where the Overview's
 * "Review" and "Open inbox" land.
 *
 * NOT ONE OF THE EIGHTEEN NOTICE REASONS: a decision is an open ask or a
 * parked run, which the `decisions` read answers, so the chip narrows the
 * list to those rows rather than filtering notices on a reason none carries —
 * which once drew an empty list under a Home figure that had just said one
 * was waiting.
 */
export const DECISIONS_VIEW = "decisions";

/** Where "Review" and "Open inbox" take a reader: the view above. */
export function decisionsHref(): string {
  return href(["inbox"], { reason: DECISIONS_VIEW });
}

/**
 * The engine's decisions as one list, newest first — the order Today, the
 * Inbox's "Needs a decision" and the Overview's card all draw.
 */
export function decisionSubjects(answer: DecisionsAnswer | null): DecisionSubject[] {
  const subjects: DecisionSubject[] = [];
  for (const item of answer?.items ?? []) {
    if (item.kind === "ask" && item.ask) subjects.push({ kind: "ask", at: item.at, ask: item.ask });
    if (item.kind === "run" && item.run) subjects.push({ kind: "run", at: item.at, run: item.run });
  }
  subjects.sort(newestFirst);
  return subjects;
}

/** Newest first, a subject with no instant last: the one order every list of decisions is drawn in. */
export function newestFirst(a: { at?: string }, b: { at?: string }): number {
  return (Date.parse(b.at ?? "") || 0) - (Date.parse(a.at ?? "") || 0);
}

/** A stable key for one subject, whichever list draws it. */
export function subjectKey(s: DecisionSubject): string {
  switch (s.kind) {
    case "ask":
      return `ask:${s.ask.comment}`;
    case "run":
      return `run:${s.run.turn_id}`;
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

export function DecisionRow({
  subject,
  decider,
  now,
}: {
  subject: DecisionSubject;
  /** Whose decisions these are: a line says so when the asker reports to them. */
  decider: Decider;
  now: number;
}) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  switch (subject.kind) {
    case "ask":
      return <AskRow ask={subject.ask} index={index} decider={decider} now={now} />;
    case "run":
      return <RunRow run={subject.run} index={index} now={now} />;
  }
}

/**
 * The asks put to one person, each a [DecisionRow] answerable where it stands.
 *
 * THE LIST MY WORK'S "ASKED OF ME" AND A SEAT'S PAGE BOTH DRAW, newest first
 * as the engine answers them (`work_my_work.asked_of_me`). Nothing when there
 * are none: what an empty one says is the caller's — a section must say
 * something, a card stacked among others must not.
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
  if (rows.length === 0) return null;
  return (
    <ul className="decision-list">
      {rows.map((ask) => (
        <DecisionRow
          key={ask.comment}
          subject={{ kind: "ask", at: ask.asked_at, ask }}
          decider={decider}
          now={now}
        />
      ))}
    </ul>
  );
}

/**
 * ONE DECISION'S ROW: who it is about, what is decided, the facts under it and
 * the controls that decide it. Exported for the one row that is not drawn
 * here — the console's stopped seat — so the Overview's card draws every
 * decision in one shape.
 */
export function DecisionLine({
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

/** The seat behind a handle, as a decision row draws it: its name and its kind. */
export function decisionSeat(
  index: OrgIndex,
  handle: string,
): { name: string; kind: "agent" | "human" } {
  const seat = index.byHandle.get(handle);
  return { name: seat?.name ?? handle, kind: seat?.kind ?? "agent" };
}

/** A key, in the mono face every key is drawn in, linking to its item. */
export function DecisionKey({ value }: { value: string }) {
  return (
    <a className="decision-key mono" href={href(["work", value])}>
      {value}
    </a>
  );
}

/** A row's facts, the empty ones dropped, joined with a middle dot. */
export function decisionFacts(parts: ReactNode[]): ReactNode {
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
  decider,
  now,
}: {
  ask: WorkAskRow;
  index: OrgIndex;
  decider: Decider;
  now: number;
}) {
  // THE PERSON, NEVER THE TOKEN: an ask a person's credential wrote is
  // authored by the token and drawn as the seat it is bound to.
  const askerHandle = ask.asked_by_seat || ask.asked_by;
  const asker = decisionSeat(index, askerHandle);
  const decision = ask.decision;
  const question = decision?.question || firstLine(ask.body) || ask.title;
  const recommended = decision?.options.find((o) => o.id === decision.recommended);
  // WHO THE LINES ARE ABOUT: "you" on the reader's own decisions, the
  // decider's name on anybody else's — see [Decider].
  const them = decider.name;
  // "REPORTS TO YOU" is DERIVED from the chart, never stated by the asker:
  // it is the reason the question came to this person, where the ask's
  // role says only what they are asked AS.
  const reports = index.byHandle
    .get(askerHandle)
    ?.managers.some((m) => m.handle === decider.handle);
  const sub = decisionFacts([
    <DecisionKey key="k" value={ask.key} />,
    decision ? `${them ? `${them} is` : "you are"} the ${decision.role}` : `asked ${them ?? "you"}`,
    reports ? `${asker.name} reports to ${them ?? "you"}` : "",
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
  const whose = them ? `${them}’s` : "your";
  const line = decision
    ? decision.inform
      ? `${asker.name} is woken with ${whose} answer and posts it to #${decision.inform.channel}`
      : `${asker.name} continues from ${whose} answer`
    : undefined;
  return (
    <DecisionLine
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

function RunRow({ run, index, now }: { run: DecisionRun; index: OrgIndex; now: number }) {
  const who = decisionSeat(index, run.agent_handle);
  const key = run.task?.key ?? "";
  const since = run.asked_at;
  const sub = decisionFacts([
    key ? <DecisionKey key="k" value={key} /> : "",
    run.question ? `“${run.question}”` : "",
    audienceOf(run, index),
    relTime(since, now),
  ]);
  return (
    <DecisionLine
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
 * WHO THE QUESTION IS PUT TO, as the chart names them: the handles the engine
 * resolved when the run parked — the seat's lead chain, where the audience
 * the run wrote named nobody the chart has. Nothing where it resolved to
 * nobody: the agent's own free label for its audience is the machine's word,
 * which the engine does not send a member (ADR-0031).
 */
export function audienceOf(run: DecisionRun, index: OrgIndex): string {
  if (run.asked.length === 0) return "";
  return `asks ${run.asked.map((h) => index.byHandle.get(h)?.name ?? h).join(", ")}`;
}

/**
 * A board row (`sandbox_runs`) as the decision it asks, for a screen that
 * draws the board's parked runs with the row the decisions draw. A FIELD
 * MAPPING: every value is one the engine served, the deadline included —
 * `keeps_place_until` is left absent rather than derived on a live entry the
 * store has not caught up with. The one stand-in is that entry's `asked_at`,
 * which the live projection does not hold: its last update is the nearest
 * instant it has, until the durable row replaces it.
 */
export function decisionOfRun(run: SandboxRun): DecisionRun {
  return {
    turn_id: run.turn_id,
    question: run.question,
    task: run.work_item ? { id: run.work_item.id, key: run.work_item.key } : null,
    agent_handle: run.agent_handle,
    asked: run.audience_handles ?? [],
    asked_fallback: run.audience_fallback,
    asked_at: run.asked_at || run.updated_at,
    keeps_place_until: run.keeps_place_until,
    answerable_in_chat: run.answerable_in_chat,
    chat_surface: run.chat_surface,
  };
}

/**
 * How long the run's box is still held — the cost of leaving the question: a
 * box past its pause window is reclaimed and the run restarts from the
 * question. The DEADLINE IS THE ENGINE'S (`keeps_place_until`, the reaper's
 * own), so this only says how far off it is; null is a box no longer held.
 */
export function holdLine(run: DecisionRun, now: number): string | undefined {
  // NOT KNOWN YET — a live entry whose durable row has not been read — says
  // nothing rather than guessing either way.
  if (run.keeps_place_until === undefined) return undefined;
  if (run.keeps_place_until === null)
    return "Its box is not held: answering restarts the work from the question";
  const left = Date.parse(run.keeps_place_until) - now;
  if (!(left > 0)) return "Its box is past its pause window and may be reclaimed";
  const minutes = Math.round(left / 60_000);
  const hours = Math.floor(minutes / 60);
  return `Its box is held for ${hours > 0 ? `${hours}h ${minutes % 60}m` : `${minutes}m`} more`;
}
