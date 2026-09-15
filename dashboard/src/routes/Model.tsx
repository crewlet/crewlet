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
import { useParam } from "~/app/router.tsx";
import { QueryState, recordTable } from "~/components/common.tsx";
import { PhaseTag } from "~/components/PhaseTag.tsx";
import { useAgents, useClient, useEngineHealth, usePhaseEvents } from "~/lib/store-hooks.ts";
import { useSettled } from "~/lib/settled.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { eventHistoryLabel, fmtCount, plural, tsKey } from "~/lib/format.ts";
import { href, useNavigator } from "~/app/router.tsx";
import {
  decisionLabel,
  fromLiveCall,
  fromPhaseEvent,
  mergePhases,
  streamedPhases,
  type PhaseRecord,
} from "~/lib/phases.ts";
import type { EventRecord } from "~/protocol/index.ts";
import {
  Button,
  Card,
  DataTable,
  DataView,
  type DataViewColumn,
  EmptyState,
  EmptyValue,
  type FilterDef,
  type FilterValues,
  NewItemsNotice,
  PageHeader,
  RelativeTime,
  Skeleton,
  StatusDot,
  Tag,
  useNow,
} from "@crewlethq/ui";
import { CloseGlyph, NeurologyGlyph } from "@crewlethq/icons/glyphs";

const PAGE = 60;

// Stable identity for the settled list. Named rather than inline so the
// hook's dependencies do not change identity on every render.
const phaseRecordKey = (r: PhaseRecord) => r.key;
// Every phase the engine emits, the turn's own two first. A phase left off
// this row is one nobody can filter to, which on this screen means its cost
// is only ever visible inside the "all phases" total.
const PHASES = ["execute", "review", "onboarding", "subagent", "auxiliary", "judge"] as const;

export function ModelActivity() {
  const { socket } = useClient();
  const agents = useAgents();
  const { data: engine } = useEngineHealth();
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

  const phaseEvents = usePhaseEvents();

  const stored = useMemo<PhaseRecord[]>(() => {
    const answered = [...(data?.phases ?? []), ...older]
      .map((row) => fromPhaseEvent(row))
      .filter((r): r is PhaseRecord => r !== null);
    // The query above is answered once. Without the phases that finish after
    // it, a row here leaves "Running now" when its phase completes and never
    // appears among the recent ones — it just goes.
    const streamed = streamedPhases(phaseEvents, (r) => !role || r.role === role);
    return [...streamed, ...answered];
  }, [data, older, phaseEvents, role]);

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
  const runningKeys = useMemo(() => running.map(phaseRecordKey), [running]);
  // The settled list does not splice new rows in under a reader — see
  // lib/settled.ts. A phase that was in the running table above is exempt: the
  // reader has been watching it, and it lands here the moment it completes.
  const settled = useSettled(done, phaseRecordKey, runningKeys);

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
  const columns = useMemo<DataViewColumn<PhaseRecord>[]>(
    () => [
      {
        key: "seat",
        header: "Seat",
        render: (r) => (
          <span className="row gap-2">
            {r.live && <StatusDot tone="info" pulse />}
            <span className="truncate">{r.role || <EmptyValue label="No seat recorded" />}</span>
          </span>
        ),
        sortable: true,
        sortValue: (r) => r.role,
      },
      {
        key: "phase",
        header: "Phase",
        shrink: true,
        render: (r) => <PhaseTag phase={r.phase} />,
        sortable: true,
        sortValue: (r) => r.phase,
      },
      {
        key: "outcome",
        header: "Outcome",
        render: (r) =>
          r.failed ? (
            <Tag variant="danger">{r.errorKind || "failed"}</Tag>
          ) : r.live ? (
            <span className="t-caption">running</span>
          ) : r.decision ? (
            <span className="truncate t-caption">{decisionLabel(r.phase, r.decision)}</span>
          ) : (
            <span className="t-caption">done</span>
          ),
        sortable: true,
        sortValue: (r) => (r.failed ? 0 : r.live ? 1 : 2),
      },
      {
        key: "model",
        header: "Model",
        render: (r) => (
          <span className="mono t-caption truncate">
            {r.model || <EmptyValue label="No model recorded" />}
          </span>
        ),
        sortable: true,
        sortValue: (r) => r.model,
      },
      {
        key: "rounds",
        header: "Rounds",
        align: "right",
        shrink: true,
        firstDirection: "desc",
        render: (r) =>
          Math.max(r.roundsUsed, r.roundNum + 1) || <EmptyValue label="No rounds recorded" />,
        sortable: true,
        sortValue: (r) => Math.max(r.roundsUsed, r.roundNum + 1),
      },
      {
        key: "tokens",
        header: "Tokens",
        align: "right",
        shrink: true,
        firstDirection: "desc",
        render: (r) =>
          r.totalTokens ? fmtCount(r.totalTokens) : <EmptyValue label="No tokens recorded" />,
        sortable: true,
        sortValue: (r) => r.totalTokens,
      },
      {
        key: "when",
        header: "When",
        align: "right",
        shrink: true,
        firstDirection: "desc",
        // Elapsed while it runs, and when it landed once it has. Two
        // different questions, and a running phase has no "when" yet.
        //
        // Measured from startedAt, which never moves. Against `at` — which
        // advances on every published round, several times a second while a
        // round streams — the answer was always about zero, so a phase nine
        // rounds deep read "0 ms".
        render: (r) =>
          r.live ? (
            <RelativeTime className="t-num" mode="elapsed" value={r.startedAt} now={now} />
          ) : (
            <RelativeTime className="t-caption" value={r.at} now={now} />
          ),
        sortable: true,
        sortValue: (r) => Date.parse(r.at) || 0,
      },
    ],
    [now],
  );

  const liveCount = live.filter((r) => r.live).length;
  const failedCount = merged.filter((r) => r.failed).length;
  const filtering = !!(role || phase || onlyFailed);

  /*
   * THE SCREEN OWNS THE NARROWING, so every axis is a URL parameter and a
   * narrowed record is a link. The seat filter is a list of the seats the
   * company has rather than a text box, because the match is exact on both
   * sides of the wire, so a typed prefix silently returned nothing while
   * looking like a search that missed.
   */
  const filters = useMemo<FilterDef<PhaseRecord>[]>(
    () => [
      {
        name: "role",
        label: "Seat",
        kind: "select",
        options: [
          { value: "", label: "Any seat" },
          ...roles.map((seat) => ({ value: seat, label: seat })),
        ],
      },
      {
        name: "phase",
        label: "Phase",
        kind: "select",
        options: [
          { value: "", label: "Any phase" },
          ...PHASES.map((p) => ({ value: p, label: p })),
        ],
      },
      {
        name: "failed",
        label: "Outcome",
        kind: "select",
        options: [
          { value: "", label: "Any outcome" },
          { value: "1", label: "Failures only" },
        ],
      },
    ],
    [roles],
  );

  const values: FilterValues = { role, phase, failed: onlyFailed };

  const onValuesChange = useCallback(
    (next: FilterValues) => {
      setRole(String(next.role ?? ""));
      setPhase(String(next.phase ?? ""));
      setOnlyFailed(String(next.failed ?? ""));
    },
    [setRole, setPhase, setOnlyFailed],
  );

  return (
    <>
      <PageHeader
        title="Model activity"
        description="Every phase the models ran, one row each. Open a row for the transcript on that seat — reading what a model said is a one-agent job, and this page has to stay readable with fifty of them running."
        badges={
          <>
            {liveCount > 0 && (
              <Tag variant="info" dot>
                {liveCount} running
              </Tag>
            )}
            {/* The count IS the control. It used to be a stat tile that said
                "4 failed" and did nothing, next to a separate chip that did
                the filtering — so a reader who saw the number had to go find
                the unrelated pill that acted on it. */}
            {failedCount > 0 && (
              <Tag
                variant="danger"
                onClick={() => setOnlyFailed(onlyFailed ? "" : "1")}
                pressed={!!onlyFailed}
                title={onlyFailed ? "show every phase" : "show only failed phases"}
              >
                {failedCount} failed
              </Tag>
            )}
            <Tag appearance="outline">{plural(filtered.length, "phase")} loaded</Tag>
          </>
        }
      />

      {loading && !merged.length && (
        <Skeleton label="Loading model activity" variant="text" rows={5} rowHeight={44} />
      )}
      {error && <QueryState error={error} loading={loading} />}

      {/* RUNNING. Fixed-height rows, sorted on the seat handle, a key that
          does not move, so a live row updates its cells and nothing around it
          reflows. Seven seats republishing five times a second turned the card
          list this replaces into a race; a number changing inside a row of
          settled height cannot move the page at all. The same filters narrow
          it: they are the screen's, read from the URL, not the table's. */}
      {running.length > 0 && (
        <Card as="section" padding="none">
          <Card.Header
            divided
            style={{ paddingInline: "var(--spacing-4)", paddingTop: "var(--spacing-3)" }}
            subtitle={`${plural(running.length, "phase")} mid-flight`}
            count={running.length}
          >
            <Card.Title>Running now</Card.Title>
          </Card.Header>
          <DataTable
            {...recordTable(running, columns)}
            getRowKey={phaseRecordKey}
            onRowClick={openSeat}
            rowTone={(r) => (r.failed ? "danger" : null)}
            defaultSort={{ key: "seat", direction: "asc" }}
            stableOrder
          />
        </Card>
      )}

      {/* SETTLED. A finished phase never changes again, so this list only
          moves when the reader asks it to. */}
      <NewItemsNotice count={settled.pending} noun="new phase" onShow={settled.flush} />

      <DataView<PhaseRecord>
        framed
        columns={columns}
        rows={settled.items}
        totalCount={merged.filter((r) => !r.live).length}
        getRowKey={phaseRecordKey}
        onRowClick={openSeat}
        rowTone={(r) => (r.failed ? "danger" : null)}
        defaultSort={{ key: "when", direction: "desc" }}
        filters={filters}
        filterValues={values}
        onFilterValuesChange={onValuesChange}
        emptyMessage={
          <EmptyState
            size="compact"
            icon={<NeurologyGlyph />}
            title={filtering ? "Nothing matches these filters" : "No model activity in the record"}
            description={
              filtering
                ? "Clear them to see every phase the engine has kept."
                : "A phase is recorded when it completes. If seats are idle and no schedule has fired, there is nothing here yet."
            }
          />
        }
        pagination={
          pageError ? (
            <QueryState error={pageError} loading={false} />
          ) : exhausted ? (
            <span>That is the beginning of the retained record.</span>
          ) : (
            <>
              <Button
                variant="secondary"
                size="small"
                onClick={() => void loadOlder()}
                disabled={paging}
              >
                {paging ? "Loading older phases" : `Load ${PAGE} older phases`}
              </Button>
              <span>{eventHistoryLabel(engine?.event_history_seconds)}</span>
            </>
          )
        }
      />

      {/* A bare row, not a panel: one link did not need card chrome. The spend
          rollup that used to sit BELOW this is gone. It was Spend's panel on
          Spend's data, and every "load older" press pushed it another sixty
          cards down a single scroller, so nobody ever reached it. A link goes
          where the screen does. */}
      <div className="row gap-2">
        <span className="spacer" />
        <a className="t-link" href={href(["spend"])}>
          where the tokens go
        </a>
      </div>
    </>
  );
}
