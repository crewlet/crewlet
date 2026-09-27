/**
 * Live › Now running: what the company is doing this minute.
 *
 * It replaces a board that answered "is the engine up" and nothing about the
 * work: five bands delivering nine scalars, four of which were summaries of
 * other screens, and NO PER-SEAT INFORMATION AT ALL — no seat list, no phase,
 * no model. It also moved on its own: a section appeared between two others
 * whenever a turn crossed a staleness threshold, the stat strip reflowed from
 * one column to three as tiles came and went, and on disconnect the whole
 * record card collapsed to a paragraph.
 *
 * This one answers the two questions an operator actually arrives with, in
 * that order:
 *
 *   1. What is my company doing right now?  → live seats, what is in a box
 *   2. What has it been doing?              → throughput, spend, the feed
 *
 * The third — "is anything waiting on me" — used to be here as an attention
 * band, and it is the Inbox's now: a condition waiting on somebody is a claim
 * on them rather than a statistic about the company, and two screens owning
 * one queue is two screens to keep agreeing about it. The band went; the
 * QUEUE ITSELF was left behind computing on every clock tick with nothing
 * rendering it, and with it a `stream` poll and a connection subscription
 * whose only reader was that dead memo.
 *
 * The layout is FIXED. Every section is always present, in the same place, in
 * the same size, whether or not it has anything in it — an empty one says so.
 * A dashboard whose sections move when the data moves cannot be read at a
 * glance, which is the only way this screen is ever read.
 */

import { useMemo } from "react";
import { href, useNavigator } from "~/app/router.tsx";
import { EventRow, SeatCard, Section } from "~/components/common.tsx";
import { WindowMeters } from "~/components/budget.tsx";
import {
  ActivityStrip,
  BarList,
  Button,
  Card,
  EmptyState,
  EmptyValue,
  Legend,
  StatCard,
  StatGroup,
  Tag,
} from "@crewlethq/ui";
// A phase is drawn in the series of the band the engine folds it into.
import { phaseColor } from "~/lib/spend.ts";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { mergeRuns, RunStatus } from "./Runs.tsx";
import {
  ArrowRightGlyph,
  BookOpenGlyph,
  ZapGlyph,
  UsersGlyph,
  LinkGlyph,
  BrainGlyph,
  ClockGlyph,
  SquareTerminalGlyph,
  ChartNoAxesGanttGlyph,
  CoinsGlyph,
} from "@crewlethq/icons/glyphs";
import {
  useAgents,
  useEvents,
  useOrg,
  useOrgBudget,
  useSandboxes,
  useTokens,
} from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { activityOf, awaitingPerson, indexOrg, nameOfIn } from "~/lib/seats.ts";
import { fmtCount, plural, relTime, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { MAX_EVENTS } from "~/contract/wire.ts";
import { cutInto, spanOf, spanWords, useTimeRange, windowLabel } from "~/lib/range.ts";
import type { Offer } from "~/lib/range.ts";
import { TimeRangePicker } from "~/ui/TimeRange.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { Mark } from "~/ui/glyph.tsx";
import { useAttention } from "~/lib/useAttention.ts";
import { WHERE_OF, watchedIn } from "~/lib/attention.ts";

/**
 * WHICH WINDOWS THE STRIP HAS.
 *
 * The short end of the shared vocabulary, and NO CUSTOM INTERVAL — this strip
 * is drawn from the events THIS TAB is holding, which is the last
 * [MAX_EVENTS] and nothing older, so a window ending last Tuesday could only
 * ever render as empty. Cost's picker offers the long end of the same
 * vocabulary, so `1d` means one thing on both screens even though neither
 * offers it to the other's reader.
 */
const STRIP_OFFER: Offer = { ranges: ["15m", "1h", "6h"], custom: false, fallback: "1h" };

/**
 * The most cells the strip is ever cut into.
 *
 * A CAP rather than a fixed count, and [cutInto] is where both halves are
 * decided: sixty is what an hour of minute cells already drew and what the
 * stylesheet is sized for, and a wider window widens the cell rather than
 * adding slivers past it.
 */
const STRIP_CELLS = 60;

/**
 * How many coding runs the "In a box" panel draws before it says how many more.
 *
 * EIGHT, which is the feed's seven beside it plus a row: both lists are on a
 * landing screen read at a glance, and a row is `--size-row-md` tall, so eight is a
 * screenful rather than a scroll. The set is NOT bounded by the seat count the
 * way the live tiles are — a run parked on a question that nobody ever answers
 * stays in the record until the retention sweep takes it, so a company can
 * accumulate more of these than it has seats. The full list is one click away
 * and the panel says how much of it is not here.
 */
const IN_BOX_ROWS = 8;

export function LiveNow() {
  const nav = useNavigator();
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const events = useEvents();
  const org = useOrg();
  const tokens = useTokens();
  const budget = useOrgBudget();
  const now = useNow();
  // THE DURABLE CODING RUNS, the same source the Inbox reads: the attention
  // queue counts down a parked run's pause window, which only the durable row
  // carries, and two screens computing one queue from two sources would
  // disagree about whether anybody is waiting.
  const { data: runs } = useQuery("sandbox_runs", undefined, { pollMs: 30_000 });
  // NOT ALIGNED to a bucket: the strip's cell is its own, finer than either of
  // the engine's, and its newest cell is the minute in progress.
  const range = useTimeRange(now, STRIP_OFFER, false);

  const index = useMemo(() => indexOrg(org), [org]);
  const nameOf = useMemo(() => nameOfIn(index), [index]);

  const live = useMemo(
    () =>
      index.seats
        .filter((s) => s.kind === "agent")
        .map((seat) => ({ seat, agent: agents.find((a) => a.role === seat.name) }))
        .filter(({ agent }) => activityOf(agent) === "working"),
    [index.seats, agents],
  );

  /**
   * WHAT IS IN A BOX RIGHT NOW — the durable rows and the live projection
   * folded by the Runs screen's own function.
   *
   * NOTHING IS CUT TO "NOT FINISHED" ANY MORE, because nothing finished can
   * arrive: a settled run's record is deleted, and the projection drops its
   * entry on completion, so both sources carry only live runs.
   *
   * Both sources, because neither alone is "right now": the projection is
   * reconciled against the durable record only every thirty seconds, so a run
   * the events never announced reaches it a reconcile late, and the store is a
   * poll behind a box that came up two seconds ago. Counting only the projection is what made the tile below disagree
   * with the Inbox about whether anything was waiting.
   */
  const inFlight = useMemo(() => mergeRuns(runs?.runs ?? [], sandboxes), [runs, sandboxes]);
  const parked = inFlight.filter((r) => awaitingPerson(r.status)).length;

  const { open: openPeek } = usePeekControls();
  // THE CONDITIONS WHOSE HOME IS THIS SCREEN — see the card below.
  const attention = useAttention();
  const watching = attention.filter((a) => WHERE_OF[a.subject] === "live");

  // The activity strip is keyed by the BUCKET, never by index: keying the cells
  // `p0..p59` over a window recomputed from the clock shifts every cell's
  // content one position left on each roll, and rewrites the lot.
  const { cell, cells } = cutInto(spanOf(range.window), STRIP_CELLS);
  const strip = useMemo(() => {
    const end = Math.floor(now / cell) * cell;
    const buckets = new Map<number, number>();
    for (let t = end - (cells - 1) * cell; t <= end; t += cell) buckets.set(t, 0);
    for (const ev of events) {
      const t = Math.floor(tsKey(ev.timestamp) / cell) * cell;
      if (buckets.has(t)) buckets.set(t, (buckets.get(t) ?? 0) + 1);
    }
    return [...buckets.entries()].map(([t, v]) => ({ t, v }));
  }, [events, now, cell, cells]);

  // The feed's own retention is the limit of what this panel can HONESTLY
  // claim: 400 events fill in minutes on a busy company, so a strip covering
  // an hour has to say where the record actually starts rather than drawing
  // the gap as quiet.
  const oldestHeld = events.length ? tsKey(events[events.length - 1]!.timestamp) : 0;
  const covered = cells * cell;
  const stripTruncated = events.length >= MAX_EVENTS && oldestHeld > now - covered;

  const seatCount = index.seats.filter((s) => s.kind === "agent").length;
  const humanCount = index.seats.length - seatCount;
  const idle = agents.filter((a) => activityOf(a) === "idle").length;
  const orgMeter = budget?.org;

  const phaseSpend = useMemo(
    () =>
      (tokens?.by_phase ?? [])
        .map((p) => ({
          // THE SERIES' OWN IDENTITY, which their BarList and Legend both
          // require: a ranked list is re-ranked by every push, and a row
          // keyed by position hands the focused row's node to whichever
          // series has taken that place.
          id: p.phase,
          label: p.phase,
          value: p.total_tokens,
          display: fmtCount(p.total_tokens),
          color: phaseColor(p.phase),
          sub: `${p.calls.toLocaleString()} calls`,
        }))
        .sort((a, b) => b.value - a.value),
    [tokens],
  );

  const topSeats = useMemo(
    () =>
      (tokens?.by_agent ?? [])
        .slice()
        .sort((a, b) => b.total_tokens - a.total_tokens)
        .slice(0, 6)
        .map((a) => ({
          id: a.handle || a.role,
          label: a.role,
          value: a.total_tokens,
          display: fmtCount(a.total_tokens),
          href: href(["agents", "seats", a.handle || a.role]),
        })),
    // `href` is a pure function of its arguments, not the navigator: listing
    // `nav` here made this recompute on every route change and named a
    // dependency the body does not read.
    [tokens],
  );

  return (
    <>
      <PageActions>
        {
          <>
            <Tag appearance="outline">{plural(seatCount, "agent seat")}</Tag>
            {humanCount > 0 && <Tag appearance="outline">{plural(humanCount, "human")}</Tag>}
            <TimeRangePicker range={range} ariaLabel="Activity window" />
            <Button
              variant="secondary"
              leadingIcon={<BrainGlyph size="sm" />}
              onClick={() => nav.to(["live", "turns"])}
            >
              Turns
            </Button>
          </>
        }
      </PageActions>
      <PageNote>
        What the company is doing at this moment — which seats are working, what is running in a
        box, what is being watched, and what it is costing. WHAT NEEDS A PERSON is not here: it is
        the Inbox, because a condition waiting on somebody is a claim on them rather than a
        statistic about the company.
      </PageNote>

      {/* 2. What the company is doing. */}
      {/* StatGroup IS the flush panel this row sat in — same surface, same
          hairline, same clip — so there is no Panel around it any more. */}
      <StatGroup columns={4}>
        <StatCard
          icon={<ZapGlyph size="xs" />}
          label="Working now"
          value={live.length}
          // THE OTHER HALF OF THE SAME COUNT, not a list of who. `sub` is one
          // ellipsized line and a seat's name is founder prose, so the join was
          // cut mid-word and named a seat nobody configured. WHO is working is
          // the Live seats card immediately below, which draws one tile per seat
          // — so the caption spends its line on the fact that card does not
          // carry, and does it on one branch rather than two that differ by a
          // suffix and drift.
          sub={`${plural(idle, "seat")} idle and waiting for work`}
        />
        <StatCard
          icon={<SquareTerminalGlyph size="xs" />}
          label="Coding runs"
          value={inFlight.length}
          sub={
            parked > 0
              ? `${plural(parked, "run")} paused on a question`
              : "detached sandbox runs in flight"
          }
        />
        <StatCard
          icon={<ChartNoAxesGanttGlyph size="xs" />}
          label={`Events · last ${windowLabel(range.window)}`}
          value={fmtCount(strip.reduce((n, b) => n + b.v, 0))}
          sub={
            stripTruncated
              ? "the tab holds the last 400 events, so this hour is partial"
              : "everything the engine published"
          }
        />
        <StatCard
          icon={<CoinsGlyph size="xs" />}
          label="Tokens"
          value={
            tokens ? fmtCount(tokens.totals.total_tokens) : <EmptyValue label="Not counted yet" />
          }
          sub={
            tokens
              ? `${tokens.totals.calls.toLocaleString()} model calls`
              : "no spend has been recorded yet"
          }
        />
      </StatGroup>

      <div className="grid grid-auto-lg">
        <Card>
          <Card.Header
            icon={<UsersGlyph size="sm" />}
            count={live.length}
            actions={
              <Button size="small" variant="ghost" onClick={() => nav.to(["agents", "roster"])}>
                All seats
              </Button>
            }
          >
            <Card.Title>Live seats</Card.Title>
          </Card.Header>
          {live.length ? (
            <div className="seat-grid">
              {live.map(({ seat, agent }) => (
                // THE TILE IS ALREADY AN ANCHOR to the seat's page, so the
                // peek is opened from a wrapper that generates NO BOX of its
                // own: `display: contents` leaves the card itself the grid
                // item, where a wrapping div would become one and the cards in
                // a row would stop matching heights. The click still bubbles —
                // `display: contents` removes the box, not the node — and
                // `rowPeekHandler` is the frame's one copy of which clicks
                // mean elsewhere, so ⌘-click still opens the seat's page.
                <div
                  key={seat.handle}
                  style={{ display: "contents" }}
                  onClick={rowPeekHandler(() => openPeek({ kind: "seat", id: seat.handle }))}
                >
                  <SeatCard seat={seat} agent={agent} nameOf={nameOf} />
                </div>
              ))}
            </div>
          ) : (
            <EmptyState
              size="compact"
              icon={<ClockGlyph size={32} />}
              title="No seat is mid-turn"
              description={
                seatCount
                  ? "Every seat is attached to its mailbox and waiting. Work arrives from a webhook, a schedule, or a colleague."
                  : "No agent seats are defined. Import a company configuration to spawn some."
              }
            />
          )}
        </Card>

        <Card>
          <Card.Header
            icon={<ChartNoAxesGanttGlyph size="sm" />}
            actions={
              <Button size="small" variant="ghost" onClick={() => nav.to(["live", "events"])}>
                Event log
              </Button>
            }
          >
            <Card.Title>{`Activity · last ${windowLabel(range.window)}`}</Card.Title>
          </Card.Header>
          <div className="col gap-3">
            {/* THEIR STRIP IS ONE PICTURE WITH A NAME. Ours was sixty bare
                divs with a per-cell `title` and no accessible name at all,
                and it scaled opacity with the value as well as height —
                which is the defect their own doc names, since the quietest
                buckets then sat under 3:1. The per-cell tooltip is what is
                lost; `summary` says the peak and the total instead. */}
            <ActivityStrip
              buckets={strip}
              label={`Events over the last ${windowLabel(range.window)}`}
              summary={({ peak, total, buckets }) =>
                `${plural(total, "event")} over ${buckets} buckets, ${peak} in the busiest.`
              }
            />
            {stripTruncated && (
              <span className="t-caption">
                This tab keeps the last {MAX_EVENTS} events, matching the engine's own feed
                retention — the earliest minutes here are cut off rather than quiet.
              </span>
            )}
            <div className="list">
              {events.slice(0, 7).map((ev) => (
                <EventRow key={ev.id} event={ev} />
              ))}
              {!events.length && (
                <EmptyState
                  size="compact"
                  icon={<ChartNoAxesGanttGlyph size={32} />}
                  title="Nothing has happened yet"
                  description="The feed fills as the engine publishes. A company with no integrations and no schedules has nothing to react to."
                />
              )}
            </div>
          </div>
        </Card>
      </div>

      {/* WHAT IS BEING WATCHED: the conditions whose home is here
          (`lib/attention.ts`, `where: live`) — a round that has stopped
          moving, and every coding run parked on a question. They are watched
          rather than decided, and a run waiting on a PERSON already reaches
          them in their own Inbox, so this is the company-wide view of both.
          Always drawn, for the reason every section here is. */}
      <Card padding="none">
        <Card.Header icon={<ClockGlyph size="sm" />} count={watching.length}>
          <Card.Title>Being watched</Card.Title>
        </Card.Header>
        {watching.length > 0 ? (
          <ul className="live-watch">
            {watching.map((item) => (
              <li key={item.id} className="live-watch-row">
                <span className="attention-icon" data-severity={item.severity}>
                  <Mark name={item.icon} size="sm" />
                </span>
                <span className="col live-watch-body">
                  <strong className="t-cell">{item.title}</strong>
                  <span className="t-caption">{item.detail}</span>
                </span>
                {item.path && (
                  <a className="t-link" href={href(item.path, item.query)}>
                    Open
                  </a>
                )}
              </li>
            ))}
          </ul>
        ) : (
          <EmptyState
            size="compact"
            icon={<ClockGlyph size={32} />}
            title="Nothing is being watched"
            description={`Checked and clear: ${watchedIn("live")}.`}
          />
        )}
      </Card>

      {/* WHAT IS IN A BOX, as rows rather than as one integer.
          The tile above has always counted these and the screen's own note
          promises "what is running in a box", and a count is the one shape
          that cannot answer which run or whose. Always drawn, empty included,
          for the reason every other section here is: a dashboard whose
          sections come and go with the data cannot be read at a glance. */}
      <Card padding="none">
        <Card.Header
          icon={<SquareTerminalGlyph size="sm" />}
          count={inFlight.length}
          subtitle="detached coding runs that have not finished"
          actions={
            <Button size="small" variant="ghost" onClick={() => nav.to(["live", "runs"])}>
              All runs
            </Button>
          }
        >
          <Card.Title>In a box</Card.Title>
        </Card.Header>
        {inFlight.length > 0 ? (
          <div className="list">
            {inFlight.slice(0, IN_BOX_ROWS).map((run) => (
              // A REAL LINK to the run's page, peeking on a plain click — the
              // same rule every row in the product follows, written once in
              // `rowPeekHandler`.
              <a
                key={run.turn_id}
                className="list-row clickable"
                href={peekHref({ kind: "run", id: run.turn_id })}
                onClick={rowPeekHandler(() => openPeek({ kind: "run", id: run.turn_id }))}
              >
                <RunStatus status={run.status} />
                <span className="col" style={{ gap: 0, minWidth: 0, flex: 1 }}>
                  <span className="truncate t-cell">
                    {run.task_description || "No task was recorded"}
                  </span>
                  <span className="truncate t-caption">
                    {run.role || run.agent_handle}
                    {run.coding_agent ? ` · ${run.coding_agent}` : ""}
                  </span>
                </span>
                <span className="t-caption nowrap">{relTime(run.started_at, now)}</span>
              </a>
            ))}
            {/* WHAT IS NOT SHOWN, said rather than left to be inferred: a list
                cut at eight with no note reads as a company with eight runs. */}
            {inFlight.length > IN_BOX_ROWS && (
              <a className="list-row clickable" href={href(["live", "runs"])}>
                <span className="t-caption">
                  {plural(inFlight.length - IN_BOX_ROWS, "more run")} in a box — all runs ↗
                </span>
              </a>
            )}
          </div>
        ) : (
          <EmptyState
            size="compact"
            icon={<SquareTerminalGlyph size={32} />}
            title="Nothing is running in a box"
            description="A coding run starts when a seat calls the sandbox tool. Every finished one is still in the record under Runs."
          />
        )}
      </Card>

      <div className="grid grid-auto-lg">
        <Card>
          <Card.Header
            icon={<CoinsGlyph size="sm" />}
            subtitle={tokens ? spanWords(tokens.since, tokens.until) : undefined}
            actions={
              <Button size="small" variant="ghost" onClick={() => nav.to(["spend"])}>
                Spend
              </Button>
            }
          >
            <Card.Title>Spend by phase</Card.Title>
          </Card.Header>
          <div className="col gap-3">
            <BarList data={phaseSpend} emptyLabel="No model calls in this window." />
            {phaseSpend.length > 0 && (
              <Legend
                items={phaseSpend.map((p) => ({ id: p.id, label: p.label, color: p.color }))}
              />
            )}
            {orgMeter && orgMeter.windows.length > 0 && (
              // THE CALENDAR WINDOWS, not the spend window above: each bar is
              // one capped window of the company's shared counter against its
              // own ceiling, in the colour the engine judged it.
              <WindowMeters windows={orgMeter.windows} whose="The company's" />
            )}
          </div>
        </Card>

        <Card>
          <Card.Header
            icon={<UsersGlyph size="sm" />}
            subtitle={tokens ? spanWords(tokens.since, tokens.until) : undefined}
          >
            <Card.Title>Top seats by spend</Card.Title>
          </Card.Header>
          <BarList data={topSeats} emptyLabel="No seat has spent tokens in this window." />
        </Card>
      </div>

      <Section
        title="Getting more out of this"
        hint="every one of these is a real screen backed by a real answer"
      >
        <div className="grid grid-auto">
          {[
            {
              icon: <BrainGlyph size="sm" />,
              title: "Turns",
              body: "Every phase the models ran, round by round, with the tools each round called and the prompts they saw.",
              path: ["live", "turns"],
            },
            {
              icon: <LinkGlyph size="sm" />,
              title: "Agent-to-agent",
              body: "The private channels seats opened with each other: one ask, one answer, then closed.",
              path: ["live", "a2a"],
            },
            {
              icon: <BookOpenGlyph size="sm" />,
              title: "Knowledge",
              body: "Search the company knowledge base the way an agent does, and read what each seat has learned for itself.",
              path: ["knowledge"],
            },
          ].map((card) => (
            <a key={card.title} className="seat-card" href={href(card.path)}>
              <div className="row">
                <span className="attention-icon" data-severity="info">
                  {card.icon}
                </span>
                <strong className="t-body">{card.title}</strong>
                <span className="spacer" />
                <ArrowRightGlyph size="sm" />
              </div>
              <span className="t-caption">{card.body}</span>
            </a>
          ))}
        </div>
      </Section>
    </>
  );
}
