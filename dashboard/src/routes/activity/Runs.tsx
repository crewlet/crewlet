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
 *
 * # The row, the rail and the page
 *
 * A plain click opens the run BESIDE the list rather than in place of it. This
 * screen is read while something is in flight, and a reader comparing three
 * parked runs loses the comparison the moment the list navigates away. The
 * path `#/activity/runs/{turn_id}` is still the run's own page — it is what a
 * ⌘-click and the rail's own `Open ↗` reach — and it is the only place the
 * bridge log is drawn, because a log of two hundred tool calls is the one
 * thing a 420 px rail is the wrong shape for.
 */

import { useCallback, useMemo } from "react";
import { useNavigator } from "~/app/router.tsx";
import { QueryState, RECORD_MAX_HEIGHT, SeatChip } from "~/components/common.tsx";
import {
  Button,
  Callout,
  Card,
  CodeBlock,
  Disclosure,
  EmptyState,
  EmptyValue,
  IconButton,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
} from "@crewlethq/ui";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, StatusCell, TextCell } from "~/app/frame/cells.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { peekHref, rowPeekHandler, usePeek, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import {
  CloseGlyph,
  HelpGlyph,
  Package2Glyph,
  TerminalGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { useSandboxes } from "~/lib/store-hooks.ts";
import {
  elapsedMs,
  fmtDateTime,
  fmtDuration,
  fmtTime,
  inTime,
  plural,
  tsKey,
} from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { indentJSON } from "~/lib/jsontext.ts";
import type { BridgeCall, SandboxEntry, SandboxRun, SandboxStatus } from "~/protocol/index.ts";
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
const STATUS_TONE: Record<SandboxStatus, "success" | "warning" | "danger" | "info" | "neutral"> = {
  launching: "info",
  running: "info",
  resumed: "info",
  // A person is being waited on. `reseed` is the same fact one step worse:
  // the box was reaped past its pause TTL, so the work is gone and only the
  // question survives.
  awaiting_clarification: "warning",
  reseed: "warning",
};

/** The statuses that mean a person is being waited on — `sandbox.Awaiting`. */
const AWAITING: SandboxStatus[] = ["awaiting_clarification", "reseed"];

/** How long the durable record is polled for. A run's lifetime is minutes. */
const POLL_MS = 20_000;

/**
 * A live projection entry as the run it is.
 *
 * The fields the projection has never held are left EMPTY rather than guessed:
 * a placement of `""` is "this node has not written the row yet", and the
 * durable row replaces the whole entry the moment the store catches up. A
 * fabricated `direct` here would be a placement a reader plans against.
 */
function fromLiveBox(box: SandboxEntry): SandboxRun {
  return {
    turn_id: box.turn_id,
    agent_handle: box.agent_handle,
    role: box.role,
    // The live projection types its status as a plain string because it
    // mirrors whatever the run reported; it is the same vocabulary.
    status: box.status as SandboxStatus,
    coding_agent: box.coding_agent,
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
  };
}

/**
 * The durable record and the live projection, as one list, newest first.
 *
 * ONE ROW PER TURN and the DURABLE row wins: it carries the placement, the
 * branch, the owner and the pause deadline the projection never held, and it
 * is the only one that survives the box being reclaimed. A live entry the
 * store has not caught up with still belongs on screen, which is the other
 * half — a run that started two seconds ago is exactly what somebody watching
 * this screen is watching for.
 *
 * Exported because Live now draws the same rows from the same two sources:
 * two screens folding "what is in a box" their own way is how they come to
 * disagree about whether anything is.
 */
export function mergeRuns(durable: SandboxRun[], live: SandboxEntry[]): SandboxRun[] {
  const byTurn = new Map<string, SandboxRun>();
  for (const run of durable) byTurn.set(run.turn_id, run);
  for (const box of live) {
    if (byTurn.has(box.turn_id)) continue;
    byTurn.set(box.turn_id, fromLiveBox(box));
  }
  return [...byTurn.values()].sort(
    (a, b) => tsKey(b.updated_at || b.started_at) - tsKey(a.updated_at || a.started_at),
  );
}

/**
 * What to call one coding run in a header, which always needs a word in the
 * slot — the page's and the peek's alike, so the two cannot drift.
 *
 * The placeholder is deliberately NOT what this screen publishes as the run's
 * label: see the `usePageLabels` call below.
 */
function runTitle(run: SandboxRun): string {
  return run.task_description || "No task was recorded";
}

/**
 * The facts a run is recognised by, in ONE order.
 *
 * The page and the rail read this same function, because a reader who peeks a
 * run and then opens it must not have to re-learn where each fact sits. The
 * STATUS is deliberately not among them: it is the header's own pill, where
 * the tone carries it — spelled once in colour and again in a list, it reads
 * as two different facts about one run.
 */
function runFacts(run: SandboxRun): Fact[] {
  return [
    {
      label: "Seat",
      value: <SeatChip name={run.role || run.agent_handle} handle={run.agent_handle} />,
    },
    { label: "Runs in", value: run.placement },
    { label: "Coding agent", value: run.coding_agent },
    { label: "Branch", value: run.branch ? <code className="inline">{run.branch}</code> : "" },
    // NOT "up / reclaimed" as a plain word pair: whether the box still exists
    // is the difference between a run somebody can answer and a run whose work
    // is gone, and `FactLine` drops a fact whose value is empty — so this one
    // is always present and always says which.
    { label: "Box", value: run.box_exists ? "up" : "reclaimed" },
  ];
}

/**
 * The engine's own status word, toned.
 *
 * Exported for the same reason [mergeRuns] is: Live now draws these runs too,
 * and a second spelling of one status is how one screen comes to call a
 * `reseed` unremarkable while this one calls it a caution.
 */
export function RunStatus({ status }: { status: SandboxStatus }) {
  return (
    <Tag variant={STATUS_TONE[status] ?? "neutral"} dot>
      {status.replace(/_/g, " ")}
    </Tag>
  );
}

/**
 * When the hold on the box runs out, or null when nothing is holding it.
 *
 * BOTH HALVES OR NEITHER. A TTL with no `paused_at` names no instant, and a
 * `paused_at` carrying a zero TTL is a run the engine parked with no hold at
 * all rather than one held for no time — the zero is a missing setting, not a
 * deadline in the past, and rendering it as one would tell a reader the box
 * was reclaimed in 1970.
 */
function pauseDeadline(run: SandboxRun): string | null {
  const paused = tsKey(run.paused_at);
  if (!paused || run.pause_ttl_seconds <= 0) return null;
  return new Date(paused + run.pause_ttl_seconds * 1000).toISOString();
}

/**
 * What a parked run is waiting for, and from whom.
 *
 * ONE COPY for the page and the rail: the two sentences below are the whole of
 * what a reader can DO about a paused run, and two spellings of them is how
 * one of them comes to describe a chat reply path the run does not have.
 */
function AwaitingBanner({ run, now }: { run: SandboxRun; now: number }) {
  const deadline = pauseDeadline(run);
  const expired = deadline !== null && tsKey(deadline) <= now;
  return (
    <Callout variant="warning" icon={<HelpGlyph size="md" />}>
      <span className="col" style={{ gap: 2 }}>
        <strong>{run.question || "The run asked a question."}</strong>
        <span className="t-caption">
          {run.answerable_in_chat
            ? `Answer it on ${run.audience || "the conversation this turn served"} — the next inbound message on that thread is taken as the reply.`
            : "The run is paused. It resumes when the sandbox coordinator carries an answer back."}
          {deadline && !expired
            ? ` The box is held for ${fmtDuration(run.pause_ttl_seconds * 1000)} from ${fmtDateTime(run.paused_at)} — ${inTime(deadline, now)}.`
            : ""}
          {/* THE HOLD IS THE DEADLINE, and a passed one is the fact that
              explains the rest of the screen: the box is reaped, the work in
              it is gone, and answering now re-seeds the run from the question
              alone. A deadline rendered as "in -3m" would bury that. */}
          {expired
            ? ` The hold expired at ${fmtDateTime(deadline)}: the box is reclaimed, so an answer now re-seeds the run rather than resuming it.`
            : ""}
        </span>
      </span>
    </Callout>
  );
}

/** What the run is doing, for a run nobody is being waited on by. */
function doingLine(run: SandboxRun): string {
  switch (run.status) {
    case "launching":
      return "The box is being provisioned. Nothing has run in it yet.";
    case "running":
      return "A box is up and the coding agent is working in it.";
    case "resumed":
      return "An answer came back and the suspended Execute phase is running again.";
    default:
      return "The run is waiting on a person — see above.";
  }
}

/**
 * What a bridged run called, in one line.
 *
 * THE COUNT, NOT THE LOG. The rail answers "is this the one I meant, and what
 * is it doing"; two hundred tool calls with their arguments and results is the
 * page's job, and `Open ↗` is one click away.
 */
function BridgeSummary({ run }: { run: SandboxRun }) {
  const calls = run.bridge_calls ?? [];
  // ABSENT ON AN ORDINARY RUN rather than empty — see [BridgeLog]: a run that
  // is not bridged made no calls through a bridge, which is not the same fact
  // as a bridged one that made none.
  if (calls.length === 0) return null;
  const failures = calls.filter((c) => c.failed).length;
  const last = calls[calls.length - 1];
  return (
    <section className="col gap-2">
      <div className="t-label">Tool calls</div>
      <div className="row gap-2 wrap">
        <span className="t-cell">{plural(calls.length, "call")} through the MCP bridge</span>
        {failures > 0 && <Tag variant="danger">{plural(failures, "failure")}</Tag>}
      </div>
      {last && (
        <span className="t-caption">
          Last: <code className="inline">{last.name}</code> · {fmtTime(last.at)}
        </span>
      )}
    </section>
  );
}

/**
 * One coding run, beside the list it was found in.
 *
 * # It asks for its own run
 *
 * From the durable record, merged with the live projection exactly as the list
 * does — because a `peek=run:` arrives from a pasted URL as often as from a
 * row, and the two sources answer different halves: the projection has the run
 * that started a second ago, and the store has the one whose box is gone.
 *
 * # What it answers
 *
 * The question it is parked on, the box it is parked in, and who is being
 * waited on — in that order, because a run nobody is waiting on is a run
 * nobody needs to open, and a run parked on a question is the reason this
 * screen exists at all.
 */
export function RunPeek({ turnId }: { turnId: string }) {
  const now = useNow();
  const live = useSandboxes();
  const { data, loading, error } = useQuery("sandbox_runs", undefined, {
    enabled: turnId !== "",
    pollMs: POLL_MS,
  });

  const run = useMemo(
    () => mergeRuns(data?.runs ?? [], live).find((r) => r.turn_id === turnId) ?? null,
    [data, live, turnId],
  );

  return (
    <>
      {loading && !data && <Skeleton variant="text" rows={6} label="Loading the run" />}
      <QueryState error={error} loading={loading}>
        {/* NOT AN EMPTY RAIL. A turn id that matches no run is a hand-edited
            URL or a run swept past the retention horizon, and naming which
            turn resolved to nothing is more use than a header over no run. */}
        {data && !run && (
          <EmptyState
            size="compact"
            icon={<TerminalGlyph size={32} />}
            title="No coding run for this turn"
            description="A run is addressed by the turn that started it. This one is not in the durable record — it may have been swept, or the id may be wrong."
          />
        )}
        {run && (
          <>
            <ObjectHeader
              size="peek"
              kind="Coding run"
              icon="terminal"
              identifier={run.turn_id.slice(0, 8)}
              title={runTitle(run)}
              status={<RunStatus status={run.status} />}
              facts={runFacts(run)}
            />
            <div className="col gap-3">
              <section className="col gap-2">
                <div className="t-label">
                  {AWAITING.includes(run.status) ? "Waiting on a person" : "Doing now"}
                </div>
                {AWAITING.includes(run.status) ? (
                  <AwaitingBanner run={run} now={now} />
                ) : (
                  <p className="t-body">{doingLine(run)}</p>
                )}
              </section>

              <section className="col gap-2">
                <div className="t-label">The box</div>
                <PropertiesRail
                  groups={[
                    {
                      properties: [
                        {
                          label: "Owner node",
                          value: run.owner,
                          title: "the node that holds this run's record",
                        },
                        { label: "Started", value: fmtDateTime(run.started_at) },
                        { label: "Updated", value: fmtDateTime(run.updated_at) },
                        {
                          label: "Ran for",
                          value: fmtDuration(elapsedMs(run.started_at, run.updated_at)),
                          title: "between the first report and the last, not wall-clock since",
                        },
                        {
                          label: "Turn",
                          value: <code className="inline">{run.turn_id}</code>,
                          path: ["activity", "turns", run.turn_id],
                        },
                      ],
                    },
                  ]}
                />
              </section>

              <BridgeSummary run={run} />
            </div>
          </>
        )}
      </QueryState>
    </>
  );
}

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
  const { data, loading, error } = useQuery("sandbox_runs", undefined, { pollMs: POLL_MS });

  const rows = useMemo(() => mergeRuns(data?.runs ?? [], live), [data, live]);

  // THE ORDER `[` AND `]` WALK is the one on screen, which is this list sorted
  // as the reader left it. Published from the merged rows rather than from the
  // query's own, so the stepper cannot walk past a live run the store has not
  // written yet — the rail would open on a run this list does not show.
  usePeekNeighbours(
    useMemo(() => rows.map((r) => ({ kind: "run" as const, id: r.turn_id })), [rows]),
  );

  const { open: openPeek } = usePeekControls();
  const peek = usePeek();
  // WHICH ROW IS THE ONE IN FOCUS — the run in the rail, or the run this path
  // names when the page was opened directly. Two sources for one highlight,
  // because both are ways of having this run open.
  const focused = peek?.kind === "run" ? peek.id : selected;

  const openRun = useCallback(
    (r: SandboxRun, e: React.MouseEvent | React.KeyboardEvent) => {
      const go = () => openPeek({ kind: "run", id: r.turn_id });
      // THE GRID HANDS THIS BOTH EVENTS. `rowPeekHandler` is the frame's one
      // copy of "which clicks mean elsewhere" and reads a mouse event — ⌘,
      // ctrl, shift, alt and the middle button belong to the browser, which is
      // what keeps the row a real link to the run's page. The `enter` chord
      // carries no button at all and is never "open elsewhere".
      if (!("button" in e)) {
        go();
        return;
      }
      rowPeekHandler(go)?.(e);
    },
    [openPeek],
  );

  const detail = rows.find((r) => r.turn_id === selected) ?? null;
  // WHAT THE RUN IS CALLED, for the breadcrumb, the browser tab and the
  // palette's recents — all of which read the one label a screen publishes and
  // fall back to the raw path segment otherwise, which here is the turn's
  // uuid. THE TASK ONLY, never the header's "No task was recorded": that
  // placeholder is a sentence about the absence of a name, and two runs
  // wearing it are two identical recents rows going to different runs. The id
  // at least tells them apart.
  usePageLabels(
    selected && detail?.task_description ? { [selected]: detail.task_description } : {},
  );
  const waiting = rows.filter((r) => AWAITING.includes(r.status)).length;
  const running = rows.filter((r) => r.status === "running").length;

  return (
    <>
      <PageActions>
        {
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
      </PageActions>
      <PageNote>
        Each one is an Execute phase that suspended. It resumes when the sandbox reports back, after
        a restart or on another node.
      </PageNote>

      {/* The flush Panel is gone: StatGroup draws that surface itself. */}
      <StatGroup columns={3}>
        <StatCard
          icon={<TerminalGlyph size="xs" />}
          label="Running"
          value={running}
          sub="a box is up and working"
        />
        <StatCard
          icon={<HelpGlyph size="xs" />}
          label="Waiting on an answer"
          value={waiting}
          sub={waiting ? "the run cannot continue until someone replies" : "nothing is blocked"}
        />
        {/* NO "FAILED" TILE, and its absence is the honest answer rather
            than a gap. A settled run has no record, so a count of failed
            rows is structurally zero on every company for ever — and a tile
            reading "Failed 0" tells an operator nothing failed, when what is
            true is that this is not where failures are recorded. They are on
            the event stream, as the resumed turn's own events or a
            `sandbox_run_failed` naming the reason. */}
        <StatCard
          icon={<Package2Glyph size="xs" />}
          label="In the record"
          value={rows.length}
          sub="every run the fleet still holds"
        />
      </StatGroup>

      {loading && !rows.length && <Skeleton variant="text" rows={4} label="Loading runs" />}
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
          <DataGrid<SandboxRun>
            rows={rows}
            rowKey={(r) => r.turn_id}
            onRowActivate={openRun}
            rowHref={(r) => peekHref({ kind: "run", id: r.turn_id })}
            isSelected={(r) => r.turn_id === focused}
            defaultSort="-updated"
            columns={[
              {
                key: "status",
                header: "Status",
                shrink: true,
                sortValue: (r) => r.status,
                // A BADGE, NOT `StatusCell`: the engine's seven words are a
                // vocabulary rather than two lifecycle states, and this is the
                // pill the Inbox and Live now draw for the same run — a third
                // rendering of one status is a third thing to keep in step.
                cell: (r) => <RunStatus status={r.status} />,
              },
              {
                key: "seat",
                header: "Seat",
                sortValue: (r) => r.role || r.agent_handle,
                // NOT `SeatCell` or `SeatChip`: both are anchors and every row
                // here is one, and an anchor inside an anchor is markup no
                // browser agrees about. The seat is one ⌘-click away from the
                // run's own page, where it is a link again.
                cell: (r) =>
                  r.role || r.agent_handle ? (
                    <TextCell icon="memory">{r.role || r.agent_handle}</TextCell>
                  ) : (
                    <EmptyValue label="No seat" />
                  ),
              },
              {
                key: "task",
                header: "Task",
                cell: (r) =>
                  r.task_description ? (
                    <TextCell>{r.task_description}</TextCell>
                  ) : (
                    <EmptyValue label="No task recorded" />
                  ),
              },
              {
                key: "agent",
                header: "Coding agent",
                shrink: true,
                sortValue: (r) => r.coding_agent,
                cell: (r) =>
                  r.coding_agent ? (
                    <Tag appearance="outline" monospace>
                      {r.coding_agent}
                    </Tag>
                  ) : (
                    <EmptyValue label="Not recorded" />
                  ),
              },
              {
                key: "where",
                header: "Runs in",
                shrink: true,
                sortValue: (r) => r.placement,
                cell: (r) =>
                  r.placement ? (
                    <Tag appearance="outline" monospace>
                      {r.placement}
                    </Tag>
                  ) : (
                    // NOT "—" FOR A LIVE ROW'S SAKE: the projection carries no
                    // placement, so this is genuinely "the store has not
                    // written this run yet" rather than a run with nowhere to
                    // run, and the dash says the first on hover.
                    <EmptyValue label="The durable row has not been written yet" />
                  ),
              },
              {
                key: "box",
                header: "Box",
                shrink: true,
                sortValue: (r) => (r.box_exists ? 1 : 0),
                cell: (r) => (
                  <StatusCell
                    glyph={r.box_exists ? "●" : "○"}
                    label={r.box_exists ? "up" : "reclaimed"}
                    tone={r.box_exists ? "info" : "neutral"}
                    title={
                      r.box_exists
                        ? "the sandbox is still there"
                        : "the sandbox has been reclaimed; the run's record remains"
                    }
                  />
                ),
              },
              {
                key: "updated",
                header: "Updated",
                shrink: true,
                sortValue: (r) => tsKey(r.updated_at || r.started_at),
                cell: (r) => <DateCell at={r.updated_at || r.started_at} now={now} />,
              },
            ]}
          />
        </Card>
      </QueryState>

      {detail && (
        <>
          <ObjectHeader
            kind="Coding run"
            icon="terminal"
            identifier={detail.turn_id.slice(0, 8)}
            title={runTitle(detail)}
            status={<RunStatus status={detail.status} />}
            facts={runFacts(detail)}
            actions={
              <>
                {detail.trace_id && (
                  <Button
                    size="small"
                    variant="secondary"
                    onClick={() => nav.to(["activity", "traces", detail.trace_id])}
                  >
                    Trace
                  </Button>
                )}
                <Button
                  size="small"
                  variant="secondary"
                  onClick={() => nav.to(["activity", "turns", detail.turn_id])}
                >
                  Turn
                </Button>
                {/* An icon with no label is an IconButton over there, and the
                    name is a required prop rather than a `title` a caller
                    remembers. */}
                <IconButton
                  size="sm"
                  variant="ghost"
                  icon={<CloseGlyph size="sm" />}
                  label="Close"
                  title="Close"
                  onClick={() => setSelected("")}
                />
              </>
            }
          />
          <Card>
            {AWAITING.includes(detail.status) && (
              <div style={{ marginBottom: "var(--space-3)" }}>
                <AwaitingBanner run={detail} now={now} />
              </div>
            )}
            <PropertiesRail
              groups={[
                {
                  properties: [
                    { label: "Owner node", value: detail.owner },
                    { label: "Started", value: fmtDateTime(detail.started_at) },
                    { label: "Updated", value: fmtDateTime(detail.updated_at) },
                    {
                      label: "Ran for",
                      value: fmtDuration(elapsedMs(detail.started_at, detail.updated_at)),
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
          </Card>
        </>
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
    <Card>
      <Card.Header
        icon={<TerminalGlyph size="sm" />}
        count={calls.length}
        subtitle="through the MCP bridge, in order"
        actions={
          failures > 0 ? <Tag variant="danger">{plural(failures, "failure")}</Tag> : undefined
        }
      >
        <Card.Title>Tool calls</Card.Title>
      </Card.Header>
      <div className="col gap-2">
        {elided > 0 && (
          // SAID OUT LOUD. The engine bounds the log at 200 and drops the
          // MIDDLE rather than the start, because how a run began and how it
          // ended are what explain it — and a log that silently skips is a
          // log that lies about what the run did.
          <Callout variant="warning" icon={<WarningGlyph size="md" />}>
            {plural(elided, "call")} from the middle of this log were dropped — the engine keeps the
            first and last hundred, so the run did more than is shown here.
          </Callout>
        )}
        <div className="list">
          {calls.map((call, i) => (
            <BridgeCallRow key={`${call.at}:${i}`} call={call} />
          ))}
        </div>
      </div>
    </Card>
  );
}

/**
 * One bridged call, opened.
 *
 * ITS OWN COMPONENT so the indenting below can be memoised: a run's log runs
 * to two hundred of these, and `useMemo` is not available inside a `map`.
 */
function BridgeCallRow({ call }: { call: BridgeCall }) {
  // JSON TEXT ON THE WIRE, never a decoded map — a large id survives one
  // encoder pass and not two — so it is INDENTED rather than re-encoded.
  // `indentJSON` walks the text and copies every literal across byte for
  // byte, which is what keeps that property while still putting each
  // argument on its own line; its own doc has the rest. The engine records
  // this compactly (`tools.RecordArgs`), so without it the whole call is one
  // line however many arguments it had.
  const args = useMemo(() => indentJSON(call.args ?? ""), [call.args]);
  // A BRIDGED TOOL'S ANSWER is a JSON document as often as the call was, and
  // what is not one comes back untouched.
  const output = useMemo(() => indentJSON(call.output ?? ""), [call.output]);
  return (
    <Disclosure
      title={
        <span className="row gap-2 baseline">
          <code className="inline">{call.name}</code>
          {call.failed && <Tag variant="danger">failed</Tag>}
          <span className="spacer" />
          <span className="t-caption">{fmtTime(call.at)}</span>
        </span>
      }
      // A LOGGED CALL IS NOT A SECTION OF THE PAGE. uilet wraps a disclosure's
      // trigger in a real heading by default, which is right for this screen's
      // own cards and wrong for a tool call: the engine bounds this log at two
      // hundred, so the default puts two hundred headings into the document
      // outline of one run. The phase card reached the same conclusion about
      // its own rows.
      headingLevel="none"
      // MOUNTED ON OPENING, for the reason uilet gives the flag: a closed row
      // is two code blocks nobody asked for, each measuring its own box, two
      // hundred times over. Closed again they unmount and take their scroll
      // position with them, which is the whole of the trade here — a record
      // block holds no other state.
      lazy
    >
      <div className="col gap-2">
        <div className="col gap-1">
          <div className="t-label">Arguments</div>
          {/* `plain` DROPS THE HEADER, which is the opposite of what the word
              meant on the block this replaced — there it turned wrapping off,
              which is `wrap` here. The header goes because the disclosure
              above already carries the tool's name and its own copy control.

              WRAPPING STAYS ON, as it is by default, and indenting is what
              makes that the right answer: these arguments used to be called
              aligned JSON, which one minified line never was. Indented, the
              newlines carry the structure and the only thing that can overrun
              the box is one long string value — a line nobody finds the end
              of, which is the case wrapping exists for.

              `focusWhenScrollable` rather than `selectable`: a bridged tool's
              arguments are not this screen's select-all subject, and a tab
              stop in front of each of two hundred short blocks is worse than
              none. The block measures its own box and takes the stop
              only when it actually scrolls, which is a fact about the
              viewport rather than about this call. */}
          <CodeBlock
            plain
            maxHeight={RECORD_MAX_HEIGHT}
            focusWhenScrollable
            label={`${call.name} arguments`}
            code={args || "(none)"}
          />
        </div>
        <div className="col gap-1">
          <div className="t-label">{call.failed ? "Error" : "Result"}</div>
          <CodeBlock
            plain
            maxHeight={RECORD_MAX_HEIGHT}
            focusWhenScrollable
            label={`${call.name} ${call.failed ? "error" : "result"}`}
            code={output || "(nothing returned)"}
          />
        </div>
      </div>
    </Disclosure>
  );
}
