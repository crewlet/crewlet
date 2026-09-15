/**
 * The org builder's table: every node of the draft as a row, in the design
 * system's own list view.
 *
 * THIS REPLACED AN OUTLINE, and the difference is what a reader can ask of it.
 * The outline was a treegrid: the same shape as the chart, redrawn as rows,
 * with the hierarchy as an indent you walked with the arrow keys. It could say
 * only one thing at a time, in one order, and finding "every human seat with no
 * manager" meant opening every unit and reading. A table is the other half of
 * the pair: the chart shows the SHAPE, and the table answers QUESTIONS about
 * it. So the two views are the visualization and the table, and this one sorts,
 * filters, pages, and lets a reader choose and reorder its columns.
 *
 * WHAT IT DOES INSTEAD OF WHAT THE OUTLINE DID.
 *
 * - REACHING EVERY SEAT AND UNIT: the rows are flat, so every node is one row
 *   and nothing has to be opened to reach it. The search box narrows by name,
 *   handle and unit, and the Kind axis narrows to units, agent seats or human
 *   seats. The outline reached a node by opening its ancestors; this reaches
 *   it by typing three letters of its name.
 * - SEEING THE HIERARCHY: the "In" column is the node's path, its ancestors
 *   joined, and the table OPENS SORTED BY IT, so the default order is the tree
 *   read top to bottom with each unit's rows together, exactly the order the
 *   outline drew. Sorting by any other column is the reader leaving that order
 *   deliberately, and the path is still on every row to say where a node came
 *   from. What is lost is the INDENT, and a path a reader can sort, filter and
 *   read on one line is a better answer than an indent at five levels.
 * - ACTING ON A ROW: the same actions the chart's cards offer, from the same
 *   one list (`nodeMenu`), in the row's own menu at the end of it. Add, Edit,
 *   Move to, Change kind, Open seat and Delete are all there.
 * - MOVING A ROW AMONG ITS SIBLINGS, which the outline did with Alt and an
 *   arrow: it is "Move up" and "Move down" in the row's menu, and it is
 *   OFFERED ONLY IN THE TABLE'S OWN ORDER. A seat's place among its siblings
 *   decides its primary manager (the engine's is the first seat that lists
 *   it), so the move has a consequence; sorted by name, "up" would name a row
 *   that is not the one above, so the entries say why they are unavailable
 *   rather than moving something nobody can see move.
 *
 * WHAT IT DOES NOT DO is draw its own chrome. The toolbar, the sortable
 * headers, the row shapes, the settings frame and the pager are the design
 * system's `DataView`, so this screen's table is the same table as every other
 * table in the dashboard. How many nodes the draft holds is the lens header's
 * to say, not a line under the rows.
 */

import { useCallback, useMemo } from "react";
import { useParam } from "~/app/router.tsx";
import { useTableChoices } from "~/components/common.tsx";
import { plural } from "~/lib/format.ts";
import { useBuilder, type BuilderApi } from "./BuilderContext.tsx";
import type { NodeView, Structure } from "./chartModel.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import { leadLabel, nodeMenu, type OpenScreen } from "./nodeActions.tsx";
import {
  LiveState,
  SeatMarks,
  UnitMarks,
  handleLabel,
  managerLabel,
  seatKindLabel,
} from "./nodeMarks.tsx";
import { useReorder, type Reorder } from "./reorder.ts";
import { useOpenScreen, useStructure } from "./useCharts.ts";
import {
  ApartmentGlyph,
  ArrowDownwardGlyph,
  ArrowUpwardGlyph,
  FolderGlyph,
} from "@crewlethq/icons/glyphs";
import {
  Avatar,
  DataView,
  EmptyState,
  Tag,
  type DataTableRowAction,
  type DataViewColumn,
  type FilterDef,
  type FilterValues,
  type MenuEntry,
} from "@crewlethq/ui";

/** One node of the draft, as a row. */
interface Row {
  readonly key: NodeKey;
  readonly view: NodeView;
  /** Every ancestor's name, outermost first. The company's own name is not one. */
  readonly path: readonly string[];
  /**
   * Where the row sits in the chart's own order, so the table can open in it.
   *
   * A NUMBER RATHER THAN THE PATH ALONE, because a path sorts alphabetically
   * and the chart's order is the DOCUMENT's: the units in the order the
   * company writes them, each unit's seats in the order it writes those. The
   * table opens on this and says "In" as its header, so what a reader sees
   * first is the tree as the chart draws it.
   */
  readonly order: number;
}

/** The kinds the Kind axis offers, which are the kinds a row can be. */
const KINDS: readonly { value: string; label: string }[] = [
  { value: "", label: "Any kind" },
  { value: "unit", label: "Units" },
  { value: "agent", label: "Agent seats" },
  { value: "human", label: "Human seats" },
];

/** What a row is narrowed by: a unit, or a seat of one kind. */
function kindOf(view: NodeView): string {
  return view.type === "seat" ? view.kind : view.type;
}

/** "Company", the unit's own type, or which kind of seat. */
function kindLabel(view: NodeView): string {
  if (view.type === "company") return "Company";
  if (view.type === "unit") return view.unitType || "Unit";
  return seatKindLabel(view);
}

/**
 * Every node as a row, in the chart's own order: the company, then each root
 * seat and unit in the order the document writes them, each unit's own rows
 * directly under it.
 *
 * THE SAME WALK THE CHART DRAWS, over the same `Structure`, so a row is never
 * a node the chart does not have and the order never disagrees with it.
 */
function rowsOf(structure: Structure): Row[] {
  const out: Row[] = [];
  const walk = (key: NodeKey, path: readonly string[]) => {
    const view = structure.nodes.get(key);
    if (!view) return;
    out.push({ key, view, path, order: out.length });
    if (view.type === "seat") return;
    // Seats before units at every level, which is the order the chart draws
    // them in and therefore the order "In" sorts the rows into.
    const under = [...view.seats, ...view.units];
    // The company is the root of the path rather than a step in it: every row
    // is in it, so naming it on every row says nothing.
    const deeper = view.type === "company" ? path : [...path, view.name];
    for (const child of under) walk(child, deeper);
  };
  walk(COMPANY_KEY, []);
  return out;
}

/**
 * A node's actions as the table's row menu takes them.
 *
 * ONE LIST OF WHAT A NODE CAN DO, which is `nodeMenu`, adapted to the row
 * menu's own shape rather than written out a second time. A separator becomes
 * the divider on the entry after it, which is the same fact said the way each
 * component says it.
 */
function rowActionsOf(entries: readonly MenuEntry[]): DataTableRowAction[] {
  const out: DataTableRowAction[] = [];
  let divide = false;
  for (const entry of entries) {
    if (entry.kind === "separator") {
      divide = out.length > 0;
      continue;
    }
    out.push({
      label: entry.label,
      onClick: entry.onSelect,
      ...(entry.icon === undefined ? {} : { icon: entry.icon }),
      ...(entry.danger === undefined ? {} : { danger: entry.danger }),
      ...(entry.disabled === undefined ? {} : { disabled: entry.disabled }),
      ...(divide ? { divider: "before" as const } : {}),
    });
    divide = false;
  }
  return out;
}

/**
 * Move up and Move down, and why they are unavailable when they are.
 *
 * THE MOVE IS ONLY MEANINGFUL IN THE TABLE'S OWN ORDER. "Up" means "past the
 * row above", and sorted by name the row above is not the sibling the move
 * would pass. Rather than move something a reader cannot see move, the entries
 * stay in the menu and say what would make them work: a menu whose entries come
 * and go is a menu nobody learns.
 */
function moveActions(reorder: Reorder, row: Row, ordered: boolean): DataTableRowAction[] {
  if (!reorder.movable(row.key)) return [];
  const why = ordered
    ? undefined
    : "Sort the table by In to move a row among the rows it is drawn beside.";
  return [
    {
      label: "Move up",
      icon: <ArrowUpwardGlyph />,
      disabled: !ordered,
      divider: "before",
      ...(why === undefined ? {} : { description: why }),
      onClick: () => reorder.move(row.key, -1),
    },
    {
      label: "Move down",
      icon: <ArrowDownwardGlyph />,
      disabled: !ordered,
      ...(why === undefined ? {} : { description: why }),
      onClick: () => reorder.move(row.key, 1),
    },
  ];
}

export function TableView() {
  const api = useBuilder();
  const open = useOpenScreen();
  const structure = useStructure(api.state);
  const reorder = useReorder(api, structure);
  const rows = useMemo(() => rowsOf(structure), [structure]);

  /*
   * THE TABLE OWNS ITS NARROWING, so both axes are URL parameters and a
   * narrowed table is a link somebody sends. They are named after the view
   * rather than bare, because the builder shares its URL with the chart and
   * the org screen's other lenses.
   */
  const [q, setQ] = useParam("table.q", "");
  const [kind, setKind] = useParam("table.kind", "");

  const shown = useMemo(() => {
    // A HANDLE IS WRITTEN WITH ITS `@` where a reader sees it, on every card of
    // the chart and in this table's own Handle column, so it is typed with one
    // too. Stored without, a search for "@sre" otherwise matched nothing.
    const needle = q.trim().toLowerCase().replace(/^@/, "");
    return rows
      .filter((row) => !kind || kindOf(row.view) === kind)
      .filter((row) => {
        if (!needle) return true;
        const handle = row.view.type === "seat" ? (row.view.handle ?? "") : "";
        return (
          row.view.name.toLowerCase().includes(needle) ||
          handle.toLowerCase().includes(needle) ||
          row.path.join(" ").toLowerCase().includes(needle)
        );
      });
  }, [rows, q, kind]);

  const filters = useMemo<FilterDef<Row>[]>(
    () => [
      {
        name: "q",
        label: "Search the organization",
        role: "search",
        placeholder: "Name, handle or unit",
      },
      { name: "kind", label: "Kind", kind: "select", options: [...KINDS] },
    ],
    [],
  );

  const onValuesChange = useCallback(
    (next: FilterValues) => {
      setQ(String(next.q ?? ""));
      setKind(String(next.kind ?? ""));
    },
    [setQ, setKind],
  );

  const columns = useMemo<DataViewColumn<Row>[]>(
    () => [
      {
        key: "name",
        header: "Name",
        sortable: true,
        // WHO. A kind, a path and a lead belonging to nobody named is a table
        // of nobody, so this column is never one a reader can hide.
        hideable: false,
        sortValue: (row) => row.view.name,
        render: (row) => <NameCell view={row.view} />,
      },
      {
        key: "in",
        header: "In",
        sortable: true,
        // The chart's own order, which is the document's rather than the
        // alphabet's: the sort value is the row's place in the walk.
        sortValue: (row) => row.order,
        // THE PATH WRAPS RATHER THAN TRUNCATES. It is the whole of what this
        // column says, and a path cut at "Product / Developer…" names no unit
        // a reader can act on; the table is in its wrapping mode, so a long
        // one takes a second line in a column a reader can also widen.
        render: (row) =>
          row.path.length > 0 ? row.path.join(" / ") : <span className="muted">The company</span>,
      },
      {
        key: "kind",
        header: "Kind or type",
        sortable: true,
        shrink: true,
        sortValue: (row) => kindLabel(row.view),
        render: (row) => kindLabel(row.view),
      },
      {
        key: "handle",
        header: "Handle",
        sortable: true,
        shrink: true,
        mono: true,
        copyable: true,
        sortValue: (row) => (row.view.type === "seat" ? row.view.handle : null),
        render: (row) =>
          row.view.type === "seat" ? (
            row.view.handle ? (
              handleLabel(row.view.handle)
            ) : (
              <span className="muted">{handleLabel(undefined)}</span>
            )
          ) : (
            <span className="muted">Not a seat</span>
          ),
      },
      {
        key: "lead",
        header: "Lead or reports to",
        sortable: true,
        sortValue: (row) =>
          row.view.type === "unit"
            ? leadLabel(row.view)
            : row.view.type === "seat"
              ? (row.view.manager ?? "")
              : "",
        render: (row) => {
          if (row.view.type === "unit") return leadLabel(row.view);
          if (row.view.type !== "seat") return <span className="muted">Not a seat or unit</span>;
          const manager = row.view.manager;
          return manager === undefined || manager === null ? (
            <span className="muted">{managerLabel(manager)}</span>
          ) : (
            managerLabel(manager)
          );
        },
      },
      {
        key: "state",
        header: "State",
        shrink: true,
        render: (row) =>
          row.view.type === "seat" ? <LiveState api={api} view={row.view} /> : null,
      },
      {
        key: "problems",
        header: "Problems",
        align: "right",
        shrink: true,
        sortable: true,
        firstDirection: "desc",
        sortValue: (row) => api.problemsFor(row.key).length,
        render: (row) => {
          const count = api.problemsFor(row.key).length;
          return count > 0 ? (
            <Tag variant="danger">{plural(count, "problem")}</Tag>
          ) : (
            <span className="muted">None</span>
          );
        },
      },
    ],
    [api],
  );

  const choices = useTableChoices({
    screen: "org",
    table: "builder",
    columns,
    // THE CHART'S OWN ORDER is what the table opens in, so a reader who has
    // not asked for anything sees the tree as the chart draws it.
    defaultSort: { key: "in", direction: "asc" },
    filterKey: `${q}|${kind}`,
  });

  // Whether the rows are in the chart's own order, which is what makes a move
  // among siblings mean anything: see `moveActions`.
  const ordered = choices.sort?.key === "in" && choices.sort.direction === "asc";

  return (
    <DataView<Row>
      framed
      {...choices}
      columns={columns}
      rows={shown}
      getRowKey={(row) => row.key}
      filters={filters}
      filterValues={{ q, kind }}
      onFilterValuesChange={onValuesChange}
      // SELECTION IS THE BUILDER'S, so the row a link names is marked here and
      // the toolbar acts on whatever the reader last touched, exactly as it
      // does on the chart.
      isSelected={(row) => row.key === api.selection.key}
      onRowClick={(row) => api.selection.select(row.key)}
      rowActions={(row) => [
        ...rowActionsOf(nodeMenu(api, row.view, open)),
        ...moveActions(reorder, row, ordered),
      ]}
      emptyMessage={
        <EmptyState
          size="compact"
          title={
            rows.length > 1
              ? "No seat or unit matches these filters"
              : "Nothing in the organization yet"
          }
          description={
            rows.length > 1
              ? "Clear them to see every seat and unit this draft declares."
              : "Add a unit or a seat from the toolbar, or from a card on the visualization."
          }
        />
      }
    />
  );
}

/** A node's icon and name, with the marks the chart's cards carry. */
function NameCell({ view }: { view: NodeView }) {
  return (
    <span className="btable-name">
      {view.type === "company" ? (
        <ApartmentGlyph size="sm" />
      ) : view.type === "unit" ? (
        <FolderGlyph size="sm" />
      ) : (
        <Avatar
          name={view.name}
          size="xs"
          variant={view.kind === "human" ? "dashed" : "solid"}
          decorative
        />
      )}
      {/* Wrapping, for the reason the path wraps: a seat cut at "Agent Dev…"
          is a seat nobody can tell from another. */}
      <span className="btable-label">{view.name || "Unnamed company"}</span>
      {view.type === "unit" && <UnitMarks view={view} />}
      {view.type === "seat" && <SeatMarks view={view} />}
    </span>
  );
}
