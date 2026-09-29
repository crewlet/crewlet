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
 * path `#/live/runs/{turn_id}` is the run's OWN page — what a ⌘-click, the
 * rail's `Open ↗` and every link to one run reach — and it draws the run
 * alone. It used to draw the whole list with the run tacked on beneath it, so
 * a link to one run landed on a board with the run below the fold.
 *
 * # What the page says a run did
 *
 * Two sources, one per half of a run's life. While the job runs, its output is
 * asked of the node that owns it (`sandbox_tail`, every few seconds, only while
 * the page is open); once the run is collected its record is deleted and what
 * it did is the `sandbox` phase its turn published, with the transcript on it.
 * So a run that has settled still has a page: it is the turn's collected runs,
 * read from the turn — the record a reader followed the link for.
 */

import { useCallback, useEffect, useMemo, useRef } from "react";
import { useNavigator } from "~/app/router.tsx";
import { QueryState, RECORD_MAX_HEIGHT } from "~/components/common.tsx";
import {
  Button,
  Callout,
  Card,
  CodeBlock,
  Disclosure,
  EmptyState,
  EmptyValue,
  Skeleton,
  StatCard,
  StatGroup,
  Tag,
} from "@crewlethq/ui";
import { PropertiesRail } from "~/app/frame/PropertiesRail.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, SeatLabel, StatusCell, TextCell } from "~/app/frame/cells.tsx";
import { usePageLabels } from "~/app/Shell.tsx";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import {
  CircleQuestionMarkGlyph,
  PackageGlyph,
  SquareTerminalGlyph,
  TriangleAlertGlyph,
} from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg, useSandboxes } from "~/lib/store-hooks.ts";
import { indexOrg, nameOfIn, useSeatBadgeOf, type NameOf } from "~/lib/seats.ts";
import { RUNS_POLL_MS } from "~/lib/runs.ts";
import { fromPhaseEvent, type PhaseRecord } from "~/lib/phases.ts";
import { AnswerRunButton } from "~/components/writes.tsx";
import { LiveOutput } from "./trace/Waterfall.tsx";
import {
  elapsedMs,
  fmtDateTime,
  fmtDuration,
  fmtCount,
  fmtTime,
  inTime,
  plural,
  tsKey,
} from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { indentJSON } from "~/lib/jsontext.ts";
import type {
  BridgeCall,
  EventRecord,
  SandboxEntry,
  SandboxRun,
  SandboxStatus,
} from "~/protocol/index.ts";
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
    // THE PROJECTION HOLDS NO RESOLUTION: who the question is put to is on
    // the durable row alone, which replaces this entry once it is read.
    audience_handles: [],
    audience_fallback: false,
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
function runFacts(run: SandboxRun, nameOf: NameOf): Fact[] {
  return [
    {
      label: "Seat",
      // THE SEAT'S NAME, LINKED, rather than a chip: a fact wraps inside its
      // two-line clamp, and a chip is one line cut at the track's edge — the
      // seat a run belongs to read "Agent Fronten…" on a page with room for
      // it three times over. An agent seat by construction: the engine never
      // runs a human seat, so a coding run is always an agent's.
      value: nameOf(run.agent_handle) || run.role,
      path: run.agent_handle ? ["agents", "seats", run.agent_handle] : undefined,
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
 * What a parked run is waiting for, from whom — and the answer to it.
 *
 * ONE COPY for the page and the rail: the sentences below are the whole of
 * what a reader can DO about a paused run, and two spellings of them is how
 * one of them comes to describe a chat reply path the run does not have.
 *
 * # Answered here, whatever launched it
 *
 * A run is answered by its TURN (`answer_run`), which reaches a run whatever
 * woke the turn that launched it. The banner used to say how to answer and
 * offer nothing to press: a run a schedule tick or a task assignment launched
 * stored no conversation at all, so "the run resumes when the coordinator
 * carries an answer back" described a door with no handle on it. The chat
 * path is still named where there is one, as the other way in.
 */
export function AwaitingBanner({
  run,
  now,
  nameOf,
}: {
  run: SandboxRun;
  now: number;
  nameOf: NameOf;
}) {
  const deadline = pauseDeadline(run);
  const expired = deadline !== null && tsKey(deadline) <= now;
  const seat = nameOf(run.agent_handle) || run.role || run.agent_handle;
  const audience = (run.audience_handles ?? []).map(nameOf);
  return (
    <Callout variant="warning" icon={<CircleQuestionMarkGlyph size="md" />}>
      <span className="col gap-2 run-awaiting">
        <strong>{run.question || "The run asked a question."}</strong>
        <span className="t-caption">
          {/* WHO IT IS PUT TO, as the engine resolved it when the run parked
              — never re-derived here from the coding agent's own label. */}
          {audience.length > 0
            ? `Put to ${listWords(audience)}${run.audience_fallback ? `, ${seat}'s lead chain — "${run.audience}" named nobody on the chart` : ""}.`
            : "Nothing recorded whom this question is put to — anybody who can act may answer it."}
          {run.answerable_in_chat
            ? " A reply on the conversation this turn served is taken as the answer too."
            : ""}
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
        <span className="row gap-2">
          <AnswerRunButton turnId={run.turn_id} seat={seat} question={run.question} />
        </span>
      </span>
    </Callout>
  );
}

/** "A", "A and B", "A, B and C". */
function listWords(names: string[]): string {
  if (names.length <= 1) return names[0] ?? "";
  return `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
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
  const nameOf = useNameOf();
  const { data, loading, error } = useQuery("sandbox_runs", undefined, {
    enabled: turnId !== "",
    pollMs: RUNS_POLL_MS,
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
            icon={<SquareTerminalGlyph size={32} />}
            title="No coding run in flight for this turn"
            description="A run's record is deleted once it settles, and what it did is then on its turn — open the run's page for its collected output. Otherwise the id may be wrong."
          />
        )}
        {run && (
          <>
            <ObjectHeader
              size="peek"
              kind="Coding run"
              icon="square-terminal"
              identifier={run.turn_id.slice(0, 8)}
              title={runTitle(run)}
              status={<RunStatus status={run.status} />}
              facts={runFacts(run, nameOf)}
            />
            <div className="col gap-3">
              <section className="col gap-2">
                <div className="t-label">
                  {AWAITING.includes(run.status) ? "Waiting on a person" : "Doing now"}
                </div>
                {AWAITING.includes(run.status) ? (
                  <AwaitingBanner run={run} now={now} nameOf={nameOf} />
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
                          path: ["live", "turns", run.turn_id],
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

export function Runs() {
  const seatBadge = useSeatBadgeOf();
  const live = useSandboxes();
  const now = useNow();
  // Durable runs have no push behind them, so this is the one place a poll is
  // correct — and it is slow, because a run's lifetime is minutes.
  const { data, loading, error } = useQuery("sandbox_runs", undefined, { pollMs: RUNS_POLL_MS });

  const rows = useMemo(() => mergeRuns(data?.runs ?? [], live), [data, live]);

  // THE ORDER `[` AND `]` WALK is the one on screen, which is this list sorted
  // as the reader left it. Published from the merged rows rather than from the
  // query's own, so the stepper cannot walk past a live run the store has not
  // written yet — the rail would open on a run this list does not show.
  usePeekNeighbours(
    useMemo(() => rows.map((r) => ({ kind: "run" as const, id: r.turn_id })), [rows]),
  );

  const { open: openPeek } = usePeekControls();

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
          icon={<SquareTerminalGlyph size="xs" />}
          label="Running"
          value={running}
          sub="a box is up and working"
        />
        <StatCard
          icon={<CircleQuestionMarkGlyph size="xs" />}
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
          icon={<PackageGlyph size="xs" />}
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
                title: "No coding run is in flight",
                hint: "A run is listed while it runs or waits on a person; a finished run's record is its turn's trace. A run starts when a seat calls the sandbox tool, and providers.sandbox gives it a place to run.",
              }
        }
      >
        <Card padding="none">
          <DataGrid<SandboxRun>
            rows={rows}
            rowKey={(r) => r.turn_id}
            onRowActivate={openRun}
            rowHref={(r) => peekHref({ kind: "run", id: r.turn_id })}
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
                // THE TWO COLUMNS A ROW IS RECOGNISED BY keep a width beside a
                // peek; the three that describe the box give way first — see
                // DataGrid's `fitColumns`. At 1280 with a run open they were
                // drawn one letter wide.
                floor: "9rem",
                sortValue: (r) => r.role || r.agent_handle,
                // NOT `SeatCell` or `SeatChip`: both are anchors and every row
                // here is one, and an anchor inside an anchor is markup no
                // browser agrees about. The seat is one ⌘-click away from the
                // run's own page, where it is a link again.
                cell: (r) =>
                  r.role || r.agent_handle ? (
                    <SeatLabel {...seatBadge(r.role || r.agent_handle)} />
                  ) : (
                    <EmptyValue label="No seat" />
                  ),
              },
              {
                key: "task",
                header: "Task",
                floor: "10rem",
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
                drop: 1,
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
                drop: 2,
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
                drop: 3,
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
    </>
  );
}

/** A handle as the seat's name, off the one org index every screen reads. */
function useNameOf(): NameOf {
  const org = useOrg();
  return useMemo(() => nameOfIn(indexOrg(org)), [org]);
}

/** The statuses whose job is running now, so its output is asked of its owner. */
const RUNNING: SandboxStatus[] = ["launching", "running"];

/**
 * The `sandbox` phases a turn published — one per collected job, oldest first.
 *
 * WHAT A SETTLED RUN LEAVES. The run's record is deleted once its box is
 * reclaimed, so after collection the only account of what the job did is the
 * phase record its turn published, with the transcript on it.
 */
export function collectedRuns(events: readonly EventRecord[] | undefined): PhaseRecord[] {
  return (events ?? [])
    .map(fromPhaseEvent)
    .filter((r): r is PhaseRecord => r !== null && r.phase === "sandbox")
    .sort((a, b) => tsKey(a.at) - tsKey(b.at));
}

/**
 * One coding run, on its own page.
 *
 * # Two records, one per half of its life
 *
 * The durable row (`sandbox_runs`, merged with the live projection exactly as
 * the board does) while the run is in flight — its status, its box, the
 * question it is parked on and the bridge's tool log — and the turn's own
 * `sandbox` phases once it is collected. The page reads both, because a run
 * page is reached from a link long after the run it names has settled, and
 * "no such run" is the wrong thing to tell somebody holding a link to a run
 * that finished.
 *
 * THE TURN IS ASKED AGAIN when the run's status moves: a collected job's phase
 * record lands on the turn, and the row leaving the board is the moment it
 * does.
 */
export function RunScreen({ turnId }: { turnId: string }) {
  const nav = useNavigator();
  const now = useNow();
  const live = useSandboxes();
  const nameOf = useNameOf();
  const board = useQuery("sandbox_runs", undefined, { pollMs: RUNS_POLL_MS });
  const turn = useQuery("turn", { turn_id: turnId });

  const run = useMemo(
    () => mergeRuns(board.data?.runs ?? [], live).find((r) => r.turn_id === turnId) ?? null,
    [board.data, live, turnId],
  );
  const collected = useMemo(() => collectedRuns(turn.data?.events), [turn.data]);

  const status = run?.status ?? "";
  const moved = useRef(status);
  const refetchTurn = turn.refetch;
  useEffect(() => {
    if (moved.current === status) return;
    moved.current = status;
    refetchTurn();
  }, [status, refetchTurn]);

  // WHAT THE RUN IS CALLED, for the breadcrumb, the browser tab and the
  // palette's recents — the task only, never the header's "No task was
  // recorded", which two runs would wear as two identical recents rows. A
  // collected run's row is gone, and its task is on the `sandbox_run_started`
  // its launch published, which the turn holds.
  const task = run?.task_description || launchedTask(turn.data?.events);
  usePageLabels(task ? { [turnId]: task } : {});

  const loading = (board.loading && !board.data) || (turn.loading && !turn.data);
  const settled = !run && collected.length > 0;
  const last = collected[collected.length - 1];
  // TWO READS, AND "NONE" NEEDS BOTH. The page is drawn from the run record
  // (`sandbox_runs`) and the turn (`turn`), and only when BOTH answered does
  // finding the run in neither mean there is none. It used to say "neither
  // holds a coding run" whenever one of them failed, which is a claim about a
  // record the page never read — "could not read" and "none" are opposite
  // answers, so a single failure is named rather than folded into either.
  const answered = !board.error && !turn.error && Boolean(board.data) && Boolean(turn.data);
  const absent = !loading && !run && collected.length === 0;
  const unread =
    board.error && turn.error ? null : board.error ? "board" : turn.error ? "turn" : null;
  // THE HEADER SAYS WHAT THE BODY KNOWS. "No task was recorded" is a fact
  // about a RUN whose launch carried no task, so it is only worn by one; a
  // turn with no run is headed as having none, and a page still reading (or
  // missing one of its two sources) claims neither.
  const title = run
    ? runTitle(run)
    : task
      ? task
      : collected.length > 0
        ? "No task was recorded"
        : absent && answered
          ? "No coding run"
          : "Coding run";
  const seat = run?.agent_handle || last?.role || "";
  // THE TRACE the run belongs to: its row names it while it is in flight, and
  // the turn's own events do once it is collected.
  const traceId = run?.trace_id || turn.data?.events?.find((e) => e.trace_id)?.trace_id || "";

  return (
    <>
      <ObjectHeader
        kind="Coding run"
        icon="square-terminal"
        identifier={turnId.slice(0, 8)}
        title={title}
        status={
          run ? (
            <RunStatus status={run.status} />
          ) : settled ? (
            <Tag variant="neutral" dot>
              collected
            </Tag>
          ) : undefined
        }
        facts={
          run
            ? runFacts(run, nameOf)
            : settled
              ? [
                  { label: "Seat", value: nameOf(seat) },
                  { label: "Coding agent", value: last?.codingAgent ?? "" },
                  { label: "Jobs", value: String(collected.length) },
                ]
              : []
        }
        actions={
          <>
            {traceId && (
              <Button
                size="small"
                variant="secondary"
                onClick={() => nav.to(["live", "traces", traceId])}
              >
                Trace
              </Button>
            )}
            <Button
              size="small"
              variant="secondary"
              onClick={() => nav.to(["live", "turns", turnId])}
            >
              Turn
            </Button>
          </>
        }
      />
      {loading && <Skeleton variant="text" rows={6} label="Loading the run" />}
      <QueryState error={board.error && turn.error ? board.error : null} loading={loading}>
        {unread && (
          <Callout variant="warning" icon={<TriangleAlertGlyph size="md" />}>
            {unread === "board"
              ? "The run record could not be read, so whether this run is in flight is unknown; what is below is what the turn holds."
              : "The turn could not be read, so what a collected run did is unknown; what is below is what the run record holds."}
          </Callout>
        )}
        {absent && answered && (
          <EmptyState
            icon={<SquareTerminalGlyph size={32} />}
            title="No coding run for this turn"
            description="Neither the run record nor the turn holds a coding run under this id. A turn's record is kept for the store's retention window; the id may be wrong, or the turn may be older than that."
          />
        )}
        {run && AWAITING.includes(run.status) && (
          <AwaitingBanner run={run} now={now} nameOf={nameOf} />
        )}
        {/* WHAT IT IS DOING, for a run nobody is being waited on by: a parked
            run's banner above already says what it is waiting for. */}
        {run && !AWAITING.includes(run.status) && (
          <Card>
            <Card.Header icon={<SquareTerminalGlyph size="sm" />}>
              <Card.Title>Doing now</Card.Title>
            </Card.Header>
            {RUNNING.includes(run.status) && run.launch_id ? (
              <LiveOutput turnId={run.turn_id} launchId={run.launch_id} now={now} />
            ) : RUNNING.includes(run.status) ? (
              <span className="t-caption">
                {run.owner
                  ? "This run's record names no job, so its live output cannot be asked for — it arrives on the turn when the run is collected."
                  : "The run's durable record has not been written yet; its live output can be asked for once it has."}
              </span>
            ) : (
              <p className="t-body">{doingLine(run)}</p>
            )}
          </Card>
        )}
        {collected.map((record, i) => (
          <CollectedRun key={record.key} record={record} ordinal={i + 1} of={collected.length} />
        ))}
        {run && (
          <Card>
            <Card.Header>
              <Card.Title>The box</Card.Title>
            </Card.Header>
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
                      path: ["live", "turns", run.turn_id],
                    },
                  ],
                },
              ]}
            />
          </Card>
        )}
        {run && <BridgeLog run={run} />}
      </QueryState>
    </>
  );
}

/**
 * The task the newest launch of this turn was given — `sandbox_run_started`'s
 * own `task` — or "" when the turn holds no launch.
 */
export function launchedTask(events: readonly EventRecord[] | undefined): string {
  const started = (events ?? [])
    .filter((e) => e.type === "sandbox_run_started")
    .sort((a, b) => tsKey(b.timestamp) - tsKey(a.timestamp))[0];
  const task = started?.payload?.task;
  return typeof task === "string" ? task : "";
}

/**
 * One collected job: what it delivered and what it said, off the `sandbox`
 * phase its turn published.
 */
function CollectedRun({
  record,
  ordinal,
  of,
}: {
  record: PhaseRecord;
  ordinal: number;
  of: number;
}) {
  return (
    <Card>
      <Card.Header
        icon={<SquareTerminalGlyph size="sm" />}
        className="card-head-stacked"
        subtitle={`collected ${fmtDateTime(record.at)}${record.launchId ? ` · job ${record.launchId.slice(0, 8)}` : ""}`}
        actions={record.failed ? <Tag variant="danger">failed</Tag> : undefined}
      >
        <Card.Title>{of > 1 ? `What job ${ordinal} of ${of} did` : "What the run did"}</Card.Title>
      </Card.Header>
      <div className="col gap-3">
        <PropertiesRail
          groups={[
            {
              properties: [
                { label: "Coding agent", value: record.codingAgent },
                {
                  label: "Box",
                  value: record.sandboxId ? <code className="inline">{record.sandboxId}</code> : "",
                },
                {
                  label: "Delivered",
                  value: record.deliveredRefs.length ? record.deliveredRefs.join(", ") : "",
                },
                {
                  label: "Tokens",
                  value: record.totalTokens > 0 ? fmtCount(record.totalTokens) : "",
                },
                // ONLY A JOB THAT FAILED HAS ONE, so only its row carries the
                // property — a dash under "Error" on every run that worked
                // reads as a field somebody forgot to fill.
                ...(record.error ? [{ label: "Error", value: record.error }] : []),
              ],
            },
          ]}
        />
        {record.response.trim() && (
          <div className="col gap-1">
            <span className="t-label">What it reported</span>
            <p className="t-body run-report">{record.response.trim()}</p>
          </div>
        )}
        {record.transcript ? (
          <CodeBlock
            plain
            selectable
            copyable
            maxHeight={RECORD_MAX_HEIGHT}
            label="What the run did"
            code={record.transcript}
          />
        ) : (
          <span className="t-caption">The run&rsquo;s record carries no transcript.</span>
        )}
      </div>
    </Card>
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
        icon={<SquareTerminalGlyph size="sm" />}
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
          <Callout variant="warning" icon={<TriangleAlertGlyph size="md" />}>
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
 * One bridged call's two records, formatted.
 *
 * INSIDE the lazy disclosure, and that placement is the point. `useMemo` in
 * the row above would run for every row the moment the log rendered, because
 * `lazy` defers a disclosure's CHILDREN and not its parent's body — so a run
 * at the engine's bound of two hundred calls parsed every argument and every
 * result, results included, before the reader opened one of them.
 */
function BridgeCallRecords({ call }: { call: BridgeCall }) {
  // JSON TEXT ON THE WIRE, never a decoded map — a large id survives one
  // encoder pass and not two — so it is INDENTED rather than re-encoded.
  // `indentJSON` walks the text and copies every literal across byte for
  // byte, which is what keeps that property while still putting each argument
  // on its own line; its own doc has the rest. The engine records this
  // compactly (`tools.RecordArgs`), so without it the whole call is one line
  // however many arguments it had.
  const args = useMemo(() => indentJSON(call.args ?? ""), [call.args]);
  // A BRIDGED TOOL'S ANSWER is a JSON document as often as the call was, and
  // what is not one comes back untouched.
  const output = useMemo(() => indentJSON(call.output ?? ""), [call.output]);
  return (
    <div className="col gap-2">
      <div className="col gap-1">
        <div className="t-label">Arguments</div>
        {/* `plain` DROPS THE HEADER, which is the opposite of what the word
            meant on the block this replaced — there it turned wrapping off,
            which is `wrap` here. The header goes because the disclosure above
            already carries the tool's name and its own copy control.

            WRAPPING STAYS ON, as it is by default, and indenting is what makes
            that the right answer: these arguments used to be called aligned
            JSON, which one minified line never was. Indented, the newlines
            carry the structure and the only thing that can overrun the box is
            one long string value — a line nobody finds the end of, which is
            the case wrapping exists for.

            `focusWhenScrollable` rather than `selectable`: a bridged tool's
            arguments are not this screen's select-all subject, and a tab stop
            in front of each of two hundred short blocks is worse than none.
            The block measures its own box and takes the stop only when it
            actually scrolls, which is a fact about the viewport rather than
            about this call. */}
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
  );
}

/**
 * One bridged call, opened.
 *
 * ITS OWN COMPONENT so the indenting below can be memoised: a run's log runs
 * to two hundred of these, and `useMemo` is not available inside a `map`.
 */
function BridgeCallRow({ call }: { call: BridgeCall }) {
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
      <BridgeCallRecords call={call} />
    </Disclosure>
  );
}
