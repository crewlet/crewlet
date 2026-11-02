/**
 * A server's Tools: every tool its instances list across the fleet, each with
 * the risk the engine read from its hints and how often it was called this
 * week, and the tools no agent is offered under their names, with the
 * engine's reason.
 *
 * THE ENGINE READS THE RISK (`risk`, `risk_declared`, `external` on each
 * tool): a tool that declares nothing counts as destructive, and the chip
 * says so.
 */

import { useMemo, useState } from "react";
import {
  Card,
  FilterChip,
  FilterChipGroup,
  Input,
  TableTwoLineCell,
  Tag,
  Text,
  Tooltip,
  type DataGridColumn,
} from "@crewlethq/ui";
import { SearchGlyph, TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import type { McpMemberServerStatus, McpRisk, McpToolView } from "~/contract/mcp.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { useParam } from "~/app/router.tsx";
import type { ServerDetail } from "../api.ts";
import { RiskChips } from "../parts.tsx";
import { RISK } from "../words.ts";

type Filter = "all" | McpRisk;

const FILTERS: readonly [Filter, string][] = [
  ["all", "All"],
  ["read_only", RISK.read_only.label],
  ["additive", RISK.additive.label],
  ["destructive", RISK.destructive.label],
];

/** A tool's name in its cell: clamped to the column, whole on hover. */
export function ToolName({ name }: { name: string }) {
  return (
    <Tooltip content={<span className="mono">{name}</span>} placement="top">
      <Text as="span" variant="inherit" mono truncate titleWhenCut={false}>
        {name}
      </Text>
    </Tooltip>
  );
}

export function ToolsTab({
  server,
  detail,
}: {
  server: McpMemberServerStatus;
  detail: ServerDetail;
}) {
  const [q, setQ] = useParam("q", "");
  const [filter, setFilter] = useState<Filter>("all");
  const calls = useMemo(
    () => new Map(detail.usage.tools.map((u) => [u.tool, u.calls])),
    [detail.usage.tools],
  );
  const matching = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return detail.tools.filter(
      (t) => !needle || `${t.name} ${t.title} ${t.description}`.toLowerCase().includes(needle),
    );
  }, [detail.tools, q]);
  const shown = filter === "all" ? matching : matching.filter((t) => t.risk === filter);
  const count = (f: Filter) =>
    f === "all" ? matching.length : matching.filter((t) => t.risk === f).length;
  const refused = server.refused_tools;
  const unlisted = detail.reading === "admin" ? detail.server.unlisted_annotations : [];

  // THE TOOL TAKES THE WIDTH, as a model's name does on Models: risk and
  // calls are content-sized, so a description is not cut at a quarter of the
  // table while three-digit counts sit in a column as wide.
  const columns: DataGridColumn<McpToolView>[] = [
    {
      key: "tool",
      header: "Tool",
      floor: "18rem",
      sortValue: (t) => t.name,
      cell: (t) => (
        <TableTwoLineCell
          value={<ToolName name={t.name} />}
          detail={t.description || t.title || undefined}
          titleWhenCut={false}
        />
      ),
    },
    {
      key: "risk",
      // The head says what the column is; each chip's tooltip says it is the
      // server's own hint.
      header: "Risk",
      shrink: true,
      sortValue: (t) => ["read_only", "additive", "destructive"].indexOf(t.risk),
      cell: (t) => <RiskChips tool={t} starred />,
    },
    {
      key: "calls",
      header: "Calls, 7 days",
      align: "right",
      shrink: true,
      sortValue: (t) => calls.get(t.name) ?? 0,
      // A zero in the column's own size, quieter: a 12px word among 13px
      // figures stood on another baseline.
      cell: (t) => {
        const n = calls.get(t.name) ?? 0;
        return (
          <Text as="span" variant="cell" tone={n > 0 ? "primary" : "tertiary"} className="mcp-num">
            {n}
          </Text>
        );
      },
    },
  ];

  return (
    <div className="mcp-stack">
      {refused.length > 0 ? (
        <Card padding="none">
          <Card.Header icon={<TriangleAlertGlyph size="sm" />} count={server.refused_count}>
            <Card.Title>Not offered</Card.Title>
          </Card.Header>
          <DataGrid
            name="refused"
            rows={[...refused]}
            rowKey={(t) => t.tool}
            columns={[
              {
                key: "tool",
                header: "Tool",
                cell: (t) => <TableTwoLineCell value={t.tool} mono="value" detail={t.message} />,
              },
            ]}
          />
        </Card>
      ) : null}
      {detail.tools.length === 0 ? (
        <Card padding="none">
          <Card.Body>
            <Text as="p" variant="body" tone="secondary">
              {server.enabled
                ? "The server lists no tools yet. They appear once an instance starts and lists them."
                : "A disabled server lists no tools."}
            </Text>
          </Card.Body>
        </Card>
      ) : (
        <Card padding="none">
          {/* THE SHIPPED TOOLS PAGE'S TOOLBAR: a search field and the kit's
              filter chips, one 26px step, the chosen chip's count in its own
              ink. */}
          <div className="mcp-toolbar">
            <span className="mcp-toolbar__search">
              <Input
                type="search"
                aria-label="Search tools"
                placeholder="Search tools"
                inputSize="sm"
                width="full"
                leading={<SearchGlyph size="sm" />}
                value={q}
                onChange={(e) => setQ(e.target.value)}
              />
            </span>
            <FilterChipGroup
              label="Show"
              hideLabel
              semantics="radio"
              value={filter}
              onValueChange={(next) => setFilter((next ?? "all") as Filter)}
            >
              {FILTERS.filter(([v]) => v === "all" || count(v) > 0).map(([value, label]) => (
                <FilterChip key={value} value={value} count={count(value)}>
                  {label}
                </FilterChip>
              ))}
            </FilterChipGroup>
          </div>
          <DataGrid<McpToolView>
            name="tools"
            rows={shown}
            rowKey={(t) => t.name}
            columns={columns}
            empty={{ title: "No tool matches", icon: "search" }}
          />
          {detail.tool_count > detail.tools.length ? (
            <Text as="p" variant="caption" tone="tertiary" className="mcp-note">
              {detail.tool_count - detail.tools.length} more are listed than a node publishes, so
              they are not shown here.
            </Text>
          ) : null}
          {server.shadowed_tools.length > 0 ? (
            <Text as="p" variant="caption" tone="tertiary" className="mcp-note">
              <Tag size="xs" variant="neutral" appearance="outline">
                Shadowed
              </Tag>{" "}
              {server.shadowed_tools.map((t) => t.message).join(" ")}
            </Text>
          ) : null}
          {unlisted.length > 0 ? (
            // AN OVERRIDE THAT MATCHES NOTHING is configuration only an
            // administrator reads: a renamed tool, or a typo, whose hint
            // overrides nothing anywhere.
            <Text as="p" variant="caption" tone="tertiary" className="mcp-note">
              {unlisted.length === 1
                ? "One tool_annotations key matches no tool any instance lists: "
                : `${unlisted.length} tool_annotations keys match no tool any instance lists: `}
              <span className="mono">{unlisted.join(", ")}</span>
            </Text>
          ) : null}
        </Card>
      )}
    </div>
  );
}
