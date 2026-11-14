/**
 * A turn's Tools tab: every tool call it made, with who answered it.
 *
 * ONE ROW PER EXECUTION, in the order the turn made them — its own phases'
 * calls and its workers', each named with the phase and round it belongs to —
 * and the ORIGIN the registry recorded when the tool was registered
 * (`builtin` or `mcp:<server>`), the one fact that tells an engine builtin
 * from somebody else's MCP server. A call no tool answered (an unknown name,
 * one a guard refused) has no origin, and says so rather than borrowing one.
 *
 * A ROW OPENS ITS SPAN on the Timeline, where its input, its output and why
 * the round asked for it are — one place for a call's detail, not two. A call
 * the waterfall has no bar for opens what holds it: a worker's call its
 * worker's span, any other the Timeline, whose list of unplaced spans names it.
 *
 * "NOT TIMED" IS THE WATERFALL'S OWN ANSWER ([callMeasured]), never a second
 * reading of the duration: a submission stamped and finished inside a
 * millisecond is a measured 0, and the two tabs said "0ms" and "not timed"
 * about the same call.
 */

import { InlineCode, TableTextCell, Tag } from "@crewlethq/ui";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { type DataGridColumn } from "@crewlethq/ui";
import { fmtDuration, plural } from "~/lib/format.ts";
import { droppedWords } from "~/lib/dropped.ts";
import { callMeasured, phaseLabel, type Waterfall } from "~/lib/waterfall.ts";
import type { PhaseRecord, ToolCall } from "~/lib/phases.ts";

interface Row {
  key: string;
  /** The Timeline span this call is drawn as. */
  span: string;
  phase: PhaseRecord;
  call: ToolCall;
  order: number;
}

/** Where a call ran, in a reader's words. */
export function originLabel(call: Pick<ToolCall, "origin" | "server">): string {
  if (call.origin === "builtin") return "built in";
  if (call.server) return `MCP · ${call.server}`;
  return call.origin || "no tool answered";
}

const COLUMNS: DataGridColumn<Row>[] = [
  {
    // THE CALL'S NAME IS THE POINT OF THE LIST, so it keeps a floor and every
    // fact beside it is sized to its content and gives way first.
    key: "tool",
    header: "Tool",
    floor: "10rem",
    overflow: "truncate",
    title: (r) => r.call.name,
    cell: (r) => <InlineCode>{r.call.name}</InlineCode>,
    sortValue: (r) => r.call.name,
    phoneLead: true,
  },
  {
    key: "origin",
    header: "Where it ran",
    shrink: true,
    drop: 2,
    cell: (r) => <TableTextCell>{originLabel(r.call)}</TableTextCell>,
    sortValue: (r) => originLabel(r.call),
  },
  {
    // THE ORDER THE TURN MADE THEM, which is the list's default.
    key: "phase",
    header: "Phase · round",
    shrink: true,
    drop: 1,
    cell: (r) => (
      <span className="t-caption">
        {r.phase.hostPhase ? `${r.phase.worker || "worker"} · ` : ""}
        {phaseLabel(r.phase)} · round {r.call.round}
      </span>
    ),
    sortValue: (r) => r.order,
  },
  {
    key: "took",
    header: "Took",
    align: "right",
    shrink: true,
    cell: (r) => (
      <span className="t-caption t-num">
        {callMeasured(r.call) ? fmtDuration(r.call.durationMs) : "not timed"}
      </span>
    ),
    sortValue: (r) => (callMeasured(r.call) ? r.call.durationMs : null),
  },
  {
    key: "outcome",
    header: "Outcome",
    shrink: true,
    // A HELD CALL IS NO FAILURE AND RETURNED NOTHING: it is the run's question
    // for a person, recorded until they approve it, as the execute card says.
    cell: (r) =>
      r.call.failed ? (
        <Tag variant="danger">failed</Tag>
      ) : (
        <span className="t-caption">{r.call.held ? "held for approval" : "returned"}</span>
      ),
    sortValue: (r) => (r.call.failed ? 0 : 1),
  },
];

export function ToolsTab({
  phases,
  model,
  onOpen,
}: {
  phases: readonly PhaseRecord[];
  /** The Timeline's model, which says which calls have a span to open. */
  model: Waterfall;
  /** Open a span on the Timeline — or the Timeline alone, for "". */
  onOpen: (spanId: string) => void;
}) {
  const drawn = new Set(model.spans.map((s) => s.id));
  // A WORKER'S CALL opens its worker — the waterfall draws a worker as one
  // span, not its calls — and any other call without a bar opens the Timeline
  // alone, whose list of what it could not place names that call.
  const target = (r: Row) =>
    drawn.has(r.span) ? r.span : r.phase.hostPhase && drawn.has(r.phase.key) ? r.phase.key : "";
  let order = 0;
  const rows: Row[] = phases.flatMap((p) =>
    p.tools.map((call, i) => ({
      key: `${p.key}#${i}`,
      span: `${p.key}.r${call.round}.t${i}`,
      phase: p,
      call,
      order: order++,
    })),
  );
  // THE CALLS NO RECORD LISTS, said above the ones that are: a record keeps a
  // long phase's head and tail within its share of one message, and a
  // collected executor's record its run's, each counting the middle it
  // dropped by tool, and a tab that listed two hundred rows of a phase that
  // made more would read as the whole phase.
  const dropped = phases.reduce((n, p) => n + p.callsElided, 0);
  return (
    <div className="col gap-2">
      {dropped > 0 && (
        <span className="t-caption">
          {plural(dropped, "more call")} {dropped === 1 ? "isn’t" : "aren’t"} listed: the phases’
          records didn’t keep {dropped === 1 ? "it" : "them"}.
          {droppedWords(phases.flatMap((p) => p.elidedCalls))}
        </span>
      )}
      <DataGrid
        name="tools"
        rows={rows}
        columns={COLUMNS}
        rowKey={(r) => r.key}
        defaultSort="phase"
        isFailed={(r) => r.call.failed}
        onRowActivate={(r) => onOpen(target(r))}
        empty={{ title: "This turn called no tool", icon: "wrench" }}
      />
    </div>
  );
}
