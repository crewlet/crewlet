/**
 * The built-in tools: the engine's own, as a catalogue a reader scans the way
 * they scan a server's Tools tab. One row a tool (its name, one plain line,
 * what it can do and who the engine lets call it), a search and the
 * capability chips over the rows, and the groups the engine files them in as
 * a rail beside them, so sixty tools read as eight short lists.
 *
 * READ ONLY, and nothing on it pretends otherwise: the engine offers each
 * agent the ones its gates admit (Agent access counts them per agent), runs
 * them itself and decides each call by who is calling, so there is no level
 * or grant to set here. A row opens the tool, its description and arguments,
 * in the dashboard's drawer.
 *
 * THE ENGINE FILES AND READS EVERY TOOL: its group and its words, who may call
 * it (`gate`) and what it can do (`risk`) are on each row.
 *
 * THE FILTERS ARE THE ADDRESS (`q`, `cap`, `group`), replaced rather than
 * pushed, so a tool opened in a new tab and Back from the drawer keep them.
 */

import { useMemo } from "react";
import {
  FilterChip,
  FilterChipGroup,
  Input,
  RailList,
  TableTwoLineCell,
  Tag,
  Text,
  VisuallyHidden,
  peekRowActivation,
  type DataGridBand,
  type DataGridColumn,
} from "@crewlethq/ui";
import { SearchGlyph } from "@crewlethq/icons/glyphs";
import type { BuiltinGroup, McpRisk } from "~/contract/mcp.ts";
import type { ToolRow } from "~/contract/tools.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { peekHref } from "~/app/frame/peekAddress.ts";
import { href, useNavigator, useParam, useRoute } from "~/app/router.tsx";
import { plural } from "~/lib/format.ts";
import { GATE, RISK } from "../words.ts";

type Cap = "all" | McpRisk;
const CAPS: readonly Cap[] = ["all", "read_only", "additive", "destructive"];
const CAP_ORDER: Record<McpRisk, number> = { read_only: 0, additive: 1, destructive: 2 };

/** One row: a tool and the group it is filed in. */
export interface CatalogueRow {
  tool: ToolRow;
  group: BuiltinGroup;
}

/** Every built-in, flat, each beside its group. */
export function builtinRows(groups: readonly BuiltinGroup[]): CatalogueRow[] {
  return groups.flatMap((group) => group.tools.map((tool) => ({ tool, group })));
}

/** What a tool can do, as the engine reads its hints, in the server Tools tab's words. */
export function CapabilityTag({ tool }: { tool: Pick<ToolRow, "risk"> }) {
  const words = RISK[tool.risk];
  return (
    <Tag size="xs" variant={words.tone} appearance="soft">
      {words.label}
    </Tag>
  );
}

export function BuiltinCatalogue({
  groups,
  onOpen,
}: {
  groups: readonly BuiltinGroup[];
  /** Opens one tool in the drawer. The grid marks the peeked row itself. */
  onOpen: (name: string) => void;
}) {
  const nav = useNavigator();
  const route = useRoute();
  const [q, setQ] = useParam("q", "");
  const [capRaw, setCap] = useParam("cap", "all");
  const [groupKey] = useParam("group", "");
  const cap: Cap = (CAPS as readonly string[]).includes(capRaw) ? (capRaw as Cap) : "all";
  const group = groups.find((g) => g.key === groupKey) ?? null;
  const all = useMemo(() => builtinRows(groups), [groups]);

  const inGroup = useMemo(
    () => (group ? all.filter((r) => r.group.key === group.key) : all),
    [all, group],
  );
  const shown = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return inGroup.filter(
      (r) =>
        (cap === "all" || r.tool.risk === cap) &&
        (!needle || `${r.tool.name} ${r.tool.description}`.toLowerCase().includes(needle)),
    );
  }, [inGroup, q, cap]);
  const capCount = (c: Cap) =>
    c === "all" ? inGroup.length : inGroup.filter((r) => r.tool.risk === c).length;

  // A GROUP IS A FILTER, so choosing one replaces the address like a chip
  // does; the rail's rows are still real links, for a new tab.
  const groupHref = (key: string) => {
    const query = Object.fromEntries(route.query);
    delete query.group;
    delete query.peek;
    return href(["settings", "mcp-tools"], key ? { ...query, group: key } : query);
  };

  const columns: DataGridColumn<CatalogueRow>[] = [
    {
      key: "tool",
      header: "Tool",
      floor: "14rem",
      phoneLead: true,
      sortValue: (r) => r.tool.name,
      cell: (r) => (
        <TableTwoLineCell
          value={r.tool.name}
          detail={r.tool.description}
          mono="value"
          titleWhenCut={false}
        />
      ),
    },
    {
      key: "capability",
      header: "Capability",
      // THE WIDEST CHIP'S TRACK ("Destructive"), so the column never cuts one.
      width: "7.5rem",
      sortValue: (r) => CAP_ORDER[r.tool.risk],
      cell: (r) => <CapabilityTag tool={r.tool} />,
    },
    {
      key: "who",
      header: "Who may call",
      // The longest gate ("Artifacts access") on one line; it gives way first
      // where the rows narrow (a peek open beside them, a phone).
      width: "9.5rem",
      drop: 1,
      sortValue: (r) => (r.tool.gate ? GATE[r.tool.gate].label : ""),
      cell: (r) => (
        <Text as="span" variant="body" tone="secondary" truncate titleWhenCut={false}>
          {r.tool.gate ? GATE[r.tool.gate].label : ""}
        </Text>
      ),
    },
  ];

  // EVERY GROUP AS A BAND while none is chosen, so the list keeps the
  // engine's filing; one group is its rows alone, under the rail's choice.
  const bands: DataGridBand<CatalogueRow>[] | undefined = group
    ? undefined
    : groups
        .map((g) => ({
          key: g.key,
          label: g.label,
          rows: shown.filter((r) => r.group.key === g.key),
        }))
        .filter((b) => b.rows.length > 0);

  return (
    <>
      <RailList
        label="Built-in tool groups"
        items={[
          { key: "", label: "All tools", count: all.length, href: groupHref("") },
          ...groups.map((g) => ({
            key: g.key,
            label: g.label,
            count: g.tools.length,
            href: groupHref(g.key),
            description: g.description,
          })),
        ]}
        value={group?.key ?? ""}
        onChange={(key) => nav.filter({ group: key || null, peek: null })}
        labels={{ counted: "tools" }}
        note="Read only. The engine runs these itself and decides each call by who is calling."
        toolbar={(picker) => (
          <>
            {/* The Tools tab's toolbar: the field, then the kit's filter
                chips at the field's own step. */}
            <span className="mcp-toolbar__search">
              <Input
                type="search"
                aria-label="Search built-in tools"
                placeholder="Search built-in tools"
                inputSize="sm"
                width="full"
                leading={<SearchGlyph size="sm" />}
                value={q}
                onChange={(e) => setQ(e.target.value)}
              />
            </span>
            {picker}
            <span className="mcp-toolbar__end">
              <FilterChipGroup
                label="Capability"
                hideLabel
                semantics="radio"
                value={cap}
                onValueChange={(v) => setCap((v ?? "all") as Cap)}
              >
                {(
                  [
                    ["all", "All"],
                    ["read_only", RISK.read_only.label],
                    ["additive", RISK.additive.label],
                    ["destructive", RISK.destructive.label],
                  ] as const
                ).map(([value, label]) => (
                  <FilterChip key={value} value={value} count={capCount(value)}>
                    {label}
                  </FilterChip>
                ))}
              </FilterChipGroup>
            </span>
          </>
        )}
      >
        <DataGrid<CatalogueRow>
          name="builtin"
          {...(bands ? { bands } : { rows: shown })}
          rowKey={(r) => r.tool.name}
          phoneRows="compact"
          rowHref={(r) => peekHref({ kind: "tool", id: r.tool.name })}
          onRowActivate={peekRowActivation<CatalogueRow>((r) => onOpen(r.tool.name))}
          columns={columns}
          empty={{
            title: "No built-in tool matches",
            hint: q.trim()
              ? `Nothing ${group ? `in ${group.label} ` : ""}says "${q.trim()}".`
              : undefined,
            icon: "search",
          }}
        />
      </RailList>
      <VisuallyHidden>
        <span role="status">
          {shown.length === all.length ? "" : `${plural(shown.length, "tool")} shown`}
        </span>
      </VisuallyHidden>
    </>
  );
}
