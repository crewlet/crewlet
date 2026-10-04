/**
 * A seat's Overview: the figures of its week, the turn it is on, its open
 * work, and who it is — the approved Agent artboard's first tab.
 *
 * # Every figure is the engine's, over a window it names
 *
 * The four tiles and the fortnight chart are ONE answer — `seat_activity`
 * with `previous` — so "+9 vs last week" and the chart's first seven columns
 * are the same days, summed from every node's usage rows rather than from the
 * answering node's event log. The first-pass rate is the engine's own
 * percentage over REVIEWED turns (a turn nobody reviewed has no verdict), the
 * median and the p90 are its quantiles, and the tokens tile is the seat's
 * CAPPED window where it has one, so the figure and its ceiling are one
 * window's (`profile.ts`'s `tokensTile`).
 *
 * # A person runs no turn
 *
 * A human seat's Overview asks nothing a runtime answers — no activity, no
 * memory, no turn list, no phase history: the engine never spawns a person,
 * so every one of those reads would be a question about a thing that cannot
 * exist. What it shows is their day (theirs, or a `fleet:operate` holder's, to read), the
 * work assigned to them and who they are.
 */

import { useId, useMemo, useRef, useState } from "react";
import {
  Card,
  DATA_COLOR_OTHER,
  EmptyValue,
  StackedColumns,
  StatCard,
  StatGroup,
  StatusDot,
  Stepper,
  Tag,
} from "@crewlethq/ui";
import { BrainGlyph, ClockGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { DueMark, PriorityMark, StatusMark } from "~/components/work.tsx";
import { describe as describeCron } from "~/lib/cron.ts";
import {
  civilAt,
  companyDateLabel,
  fmtCount,
  fmtDate,
  fmtDuration,
  fmtElapsed,
  fmtTime,
  inTime,
  plural,
  relTime,
} from "~/lib/format.ts";
import {
  awaitingPerson,
  heldBy,
  reportsCaption,
  resolvedAbsence,
  resolvedWithheld,
  seatPath,
  type NameOf,
  type OrgIndex,
  type Seat,
  type SeatReading,
} from "~/lib/seats.ts";
import { renderInline } from "~/lib/markdown.ts";
import { useMediaQuery } from "~/lib/media.ts";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { useEngineHealth, useOrg, useSandboxes, useSchedules } from "~/lib/store-hooks.ts";
import { turnSteps } from "~/lib/turnsteps.ts";
import { useClipped } from "~/lib/useClipped.ts";
import { inboxFigure, useInboxCountsOf } from "~/lib/useInboxCounts.ts";
import { useQuery, type QueryResult } from "~/lib/useQuery.ts";

import { useViewer } from "~/lib/viewer.ts";
import type { AgentMemory } from "~/contract/memory.ts";
import type {
  AgentRow,
  SeatActivityAnswer,
  SeatActivityRow,
  WorkItemsAnswer,
  WorkSummary,
} from "~/protocol/index.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { toolSourceLabel, turnOrdinal } from "../SeatPeek.tsx";
import {
  companyCeilings,
  companyCeilingShort,
  failedIn,
  feedRows,
  fortnight,
  mayReadRecord,
  placementWords,
  roundWords,
  sandboxWords,
  seatSchedules,
  signed,
  tokensTile,
  turnTokens,
  workersWords,
  type SeatTab,
} from "./profile.ts";
import { ModelChain } from "./shared.tsx";
import { itemPath } from "~/lib/work.ts";

/** The window the tiles count: a week of company days, and the week before. */
export const ACTIVITY_DAYS = 7;

/** How many assigned tasks the Overview lists before "All n". */
export const ASSIGNED_ROWS = 5;

/** How many schedules the Overview's card lists before "All n". */
const SCHEDULE_ROWS = 3;

/**
 * How many responsibilities About lists before "Show all" — the artboard's
 * three, which keep Setup and Memory beside the week's tiles rather than a
 * screen further down. A charter is written for the seat's prompt, not for
 * this card, and the example company's run to a dozen bullets of four lines
 * each: drawn whole, About stood 1,700px tall and pushed the rest of the side
 * off the page.
 */
export const ABOUT_ROWS = 3;

/** Where a link to one of this seat's tabs goes. */
export function tabHref(seat: Seat, tab: SeatTab): string {
  return href(seatPath(seat), tab === "overview" ? {} : { tab });
}

export interface OverviewProps {
  seat: Seat;
  agent: AgentRow | undefined;
  index: OrgIndex;
  /** The seat's open work — the one answer the tab strip counts. */
  work: QueryResult<WorkItemsAnswer>;
  reading: SeatReading;
  nameOf: NameOf;
  now: number;
}

export function Overview(props: OverviewProps) {
  return props.seat.kind === "human" ? <HumanOverview {...props} /> : <AgentOverview {...props} />;
}

// ---------------------------------------------------------------------------
// An agent
// ---------------------------------------------------------------------------

function AgentOverview({ seat, agent, index, work, reading, now }: OverviewProps) {
  const handle = seat.handle;
  const activity = useQuery(
    "seat_activity",
    { seat: handle, days: ACTIVITY_DAYS, previous: true },
    { enabled: handle !== "", pollMs: 60_000 },
  );
  const row = activity.data?.seats?.find((r) => r.handle === handle);
  // THE TOTALS AND THE NEWEST REFLECTION, from the node HOLDING the seat —
  // one row of each list is all this card draws, and the totals are counted
  // there rather than read off a page.
  const memory = useQuery(
    "agent_memory",
    { id: handle, limit: 1 },
    { enabled: handle !== "", pollMs: 60_000 },
  );
  return (
    <div className="prof-overview">
      <div className="prof-main">
        <Kpis seat={seat} agent={agent} activity={activity} />
        <CurrentTurn seat={seat} agent={agent} now={now} />
        <div className="prof-pair">
          <AssignedWork seat={seat} work={work} now={now} />
          <TurnsPerDay activity={activity} row={row} />
        </div>
        <Reports seat={seat} index={index} />
      </div>
      <aside className="prof-side" aria-label={`About ${seat.name}`}>
        <About seat={seat} onboardedAt={memory.data?.onboarded_at} />
        <Setup seat={seat} agent={agent} reading={reading} />
        <MemoryCard seat={seat} memory={memory} now={now} />
        <SchedulesCard seat={seat} now={now} />
      </aside>
    </div>
  );
}

/** The week's four figures. */
function Kpis({
  seat,
  agent,
  activity,
}: {
  seat: Seat;
  agent: AgentRow | undefined;
  activity: QueryResult<SeatActivityAnswer>;
}) {
  const row = activity.data?.seats?.find((r) => r.handle === seat.handle);
  const waiting = !row && !activity.error;
  const unanswered = !row && !!activity.error;
  const prev = row?.previous;
  const tokens = tokensTile(agent?.budget?.windows, row?.tokens);
  // THE COMPANY'S CEILINGS BOUND EVERY SEAT, so a seat with none of its own
  // is not one "nothing caps" — the tile said so over a company with a
  // day and a month set — and they are named, as the Settings tab names them.
  const budget = useOrg()?.token_budget;
  const company = companyCeilings(budget);
  const companyShort = companyCeilingShort(budget);
  // A CAPPED WINDOW IS PUSHED: its figure needs no read, so it never waits on
  // the usage history the other three do.
  const tokensWaiting = tokens.window === null && waiting;
  return (
    <StatGroup columns={4} aria-label={`${seat.name}'s week`}>
      <StatCard
        label={`Turns · ${ACTIVITY_DAYS} days`}
        loading={waiting}
        value={
          row ? row.turns.toLocaleString() : <EmptyValue label="The usage history did not answer" />
        }
        // WHETHER MORE TURNS IS BETTER is not a thing this tile knows, so the
        // change is neutral: a seat twice as busy may be twice as stuck.
        delta={
          row && prev ? { value: signed(row.turns - prev.turns), polarity: "neutral" } : undefined
        }
        sub={
          row
            ? prev
              ? "vs last week"
              : row.failed > 0
                ? `${row.failed.toLocaleString()} failed`
                : "none failed"
            : unanswered
              ? "not answered"
              : undefined
        }
      />
      <StatCard
        label="Approved first pass"
        loading={waiting}
        // THE ENGINE'S RATE, over the turns it REVIEWED — never first passes
        // over every turn, which counts a turn nobody judged as a failure.
        // Absent is no rate at all, which a 0% would misstate.
        value={
          row && row.first_pass_pct !== undefined ? (
            `${Math.round(row.first_pass_pct)}%`
          ) : (
            <EmptyValue label={row ? "No turn was reviewed" : "Not answered"} />
          )
        }
        sub={
          row
            ? row.reviewed > 0
              ? `${row.sent_back.toLocaleString()} sent back`
              : "none reviewed this week"
            : unanswered
              ? "not answered"
              : undefined
        }
      />
      <StatCard
        label="Median turn"
        loading={waiting}
        value={
          row && row.p50_ms !== undefined ? (
            fmtDuration(row.p50_ms)
          ) : (
            <EmptyValue label={row ? "No turn this week" : "Not answered"} />
          )
        }
        sub={
          row?.p90_ms !== undefined ? (
            <span
              title={`Quantiles within ±${Math.round((activity.data?.quantile_resolution ?? 0) * 100)}%`}
            >
              p90 {fmtDuration(row.p90_ms)}
            </span>
          ) : unanswered ? (
            "not answered"
          ) : undefined
        }
      />
      <StatCard
        label={tokens.label}
        loading={tokensWaiting}
        value={
          tokens.value !== undefined ? (
            fmtCount(tokens.value)
          ) : (
            <EmptyValue label="The usage history did not answer" />
          )
        }
        sub={
          tokens.of !== null ? (
            `of ${fmtCount(tokens.of)} budget`
          ) : row ? (
            company ? (
              // ONE LINE, the whole sentence on hover: see
              // `companyCeilingShort`.
              <span className="prof-tile-sub" title={`No seat cap — ${company}`}>
                {companyShort}
              </span>
            ) : (
              "no budget caps it"
            )
          ) : unanswered ? (
            "not answered"
          ) : undefined
        }
      />
    </StatGroup>
  );
}

/**
 * The turn in flight: where it is, what it has called, and the call running
 * now — or, between turns, the seat's state in one line.
 */
function CurrentTurn({
  seat,
  agent,
  now,
}: {
  seat: Seat;
  agent: AgentRow | undefined;
  now: number;
}) {
  const sandboxes = useSandboxes();
  const phone = useMediaQuery(`(width < ${PHONE_BREAKPOINT}px)`);
  const turn = agent?.turn ?? null;
  const call = agent?.live_call ?? null;
  const turnId = turn?.turn_id ?? call?.turn_id ?? "";
  const key = call?.work_item?.key || turn?.work_item?.key || "";
  const onTurn = turnId !== "" && !!agent;
  // WHICH TURN ON THE TASK, from the task's own turn list — the same reading
  // the seat's peek makes.
  const itemTurns = useQuery(
    "work_item_turns",
    { id: key, limit: 1 },
    { enabled: onTurn && key !== "" },
  );
  // WHAT THE TASK IS CALLED, read by its key — the turn's own item, never a
  // lookup in the Assigned-work card's top five: a turn on a task outside
  // those five (one just asked of it, with no priority) read "On ENG-35" with
  // no title at all.
  const item = useQuery("work_item", { id: key }, { enabled: onTurn && key !== "" });
  // THE PHASES THIS TURN HAS RECORDED, for its tokens: the seat's newest
  // turn row is this turn's once any phase of it has completed.
  const newest = useQuery(
    "turns",
    { seat: seat.handle, limit: 1 },
    { enabled: onTurn && seat.handle !== "", pollMs: 20_000 },
  );
  if (!agent || !onTurn) {
    const last = agent?.last_turn;
    return (
      <Card padding="none" className="prof-turn">
        <Card.Header
          divided={false}
          icon={<StatusDot tone="neutral" />}
          // NOT ON A TURN, AND NOTHING MORE: a stopped seat's why is the
          // notice above the tabs and its last turn is the line below, each
          // said once — the state line said "last turn 16h ago" beside a
          // "last turn ended 15h ago" read off the same instant.
          subtitle="Not on a turn"
          // THE WAY TO IT IS THE HEADER'S ACTION, where "Watch live" is while
          // a turn runs — one place for the card's link in both states, in
          // the card-link register every other card's action wears. Inside
          // the sentence below it was an underlined prose link, the one link
          // on the page drawn differently.
          actions={
            last ? (
              <a className="t-link" href={href(["live", "turns", last.turn_id])}>
                Open last turn
              </a>
            ) : undefined
          }
        >
          <Card.Title as="h3">Current turn</Card.Title>
        </Card.Header>
        {last && (
          <p className="prof-turn-last">
            Its last turn {last.outcome === "failed" ? "failed" : "ended"}{" "}
            {relTime(last.ended_at, now)}.
          </p>
        )}
      </Card>
    );
  }
  const ordinal = key
    ? turnOrdinal(
        turnId,
        itemTurns.data?.turns?.[0],
        !!itemTurns.data && itemTurns.data.key === key,
      )
    : null;
  // THE ANSWER FOR THIS KEY ONLY: while a turn moves to another task the last
  // read is still the previous one's, and its title under the new key would
  // name the wrong task.
  const task = item.data?.task;
  const title = key && task && task.key === key ? task.title : undefined;
  // ON A PHONE THE ROUND IS SAID BESIDE THE STEPS rather than inside one:
  // "Execute · round 7 of 25" left the row no room for Review, which wrapped
  // onto a line of its own behind a dangling connector.
  const { steps, current, detail } = turnSteps(agent, { inLabel: !phone });
  const sandbox = sandboxes.find((s) => s.turn_id === turnId) ?? null;
  const asking = !!sandbox && awaitingPerson(sandbox.status);
  const started = Date.parse(turn?.started_at ?? call?.started_at ?? "");
  const tokens = turnTokens(turnId, newest.data?.turns?.[0], !!newest.data, agent);
  const rows = feedRows(agent);
  const round = roundWords(agent);
  return (
    <Card padding="none" className="prof-turn" data-live="true">
      <Card.Header
        divided={false}
        // THE TITLE HOLDS ITS WORDS ALONE (screens.css makes every card title
        // a block that ellipses), so the state is the header's icon and the
        // turn's line is its subtitle — which gives way first.
        icon={<StatusDot tone={asking ? "warning" : "info"} pulse={!asking} />}
        // THE TASK THE TURN IS CHARGED TO, once the engine names one. A turn
        // names its item when it is woken for it or when it writes to it, so
        // a turn that has not is said as nothing rather than as "on no task",
        // which it may yet not be.
        subtitle={
          key ? (
            <span className="prof-turn-about">
              {ordinal ? `Turn ${ordinal} on ` : "On "}
              <a className="work-key mono" href={href(["work", key])}>
                {key}
              </a>
              {title && ` · ${title}`}
            </span>
          ) : undefined
        }
        actions={
          <a className="t-link" href={href(["live", "turns", turnId])}>
            Watch live
          </a>
        }
      >
        <Card.Title as="h3">Current turn</Card.Title>
      </Card.Header>
      <div className="prof-turn-steps">
        <Stepper
          label={`${seat.name}'s turn`}
          steps={steps}
          current={current}
          tone={asking ? "warning" : "info"}
          pulse={!asking}
        />
        <span className="prof-turn-figures">
          {[
            phone ? detail : "",
            Number.isFinite(started) ? fmtElapsed(now - started) : "",
            tokens !== null ? `${fmtCount(tokens)} tokens` : "",
          ]
            .filter(Boolean)
            .join(" · ")}
        </span>
      </div>
      {asking && (
        <p className="prof-turn-ask">
          Its coding run is waiting on a question: {sandbox?.question || "(no question recorded)"}
        </p>
      )}
      {rows.length > 0 ? (
        <ol className="prof-feed" aria-label={`Calls this phase${round ? `, ${round}` : ""}`}>
          {rows.map((r) => (
            <li key={r.key} className="prof-feed-row" data-running={r.running || undefined}>
              <span className="prof-feed-at mono">{r.at ? fmtTime(r.at) : ""}</span>
              <span className="prof-feed-name mono">{r.name}</span>
              <span className="prof-feed-words" title={r.words}>
                {r.words}
              </span>
              <span className="prof-feed-took" data-failed={r.failed || undefined}>
                {r.running
                  ? `running ${fmtElapsed(now - Date.parse(r.at))}`
                  : r.failed
                    ? `failed${r.tookMs !== undefined ? ` · ${fmtDuration(r.tookMs)}` : ""}`
                    : r.tookMs !== undefined
                      ? fmtDuration(r.tookMs)
                      : ""}
              </span>
            </li>
          ))}
        </ol>
      ) : (
        <p className="prof-turn-last">
          {current === "execute"
            ? "No tool called yet this phase."
            : "No tool calls in this phase."}
        </p>
      )}
    </Card>
  );
}

/** The open work on the seat, most urgent first, with the way to all of it. */
function AssignedWork({
  seat,
  work,
  now,
}: {
  seat: Seat;
  work: QueryResult<WorkItemsAnswer>;
  now: number;
}) {
  const rows = work.data?.items ?? [];
  const total = work.data?.total_hint ?? rows.length;
  return (
    <Card padding="none" className="prof-card">
      <Card.Header
        actions={
          total > 0 ? (
            <a className="t-link" href={href(["work"], { assignee: seat.handle })}>
              All {total.toLocaleString()}
              {work.data?.total_capped ? "+" : ""}
            </a>
          ) : undefined
        }
      >
        <Card.Title as="h3">Assigned work</Card.Title>
      </Card.Header>
      <QueryState
        error={work.error}
        refusal={work.refusal}
        loading={work.loading && !work.data}
        empty={
          work.data && rows.length === 0
            ? {
                title: "Nothing open is assigned to them",
                hint: "Work reaches a seat by assignment; closed work is not counted here.",
              }
            : undefined
        }
      >
        <ul className="prof-rows">
          {rows.slice(0, ASSIGNED_ROWS).map((row) => (
            <AssignedRow key={row.id} row={row} now={now} />
          ))}
        </ul>
      </QueryState>
    </Card>
  );
}

function AssignedRow({ row, now }: { row: WorkSummary; now: number }) {
  return (
    <li>
      <a className="prof-row" href={href(itemPath(row))}>
        <StatusMark status={row.status} />
        <span className="work-key mono prof-row-key" title={row.key}>
          {row.key}
        </span>
        <span className="prof-row-title">{row.title}</span>
        <PriorityMark priority={row.priority} />
        <span className="prof-row-due">
          <DueMark due={row.due} overdue={row.overdue} now={now} />
        </span>
      </a>
    </li>
  );
}

const TURN_SERIES = [{ id: "turns", name: "Turns", color: DATA_COLOR_OTHER }] as const;

/** Turns per company day over the fortnight the tiles' week-on-week spans. */
function TurnsPerDay({
  activity,
  row,
}: {
  activity: QueryResult<SeatActivityAnswer>;
  row: SeatActivityRow | undefined;
}) {
  const days = fortnight(row);
  const last = days.length - 1;
  const buckets = days.map((d) => ({ t: civilAt(d.day) ?? 0, values: { turns: d.turns } }));
  const labels = new Map(
    days.map((d, i) => [civilAt(d.day) ?? 0, i === last ? "Today" : companyDateLabel(d.day)]),
  );
  const failed = failedIn(days);
  return (
    <Card padding="none" className="prof-card prof-chart">
      <Card.Header
        // THE CHART'S KEY UNDER ITS NAME, as the artboard stacks it.
        className="card-head-stacked"
        subtitle={
          days.length > 0
            ? `Last ${days.length} days · ${failed > 0 ? `${failed.toLocaleString()} failed` : "none failed"}`
            : undefined
        }
      >
        <Card.Title as="h3">Turns per day</Card.Title>
      </Card.Header>
      <div className="prof-chart-body">
        <QueryState
          error={activity.error}
          refusal={activity.refusal}
          loading={activity.loading && !activity.data}
        >
          <StackedColumns
            series={TURN_SERIES}
            buckets={buckets}
            legend="none"
            height="9rem"
            label={`Turns per company day, the last ${days.length} days`}
            format={(v) => Math.round(v).toLocaleString()}
            formatTime={(at) => labels.get(at) ?? ""}
          />
        </QueryState>
      </div>
    </Card>
  );
}

/** Who reports to this seat, when anybody does. */
function Reports({ seat, index }: { seat: Seat; index: OrgIndex }) {
  if (seat.reports.length === 0) return null;
  return (
    <Card padding="none" className="prof-card">
      <Card.Header count={seat.reports.length} subtitle={reportsCaption(seat, index.hierarchy)}>
        <Card.Title as="h3">Direct reports</Card.Title>
      </Card.Header>
      <div className="seat-grid prof-reports">
        {seat.reports.map((r) => {
          const about = r.goal || r.unit?.name || "";
          return (
            <a key={r.key} className="seat-card" href={href(seatPath(r))}>
              <div className="row">
                <SeatAvatar
                  name={r.name}
                  size="sm"
                  kind={r.kind === "human" ? "human" : "agent"}
                  decorative
                />
                <span className="truncate t-cell prof-report-name">{r.name}</span>
              </div>
              {/* A GOAL IS PROSE, SO IT GETS THE CARD'S OWN WIDTH and two
                  lines of it, cut at a line rather than at whichever pixel
                  came next; the whole of it is on the title. */}
              {about && (
                <span className="clamp t-caption" title={about}>
                  {renderInline(about, r.key)}
                </span>
              )}
            </a>
          );
        })}
      </div>
    </Card>
  );
}

/**
 * The seat's goal and what it is responsible for — its charter.
 *
 * THE ARTBOARD'S SHAPE, THEN THE WHOLE OF IT ON REQUEST: the goal to three
 * lines and the first [ABOUT_ROWS] responsibilities to two each, and a
 * disclosure where that cut anything — more responsibilities than three, or a
 * clamp that measured short ([useClipped]). The charter is the seat's prompt
 * text, so nothing in it is summarised: "Show all" draws every word.
 *
 * INLINE MARKDOWN, because that is what a prompt is written in: a
 * responsibility naming ``nimbuscore`` meant a code span, and drawn as plain
 * text it read as two pairs of stray backticks.
 */
function About({ seat, onboardedAt }: { seat: Seat; onboardedAt?: string | undefined }) {
  const human = seat.kind === "human";
  const empty = !seat.goal && seat.responsibilities.length === 0;
  const [whole, setWhole] = useState(false);
  const bodyId = useId();
  const body = useRef<HTMLDivElement>(null);
  const clipped = useClipped(body, `${whole}|${seat.goal}|${seat.responsibilities.join("\n")}`);
  const more = seat.responsibilities.length - ABOUT_ROWS;
  const shown = whole ? seat.responsibilities : seat.responsibilities.slice(0, ABOUT_ROWS);
  const offer = whole || more > 0 || clipped;
  return (
    <Card padding="none" className="prof-card">
      <Card.Header divided={false}>
        <Card.Title as="h3">About</Card.Title>
      </Card.Header>
      <div className="prof-about" id={bodyId} ref={body} data-whole={whole || undefined}>
        {seat.goal && (
          <p className="prof-about-goal">
            <span className={whole ? undefined : "clamp"}>{renderInline(seat.goal, "goal")}</span>
          </p>
        )}
        {shown.length > 0 && (
          <ul className="prof-about-list">
            {shown.map((r, i) => (
              // THE CLAMP IS ON A SPAN INSIDE THE ITEM: on the `li` itself
              // its `display` would take the bullet with it.
              <li key={i}>
                <span className={whole ? undefined : "clamp"}>{renderInline(r, `resp-${i}`)}</span>
              </li>
            ))}
          </ul>
        )}
        {offer && (
          // THE CARD-LINK REGISTER, flush with the text above it: a ghost
          // button's own inline padding stood it nine pixels in from the goal
          // and the list it opens, and it was the one action on the side
          // column not drawn the way "Edit" and "Open" are.
          <button
            type="button"
            className="t-link prof-about-more"
            aria-expanded={whole}
            aria-controls={bodyId}
            onClick={() => setWhole(!whole)}
          >
            {whole
              ? "Show less"
              : more > 0
                ? `Show all ${seat.responsibilities.length} responsibilities`
                : "Show the whole charter"}
          </button>
        )}
        {empty && (
          <p className="t-caption">
            {human
              ? "No goal or responsibilities are written for this person."
              : "No goal or responsibilities are written for this seat — both render straight into its prompt."}
          </p>
        )}
        {human && seat.availability && <p className="t-caption">Available {seat.availability}</p>}
        {onboardedAt && <p className="t-caption">Onboarded {fmtDate(onboardedAt)}</p>}
      </div>
    </Card>
  );
}

/**
 * How the seat is set up: the model it runs on and the tools it is granted —
 * both on the org projection, which carries them only to a reader holding
 * `config:read` because both are derived from the company document — and
 * where its code runs, which nodes may hold it and which workers it may
 * delegate to, which are the company document's and read only with
 * `config:read`.
 *
 * AN ABSENT CHAIN OR TOOL LIST IS THE SEAT'S OWN ONLY FOR A READER THE ENGINE
 * WOULD HAVE SENT IT TO ([resolvedAbsence]): "No provider configured" and
 * "none granted" are said to a `config:read` holder, and everybody else is
 * told which grant shows them.
 *
 * FOUR STATES FOR THE GUARDED HALF, never an empty value over a setting
 * nobody could read: read, refused (the reader does not hold `config:read`),
 * absent (the active revision names no seat by this handle) and unread (still
 * in flight).
 */
function Setup({
  seat,
  agent,
  reading,
}: {
  seat: Seat;
  agent: AgentRow | undefined;
  reading: SeatReading;
}) {
  const viewer = useViewer();
  const health = useEngineHealth();
  // THE FLEET'S LEASES ARE THE DEPLOYMENT'S, read on `fleet:operate`.
  const fleet = useQuery("fleet", undefined, { enabled: viewer.operatesFleet, pollMs: 60_000 });
  const chain = seat.raw.llm?.["execute"] ?? [];
  const tools = seat.raw.tool_sources ?? [];
  const absent = resolvedAbsence(viewer);
  const lease = fleet.data?.seats?.find((s) => s.handle === seat.handle);
  // WHICH NODE HOLDS IT NOW: the fleet's lease for a `fleet:operate` holder,
  // else what the health push says (`heldBy`), which the peek says too. Empty
  // only while that holder's fleet read is in flight.
  const holder = viewer.operatesFleet
    ? lease
      ? lease.node
      : fleet.data
        ? "no node holds it"
        : ""
    : heldBy(seat.handle, agent, health);
  return (
    <Card padding="none" className="prof-card">
      <Card.Header
        divided={false}
        actions={
          <a className="t-link" href={href(["agents", "edit"], { seat: seat.handle || seat.name })}>
            Edit
          </a>
        }
      >
        <Card.Title as="h3">Setup</Card.Title>
      </Card.Header>
      <dl className="prof-facts">
        <dt>Model</dt>
        <dd>
          {chain.length > 0 ? (
            <ModelChain keys={chain} />
          ) : absent === "none" ? (
            "No provider configured"
          ) : absent === "withheld" ? (
            <span className="muted">{resolvedWithheld("its model chain")}</span>
          ) : (
            <EmptyValue label="Not reported yet" />
          )}
        </dd>
        {reading.state === "read" ? (
          <>
            <dt>Sandbox</dt>
            <dd>{sandboxWords(reading.role)}</dd>
            <dt>Runs on</dt>
            <dd>
              {placementWords(reading.role)}
              {holder && <span className="muted"> · now {holder}</span>}
            </dd>
            <dt>Workers</dt>
            <dd>{workersWords(reading.role)}</dd>
          </>
        ) : (
          <>
            <dt>Runs on</dt>
            <dd>{holder || <EmptyValue label="Reading the fleet" />}</dd>
          </>
        )}
        <dt>Tools</dt>
        <dd className="prof-tools">
          {tools.length > 0 ? (
            tools.map((t) => (
              <Tag key={t} size="xs" appearance="outline">
                {toolSourceLabel(t)}
              </Tag>
            ))
          ) : absent === "none" ? (
            "none granted"
          ) : absent === "withheld" ? (
            <span className="muted">{resolvedWithheld("its tool sources")}</span>
          ) : (
            <EmptyValue label="Not reported yet" />
          )}
        </dd>
      </dl>
      {reading.state !== "read" && (
        <p className="prof-note">
          {reading.state === "refused" || (reading.state === "unread" && absent === "withheld")
            ? // A READER WITHOUT THE GRANT IS NEVER ASKED (`useSeatSetup`):
              // they stay `unread`, and what they are missing is the grant,
              // not a read in flight.
              "Its sandbox, placement and workers are in the company document, which config:read reads."
            : reading.state === "absent"
              ? "The active revision names no seat by this handle, so its sandbox, placement and workers cannot be read."
              : "Reading its sandbox, placement and workers from the company document…"}
        </p>
      )}
    </Card>
  );
}

/** What the seat remembers, counted, and the newest thing it chose to keep. */
function MemoryCard({
  seat,
  memory,
  now,
}: {
  seat: Seat;
  memory: QueryResult<AgentMemory>;
  now: number;
}) {
  const data = memory.data;
  const reflection = data?.latest_reflection ?? null;
  return (
    <Card padding="none" className="prof-card">
      <Card.Header
        divided={false}
        actions={
          <a className="t-link" href={tabHref(seat, "memory")}>
            Open
          </a>
        }
      >
        <Card.Title as="h3">Memory</Card.Title>
      </Card.Header>
      <div className="prof-memory">
        <QueryState error={memory.error} refusal={memory.refusal} loading={memory.loading && !data}>
          {data && (
            <>
              {/* THE TOTALS, counted by the node holding the seat — never the
                  length of the page it sent, which is one row here. */}
              <dl className="prof-memory-totals">
                <div>
                  <dt>diary</dt>
                  <dd>{data.diary_total.toLocaleString()}</dd>
                </div>
                <div>
                  <dt>episodes</dt>
                  <dd>{data.episodes_total.toLocaleString()}</dd>
                </div>
                <div>
                  <dt>learned skills</dt>
                  <dd>{data.skills_total.toLocaleString()}</dd>
                </div>
              </dl>
              {reflection ? (
                <figure className="prof-reflection">
                  <figcaption>
                    <BrainGlyph size="xs" aria-hidden="true" />
                    Latest reflection · {relTime(reflection.created_at, now)}
                  </figcaption>
                  <blockquote>“{reflection.content}”</blockquote>
                </figure>
              ) : (
                <p className="t-caption">
                  {data.held_by === "none"
                    ? "No node holds this seat, so no copy of its memory is current."
                    : "Nothing written yet — a seat keeps a note when a turn teaches it something."}
                </p>
              )}
            </>
          )}
        </QueryState>
      </div>
    </Card>
  );
}

/** The recurring work that wakes the seat, soonest first. */
function SchedulesCard({ seat, now }: { seat: Seat; now: number }) {
  const pushed = useSchedules();
  const rows = useMemo(() => seatSchedules(pushed, seat.handle), [pushed, seat.handle]);
  return (
    <Card padding="none" className="prof-card">
      <Card.Header
        divided={false}
        actions={
          rows.length > SCHEDULE_ROWS ? (
            <a className="t-link" href={tabHref(seat, "schedules")}>
              All {rows.length}
            </a>
          ) : undefined
        }
      >
        <Card.Title as="h3">Schedules</Card.Title>
      </Card.Header>
      {rows.length === 0 ? (
        <p className="prof-note">Nothing recurring reaches {seat.name}.</p>
      ) : (
        <ul className="prof-sched">
          {rows.slice(0, SCHEDULE_ROWS).map((row) => (
            <li key={`${row.scope_type}/${row.scope_id}/${row.name}`}>
              {/* THE ARTBOARD'S CLOCK: each row is a recurring time. */}
              <ClockGlyph size="sm" className="prof-sched-glyph" aria-hidden="true" />
              <span className="prof-sched-name">{row.name}</span>
              <span className="prof-sched-when">
                {row.problem ? (
                  <Tag size="xs" variant="danger" title={row.problem}>
                    cannot fire
                  </Tag>
                ) : !row.enabled ? (
                  "disabled"
                ) : (
                  <span title={row.next_run ? `next ${inTime(row.next_run, now)}` : undefined}>
                    {describeCron(row.cron) ?? row.cron}
                  </span>
                )}
              </span>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

// ---------------------------------------------------------------------------
// A person
// ---------------------------------------------------------------------------

function HumanOverview({ seat, index, work, nameOf, now }: OverviewProps) {
  const viewer = useViewer();
  // A PERSON RECORD IS THEIRS: their unread notices, the order they mean to
  // work in and who set it. The engine answers it to its owner, to whoever
  // leads them and to a `fleet:operate` holder — so it is asked where this
  // page can tell it will be answered ([mayReadRecord]), and WITHHELD by a
  // sentence elsewhere rather than drawn as a person with nothing to do.
  const self = viewer.handle !== "" && viewer.handle === seat.handle;
  const mayRead = mayReadRecord(viewer, index, seat.handle);
  const person = useQuery(
    "work_person",
    { handle: seat.handle },
    { enabled: mayRead && seat.handle !== "", pollMs: 60_000 },
  );
  // WHAT IS WAITING ON THEM is the inbox's own count — the sidebar badge's
  // reading when it is the viewer's own day — never the person record's
  // `unread`, which holds only the EXCEPTIONS below their read mark.
  const inbox = useInboxCountsOf(seat.handle, mayRead);
  // WHAT THEY MEAN TO DO FIRST, COUNTED BY THE ENGINE: the resolved list's
  // total, the figure My work and this profile's Work tab both read. The
  // record's `priorities` is the list AS STORED — a person's own object, which
  // a read never rewrites — so it still names a task they finished until
  // their next reorder drops it, and a tile counting it said 3 beside a Work
  // tab listing 2.
  const mine = useQuery(
    "work_my_work",
    { handle: seat.handle },
    { enabled: mayRead && seat.handle !== "", pollMs: 60_000 },
  );
  const priorities = mine.data?.totals?.priorities;
  const who = self ? "you" : "them";
  return (
    <div className="prof-overview">
      <div className="prof-main">
        {mayRead ? (
          // A SECTION RATHER THAN A CARD: the tiles draw their own surface,
          // and a card around them was a frame inside a frame.
          <section className="prof-day" aria-labelledby="prof-day-title">
            <div className="prof-day-head">
              <h3 id="prof-day-title" className="prof-section-title">
                {self ? "Your day" : "Their day"}
              </h3>
              {self ? (
                // YOURS IS MOVED ON WHERE YOU WORK IT, so this says where.
                <span className="t-caption">
                  <a className="t-link" href={href(["inbox"])}>
                    Inbox
                  </a>
                  {" · "}
                  <a className="t-link" href={href(["me"])}>
                    My work
                  </a>
                </span>
              ) : (
                <span className="t-caption">
                  Read-only here: an inbox is moved on by the person whose it is.
                </span>
              )}
            </div>
            <QueryState
              error={person.error}
              refusal={person.refusal}
              loading={person.loading && !person.data}
            >
              {person.data && (
                <StatGroup columns={3}>
                  <StatCard
                    label="Unread"
                    loading={inbox.waiting === null}
                    value={inboxFigure(inbox)}
                    sub={
                      person.data.due?.length
                        ? `waiting on ${who} · ${plural(person.data.due.length, "snoozed notice")} now due`
                        : `waiting on ${who}`
                    }
                  />
                  <StatCard
                    // THE LIST SOMEBODY WROTE, called what My work calls it —
                    // "Queue" there is the open work ASSIGNED to them.
                    label="Priorities"
                    loading={!mine.data && !mine.error}
                    value={
                      priorities ? (
                        `${priorities.total.toLocaleString()}${priorities.capped ? "+" : ""}`
                      ) : (
                        <EmptyValue
                          label={
                            mine.error
                              ? "Their queue did not answer"
                              : "This engine counts no queue"
                          }
                        />
                      )
                    }
                    sub={
                      person.data.priorities_set_by
                        ? `set by ${nameOf(person.data.priorities_set_by)}${
                            person.data.priorities_set_at
                              ? ` ${relTime(person.data.priorities_set_at, now)}`
                              : ""
                          }`
                        : `${self ? "your" : "their"} own order`
                    }
                  />
                  <StatCard
                    label="Pinned views"
                    value={(person.data.pinned_views?.length ?? 0).toLocaleString()}
                    sub={`${person.data.favorites?.length ?? 0} starred`}
                  />
                </StatGroup>
              )}
            </QueryState>
          </section>
        ) : (
          <Card padding="none" className="prof-card">
            <Card.Header divided={false}>
              <Card.Title as="h3">Their day</Card.Title>
            </Card.Header>
            <p className="prof-note">
              Their inbox, their priorities and their pinned views are theirs. Reading another
              person&rsquo;s needs fleet:operate, or leading them.
            </p>
          </Card>
        )}
        <AssignedWork seat={seat} work={work} now={now} />
        <Reports seat={seat} index={index} />
      </div>
      <aside className="prof-side" aria-label={`About ${seat.name}`}>
        <About seat={seat} />
      </aside>
    </div>
  );
}
