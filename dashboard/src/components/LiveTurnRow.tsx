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
 * # A turn that stopped moving says so
 *
 * A live row animates, which sells motion — so a round that has not moved in
 * eleven minutes looked exactly like one that started two seconds ago. The
 * row carries `lib/seats.ts`' own staleness mark beside its elapsed time,
 * SUPPRESSED while the turn is parked: a detached coding run is silent on
 * purpose for as long as it runs, and an alarm on its silence is an alarm on
 * every legitimate run.
 *
 * # The row is the way into the turn
 *
 * The whole row is a link to the turn's trace — the one place its rounds,
 * calls and output are readable — whenever the engine has named the turn. A
 * seat whose turn has not published an id yet draws the same row, unlinked,
 * rather than a link to nothing.
 */

import { StatusDot, Stepper, Tag } from "@crewlethq/ui";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { href } from "~/app/router.tsx";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { fmtElapsed } from "~/lib/format.ts";
import { useMediaQuery } from "~/lib/media.ts";
import { staleness, stateLine, type OrgIndex, type Seat } from "~/lib/seats.ts";
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
  const quiet = Number.isFinite(since) ? fmtElapsed(now - since) : "";
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
  const keyed = row.live_call?.work_item?.key || row.turn?.work_item?.key;
  if (keyed) return verb;
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
  const turnId = row.turn?.turn_id ?? call?.turn_id ?? "";
  // THE ONE DERIVATION every running-turn row shares — see lib/turnsteps.ts.
  // ON A PHONE THE ROUND LEAVES THE STEP and leads the call line, as the seat
  // profile's Current turn does: "Execute · round 1 of 24" left a 358px row no
  // room for Review, which wrapped onto a line of its own behind a dangling
  // connector.
  const phone = useMediaQuery(`(width < ${PHONE_BREAKPOINT}px)`);
  const { steps, current, detail } = turnSteps(row, { inLabel: !phone });
  const callLine = lastCallLine(row);
  const last = phone && detail ? [detail, callLine].filter(Boolean).join(" · ") : callLine;
  const quiet = quietMark(row, now);
  const doing = liveDoing(row, now, seat);
  const body = (
    <>
      <SeatAvatar name={name} kind="agent" ring="info" size="sm" />
      <span className="live-body">
        <span className="live-head">
          <span className="live-name">{name}</span>
          <span className="live-doing" title={doing}>
            {doing}
          </span>
          {quiet && (
            <Tag size="xs" variant={quiet.tone} className="live-quiet">
              {quiet.text}
            </Tag>
          )}
          <span className="live-elapsed">
            {Number.isFinite(started) ? fmtElapsed(now - started) : ""}
          </span>
        </span>
        <Stepper label={`${name}'s turn`} steps={steps} current={current} pulse={!quiet} />
        {last && <span className="live-call mono">{last}</span>}
      </span>
    </>
  );
  return (
    <li>
      {turnId ? (
        <a className="live-row" href={href(["live", "turns", turnId])}>
          {body}
        </a>
      ) : (
        <span className="live-row">{body}</span>
      )}
    </li>
  );
}

/** The dot a card of running turns carries in its header: pulsing while any runs. */
export function LiveDot({ running }: { running: number }) {
  return <StatusDot tone={running > 0 ? "info" : "neutral"} pulse={running > 0} />;
}
