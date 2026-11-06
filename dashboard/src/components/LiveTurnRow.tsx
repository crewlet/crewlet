/**
 * One running turn, as a row: who, what it is doing, how long it has been
 * going, where it is in its turn, and the call it is making — drawn the same
 * way wherever a running turn is listed (Home's Live now, Live › Now running).
 *
 * ONE ROW, because two screens that each spell a running turn disagree the
 * first time either moves: Home said "round 7" in the stepper while Live's
 * phase table said "7r" in a column, about the same call at the same moment.
 * The stepper and the call line are `lib/turnsteps.ts`' single derivation.
 *
 * # A row holds still
 *
 * EVERY ROW IS ONE SIZE, whatever its turn is doing: the steps say the same words,
 * the call line keeps its line with no call out, and the elapsed time has its own
 * slot. What waits is one quiet count, as a Structure status line says it.
 *
 * # A turn that stopped moving says so
 *
 * A live row animates, which sells motion — so a round that has not moved in
 * eleven minutes looked exactly like one that started two seconds ago. The
 * row carries `lib/seats.ts`' own staleness mark beside its elapsed time,
 * SUPPRESSED while the turn is parked: its executor, running in its box, is
 * silent on purpose for as long as it runs, and an alarm on its silence is an
 * alarm on every executor doing its work.
 *
 * # The row is the way into the turn
 *
 * The whole row is a link to the turn's trace — the one place its rounds,
 * calls and output are readable — whenever the engine has named the turn. A
 * seat whose turn has not published an id yet draws the same row, unlinked,
 * rather than a link to nothing.
 *
 * # Status for everybody, the trail for an admin
 *
 * The row is STATUS — who, which step, which round, the tool running now —
 * and every signed-in reader is sent it. What the seat was woken by and what
 * it passed to its tools are its trail, which the engine withholds from a
 * reader who may not read that seat's (`body_withheld`): the row then names
 * the tool and nothing it was handed ([lastCallLine]). The turn's page is the
 * company-wide trail and an admin's, so only an admin's row is a link to it;
 * anybody else's would open on a lock.
 */

import { useEffect, useRef, useState, type ReactNode } from "react";
import { StatusDot, Stepper, Tag, VisuallyHidden } from "@crewlethq/ui";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { href } from "~/app/router.tsx";
import { pathOf } from "~/app/frame/objects.ts";
import { fmtElapsed, fmtUptime } from "~/lib/format.ts";
import { waitingCount } from "~/lib/inboxWaiting.ts";
import { staleness, stateLine, turnItemKey, type OrgIndex, type Seat } from "~/lib/seats.ts";
import { turnIdOf } from "~/lib/turns.ts";
import { useViewer } from "~/lib/viewer.ts";
import { lastCallLine, turnSteps } from "~/lib/turnsteps.ts";
import type { AgentRow } from "~/protocol/index.ts";

/** The words a stale or stalled round carries, with how long it has been quiet. */
export function quietMark(
  row: AgentRow,
  now: number,
): { tone: "warning" | "danger"; text: string } | null {
  const call = row.live_call;
  const mark = staleness(call?.updated_at, now, row.turn?.stage);
  if (!mark) return null;
  // THE SAME INSTANT `staleness` measured, read the same way: the push's
  // stamp may arrive without its zone, and it is UTC.
  const at = call?.updated_at ?? "";
  const since = Date.parse(at.endsWith("Z") ? at : `${at}Z`);
  // COARSE, because the row must hold still: past a minute a second ticking by in
  // this tag moved the words beside it with nothing new to say.
  const quiet = Number.isFinite(since) ? fmtUptime(now - since) : "";
  // STALE IS A CAUTION AND STALLED IS A FAILURE, in the tones every other
  // caution and failure in the product wears: a long tool call is worth a
  // look, a round ten minutes past any provider timeout is not coming back.
  return mark === "stalled"
    ? { tone: "danger", text: quiet ? `stalled · ${quiet} quiet` : "stalled" }
    : { tone: "warning", text: quiet ? `no update · ${quiet}` : "no update" };
}

/**
 * What a running turn's words say it is ON: the item's key where the turn is
 * charged to one (which `stateLine` already names), otherwise what woke it —
 * the trigger's own summary, the subject the artboard draws after the verb.
 *
 * A turn on no item used to read "Agent PM  Executing" and stop, which says a
 * seat is busy and nothing about with what; the trigger is the one thing every
 * running turn has, since it rides on every phase frame from the first.
 */
export function liveDoing(row: AgentRow, now: number, seat: Seat | null): string {
  const verb = stateLine(row, { now, seat });
  if (turnItemKey(row)) return verb;
  const summary = row.live_call?.trigger?.["summary"];
  return typeof summary === "string" && summary.trim() ? `${verb} · ${summary.trim()}` : verb;
}

export function LiveTurnRow({ row, index, now }: { row: AgentRow; index: OrgIndex; now: number }) {
  const handle = row.handle ?? "";
  const seat = index.byHandle.get(handle) ?? null;
  const name = seat?.name ?? row.role;
  const call = row.live_call;
  // FROM THE TURN'S OWN START, which `agent_turn_started` stamps before any
  // phase has run — so the clock starts when the turn does, not at its first
  // model call, and a turn still gathering its context is already timed.
  const started = Date.parse(row.turn?.started_at ?? call?.started_at ?? "");
  // THE ONE READING of which turn a seat is on (`turnIdOf`): a turn record
  // whose id is still "" falls through to the call's, where `??` stopped at
  // the empty string and drew a seat the sidebar linked as an unlinked row.
  const turnId = turnIdOf(row);
  // THE ONE DERIVATION every running-turn row shares (lib/turnsteps.ts). THE ROUND
  // LEADS THE CALL LINE, never a step, so the steps are one width at every stage.
  const { steps, current, detail } = turnSteps(row, { inLabel: false });
  const last = [detail, lastCallLine(row)].filter(Boolean).join(" · ");
  const quiet = quietMark(row, now);
  const waiting = waitingCount(row.waiting);
  const doing = liveDoing(row, now, seat);
  const { admin } = useViewer();
  const linked = turnId !== "" && admin;
  const body = (
    <>
      <SeatAvatar name={name} kind="agent" avatar={seat?.avatar} status="working" size="sm" />
      <span className="live-body">
        <span className="live-head">
          <span className="live-name" title={name}>
            {name}
          </span>
          <span className="live-state">
            <span className="live-doing" title={doing}>
              {doing}
            </span>
            {quiet && (
              <Tag size="xs" variant={quiet.tone} className="live-quiet">
                {quiet.text}
              </Tag>
            )}
            {/* WHAT WAITS BEHIND THIS TURN, as the node holding the seat
                counts it: the turn's own mail is not in it. */}
            {waiting && (
              <span className="live-waiting" title={waiting.detail}>
                <span aria-hidden="true">· {waiting.text}</span>
                <VisuallyHidden>{waiting.detail}</VisuallyHidden>
              </span>
            )}
          </span>
          <span className="live-elapsed">
            {Number.isFinite(started) ? fmtElapsed(now - started) : ""}
          </span>
        </span>
        <Stepper label={`${name}'s turn`} steps={steps} current={current} pulse={!quiet} />
        <span className="live-call mono" title={last || undefined}>
          {last}
        </span>
      </span>
    </>
  );
  return (
    <li>
      {linked ? (
        <a className="live-row" href={href(pathOf({ kind: "turn", id: turnId }))}>
          {body}
        </a>
      ) : (
        <span className="live-row">{body}</span>
      )}
    </li>
  );
}

/**
 * The room a card keeps for its running turns, held for the visit: a turn that
 * finishes leaves its room and the next fills it, so what stands under the card
 * holds still. A new width starts again, since a row's height follows it.
 */
export function LiveRoom({ children }: { children: ReactNode }) {
  const body = useRef<HTMLDivElement>(null);
  const [height, setHeight] = useState(0);
  useEffect(() => {
    const el = body.current;
    if (!el || typeof ResizeObserver !== "function") return;
    let width = el.getBoundingClientRect().width;
    const observer = new ResizeObserver(([entry]) => {
      if (!entry) return;
      const box = entry.contentRect;
      const fresh = Math.abs(box.width - width) > 1;
      width = box.width;
      setHeight((held) => (fresh ? box.height : Math.max(held, box.height)));
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, []);
  return (
    <div className="live-room" style={height > 0 ? { minBlockSize: height } : undefined}>
      <div ref={body}>{children}</div>
    </div>
  );
}

/** The dot a card of running turns carries in its header: pulsing while any runs. */
export function LiveDot({ running }: { running: number }) {
  return <StatusDot tone={running > 0 ? "info" : "neutral"} pulse={running > 0} />;
}
