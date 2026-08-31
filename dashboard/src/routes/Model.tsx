/**
 * Model activity — a FLEET MONITOR, not a reader.
 *
 * This screen showed every running phase as an expanded transcript. With one
 * agent that was pleasant; with seven it was a race — seven seats each
 * republishing streamed prose five times a second, in cards that grew as they
 * wrote, so nothing on the page held still long enough to read. Scale is the
 * whole design constraint here and a transcript does not have it: reading what
 * a model actually said is a ONE-AGENT activity, and it belongs on that
 * agent's own page, where exactly one turn is in focus.
 *
 * So this is rows. One fixed-height row per phase, the same shape running or
 * finished, sorted on a key that does not move — a live row updates its cells
 * (rounds, tokens, elapsed) and NOTHING reflows, because a number changing
 * inside a fixed row cannot change the layout around it. That property is why
 * the table survives fifty seats when a list of cards did not survive seven.
 *
 * The transcript is one click away, on the seat.
 */

import { useCallback, useMemo, useState } from "react";
import { ScreenHead } from "~/app/Shell.tsx";
import { useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { Badge, Button, Chip, Empty, PhaseTag, Select, Skeleton } from "~/ui/primitives.tsx";
import { DataTable, type Column } from "~/ui/DataTable.tsx";
import { useAgents, useClient } from "~/lib/store-hooks.ts";
import { useSettled } from "~/lib/settled.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtCount, fmtElapsed, plural, relTime, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { href, useNavigator } from "~/app/router.tsx";
import {
  decisionLabel,
  fromLiveCall,
  fromPhaseEvent,
  mergePhases,
  type PhaseRecord,
} from "~/lib/phases.ts";
import type { EventRecord } from "~/protocol/index.ts";

const PAGE = 60;

// Stable identity for the settled list. Named rather than inline so the
// hook's dependencies do not change identity on every render.
const phaseRecordKey = (r: PhaseRecord) => r.key;
const PHASES = ["plan", "execute", "review", "auxiliary"] as const;

export function ModelActivity() {
  const { socket } = useClient();
  const agents = useAgents();
  const [phase, setPhase] = useParam("phase", "");
  const [role, setRole] = useParam("role", "");
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

  // RUNNING AND SETTLED ARE DIFFERENT LISTS, and that split is the whole
  // point of this layout. A live phase changes every couple of hundred
  // milliseconds; a finished one never changes again. As one list, the churn
  // of the first reflowed the second — so a reader working through a
  // completed transcript had it shoved down the page every time any seat
  // anywhere published a round.
  const running = useMemo(() => filtered.filter((r) => r.live), [filtered]);
  const done = useMemo(() => filtered.filter((r) => !r.live), [filtered]);
  // The settled list does not splice new rows in under a reader — see
  // lib/settled.ts.
  const settled = useSettled(done, phaseRecordKey);

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

  const nav = useNavigator();
  const now = useNow();

  // A row goes to the seat, because that is where a transcript is readable:
  // one turn in focus instead of seven competing for the page.
  const openSeat = useCallback(
    (r: PhaseRecord) => nav.to(["seats", r.role], { tab: "model" }),
    [nav],
  );

  // Defined here rather than at module scope because two cells need `now` to
  // render an elapsed time, and memoised so the table's own sort does not see
  // a new column set on every push.
  const columns = useMemo<Column<PhaseRecord>[]>(
    () => [
      {
        key: "seat",
        header: "Seat",
        cell: (r) => (
          <span className="row gap-2">
            {r.live && <span className="dot info" />}
            <span className="truncate">{r.role || "—"}</span>
          </span>
        ),
        sortValue: (r) => r.role,
      },
      {
        key: "phase",
        header: "Phase",
        shrink: true,
        cell: (r) => <PhaseTag phase={r.phase} />,
        sortValue: (r) => r.phase,
      },
      {
        key: "outcome",
        header: "Outcome",
        cell: (r) =>
          r.failed ? (
            <Badge tone="critical">{r.errorKind || "failed"}</Badge>
          ) : r.live ? (
            <span className="t-caption">running</span>
          ) : r.decision ? (
            <span className="truncate t-caption">{decisionLabel(r.phase, r.decision)}</span>
          ) : (
            <span className="t-caption">done</span>
          ),
        sortValue: (r) => (r.failed ? 0 : r.live ? 1 : 2),
      },
      {
        key: "model",
        header: "Model",
        cell: (r) => <span className="mono t-caption truncate">{r.model || "—"}</span>,
        sortValue: (r) => r.model,
      },
      {
        key: "rounds",
        header: "Rounds",
        align: "right",
        shrink: true,
        cell: (r) => Math.max(r.roundsUsed, r.roundNum + 1) || "—",
        sortValue: (r) => Math.max(r.roundsUsed, r.roundNum + 1),
      },
      {
        key: "tokens",
        header: "Tokens",
        align: "right",
        shrink: true,
        cell: (r) => (r.totalTokens ? fmtCount(r.totalTokens) : "—"),
        sortValue: (r) => r.totalTokens,
      },
      {
        key: "when",
        header: "When",
        align: "right",
        shrink: true,
        // Elapsed while it runs, and when it landed once it has. Two
        // different questions, and a running phase has no "when" yet.
        //
        // Measured from startedAt, which never moves. Against `at` — which
        // advances on every published round, several times a second while a
        // round streams — the answer was always about zero, so a phase nine
        // rounds deep read "0 ms".
        cell: (r) =>
          r.live ? (
            <span className="t-num">{fmtElapsed(now - tsKey(r.startedAt))}</span>
          ) : (
            <span className="t-caption">{relTime(r.at, now)}</span>
          ),
        sortValue: (r) => Date.parse(r.at) || 0,
      },
    ],
    [now],
  );

  const liveCount = live.filter((r) => r.live).length;
  const failedCount = merged.filter((r) => r.failed).length;
  const filtering = !!(role || phase || onlyFailed);

  return (
    <>
      <ScreenHead
        title="Model activity"
        sub="Every phase the models ran, one row each. Open a row for the transcript on that seat — reading what a model said is a one-agent job, and this page has to stay readable with fifty of them running."
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
            <Badge outline>{plural(filtered.length, "phase")} loaded</Badge>
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

      {/* RUNNING. Fixed-height rows, sorted on the seat handle — a key that
          does not move — so a live row updates its cells and nothing around
          it reflows. Seven seats republishing five times a second turned the
          card list this replaces into a race; a number changing inside a row
          of settled height cannot move the page at all. */}
      {running.length > 0 && (
        <section className="col gap-1">
          <div className="t-label">
            Running now
            <span className="faint"> · {plural(running.length, "phase")} mid-flight</span>
          </div>
          <DataTable
            rows={running}
            columns={columns}
            rowKey={phaseRecordKey}
            onRowClick={openSeat}
            isFailed={(r) => r.failed}
            defaultSort={{ key: "seat", dir: "asc" }}
          />
        </section>
      )}

      {/* SETTLED. A finished phase never changes again, so this list only
          moves when the reader asks it to. */}
      {settled.pending > 0 && (
        <button className="new-rows" onClick={settled.flush}>
          {plural(settled.pending, "new phase")} finished while you were reading — show
        </button>
      )}

      {settled.items.length > 0 && (
        <section className="col gap-1">
          <div className="t-label">
            Recent phases
            <span className="faint"> · newest first · open a row for its transcript</span>
          </div>
          <DataTable
            rows={settled.items}
            columns={columns}
            rowKey={phaseRecordKey}
            onRowClick={openSeat}
            isFailed={(r) => r.failed}
          />
        </section>
      )}

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
