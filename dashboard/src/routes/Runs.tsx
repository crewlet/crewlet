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
import { ScreenHead } from "~/app/Shell.tsx";
import { useNavigator, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip } from "~/components/common.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useSandboxes } from "~/lib/store-hooks.ts";
import { fmtDateTime, fmtDuration, plural, tsKey } from "~/lib/format.ts";
import type { SandboxRun } from "~/protocol/index.ts";
import {
  Button,
  Card,
  DescriptionList,
  IconButton,
  RelativeTime,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
  useNow,
} from "@crewlethq/ui";
import type { Tone } from "@crewlethq/ui";
import {
  CloseGlyph,
  ErrorGlyph,
  HelpGlyph,
  Package2Glyph,
  TerminalGlyph,
} from "@crewlethq/icons/glyphs";

const STATUS_TONE: Record<string, Tone> = {
  running: "info",
  awaiting_input: "warning",
  succeeded: "success",
  completed: "success",
  failed: "danger",
  cancelled: "neutral",
  reclaimed: "neutral",
};

export function Runs() {
  const nav = useNavigator();
  const live = useSandboxes();
  const now = useNow();
  const [selected, setSelected] = useParam("run", "");
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
        status: box.status,
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
  const waiting = rows.filter((r) => r.status === "awaiting_input").length;
  const running = rows.filter((r) => r.status === "running").length;

  return (
    <>
      <ScreenHead
        title="Coding runs"
        sub="Each one is an Execute phase that suspended. It resumes when the sandbox reports back — after a restart, or on another node."
        badges={
          <>
            {running > 0 && (
              <Tag variant="info" dot>
                {running} running
              </Tag>
            )}
            {waiting > 0 && (
              <Tag variant="warning">{plural(waiting, "run")} waiting on a person</Tag>
            )}
          </>
        }
      />

      <StatGroup columns={4}>
        <StatCard
          icon={<TerminalGlyph />}
          label="Running"
          value={running}
          sub="a box is up and working"
        />
        <StatCard
          icon={<HelpGlyph />}
          label="Waiting on an answer"
          value={waiting}
          sub={waiting ? "the run cannot continue until someone replies" : "nothing is blocked"}
        />
        <StatCard
          icon={<Package2Glyph />}
          label="In the record"
          value={rows.length}
          sub="live and finished"
        />
        <StatCard
          icon={<ErrorGlyph />}
          label="Failed"
          value={rows.filter((r) => r.status === "failed").length}
          sub="in the retained record"
        />
      </StatGroup>

      {loading && !rows.length && (
        <Skeleton label="Loading the coding runs" variant="text" rows={4} />
      )}
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
        <Card padding="none">
          <DataTable<SandboxRun>
            rows={rows}
            rowKey={(r) => r.turn_id}
            onRowClick={(r) => setSelected(r.turn_id === selected ? "" : r.turn_id)}
            isSelected={(r) => r.turn_id === selected}
            isFailed={(r) => r.status === "failed"}
            defaultSort={{ key: "updated", dir: "desc" }}
            columns={[
              {
                key: "status",
                header: "Status",
                shrink: true,
                sortValue: (r) => r.status,
                cell: (r) => (
                  <Tag variant={STATUS_TONE[r.status] ?? "neutral"} dot>
                    {r.status.replace(/_/g, " ")}
                  </Tag>
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
                  <Tag appearance="outline" monospace>
                    {r.coding_agent || "—"}
                  </Tag>
                ),
              },
              {
                key: "where",
                header: "Runs in",
                shrink: true,
                sortValue: (r) => r.placement,
                cell: (r) => (
                  <Tag appearance="outline" monospace>
                    {r.placement || "—"}
                  </Tag>
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
                      className="t-caption"
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
                  <RelativeTime
                    className="t-caption"
                    value={r.updated_at || r.started_at}
                    now={now}
                  />
                ),
              },
            ]}
          />
        </Card>
      </QueryState>

      {detail && (
        <Card as="section">
          <Card.Header
            icon={<TerminalGlyph size="sm" />}
            actions={
              <>
                {detail.trace_id && (
                  <Button
                    variant="secondary"
                    size="small"
                    onClick={() => nav.to(["traces", detail.trace_id])}
                  >
                    Trace
                  </Button>
                )}
                <Button
                  variant="secondary"
                  size="small"
                  onClick={() => nav.to(["turns", detail.turn_id])}
                >
                  Turn
                </Button>
                <IconButton
                  label="Close"
                  icon={<CloseGlyph />}
                  size="sm"
                  onClick={() => setSelected("")}
                />
              </>
            }
          >
            <Card.Title>{`Run ${detail.turn_id.slice(0, 8)}`}</Card.Title>
          </Card.Header>
          {detail.status === "awaiting_input" && (
            <div className="banner caution" style={{ marginBottom: "var(--spacing-3)" }}>
              <HelpGlyph size="sm" />
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
          <DescriptionList
            items={[
              ["Task", detail.task_description || "—"],
              [
                "Seat",
                <SeatChip
                  key="s"
                  name={detail.role || detail.agent_handle}
                  handle={detail.agent_handle}
                />,
              ],
              ["Coding agent", detail.coding_agent || "—"],
              ["Runs in", detail.placement || "—"],
              [
                "Branch",
                detail.branch ? (
                  <code key="b" className="inline">
                    {detail.branch}
                  </code>
                ) : (
                  "—"
                ),
              ],
              ["Owner node", detail.owner || "—"],
              ["Started", fmtDateTime(detail.started_at)],
              ["Updated", fmtDateTime(detail.updated_at)],
              ["Ran for", fmtDuration(tsKey(detail.updated_at) - tsKey(detail.started_at))],
              [
                "Turn",
                <code key="t" className="inline">
                  {detail.turn_id}
                </code>,
              ],
            ]}
          />
        </Card>
      )}
    </>
  );
}
