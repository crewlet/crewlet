/**
 * Coding runs.
 *
 * A coding run is a DETACHED Execute phase: the tool starts it, the phase
 * suspends, and the engine resumes that same loop minutes later — possibly
 * after a restart, possibly on another node. So there are two sources and they
 * answer different questions: the live projection knows what is in flight
 * right now, and `sandbox_runs` knows what the store remembers, including runs
 * whose box has already been reclaimed. The screen this replaces showed only
 * the first, so a run parked on a question with its box gone appeared NOWHERE.
 */

import { useMemo } from "react";
import { useNavigator, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import {
  Badge,
  Button,
  Code,
  Disclosure,
  Panel,
  Skeleton,
  Stat,
  StatRow,
} from "~/ui/primitives.tsx";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useSandboxes } from "~/lib/store-hooks.ts";
import { fmtDateTime, fmtDuration, fmtTime, plural, relTime, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { SandboxRun, SandboxStatus } from "~/protocol/index.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

/**
 * THE ENGINE'S OWN SEVEN WORDS, and only those.
 *
 * This map used to name `awaiting_input`, `succeeded`, `completed`,
 * `cancelled` and `reclaimed` — five values `sandbox.PendingRun` cannot write.
 * So the two states this screen exists to show, `awaiting_clarification` and
 * `reseed`, fell through to the neutral default and read as unremarkable,
 * while five entries could never match anything. `internal/sandbox/pending.go`
 * is the list.
 */
const STATUS_TONE: Record<SandboxStatus, "positive" | "caution" | "critical" | "info" | "neutral"> =
  {
    launching: "info",
    running: "info",
    resumed: "info",
    // A person is being waited on. `reseed` is the same fact one step worse:
    // the box was reaped past its pause TTL, so the work is gone and only the
    // question survives.
    awaiting_clarification: "caution",
    reseed: "caution",
    done: "positive",
    failed: "critical",
  };

/** The statuses that mean a person is being waited on — `sandbox.Awaiting`. */
const AWAITING: SandboxStatus[] = ["awaiting_clarification", "reseed"];

export function Runs({ runId }: { runId?: string }) {
  const nav = useNavigator();
  const live = useSandboxes();
  const now = useNow();
  // THE OPEN RUN IS A PATH (`#/activity/runs/{turn_id}`), not a query key: a
  // detached run is an object with a life of its own — it outlives the turn
  // that started it and is resumed by another process on another node — so it
  // is addressed rather than filtered for.
  const selected = runId ?? "";
  const setSelected = (id: string) => nav.to(id ? ["activity", "runs", id] : ["activity", "runs"]);
  // Durable runs have no push behind them, so this is the one place a poll is
  // correct — and it is slow, because a run's lifetime is minutes.
  const { data, loading, error } = useQuery("sandbox_runs", undefined, { pollMs: 20_000 });

  const rows = useMemo(() => {
    const byTurn = new Map<string, SandboxRun>();
    for (const run of data?.runs ?? []) byTurn.set(run.turn_id, run);
    // A live entry the store has not caught up with still belongs on screen.
    for (const box of live) {
      if (byTurn.has(box.turn_id)) continue;
      byTurn.set(box.turn_id, {
        turn_id: box.turn_id,
        agent_handle: box.agent_handle,
        role: box.role,
        // The live projection types its status as a plain string because it
        // mirrors whatever the run reported; it is the same vocabulary.
        status: box.status as SandboxStatus,
        coding_agent: box.coding_agent,
        // The live projection carries no placement; the durable row does,
        // and it replaces this entry as soon as the store catches up.
        placement: "",
        task_description: box.task,
        question: box.question ?? "",
        audience: box.audience ?? "",
        branch: "",
        trace_id: "",
        owner: "",
        box_exists: true,
        paused_at: "",
        pause_ttl_seconds: 0,
        started_at: box.started_at,
        updated_at: box.started_at,
        answerable_in_chat: false,
      });
    }
    return [...byTurn.values()].sort(
      (a, b) => tsKey(b.updated_at || b.started_at) - tsKey(a.updated_at || a.started_at),
    );
  }, [data, live]);

  const detail = rows.find((r) => r.turn_id === selected) ?? null;
  const waiting = rows.filter((r) => AWAITING.includes(r.status)).length;
  const running = rows.filter((r) => r.status === "running").length;

  return (
    <>
      <PageActions>
        {
          <>
            {running > 0 && (
              <Badge tone="info" dot>
                {running} running
              </Badge>
            )}
            {waiting > 0 && (
              <Badge tone="caution">{plural(waiting, "run")} waiting on a person</Badge>
            )}
          </>
        }
      </PageActions>
      <PageNote>
        Each one is an Execute phase that suspended. It resumes when the sandbox reports back —
        after a restart, or on another node.
      </PageNote>

      <Panel padding="none">
        <StatRow cols={4}>
          <Stat icon="terminal" label="Running" value={running} sub="a box is up and working" />
          <Stat
            icon="help"
            label="Waiting on an answer"
            value={waiting}
            sub={waiting ? "the run cannot continue until someone replies" : "nothing is blocked"}
          />
          <Stat icon="box" label="In the record" value={rows.length} sub="live and finished" />
          <Stat
            icon="alert"
            label="Failed"
            value={rows.filter((r) => r.status === "failed").length}
            sub="in the retained record"
          />
        </StatRow>
      </Panel>

      {loading && !rows.length && <Skeleton rows={4} />}
      <QueryState
        error={error}
        loading={loading}
        empty={
          rows.length
            ? undefined
            : {
                title: "No coding runs",
                hint: "A run starts when a seat calls the sandbox tool. Configure providers.sandbox to give one a place to run.",
              }
        }
      >
        <Panel padding="none">
          <DataGrid<SandboxRun>
            rows={rows}
            rowKey={(r) => r.turn_id}
            onRowActivate={(r) => setSelected(r.turn_id === selected ? "" : r.turn_id)}
            isSelected={(r) => r.turn_id === selected}
            isFailed={(r) => r.status === "failed"}
            defaultSort="-updated"
            columns={[
              {
                key: "status",
                header: "Status",
                shrink: true,
                sortValue: (r) => r.status,
                cell: (r) => (
                  <Badge tone={STATUS_TONE[r.status] ?? "neutral"} dot>
                    {r.status.replace(/_/g, " ")}
                  </Badge>
                ),
              },
              {
                key: "seat",
                header: "Seat",
                sortValue: (r) => r.role || r.agent_handle,
                cell: (r) => <SeatChip name={r.role || r.agent_handle} handle={r.agent_handle} />,
              },
              {
                key: "task",
                header: "Task",
                cell: (r) => <span className="truncate">{r.task_description || "—"}</span>,
              },
              {
                key: "agent",
                header: "Coding agent",
                shrink: true,
                sortValue: (r) => r.coding_agent,
                cell: (r) => (
                  <Badge outline mono>
                    {r.coding_agent || "—"}
                  </Badge>
                ),
              },
              {
                key: "where",
                header: "Runs in",
                shrink: true,
                sortValue: (r) => r.placement,
                cell: (r) => (
                  <Badge outline mono>
                    {r.placement || "—"}
                  </Badge>
                ),
              },
              {
                key: "box",
                header: "Box",
                shrink: true,
                sortValue: (r) => (r.box_exists ? 1 : 0),
                cell: (r) =>
                  r.box_exists ? (
                    <span className="t-caption">up</span>
                  ) : (
                    <span
                      className="t-caption faint"
                      title="the sandbox has been reclaimed; the run's record remains"
                    >
                      reclaimed
                    </span>
                  ),
              },
              {
                key: "updated",
                header: "Updated",
                shrink: true,
                sortValue: (r) => tsKey(r.updated_at || r.started_at),
                cell: (r) => (
                  <span className="t-caption" title={fmtDateTime(r.updated_at || r.started_at)}>
                    {relTime(r.updated_at || r.started_at, now)}
                  </span>
                ),
              },
            ]}
          />
        </Panel>
      </QueryState>

      {detail && (
        <Panel
          title={`Run ${detail.turn_id.slice(0, 8)}`}
          icon="terminal"
          actions={
            <>
              {detail.trace_id && (
                <Button size="sm" onClick={() => nav.to(["activity", "traces", detail.trace_id])}>
                  Trace
                </Button>
              )}
              <Button size="sm" onClick={() => nav.to(["activity", "turns", detail.turn_id])}>
                Turn
              </Button>
              <Button
                size="sm"
                variant="ghost"
                icon="x"
                onClick={() => setSelected("")}
                title="Close"
              />
            </>
          }
        >
          {AWAITING.includes(detail.status) && (
            <div className="banner caution" style={{ marginBottom: "var(--space-3)" }}>
              <Icon name="help" size="sm" />
              <span className="col" style={{ gap: 2 }}>
                <strong>{detail.question || "The run asked a question."}</strong>
                <span className="t-caption">
                  {detail.answerable_in_chat
                    ? `Answer it on ${detail.audience || "the conversation this turn served"} — the next inbound message on that thread is taken as the reply.`
                    : "The run is paused. It resumes when the sandbox coordinator carries an answer back."}
                  {detail.pause_ttl_seconds > 0 && detail.paused_at
                    ? ` The box is held for ${fmtDuration(detail.pause_ttl_seconds * 1000)} from ${fmtDateTime(detail.paused_at)}.`
                    : ""}
                </span>
              </span>
            </div>
          )}
          <PropertiesRail
            groups={[
              {
                properties: [
                  { label: "Task", value: detail.task_description },
                  {
                    label: "Seat",
                    value: (
                      <SeatChip
                        name={detail.role || detail.agent_handle}
                        handle={detail.agent_handle}
                      />
                    ),
                  },
                  { label: "Coding agent", value: detail.coding_agent },
                  { label: "Runs in", value: detail.placement },
                  {
                    label: "Branch",
                    value: detail.branch ? (
                      <code className="inline">{detail.branch}</code>
                    ) : undefined,
                  },
                  { label: "Owner node", value: detail.owner },
                  { label: "Started", value: fmtDateTime(detail.started_at) },
                  { label: "Updated", value: fmtDateTime(detail.updated_at) },
                  {
                    label: "Ran for",
                    value: fmtDuration(tsKey(detail.updated_at) - tsKey(detail.started_at)),
                  },
                  {
                    label: "Turn",
                    value: <code className="inline">{detail.turn_id}</code>,
                    path: ["activity", "turns", detail.turn_id],
                  },
                ],
              },
            ]}
          />
        </Panel>
      )}

      {detail && <BridgeLog run={detail} />}
    </>
  );
}

/**
 * What a bridged run actually called.
 *
 * THE ONE TOOL LOG WITH NOWHERE ELSE TO LIVE, which is why the engine keeps it
 * on the run's own durable row: a native tool loop's calls sit on a surface in
 * memory until the turn writes them, while a bridged run's are made by a
 * process outside the engine, minutes or hours apart and possibly across a
 * restart. Nothing read it, so a resumed run's whole tool log was answerable
 * and unaskable — and "it called nothing" is exactly the shape the delivery
 * check reads as a turn that did not act.
 *
 * ABSENT ON AN ORDINARY RUN rather than empty: a coding run that is not
 * bridged made no calls through a bridge, which is not the same as a bridged
 * one that made none. The panel is not rendered at all for the first.
 */
export function BridgeLog({ run }: { run: SandboxRun }) {
  const calls = run.bridge_calls ?? [];
  const elided = run.bridge_calls_elided ?? 0;
  // AN EMPTY LOG IS AN EMPTY LOG, elision included: the engine drops the
  // MIDDLE of a bounded list and keeps the first and last hundred, so a run
  // with calls elided always still has calls. Guarding on the count alone is
  // therefore complete, and a second clause for `elided > 0 && no calls` would
  // be a branch nothing can reach and nothing can test.
  if (calls.length === 0) return null;
  const failures = calls.filter((c) => c.failed).length;
  return (
    <Panel
      title="Tool calls"
      icon="terminal"
      count={calls.length}
      subtitle="through the MCP bridge, in order"
      actions={
        failures > 0 ? <Badge tone="critical">{plural(failures, "failure")}</Badge> : undefined
      }
    >
      <div className="col gap-2">
        {elided > 0 && (
          // SAID OUT LOUD. The engine bounds the log at 200 and drops the
          // MIDDLE rather than the start, because how a run began and how it
          // ended are what explain it — and a log that silently skips is a
          // log that lies about what the run did.
          <div className="banner caution">
            <Icon name="alert" size="sm" />
            <span>
              {plural(elided, "call")} from the middle of this log were dropped — the engine keeps
              the first and last hundred, so the run did more than is shown here.
            </span>
          </div>
        )}
        <div className="list">
          {calls.map((call, i) => (
            <Disclosure
              key={`${call.at}:${i}`}
              label={
                <span className="row gap-2 baseline">
                  <code className="inline">{call.name}</code>
                  {call.failed && <Badge tone="critical">failed</Badge>}
                  <span className="spacer" />
                  <span className="t-caption">{fmtTime(call.at)}</span>
                </span>
              }
            >
              <div className="col gap-2">
                {/* ARGUMENTS ARE JSON TEXT ON THE WIRE, never a decoded map —
                    a large id survives one encoder pass and not two — so they
                    are rendered as the text they are rather than re-encoded
                    into a shape the engine never wrote. */}
                <div className="col gap-1">
                  <div className="t-label">Arguments</div>
                  <Code plain label={`${call.name} arguments`}>
                    {call.args || "(none)"}
                  </Code>
                </div>
                <div className="col gap-1">
                  <div className="t-label">{call.failed ? "Error" : "Result"}</div>
                  <Code plain label={`${call.name} ${call.failed ? "error" : "result"}`}>
                    {call.output || "(nothing returned)"}
                  </Code>
                </div>
              </div>
            </Disclosure>
          ))}
        </div>
      </div>
    </Panel>
  );
}
