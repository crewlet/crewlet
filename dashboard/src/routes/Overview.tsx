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
import { AttentionRow, EventRow, SeatCard } from "~/components/common.tsx";
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
import { indexOrg, runState } from "~/lib/seats.ts";
import { fmtCount, plural, tsKey } from "~/lib/format.ts";
import { phaseColor } from "~/lib/phases.ts";
import { MAX_EVENTS } from "~/protocol/index.ts";
import {
  ActivityStrip,
  AutoGrid,
  BarList,
  Button,
  Card,
  EmptyState,
  Legend,
  Meter,
  PageHeader,
  Section,
  Stack,
  StatCard,
  StatGroup,
  Tag,
  useNow,
} from "@crewlethq/ui";
import {
  ArrowForwardGlyph,
  BoltGlyph,
  Book2Glyph,
  CheckGlyph,
  FlagGlyph,
  GroupGlyph,
  LinkGlyph,
  MoreVertGlyph,
  NeurologyGlyph,
  ScheduleGlyph,
  TerminalGlyph,
  TimelineGlyph,
  TokenGlyph,
} from "@crewlethq/icons/glyphs";

/** How far back the activity strip reaches, and how finely it is cut. */
const STRIP_MINUTES = 60;

export function Overview() {
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

  const index = useMemo(() => indexOrg(org), [org]);
  const attention = useMemo(
    () =>
      attentionQueue({
        agents,
        sandboxes,
        budget,
        engine: engine ?? null,
        seats: index.seats,
        connected,
        authRejected,
        now,
      }),
    [agents, sandboxes, budget, engine, index.seats, connected, authRejected, now],
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

  // The activity strip is bucketed by MINUTE and keyed by the bucket, never by
  // index: keying 60 cells `p0..p59` over a window recomputed from the clock
  // shifts every cell's content one position left on each minute roll, and
  // rewrites the lot.
  const strip = useMemo(() => {
    const minute = 60_000;
    const end = Math.floor(now / minute) * minute;
    const buckets = new Map<number, number>();
    for (let i = STRIP_MINUTES - 1; i >= 0; i--) buckets.set(end - i * minute, 0);
    for (const ev of events) {
      const t = Math.floor(tsKey(ev.timestamp) / minute) * minute;
      if (buckets.has(t)) buckets.set(t, (buckets.get(t) ?? 0) + 1);
    }
    return [...buckets.entries()].map(([t, v]) => ({ t, v }));
  }, [events, now]);

  // The feed's own retention is the limit of what this panel can HONESTLY
  // claim: 400 events fill in minutes on a busy company, so a strip covering
  // an hour has to say where the record actually starts rather than drawing
  // the gap as quiet.
  const oldestHeld = events.length ? tsKey(events[events.length - 1]!.timestamp) : 0;
  const stripTruncated = events.length >= MAX_EVENTS && oldestHeld > now - STRIP_MINUTES * 60_000;

  const seatCount = index.seats.filter((s) => s.kind === "agent").length;
  const humanCount = index.seats.length - seatCount;
  const idle = agents.filter((a) => runState(a, sandboxes) === "idle").length;
  const orgMeter = budget?.org;

  const phaseSpend = useMemo(
    () =>
      (tokens?.by_phase ?? [])
        .map((p) => ({
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
          id: a.agent_id || a.role,
          label: a.role,
          value: a.total_tokens,
          display: fmtCount(a.total_tokens),
          href: href(["seats", a.handle || a.role]),
        })),
    [tokens],
  );

  return (
    <>
      <PageHeader
        title={org?.name || "Your company"}
        description={
          org?.mission ||
          "The engine is running. This screen answers what needs a person, what the company is doing, and what it has cost."
        }
        badges={
          <>
            <Tag appearance="outline">{plural(seatCount, "agent seat")}</Tag>
            {humanCount > 0 && <Tag appearance="outline">{plural(humanCount, "human")}</Tag>}
          </>
        }
        actions={
          <>
            <Button
              variant="secondary"
              leadingIcon={<GroupGlyph />}
              onClick={() => nav.to(["people"])}
            >
              People
            </Button>
            <Button
              variant="secondary"
              leadingIcon={<NeurologyGlyph />}
              onClick={() => nav.to(["model"])}
            >
              Model activity
            </Button>
          </>
        }
      />

      {/* 1. What needs a person. Always first, always present. */}
      <Card as="section" padding="none">
        <Card.Header
          divided
          icon={<FlagGlyph size="sm" />}
          count={attention.length}
          actions={
            attention.length > 0 ? (
              <span className="t-caption">most costly to ignore first</span>
            ) : undefined
          }
        >
          <Card.Title>Needs a person</Card.Title>
        </Card.Header>
        {attention.length ? (
          <Stack gap={0}>
            {attention.slice(0, 8).map((item) => (
              <AttentionRow key={item.id} item={item} />
            ))}
            {attention.length > 8 && (
              <div className="attention-row" data-severity="info">
                <span className="attention-icon">
                  <MoreVertGlyph size="sm" />
                </span>
                <span className="t-caption">
                  and {attention.length - 8} more — every one of them is on the screen it belongs
                  to.
                </span>
              </div>
            )}
          </Stack>
        ) : (
          <EmptyState
            size="compact"
            icon={<CheckGlyph />}
            title="Nothing is waiting on you"
            description="No stopped seats, no paused coding runs, no budget refusing charges, and a company configuration is active."
          />
        )}
      </Card>

      {/* 2. What the company is doing. */}
      <StatGroup columns={4}>
        <StatCard
          icon={<BoltGlyph />}
          label="Working now"
          value={live.length}
          sub={
            live.length
              ? live.map(({ seat }) => seat.name).join(", ")
              : `${plural(idle, "seat")} idle and waiting for work`
          }
        />
        <StatCard
          icon={<TerminalGlyph />}
          label="Coding runs"
          value={sandboxes.length}
          sub={
            sandboxes.filter((s) => s.status === "awaiting_input").length
              ? `${plural(sandboxes.filter((s) => s.status === "awaiting_input").length, "run")} paused on a question`
              : "detached sandbox runs in flight"
          }
        />
        <StatCard
          icon={<TimelineGlyph />}
          label={`Events · last ${STRIP_MINUTES}m`}
          value={fmtCount(strip.reduce((n, b) => n + b.v, 0))}
          sub={
            stripTruncated
              ? "the tab holds the last 400 events, so this hour is partial"
              : "everything the engine published"
          }
        />
        <StatCard
          icon={<TokenGlyph />}
          label={tokens ? `Tokens · ${tokens.since_days}d` : "Tokens"}
          loading={!tokens}
          loadingLabel="Loading the token total"
          value={tokens ? fmtCount(tokens.totals.total_tokens) : null}
          sub={
            tokens
              ? `${tokens.totals.calls.toLocaleString()} model calls`
              : "no spend has been recorded yet"
          }
        />
      </StatGroup>

      <div className="grid grid-auto-lg">
        <Card as="section">
          <Card.Header
            icon={<GroupGlyph size="sm" />}
            count={live.length}
            actions={
              <Button size="small" variant="tertiary" onClick={() => nav.to(["people"])}>
                All seats
              </Button>
            }
          >
            <Card.Title>Live seats</Card.Title>
          </Card.Header>
          {live.length ? (
            <AutoGrid min="lg" gap={3}>
              {live.map(({ seat, agent }) => (
                <SeatCard key={seat.key} seat={seat} agent={agent} sandboxes={sandboxes} />
              ))}
            </AutoGrid>
          ) : (
            <EmptyState
              size="compact"
              icon={<ScheduleGlyph />}
              title="No seat is mid-turn"
              description={
                seatCount
                  ? "Every seat is attached to its mailbox and waiting. Work arrives from a webhook, a schedule, or a colleague."
                  : "No agent seats are defined. Import a company configuration to spawn some."
              }
            />
          )}
        </Card>

        <Card as="section">
          <Card.Header
            icon={<TimelineGlyph size="sm" />}
            actions={
              <Button size="small" variant="tertiary" onClick={() => nav.to(["activity"])}>
                Event log
              </Button>
            }
          >
            <Card.Title>{`Activity · last ${STRIP_MINUTES} minutes`}</Card.Title>
          </Card.Header>
          <div className="col gap-3">
            <ActivityStrip
              buckets={strip}
              label={`Events published in the last ${STRIP_MINUTES} minutes`}
            />
            {stripTruncated && (
              <span className="t-caption">
                This tab keeps the last {MAX_EVENTS} events, matching the engine's own feed
                retention — the earliest minutes here are cut off rather than quiet.
              </span>
            )}
            <Stack gap={0}>
              {events.slice(0, 7).map((ev) => (
                <EventRow key={ev.id} event={ev} />
              ))}
              {!events.length && (
                <EmptyState
                  size="compact"
                  icon={<TimelineGlyph />}
                  title="Nothing has happened yet"
                  description="The feed fills as the engine publishes. A company with no integrations and no schedules has nothing to react to."
                />
              )}
            </Stack>
          </div>
        </Card>
      </div>

      <div className="grid grid-auto-lg">
        <Card as="section">
          <Card.Header
            icon={<TokenGlyph size="sm" />}
            subtitle={tokens ? `${tokens.since_days}-day window` : undefined}
            actions={
              <Button size="small" variant="tertiary" onClick={() => nav.to(["spend"])}>
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
            {orgMeter && orgMeter.max > 0 && (
              <Meter
                value={orgMeter.used}
                max={orgMeter.max}
                label={
                  <span title="a process-lifetime meter — not comparable to the spend window above">
                    Company budget meter
                  </span>
                }
                valueText={`${fmtCount(orgMeter.used)} / ${fmtCount(orgMeter.max)}`}
              />
            )}
          </div>
        </Card>

        <Card as="section">
          <Card.Header
            icon={<GroupGlyph size="sm" />}
            subtitle={tokens ? `${tokens.since_days}-day window` : undefined}
          >
            <Card.Title>Top seats by spend</Card.Title>
          </Card.Header>
          <BarList data={topSeats} emptyLabel="No seat has spent tokens in this window." />
        </Card>
      </div>

      <Section
        title="Getting more out of this"
        description="every one of these is a real screen backed by a real answer"
      >
        <div className="grid grid-auto">
          {[
            {
              icon: NeurologyGlyph,
              title: "Model activity",
              body: "Every phase the models ran, round by round, with the tools each round called and the prompts they saw.",
              path: ["model"],
            },
            {
              icon: LinkGlyph,
              title: "Agent-to-agent",
              body: "The private channels seats opened with each other: one ask, one answer, then closed.",
              path: ["conversations"],
            },
            {
              icon: Book2Glyph,
              title: "Knowledge",
              body: "Search the company knowledge base the way an agent does, and read what each seat has learned for itself.",
              path: ["knowledge"],
            },
          ].map((card) => (
            <Card key={card.title} variant="subtle" href={href(card.path)}>
              <Stack gap={2}>
                <div className="row">
                  <card.icon size="sm" />
                  <strong className="t-body">{card.title}</strong>
                  <span className="spacer" />
                  <ArrowForwardGlyph size="sm" />
                </div>
                <span className="t-caption">{card.body}</span>
              </Stack>
            </Card>
          ))}
        </div>
      </Section>
    </>
  );
}
