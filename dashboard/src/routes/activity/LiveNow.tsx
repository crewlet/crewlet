/**
 * The landing screen.
 *
 * It replaces a board that answered "is the engine up" and nothing about the
 * work: five bands delivering nine scalars, four of which were summaries of
 * other screens, and NO PER-SEAT INFORMATION AT ALL — no seat list, no phase,
 * no model. It also moved on its own: a section appeared between two others
 * whenever a turn crossed a staleness threshold, the stat strip reflowed from
 * one column to three as tiles came and went, and on disconnect the whole
 * record card collapsed to a paragraph.
 *
 * This one answers the three questions an operator actually arrives with, in
 * that order:
 *
 *   1. Is anything waiting on me?          → the attention queue, first
 *   2. What is my company doing right now?  → live seats, and what each is on
 *   3. What has it been doing?              → throughput, spend, the feed
 *
 * The layout is FIXED. Every section is always present, in the same place, in
 * the same size, whether or not it has anything in it — an empty one says so.
 * A dashboard whose sections move when the data moves cannot be read at a
 * glance, which is the only way this screen is ever read.
 */

import { useMemo } from "react";
import { href, useNavigator } from "~/app/router.tsx";
import { AttentionRow, EventRow, SeatCard, Section } from "~/components/common.tsx";
import { Badge, Button, Empty, Meter, Panel, Stat, StatRow } from "~/ui/primitives.tsx";
import { ActivityStrip, BarList, Legend, phaseColor } from "~/ui/charts.tsx";
import { Icon } from "~/ui/Icon.tsx";
import {
  useAgents,
  useEvents,
  useOrg,
  useOrgBudget,
  useSandboxes,
  useTokens,
  useConnection,
} from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { attentionQueue } from "~/lib/attention.ts";
import { awaitingPerson, indexOrg, runState } from "~/lib/seats.ts";
import { fmtCount, plural, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { MAX_EVENTS } from "~/protocol/index.ts";
import { cutInto, spanOf, spanWords, useTimeRange, windowLabel } from "~/lib/range.ts";
import type { Offer } from "~/lib/range.ts";
import { TimeRangePicker } from "~/ui/TimeRange.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

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

export function LiveNow() {
  const nav = useNavigator();
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const events = useEvents();
  const org = useOrg();
  const tokens = useTokens();
  const budget = useOrgBudget();
  const { connected, authRejected } = useConnection();
  const now = useNow();
  const { data: engine } = useQuery("stream", undefined, { pollMs: 15_000 });
  // THE DURABLE CODING RUNS, the same source the Inbox reads for the same
  // reason: the live projection sweeps a parked run after twelve hours, and
  // two screens computing one queue from two sources would disagree about
  // whether anybody is waiting.
  const { data: runs } = useQuery("sandbox_runs", undefined, { pollMs: 30_000 });
  // NOT ALIGNED to a bucket: the strip's cell is its own, finer than either of
  // the engine's, and its newest cell is the minute in progress.
  const range = useTimeRange(now, STRIP_OFFER, false);

  const index = useMemo(() => indexOrg(org), [org]);
  const attention = useMemo(
    () =>
      attentionQueue({
        agents,
        sandboxes,
        runs: runs?.runs ?? [],
        budget,
        engine: engine ?? null,
        seats: index.seats,
        connected,
        authRejected,
        now,
      }),
    [agents, sandboxes, runs, budget, engine, index.seats, connected, authRejected, now],
  );

  const live = useMemo(
    () =>
      index.seats
        .filter((s) => s.kind === "agent")
        .map((seat) => ({ seat, agent: agents.find((a) => a.role === seat.name) }))
        .filter(({ agent }) => {
          const state = runState(agent, sandboxes);
          return state === "working" || state === "awaiting_sandbox";
        }),
    [index.seats, agents, sandboxes],
  );

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
  const idle = agents.filter((a) => runState(a, sandboxes) === "idle").length;
  const orgMeter = budget?.org;

  const phaseSpend = useMemo(
    () =>
      (tokens?.by_phase ?? [])
        .map((p) => ({
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
          label: a.role,
          value: a.total_tokens,
          display: fmtCount(a.total_tokens),
          href: href(["company", "people", a.handle || a.role]),
        })),
    [tokens, nav],
  );

  return (
    <>
      <PageActions>
        {
          <>
            <Badge outline>{plural(seatCount, "agent seat")}</Badge>
            {humanCount > 0 && <Badge outline>{plural(humanCount, "human")}</Badge>}
            <TimeRangePicker range={range} ariaLabel="Activity window" />
            <Button icon="brain" onClick={() => nav.to(["activity", "turns"])}>
              Turns
            </Button>
          </>
        }
      </PageActions>
      <PageNote>
        What the company is doing at this moment — which seats are working, what is running in a
        box, and what it is costing. WHAT NEEDS A PERSON is not here: it is the Inbox, because a
        condition waiting on somebody is a claim on them rather than a statistic about the company.
      </PageNote>

      {/* 2. What the company is doing. */}
      <Panel padding="none">
        <StatRow cols={4}>
          <Stat
            icon="zap"
            label="Working now"
            value={live.length}
            sub={
              live.length
                ? live.map(({ seat }) => seat.name).join(", ")
                : `${plural(idle, "seat")} idle and waiting for work`
            }
          />
          <Stat
            icon="terminal"
            label="Coding runs"
            value={sandboxes.length}
            sub={
              sandboxes.filter((s) => awaitingPerson(s.status)).length
                ? `${plural(sandboxes.filter((s) => awaitingPerson(s.status)).length, "run")} paused on a question`
                : "detached sandbox runs in flight"
            }
          />
          <Stat
            icon="activity"
            label={`Events · last ${windowLabel(range.window)}`}
            value={fmtCount(strip.reduce((n, b) => n + b.v, 0))}
            sub={
              stripTruncated
                ? "the tab holds the last 400 events, so this hour is partial"
                : "everything the engine published"
            }
          />
          <Stat
            icon="coin"
            label="Tokens"
            value={tokens ? fmtCount(tokens.totals.total_tokens) : "—"}
            sub={
              tokens
                ? `${tokens.totals.calls.toLocaleString()} model calls`
                : "no spend has been recorded yet"
            }
          />
        </StatRow>
      </Panel>

      <div className="grid grid-auto-lg">
        <Panel
          title="Live seats"
          icon="users"
          count={live.length}
          actions={
            <Button size="sm" variant="ghost" onClick={() => nav.to(["company", "people"])}>
              All seats
            </Button>
          }
        >
          {live.length ? (
            <div className="seat-grid">
              {live.map(({ seat, agent }) => (
                <SeatCard key={seat.handle} seat={seat} agent={agent} sandboxes={sandboxes} />
              ))}
            </div>
          ) : (
            <Empty
              inline
              icon="clock"
              title="No seat is mid-turn"
              hint={
                seatCount
                  ? "Every seat is attached to its mailbox and waiting. Work arrives from a webhook, a schedule, or a colleague."
                  : "No agent seats are defined. Import a company configuration to spawn some."
              }
            />
          )}
        </Panel>

        <Panel
          title={`Activity · last ${windowLabel(range.window)}`}
          icon="activity"
          actions={
            <Button size="sm" variant="ghost" onClick={() => nav.to(["activity"])}>
              Event log
            </Button>
          }
        >
          <div className="col gap-3">
            <ActivityStrip
              buckets={strip}
              title={(b) =>
                `${new Date(b.t).toLocaleTimeString()} — ${b.v} event${b.v === 1 ? "" : "s"}`
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
                <Empty
                  inline
                  icon="activity"
                  title="Nothing has happened yet"
                  hint="The feed fills as the engine publishes. A company with no integrations and no schedules has nothing to react to."
                />
              )}
            </div>
          </div>
        </Panel>
      </div>

      <div className="grid grid-auto-lg">
        <Panel
          title="Spend by phase"
          icon="coin"
          subtitle={tokens ? spanWords(tokens.since, tokens.until) : undefined}
          actions={
            <Button size="sm" variant="ghost" onClick={() => nav.to(["cost"])}>
              Spend
            </Button>
          }
        >
          <div className="col gap-3">
            <BarList data={phaseSpend} emptyLabel="No model calls in this window." />
            {phaseSpend.length > 0 && (
              <Legend items={phaseSpend.map((p) => ({ label: p.label, color: p.color }))} />
            )}
            {orgMeter && orgMeter.max > 0 && (
              <Meter
                used={orgMeter.used}
                max={orgMeter.max}
                ariaLabel="Company budget meter"
                fullMeans="spent"
                label={
                  <span title="a process-lifetime meter — not comparable to the spend window above">
                    Company budget meter
                  </span>
                }
                right={`${fmtCount(orgMeter.used)} / ${fmtCount(orgMeter.max)}`}
              />
            )}
          </div>
        </Panel>

        <Panel
          title="Top seats by spend"
          icon="users"
          subtitle={tokens ? spanWords(tokens.since, tokens.until) : undefined}
        >
          <BarList data={topSeats} emptyLabel="No seat has spent tokens in this window." />
        </Panel>
      </div>

      <Section
        title="Getting more out of this"
        hint="every one of these is a real screen backed by a real answer"
      >
        <div className="grid grid-auto">
          {[
            {
              icon: "brain" as const,
              title: "Turns",
              body: "Every phase the models ran, round by round, with the tools each round called and the prompts they saw.",
              path: ["activity", "turns"],
            },
            {
              icon: "link" as const,
              title: "Agent-to-agent",
              body: "The private channels seats opened with each other: one ask, one answer, then closed.",
              path: ["activity", "a2a"],
            },
            {
              icon: "book" as const,
              title: "Knowledge",
              body: "Search the company knowledge base the way an agent does, and read what each seat has learned for itself.",
              path: ["knowledge"],
            },
          ].map((card) => (
            <a key={card.title} className="seat-card" href={href(card.path)}>
              <div className="row">
                <span className="attention-icon" data-severity="info">
                  <Icon name={card.icon} size="sm" />
                </span>
                <strong className="t-body">{card.title}</strong>
                <span className="spacer" />
                <Icon name="arrowRight" size="sm" />
              </div>
              <span className="t-caption">{card.body}</span>
            </a>
          ))}
        </div>
      </Section>
    </>
  );
}
