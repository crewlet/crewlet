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

import { Tag } from "@crewlethq/ui";
import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { fmtDuration } from "~/lib/format.ts";
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

const COLUMNS: GridColumn<Row>[] = [
  {
    key: "tool",
    header: "Tool",
    cell: (r) => <code className="inline">{r.call.name}</code>,
    sortValue: (r) => r.call.name,
    phoneLead: true,
  },
  {
    key: "origin",
    header: "Where it ran",
    cell: (r) => <span className="t-caption">{originLabel(r.call)}</span>,
    sortValue: (r) => originLabel(r.call),
  },
  {
    key: "phase",
    header: "Phase · round",
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
    cell: (r) =>
      r.call.failed ? (
        <Tag variant="danger">failed</Tag>
      ) : (
        <span className="t-caption">returned</span>
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
  return (
    <DataGrid
      name="tools"
      rows={rows}
      columns={COLUMNS}
      rowKey={(r) => r.key}
      isFailed={(r) => r.call.failed}
      onRowActivate={(r) => onOpen(target(r))}
      empty={{ title: "This turn called no tool", icon: "wrench" }}
    />
  );
}
