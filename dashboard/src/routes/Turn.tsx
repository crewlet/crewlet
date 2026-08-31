/**
 * One turn, end to end.
 *
 * A long self-iterating turn pushes its own earlier phases out of any per-seat
 * window, so "show me everything that happened in THIS turn" needs to be a
 * question of its own — which is what the `turn` query answers. It carries
 * more than the phases: the fallbacks, the guard breaches, the missing-tool
 * notices and the turn's own completion record all name the same turn id and
 * render as anonymous rows in the log otherwise.
 */

import { useMemo } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { useNavigator } from "~/app/router.tsx";
import { EventRow, QueryState, SeatChip } from "~/components/common.tsx";
import { PhaseCard } from "~/components/PhaseCard.tsx";
import { Badge, Button, Panel, Skeleton, Stat, StatRow } from "~/ui/primitives.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtCount, fmtDateTime, fmtDuration, oldestFirst, tsKey } from "~/lib/format.ts";
import { fromPhaseEvent, type PhaseRecord } from "~/lib/phases.ts";
import type { EventRecord, FeedRow } from "~/protocol/index.ts";

export function TurnScreen({ turnId }: { turnId: string }) {
  const nav = useNavigator();
  const { data, loading, error } = useQuery("turn", { turn_id: turnId });

  const events = useMemo(() => [...(data?.events ?? [])].sort(oldestFirst), [data]);

  const phases = useMemo<PhaseRecord[]>(
    () =>
      events
        .filter((e) => e.type === "agent_phase_completed")
        .map((e) => fromPhaseEvent(e))
        .filter((r): r is PhaseRecord => r !== null)
        // Within a turn, oldest first: a turn is read forwards.
        .sort((a, b) => tsKey(a.at) - tsKey(b.at)),
    [events],
  );

  const completed = events.find((e) => e.type === "agent_turn_completed");
  const other = events.filter(
    (e) => e.type !== "agent_phase_completed" && e.type !== "agent_turn_progress",
  );

  const role = phases[0]?.role ?? (completed?.actor || "");
  const from = events.length ? tsKey(events[0]!.timestamp) : 0;
  const to = events.length ? tsKey(events[events.length - 1]!.timestamp) : 0;
  // The turn's own record carries an exact wall clock; the span between its
  // first and last event is the fallback, and the screen says which it used.
  const durationMs = (completed?.payload?.duration_ms as number | undefined) ?? null;

  return (
    <>
      <ScreenHead
        title="Turn"
        sub={<code className="inline">{turnId}</code>}
        badges={
          <>
            {role && <Badge outline>{role}</Badge>}
            <Badge outline>{phases.length} phases</Badge>
            {phases.some((p) => p.failed) && <Badge tone="critical">failed</Badge>}
          </>
        }
        actions={
          <>
            {role && (
              <Button size="sm" icon="user" onClick={() => nav.to(["seats", role])}>
                The seat
              </Button>
            )}
            {events[0]?.trace_id && (
              <Button
                size="sm"
                icon="gitBranch"
                onClick={() => nav.to(["traces", events[0]!.trace_id])}
              >
                Trace
              </Button>
            )}
          </>
        }
      />

      {loading && <Skeleton rows={6} />}
      <QueryState
        error={error}
        loading={loading}
        empty={
          events.length
            ? undefined
            : {
                title: "No events for this turn",
                hint: "A turn is assembled from the events that name its id. If it ran outside the store's 30-day window there is nothing to assemble.",
              }
        }
      >
        <Panel padding="none">
          <StatRow cols={4}>
            <Stat
              icon="user"
              label="Seat"
              value={role ? <SeatChip name={role} handle={role} size="md" /> : "—"}
              sub={
                completed?.payload?.conversation_key
                  ? String(completed.payload.conversation_key)
                  : ""
              }
            />
            <Stat
              icon="clock"
              label="Took"
              value={
                durationMs != null
                  ? fmtDuration(durationMs)
                  : to > from
                    ? fmtDuration(to - from)
                    : "—"
              }
              sub={
                durationMs != null
                  ? "the engine's own measurement"
                  : "spanning the turn's first and last event"
              }
            />
            <Stat
              icon="coin"
              label="Tokens"
              value={fmtCount(phases.reduce((n, p) => n + p.totalTokens, 0))}
              sub={`${phases.reduce((n, p) => n + p.tools.length, 0)} tool calls`}
            />
            <Stat
              icon="check"
              label="Review outcome"
              value={String(completed?.payload?.review_outcome ?? "—")}
              sub={completed ? "from the turn's own record" : "no turn_completed event"}
            />
          </StatRow>
        </Panel>

        <Panel title="Phases" icon="brain" count={phases.length} padding="tight">
          <div className="col gap-2">
            {phases.map((p, i) => (
              <PhaseCard key={p.key} record={p} defaultOpen={i === 0} />
            ))}
            {!phases.length && (
              <span className="t-caption">
                No phase completed in this turn — it may have died before its first phase published.
              </span>
            )}
          </div>
        </Panel>

        <Panel
          title="Everything else this turn published"
          icon="activity"
          count={other.length}
          subtitle="fallbacks, guard breaches, deliveries — anonymous rows in the log, in context here"
          padding="none"
        >
          <div className="list">
            {other.map((e) => (
              <EventRow key={e.id} event={e as unknown as FeedRow} showDate />
            ))}
            {!other.length && (
              <div className="empty inline">
                <span className="empty-title">Nothing but the phases</span>
                <span className="empty-sub">
                  No fallback, no guard breach, no separate delivery.
                </span>
              </div>
            )}
          </div>
        </Panel>

        {completed && (
          <Panel title="Turn record" icon="file" subtitle={fmtDateTime(completed.timestamp)}>
            <pre className="code">{JSON.stringify(completed.payload, null, 2)}</pre>
          </Panel>
        )}
      </QueryState>
    </>
  );
}
