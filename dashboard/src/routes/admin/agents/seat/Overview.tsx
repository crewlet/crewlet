/**
 * A seat's Overview in the console: the figures of its week, the turn it is
 * on, how it is set up and what it remembers — beside the person shell's own
 * panels for its open work and who it is (`routes/team/seat/Overview.tsx`),
 * which this page draws rather than restates, so a teammate and an admin are
 * never shown one seat's work or charter two ways.
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
 * exist. What it shows is their day (theirs, and their leads', to read), the
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
import { turnIdOf, watchHref } from "~/lib/turns.ts";
import { useClipped } from "~/lib/useClipped.ts";
import { inboxFigure, useInboxCountsOf, waitingWords } from "~/lib/useInboxCounts.ts";
import { useQuery, type QueryResult } from "~/lib/useQuery.ts";
import { readsPersonOf, useViewer } from "~/lib/viewer.ts";
import type { AgentMemory } from "~/contract/memory.ts";
import type {
  AgentRow,
  SeatActivityAnswer,
  SeatActivityRow,
  WorkItemsAnswer,
  WorkSummary,
} from "~/protocol/index.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { toolSourceLabel } from "../SeatPeek.tsx";
import {
  About,
  AssignedWork,
  HumanOverview,
  Reports,
  SchedulesCard,
  type OverviewProps as PersonOverviewProps,
} from "~/routes/team/seat/Overview.tsx";
import { useTurnOrdinal } from "~/lib/useTurnOrdinal.ts";
import {
  companyCeilings,
  companyCeilingShort,
  failedIn,
  feedRows,
  fortnight,
  placementWords,
  roundWords,
  sandboxWords,
  seatSchedules,
  signed,
  tokensTile,
  turnTokens,
  workersWords,
  type SeatTab,
} from "~/routes/team/seat/profile.ts";
import { ModelChain } from "./shared.tsx";

/** The window the tiles count: a week of company days, and the week before. */
export const ACTIVITY_DAYS = 7;

/** Where a link to one of this seat's console tabs goes. */
export function tabHref(seat: Seat, tab: SeatTab): string {
  return href(seatPath(seat, "admin"), tab === "overview" ? {} : { tab });
}

/**
 * The person shell's props, and the guarded half of the seat: the company
 * document's entry for it, which only the console reads (`useSeatSetup`).
 */
export interface OverviewProps extends PersonOverviewProps {
  reading: SeatReading;
}

export function Overview(props: OverviewProps) {
  return props.seat.kind === "human" ? <HumanOverview {...props} /> : <AgentOverview {...props} />;
}

// ---------------------------------------------------------------------------
// An agent: the person shell's panels, and how the machine runs it
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
        <Reports seat={seat} />
      </div>
      <aside className="prof-side" aria-label={`About ${seat.name}`}>
        <About seat={seat} onboardedAt={memory.data?.onboarded_at} />
        <Setup seat={seat} agent={agent} reading={reading} />
        <MemoryCard seat={seat} memory={memory} now={now} />
        <SchedulesCard seat={seat} now={now} all={tabHref(seat, "schedules")} />
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
  const turnId = turnIdOf(agent);
  const key = call?.work_item?.key || turn?.work_item?.key || "";
  const onTurn = turnId !== "" && !!agent;
  // WHICH TURN ON THE TASK, by the task's own count off the turn's story —
  // the same reading the seat's peek and the turn's crumb make.
  const ordinal = useTurnOrdinal(onTurn ? key : "", turnId);
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
        // A WATCH LINK, onto the Transcript with the running phase open —
        // never the trace's Timeline, which is where "Open last turn" goes.
        actions={
          <a className="t-link" href={watchHref(turnId)}>
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
        <QueryState error={activity.error} loading={activity.loading && !activity.data}>
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

/**
 * How the seat is set up: the model it runs on and the tools it is granted —
 * both on the public chart — and where its code runs, which nodes may hold
 * it and which workers it may delegate to, which are the company document's
 * and read only with an admin's key.
 *
 * FOUR STATES FOR THE GUARDED HALF, never an empty value over a setting
 * nobody could read: read, refused (the reader's key does not reach it),
 * absent (the active revision names no single seat by this name) and unread
 * (still in flight).
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
  const fleet = useQuery("fleet", undefined, { enabled: viewer.admin, pollMs: 60_000 });
  const chain = seat.raw.llm?.["execute"] ?? [];
  const tools = seat.raw.tool_sources ?? [];
  const lease = fleet.data?.seats?.find((s) => s.handle === seat.handle);
  // WHICH NODE HOLDS IT NOW: the fleet's lease for an admin, else what
  // the public health push says (`heldBy`), which the peek says too. Empty
  // only while an admin's fleet read is in flight.
  const holder = viewer.admin
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
          <a className="t-link" href={href(["agents", "edit"], { seat: seat.handle })}>
            Edit
          </a>
        }
      >
        <Card.Title as="h3">Setup</Card.Title>
      </Card.Header>
      <dl className="prof-facts">
        <dt>Model</dt>
        <dd>{chain.length > 0 ? <ModelChain keys={chain} /> : "No provider configured"}</dd>
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
          {tools.length > 0
            ? tools.map((t) => (
                <Tag key={t} size="xs" appearance="outline">
                  {toolSourceLabel(t)}
                </Tag>
              ))
            : "none granted"}
        </dd>
      </dl>
      {reading.state !== "read" && (
        <p className="prof-note">
          {reading.state === "refused"
            ? // A READER THE ENGINE WOULD REFUSE IS NEVER ASKED, and is
              // `refused` all the same (`useSeatSetup`): what they are
              // missing is an admin's key, not a read in flight.
              "Its sandbox, placement and workers are in the company document, which an admin's key reads."
            : reading.state === "absent"
              ? "The active revision names no single seat by this name, so its sandbox, placement and workers cannot be read."
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
        <QueryState error={memory.error} loading={memory.loading && !data}>
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
