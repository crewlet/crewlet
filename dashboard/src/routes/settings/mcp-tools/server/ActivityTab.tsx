/**
 * A server's Activity: calls per tool over the last week, every data node's
 * counts summed by the engine, and for an administrator the newest calls with
 * the agent that made each and how it ended, never its arguments or its
 * result. A member reads the counts only.
 */

import {
  Card,
  RelativeTime,
  TableTwoLineCell,
  Tag,
  Text,
  type DataGridColumn,
} from "@crewlethq/ui";
import { ChartColumnGlyph, ScrollTextGlyph } from "@crewlethq/icons/glyphs";
import type { McpCallRow, McpMemberToolUsage } from "~/contract/mcp.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { listed, plural } from "~/lib/format.ts";
import type { ServerDetail } from "../api.ts";
import type { McpData } from "../data.ts";
import { OUTCOME } from "../words.ts";
import { ToolName } from "./ToolsTab.tsx";

export function ActivityTab({ detail, data }: { detail: ServerDetail; data: McpData }) {
  const usage = detail.usage;
  // THE TOOL TAKES THE WIDTH and clamps inside it, its name whole on hover;
  // counts are content-sized at the row's end.
  const usageColumns: DataGridColumn<McpMemberToolUsage>[] = [
    {
      key: "tool",
      header: "Tool",
      floor: "16rem",
      sortValue: (u) => u.tool,
      cell: (u) => <ToolName name={u.tool} />,
    },
    {
      key: "calls",
      header: "Calls",
      align: "right",
      shrink: true,
      sortValue: (u) => u.calls,
      cell: (u) => <span className="mcp-num">{u.calls}</span>,
    },
    {
      key: "failed",
      header: "Failed",
      align: "right",
      shrink: true,
      sortValue: (u) => u.failed,
      cell: (u) =>
        u.failed ? (
          <Tag size="xs" variant="danger" appearance="soft">
            {u.failed}
          </Tag>
        ) : (
          <Text as="span" variant="cell" tone="tertiary" className="mcp-num">
            0
          </Text>
        ),
    },
  ];
  const callColumns: DataGridColumn<McpCallRow>[] = [
    { key: "at", header: "When", shrink: true, cell: (c) => <RelativeTime value={c.at} /> },
    {
      key: "seat",
      header: "Agent",
      cell: (c) => (
        <Text as="span" variant="inherit" truncate>
          {data.seatName(c.seat)}
        </Text>
      ),
    },
    { key: "tool", header: "Tool", floor: "16rem", cell: (c) => <ToolName name={c.tool} /> },
    {
      key: "outcome",
      header: "Outcome",
      cell: (c) => (
        <TableTwoLineCell
          value={
            <Tag size="xs" variant={OUTCOME[c.outcome].tone} appearance="soft">
              {OUTCOME[c.outcome].label}
            </Tag>
          }
          detail={c.refusal || undefined}
          mono="detail"
        />
      ),
    },
    {
      key: "ms",
      header: "Took",
      align: "right",
      shrink: true,
      cell: (c) =>
        c.duration_ms === null ? (
          ""
        ) : (
          <span className="mcp-num">{(c.duration_ms / 1000).toFixed(1)} s</span>
        ),
    },
  ];
  const quiet =
    detail.reading === "admin" ? detail.calls.coverage.nodes.filter((n) => !n.answered) : [];
  return (
    <div className="mcp-stack">
      <Card padding="none">
        <Card.Header icon={<ChartColumnGlyph size="sm" />}>
          <Card.Title>Calls by tool, last {usage.days} days</Card.Title>
        </Card.Header>
        <DataGrid<McpMemberToolUsage>
          name="usage"
          rows={[...usage.tools]}
          rowKey={(u) => u.tool}
          columns={usageColumns}
          defaultSort="-calls"
          empty={{ title: `No calls in the last ${usage.days} days`, icon: "chart-column" }}
        />
        {usage.elided > 0 ? (
          <Text as="p" variant="caption" tone="tertiary" className="mcp-note">
            Some counts are short: an agent called more tools in a day than the engine keeps a count
            for, so {plural(usage.elided, "count")} across its servers{" "}
            {usage.elided === 1 ? "was" : "were"} not kept.
          </Text>
        ) : null}
      </Card>
      {detail.reading === "admin" ? (
        <Card padding="none">
          <Card.Header icon={<ScrollTextGlyph size="sm" />}>
            <Card.Title>Recent calls</Card.Title>
          </Card.Header>
          <DataGrid<McpCallRow>
            name="calls"
            rows={[...detail.calls.calls]}
            rowKey={(c) => `${c.at}-${c.tool}-${c.seat}-${c.turn_id}`}
            columns={callColumns}
            empty={{ title: "No call yet", icon: "scroll-text" }}
          />
          {quiet.length > 0 ? (
            <Text as="p" variant="caption" tone="tertiary" className="mcp-note">
              {listed(quiet.map((n) => n.id))} did not answer, so calls kept there are not listed.
            </Text>
          ) : null}
        </Card>
      ) : null}
    </div>
  );
}
