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
import { Badge, Button, Chip, Empty, Segmented, Select, Skeleton } from "~/ui/primitives.tsx";
import { useAgents, useClient } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { plural } from "~/lib/format.ts";
import { href } from "~/app/router.tsx";
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
  const failedCount = merged.filter((r) => r.failed).length;
  const filtering = !!(role || phase || onlyFailed);

  return (
    <>
      <ScreenHead
        title="Model activity"
        sub="Every phase the models ran, round by round. A phase that finishes updates in place rather than moving."
        badges={
          <>
            {liveCount > 0 && (
              <Badge tone="info" dot>
                {liveCount} running
              </Badge>
            )}
            {/* The count IS the control. It used to be a stat tile that said
                "4 failed" and did nothing, next to a separate chip that did
                the filtering — so a reader who saw the number had to go find
                the unrelated pill that acted on it. */}
            {failedCount > 0 && (
              <Badge
                tone="critical"
                onClick={() => setOnlyFailed(onlyFailed ? "" : "1")}
                pressed={!!onlyFailed}
                title={onlyFailed ? "show every phase" : "show only failed phases"}
              >
                {failedCount} failed
              </Badge>
            )}
            <Badge outline>
              {plural(filtered.length, "phase")} · {plural(turns.length, "turn")}
            </Badge>
          </>
        }
      />

      {/* ONE row of controls. This screen had eighteen: a segmented control, a
          free-text box, a chip per seat, a chip per phase and a failures chip
          — fifteen of them near-identical pills in two different active
          idioms. The seat filter is a picker rather than a text box because
          the match is exact on both sides of the wire, so a typed prefix
          silently returned nothing while looking like a search that missed. */}
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
        <Select
          value={role}
          onChange={setRole}
          options={roles}
          ariaLabel="Filter by seat"
          anyLabel="Any seat"
        />
        <span className="spacer" />
        {PHASES.map((p) => (
          <Chip key={p} on={phase === p} onClick={() => setPhase(phase === p ? "" : p)}>
            {p}
          </Chip>
        ))}
        {filtering && (
          <Button
            size="sm"
            icon="x"
            onClick={() => {
              setRole("");
              setPhase("");
              setOnlyFailed("");
            }}
          >
            Clear
          </Button>
        )}
      </div>

      {loading && !merged.length && <Skeleton rows={5} height={44} />}
      {error && <QueryState error={error} loading={loading} />}

      {!loading && !filtered.length && !error && (
        <Empty
          icon="brain"
          title={filtering ? "Nothing matches these filters" : "No model activity in the record"}
          hint={
            filtering
              ? "Clear them to see every phase the engine has kept."
              : "A phase is recorded when it completes. If seats are idle and no schedule has fired, there is nothing here yet."
          }
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

      {/* A bare row, not a Panel: one button did not need card chrome. The
          spend rollup that used to sit BELOW this is gone — it was Spend's
          panel on Spend's data, and every "load older" click pushed it
          another sixty cards down a single scroller, so nobody ever reached
          it. A link goes where the screen does. */}
      <div className="row gap-2">
        {pageError ? (
          <QueryState error={pageError} loading={false} />
        ) : exhausted ? (
          <span className="t-caption">That is the beginning of the retained record.</span>
        ) : (
          <>
            <Button size="sm" onClick={() => void loadOlder()} disabled={paging}>
              {paging ? "Loading…" : `Load ${PAGE} older phases`}
            </Button>
            <span className="t-caption">the event store keeps 30 days</span>
          </>
        )}
        <span className="spacer" />
        <a className="t-caption" href={href(["spend"])}>
          where the tokens go →
        </a>
      </div>
    </>
  );
}
