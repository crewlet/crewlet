/**
 * Model activity — every phase the models ran, round by round.
 *
 * This is the screen the previous dashboard did not have. Its transcript lived
 * inside one seat's page, capped at fifty rows with no pager, no filter and no
 * search, while the events behind it sat in the store addressable by id. So
 * the two questions people actually ask about an agent company — *what is my
 * money being spent on* and *what did the model actually do* — had no screen.
 *
 * Live phases and finished ones are ONE list here, keyed identically
 * (`turn|phase|iteration`), so a phase that completes updates in place rather
 * than disappearing from the top of the list and reappearing further down.
 */

import { useCallback, useMemo, useState } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { TurnCard } from "~/components/TurnCard.tsx";
import { PhaseCard } from "~/components/PhaseCard.tsx";
import {
  Badge,
  Button,
  Chip,
  Empty,
  Panel,
  SearchInput,
  Segmented,
  Skeleton,
  Stat,
  StatRow,
} from "~/ui/primitives.tsx";
import { BarList, phaseColor } from "~/ui/charts.tsx";
import { useAgents, useClient, useTokens } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import {
  fromLiveCall,
  fromPhaseEvent,
  groupTurns,
  mergePhases,
  type PhaseRecord,
} from "~/lib/phases.ts";
import type { EventRecord } from "~/protocol/index.ts";

const PAGE = 60;
const PHASES = ["plan", "execute", "review", "auxiliary"] as const;

export function ModelActivity() {
  const { socket } = useClient();
  const agents = useAgents();
  const tokens = useTokens();
  const [phase, setPhase] = useParam("phase", "");
  const [role, setRole] = useParam("role", "");
  const [grouping, setGrouping] = useParam("by", "turn", "section");
  const [onlyFailed, setOnlyFailed] = useParam("failed", "");

  const [older, setOlder] = useState<EventRecord[]>([]);
  const [cursor, setCursor] = useState<{ before_time?: string; before_id?: string } | null>(null);
  const [exhausted, setExhausted] = useState(false);
  const [paging, setPaging] = useState(false);
  const [pageError, setPageError] = useState<string | null>(null);

  // `phases`, not `events?type=…`. The event listing deliberately never
  // selects the payload — a page of ordinary events with every payload
  // attached is the query that makes an activity screen slow — and a phase
  // record without its payload has no prompts, no response, no tool calls and
  // no decision, which is everything this screen is for.
  const { data, loading, error } = useQuery("phases", {
    limit: PAGE,
    ...(role ? { role } : {}),
  });

  const stored = useMemo<PhaseRecord[]>(
    () =>
      [...(data?.phases ?? []), ...older]
        .map((row) => fromPhaseEvent(row))
        .filter((r): r is PhaseRecord => r !== null),
    [data, older],
  );

  const live = useMemo<PhaseRecord[]>(
    () =>
      agents
        .filter((a) => a.live_call)
        .map((a) => fromLiveCall(a.live_call!, a.role))
        .filter((r) => !role || r.role === role),
    [agents, role],
  );

  const merged = useMemo(() => mergePhases(stored, live), [stored, live]);

  const filtered = useMemo(
    () => merged.filter((r) => !phase || r.phase === phase).filter((r) => !onlyFailed || r.failed),
    [merged, phase, onlyFailed],
  );

  const turns = useMemo(() => groupTurns(filtered), [filtered]);

  const loadOlder = useCallback(async () => {
    setPaging(true);
    setPageError(null);
    try {
      const params: Record<string, unknown> = { type: "agent_phase_completed", limit: PAGE };
      if (role) params.actor = role;
      const last = stored[stored.length - 1];
      if (cursor) {
        params.before_time = cursor.before_time;
        params.before_id = cursor.before_id;
      } else if (last) {
        params.before_time = last.at;
        params.before_id = last.eventId;
      }
      const page = await socket.query("events", params);
      // A feed row has no payload; a phase card needs one. Each row is read
      // back through `event`, in parallel and bounded by the page size.
      const full = await Promise.all(
        (page.events ?? []).map((row) => socket.query("event", { id: row.id }).catch(() => null)),
      );
      setOlder((prev) => [...prev, ...full.filter((e): e is EventRecord => e !== null)]);
      setCursor(page.next ?? null);
      setExhausted(page.exhausted || !page.next);
    } catch (err) {
      setPageError(err instanceof Error ? err.message : "query_failed");
    } finally {
      setPaging(false);
    }
  }, [socket, cursor, stored, role]);

  const roles = useMemo(
    () => [...new Set(agents.map((a) => a.role).filter(Boolean))].sort(),
    [agents],
  );

  const liveCount = live.filter((r) => r.live).length;
  const phaseSpend = (tokens?.by_phase ?? [])
    .map((p) => ({
      label: p.phase,
      value: p.total_tokens,
      display: fmtCount(p.total_tokens),
      color: phaseColor(p.phase),
      sub: `${p.calls.toLocaleString()} calls · ${fmtCount(Math.round(p.total_tokens / Math.max(1, p.calls)))} avg`,
    }))
    .sort((a, b) => b.value - a.value);

  return (
    <>
      <ScreenHead
        title="Model activity"
        sub="Every phase the models ran. Rounds are shown in the order the engine recorded them, and a phase that finishes updates in place rather than moving."
        badges={
          <>
            {liveCount > 0 && (
              <Badge tone="info" dot>
                {liveCount} running
              </Badge>
            )}
            <Badge outline>{plural(filtered.length, "phase")} loaded</Badge>
          </>
        }
      />

      <Panel padding="none">
        <StatRow cols={4}>
          <Stat
            icon="zap"
            label="Running now"
            value={liveCount}
            sub={
              liveCount
                ? live
                    .filter((r) => r.live)
                    .map((r) => r.role)
                    .join(", ")
                : "no phase is mid-flight"
            }
          />
          <Stat
            icon="layers"
            label="Turns loaded"
            value={turns.length}
            sub={`${filtered.length} phases across them`}
          />
          <Stat
            icon="coin"
            label={tokens ? `Tokens · ${tokens.since_days}d` : "Tokens"}
            value={tokens ? fmtCount(tokens.totals.total_tokens) : "—"}
            sub={
              tokens ? `${tokens.totals.calls.toLocaleString()} model calls` : "nothing recorded"
            }
          />
          <Stat
            icon="alert"
            label="Failed phases"
            value={merged.filter((r) => r.failed).length}
            sub="in the window loaded here"
          />
        </StatRow>
      </Panel>

      <div className="toolbar">
        <Segmented
          ariaLabel="Grouping"
          value={grouping}
          onChange={setGrouping}
          options={[
            { value: "turn", label: "By turn" },
            { value: "phase", label: "Flat" },
          ]}
        />
        <div style={{ maxWidth: 200 }}>
          <SearchInput
            value={role}
            onChange={setRole}
            ariaLabel="Filter by seat"
            placeholder="Seat"
          />
        </div>
        {roles.length > 0 && roles.length <= 10 && (
          <div className="row wrap gap-1">
            {roles.map((r) => (
              <Chip key={r} on={role === r} onClick={() => setRole(role === r ? "" : r)}>
                {r}
              </Chip>
            ))}
          </div>
        )}
        <span className="spacer" />
        {PHASES.map((p) => (
          <Chip key={p} on={phase === p} onClick={() => setPhase(phase === p ? "" : p)}>
            {p}
          </Chip>
        ))}
        <Chip on={!!onlyFailed} onClick={() => setOnlyFailed(onlyFailed ? "" : "1")}>
          Failures
        </Chip>
      </div>

      {loading && !merged.length && <Skeleton rows={5} height={44} />}
      {error && <QueryState error={error} loading={loading} />}

      {!loading && !filtered.length && !error && (
        <Empty
          icon="brain"
          title="No model activity in the record"
          hint="A phase is recorded when it completes. If seats are idle and no schedule has fired, there is nothing here yet."
        />
      )}

      <div className="col gap-2">
        {grouping === "turn"
          ? turns.map((g, i) => (
              <TurnCard key={g.turnId} group={g} defaultOpen={i === 0} showRole />
            ))
          : filtered.map((rec, i) => (
              <PhaseCard key={rec.key} record={rec} defaultOpen={i === 0} showRole />
            ))}
      </div>

      <Panel padding="tight">
        <div className="row">
          {pageError ? (
            <QueryState error={pageError} loading={false} />
          ) : exhausted ? (
            <span className="t-caption">That is the beginning of the retained record.</span>
          ) : (
            <>
              <Button size="sm" onClick={() => void loadOlder()} disabled={paging}>
                {paging ? "Loading…" : `Load ${PAGE} older phases`}
              </Button>
              <span className="spacer" />
              <span className="t-caption">the event store keeps 30 days</span>
            </>
          )}
        </div>
      </Panel>

      <Panel
        title="Where the tokens go"
        icon="coin"
        subtitle={tokens ? `${tokens.since_days}-day window` : undefined}
      >
        <BarList data={phaseSpend} emptyLabel="No model calls have been recorded in this window." />
      </Panel>
    </>
  );
}
