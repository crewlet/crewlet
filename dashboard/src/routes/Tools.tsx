/**
 * Every tool a seat can call, grouped by where it came from.
 *
 * The origin grammar is two-valued — `builtin`, `mcp:<server>`, `a2a` — and it
 * is recorded at registration, which is the only frame that knows and the last
 * that can say.
 */

import { useCallback, useMemo } from "react";
import { plural } from "~/lib/format.ts";
import { useParam } from "~/app/router.tsx";
import { Section } from "~/components/common.tsx";
import { useTools } from "~/lib/store-hooks.ts";
import type { ToolRow } from "~/protocol/index.ts";
import { BuildGlyph, CableGlyph, Package2Glyph } from "@crewlethq/icons/glyphs";
import {
  Callout,
  DataView,
  type DataViewColumn,
  EmptyState,
  type FilterDef,
  type FilterValues,
  InlineCode,
  PageHeader,
  StatCard,
  StatGroup,
  Tag,
} from "@crewlethq/ui";

function originOf(source: string): { kind: string; detail: string } {
  const idx = source.indexOf(":");
  if (idx < 0) return { kind: source || "unknown", detail: "" };
  return { kind: source.slice(0, idx), detail: source.slice(idx + 1) };
}

export function Tools() {
  const tools = useTools();
  const [q, setQ] = useParam("q", "");
  const [origin, setOrigin] = useParam("origin", "");

  const origins = useMemo(() => {
    const map = new Map<string, number>();
    for (const t of tools) map.set(t.source, (map.get(t.source) ?? 0) + 1);
    return [...map.entries()].sort((a, b) => b[1] - a[1]);
  }, [tools]);

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return tools
      .filter((t) => !origin || t.source === origin)
      .filter(
        (t) =>
          !needle ||
          t.name.toLowerCase().includes(needle) ||
          (t.description ?? "").toLowerCase().includes(needle),
      );
  }, [tools, q, origin]);

  const builtins = tools.filter((t) => t.source === "builtin").length;
  const mcp = tools.filter((t) => t.source.startsWith("mcp")).length;

  /*
   * THE SCREEN OWNS THE NARROWING: both axes are URL parameters, so a narrowed
   * catalogue is a link. The origin list is built from what is registered,
   * which is honest here in a way it is not on the event log: an origin with
   * no tools in it does not exist.
   */
  const filters = useMemo<FilterDef<ToolRow>[]>(
    () => [
      {
        name: "q",
        label: "Search tools",
        role: "search",
        placeholder: "Search by name or description",
      },
      {
        name: "origin",
        label: "Origin",
        kind: "select",
        options: [
          { value: "", label: "Any origin" },
          ...origins.map(([source, count]) => ({ value: source, label: `${source} (${count})` })),
        ],
      },
    ],
    [origins],
  );

  const values: FilterValues = { q, origin };

  const onValuesChange = useCallback(
    (next: FilterValues) => {
      setQ(String(next.q ?? ""));
      setOrigin(String(next.origin ?? ""));
    },
    [setQ, setOrigin],
  );

  const columns = useMemo<DataViewColumn<ToolRow>[]>(
    () => [
      {
        key: "name",
        header: "Tool",
        sortable: true,
        mono: true,
        copyable: true,
        sortValue: (t) => t.name,
      },
      {
        key: "origin",
        header: "Origin",
        shrink: true,
        sortable: true,
        sortValue: (t) => t.source,
        render: (t) => {
          const { kind, detail } = originOf(t.source);
          return (
            <span className="row gap-1">
              <Tag appearance="outline">{kind}</Tag>
              {detail && <span className="t-caption mono">{detail}</span>}
            </span>
          );
        },
      },
      {
        key: "description",
        header: "What it does",
        sortable: true,
        sortValue: (t) => t.description ?? "",
        render: (t) => t.description || <span className="muted">no description</span>,
      },
    ],
    [],
  );

  return (
    <>
      <PageHeader
        title="Tools"
        description="What the models can actually call. A planner sees only the server names; the tool names below are discovered and activated during a turn."
        badges={<Tag appearance="outline">{plural(tools.length, "tool")} registered</Tag>}
      />

      <StatGroup columns={3}>
        <StatCard
          icon={<BuildGlyph />}
          label="Total"
          value={tools.length}
          sub="across every origin"
        />
        <StatCard
          icon={<Package2Glyph />}
          label="Built in"
          value={builtins}
          sub="shipped by the engine itself"
        />
        <StatCard
          icon={<CableGlyph />}
          label="From MCP servers"
          value={mcp}
          sub={`${origins.filter(([s]) => s.startsWith("mcp")).length} server(s)`}
        />
      </StatGroup>

      <DataView<ToolRow>
        framed
        columns={columns}
        rows={rows}
        totalCount={tools.length}
        getRowKey={(t) => `${t.source}:${t.name}`}
        defaultSort={{ key: "name", direction: "asc" }}
        filters={filters}
        filterValues={values}
        onFilterValuesChange={onValuesChange}
        emptyMessage={
          <EmptyState
            size="compact"
            icon={<BuildGlyph />}
            title={tools.length ? "No tool matches these filters" : "No tools are registered"}
            description={
              tools.length
                ? "Clear them to see every tool a seat can reach."
                : "Builtins register at boot and MCP tools are discovered from the servers in mcp_servers, so an engine with no active configuration has neither."
            }
          />
        }
      />

      <Section title="How a model reaches these">
        <Callout variant="neutral">
          <span className="col" style={{ gap: 4 }}>
            <span>
              An executor prompt lists <strong>server names</strong>, not tool names — a role with
              50–150 MCP tools would push 15–25 KB of catalogue into every call.
            </span>
            <span className="t-caption">
              The model calls <InlineCode tone="inherit">list_mcp_server_tools(server)</InlineCode>{" "}
              to see what a server offers, then{" "}
              <InlineCode tone="inherit">activate_tool(name)</InlineCode> to promote one into the
              schemas it can actually invoke. It activates what it needs the moment it needs it —
              there is no separate planning pass to name a tool in advance.
            </span>
          </span>
        </Callout>
      </Section>
    </>
  );
}
