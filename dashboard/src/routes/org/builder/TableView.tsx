/**
 * The org builder's table: the draft as an indented table of rows, which is
 * the console's own org table.
 *
 * THE OTHER HALF OF A PAIR. The visualization shows the organization's SHAPE;
 * this shows what every seat and unit in it SAYS, on one line each, in the
 * order the chart draws them. The two are one drawing in two arrangements, so
 * a reader who learned the canvas finds the same marks, the same words and the
 * same actions here.
 *
 * IT IS A TREE, NOT A LIST. This screen briefly drew a flat list with the
 * hierarchy demoted to a path column and a sort over the top of it, and a
 * table of an organization is the one table a sort destroys: "Engineering /
 * Platform" on every row is a sentence a reader has to reassemble, where an
 * indent is the answer already drawn. So the rows are the tree, the indent is
 * where a node sits, and the wires down the gutter are what a reader follows
 * back to the unit a seat is in. The table has no sort, no pager and no column
 * settings for the same reason: each of them offers to take the rows out of
 * the one order they mean anything in.
 *
 * WHAT IT DOES NOT DRAW IS ANY OF THAT CHROME. The rows, the cells, the keys,
 * the tree's wires, the two-line name group, the strip of row controls and the
 * split add pill are the design system's `OrgTable`, so this screen's table is
 * the console's table. What is here is only what the engine knows: what a row
 * IS, what its handle and its manager are, what the last dry run placed on it,
 * and what each action MEANS to the draft.
 *
 * EVERY ACTION IS THE CHART'S OWN LIST (`nodeMenu`), in the row's menu at the
 * end of it, with Edit and Delete also drawn as the two controls the console
 * puts on a row and the Add pill offering the three kinds a unit can take.
 * Read-only disables them and never hides them, exactly as on the canvas.
 *
 * MOVING A ROW AMONG ITS SIBLINGS is Alt with an arrow, and the same two
 * entries in the row's menu. It has a consequence: the engine's primary
 * manager of a seat is the FIRST seat that lists it, so passing a sibling can
 * change who a seat reports to with nothing about reporting edited, which is
 * why `reorder` announces it when it happens.
 */

import { useCallback, useMemo, useRef, type KeyboardEvent } from "react";
import { plural } from "~/lib/format.ts";
import { useBuilder, useBuilderView, type BuilderApi } from "./BuilderContext.tsx";
import type { NodeView } from "./chartModel.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import { isDeletable, leadLabel, nodeMenu, type OpenScreen } from "./nodeActions.tsx";
import {
  LiveState,
  SeatTags,
  UnitTags,
  handleLabel,
  managerLabel,
  seatKindLabel,
} from "./nodeMarks.tsx";
import { nodeTone } from "./nodeTone.ts";
import { useReorder, type Reorder } from "./reorder.ts";
import { useOpenScreen, useStructure } from "./useCharts.ts";
import { CrewletIcon } from "@crewlethq/icons";
import {
  AccountTreeGlyph,
  ApartmentGlyph,
  ArrowDownwardGlyph,
  ArrowUpwardGlyph,
  CreateNewFolderGlyph,
  DeleteGlyph,
  EditGlyph,
  MoreVertGlyph,
  PersonGlyph,
  SmartToyGlyph,
} from "@crewlethq/icons/glyphs";
import {
  IconButton,
  Menu,
  OrgTable,
  OrgTableActions,
  OrgTableAdd,
  OrgTableName,
  Tag,
  type MenuEntry,
  type TreeGridColumn,
  type TreeGridContext,
  type TreeItemAction,
  type TreeViewHandle,
} from "@crewlethq/ui";

/**
 * What a row says, beside its name.
 *
 * THE OUTLINE'S OWN COLUMNS, which the design guide names: what kind of thing
 * this is, the handle the engine runs it under, who leads or manages it, and
 * what the last dry run placed on it. The console's table has none of them,
 * because a node there holds nothing else; a Crewlet seat does, and a table
 * that dropped them would be a chart with the cards taken away.
 */
const COLUMNS: readonly TreeGridColumn[] = [
  // THE NAME TAKES THE MOST, and by more than it looks: it is the only column
  // that also holds the row's mark, its indent and whatever a push puts in its
  // trailing slot, so a seat four levels down has a fraction of this track
  // left for the word a reader is scanning for.
  { key: "name", header: "Name", width: "minmax(0, 4fr)" },
  { key: "kind", header: "Kind or type", width: "minmax(0, 1.25fr)" },
  { key: "handle", header: "Handle", width: "minmax(0, 1.75fr)" },
  { key: "lead", header: "Lead or reports to", width: "minmax(0, 1.75fr)" },
  { key: "problems", header: "Problems", width: "minmax(0, 1fr)" },
  // Read but not seen: the strip at the end of a row is drawn where every
  // reader can see what it holds, and a heading over it would be a word
  // saying what the glyphs under it already say. THE TRACK IS FIXED, and the
  // design system's own measure of the strip it draws: every row is its own
  // grid, so an `auto` track would be as wide as that row's controls and the
  // header, which holds none, would name a different column to every row.
  {
    key: "actions",
    header: "Actions",
    headerHidden: true,
    width: "var(--crewlet-org-table-actions)",
  },
];

/**
 * Why a control on a row refuses, in the guarded and read-only postures and
 * while a conflict or a kept draft is waiting. It is a REASON rather than a
 * flag for the same rule the menus follow: read-only disables, it never hides,
 * so an operator learns what the builder does and why it will not do it now.
 */
const REFUSED = "This draft is read-only here.";

/** Which column is which, so a cell is never drawn by its number alone. */
const NAME = 1;
const KIND = 2;
const HANDLE = 3;
const LEAD = 4;
const PROBLEMS = 5;
const ACTIONS = 6;

/** "Company", the unit's own type, or which kind of seat. */
function kindLabel(view: NodeView): string {
  if (view.type === "company") return "Company";
  if (view.type === "unit") return view.unitType || "Unit";
  return seatKindLabel(view);
}

/** A node's mark: the same one the chart draws for it, from the same list. */
function nodeIcon(view: NodeView) {
  if (view.type === "company") return <ApartmentGlyph />;
  if (view.type === "unit") return <AccountTreeGlyph />;
  return view.kind === "human" ? <PersonGlyph size="sm" /> : <CrewletIcon />;
}

/**
 * Move up and Move down, as the row's menu offers them.
 *
 * ALWAYS MEANINGFUL HERE, because the rows are always in the chart's own
 * order: there is no sort to take "the row above" away from the sibling the
 * move would pass. A node that cannot move at all (the company, and a root
 * seat a unit reference placed, which lives in the company's list) is offered
 * neither rather than two entries that would write somewhere else.
 */
function moveEntries(reorder: Reorder, key: NodeKey): MenuEntry[] {
  if (!reorder.movable(key)) return [];
  return [
    { kind: "separator", key: "sep-move" },
    {
      key: "move-up",
      label: "Move up",
      icon: <ArrowUpwardGlyph />,
      onSelect: () => reorder.move(key, -1),
    },
    {
      key: "move-down",
      label: "Move down",
      icon: <ArrowDownwardGlyph />,
      onSelect: () => reorder.move(key, 1),
    },
  ];
}

export function TableView() {
  const api = useBuilder();
  const open = useOpenScreen();
  const structure = useStructure(api.state);
  const reorder = useReorder(api, structure);

  /*
   * THE VIEW ON SCREEN IS WHAT FOCUSES A NODE. Which node to focus after an
   * operation, an undo or a redo is the Builder's decision, and performing it
   * belongs to whichever view is mounted: on the canvas that is a pan, here it
   * is a row taking focus. Registered exactly as the canvas registers its own,
   * so an add made from this table leaves focus on the row it added.
   */
  const grid = useRef<TreeViewHandle | null>(null);
  const handle = useMemo(
    () => ({
      focusNode: (key: NodeKey) => grid.current?.focusNode(key),
      expandAll: () => grid.current?.expandAll(),
      collapseAll: () => grid.current?.collapseAll(),
    }),
    [],
  );
  useBuilderView(handle);

  const cell = useCallback(
    (id: string, column: number, context: TreeGridContext) => {
      const key = id as NodeKey;
      const view = structure.nodes.get(key);
      if (!view) return null;
      switch (column) {
        case NAME:
          return (
            /* `btable-name` is what this screen's own stylesheet sizes, and
               what a suite finds a row by: the NAME cell rather than whichever
               cell happens to mention a name. */
            <OrgTableName
              className="btable-name"
              icon={nodeIcon(view)}
              iconRing={view.type === "seat" && view.kind === "human" ? "dashed" : "none"}
              name={<span className="btable-label">{view.name || "Unnamed company"}</span>}
              caption={kindLabel(view)}
              captionMarks={
                <>
                  {view.type === "unit" && <UnitTags view={view} />}
                  {view.type === "seat" && <SeatTags view={view} />}
                </>
              }
              // The live state of a saved agent seat, in the slot that keeps
              // its room: a push twice a tool-loop round changes a word here
              // and never the row.
              trailing={view.type === "seat" ? <LiveState api={api} view={view} /> : undefined}
              tone={nodeTone(view)}
            />
          );
        case KIND:
          return kindLabel(view);
        case HANDLE:
          if (view.type !== "seat") return <span className="muted">Not a seat</span>;
          return view.handle ? (
            handleLabel(view.handle)
          ) : (
            <span className="muted">{handleLabel(undefined)}</span>
          );
        case LEAD:
          if (view.type === "unit") return leadLabel(view);
          if (view.type !== "seat") return <span className="muted">Not a seat or unit</span>;
          return view.manager === undefined || view.manager === null ? (
            <span className="muted">{managerLabel(view.manager)}</span>
          ) : (
            managerLabel(view.manager)
          );
        case PROBLEMS: {
          const count = api.problemsFor(key).length;
          return count > 0 ? (
            <Tag variant="danger">{plural(count, "problem")}</Tag>
          ) : (
            <span className="muted">None</span>
          );
        }
        default:
          return <RowControls api={api} grid={context} open={open} reorder={reorder} view={view} />;
      }
    },
    [api, open, reorder, structure],
  );

  /*
   * ENTER EDITS AND DELETE REMOVES, while the row itself holds focus. The
   * same two the canvas gives a card, from the same list, so what an operator
   * learned there holds here.
   */
  const onRowKey = useCallback(
    (id: string, action: Exclude<TreeItemAction, "menu">) => {
      const node = structure.nodes.get(id as NodeKey);
      if (!node) return false;
      if (action === "activate") {
        api.openEditor(node.key);
        return true;
      }
      if (api.readOnly || !isDeletable(node)) return false;
      api.openDelete(node.key);
      return true;
    },
    [api, structure],
  );

  /* Alt with an arrow moves a row among the siblings it is drawn beside. */
  const onRowKeyDown = useCallback(
    (id: string, event: KeyboardEvent<HTMLElement>) => {
      if (!event.altKey || (event.key !== "ArrowUp" && event.key !== "ArrowDown")) return false;
      reorder.move(id as NodeKey, event.key === "ArrowUp" ? -1 : 1);
      return true;
    },
    [reorder],
  );

  return (
    <OrgTable
      label="Organization"
      columns={COLUMNS}
      rows={structure.tree}
      ref={grid}
      tone={(id) => nodeTone(structure.nodes.get(id as NodeKey))}
      renderCell={cell}
      cellHasControl={(_id, column) => column === ACTIONS}
      onRowKey={onRowKey}
      onRowKeyDown={onRowKeyDown}
      hasRowMenu={() => true}
      // SELECTION IS THE BUILDER'S, so the row a link names is marked here and
      // the toolbar acts on whatever the reader last touched, exactly as it
      // does on the chart.
      selectedId={api.selection.key}
      onSelect={(id) => api.selection.select(id as NodeKey)}
      readOnly={api.readOnly}
    />
  );
}

/**
 * The controls at the end of a row: what the console puts there, plus the rest
 * of what the engine can do to a node.
 *
 * TWO DRAWINGS OF ONE LIST. Edit and Delete are drawn as the two controls the
 * console's table gives a row, because they are the two an operator reaches
 * for; every action, those two included, is also in the row's own menu, which
 * is `nodeMenu` itself rather than a second list. The menu is what the
 * ContextMenu key and Shift+F10 open, so the grid owns whether it is up.
 */
function RowControls({
  api,
  grid,
  open,
  reorder,
  view,
}: {
  api: BuilderApi;
  grid: TreeGridContext;
  open: OpenScreen;
  reorder: Reorder;
  view: NodeView;
}) {
  const at = view.key === COMPANY_KEY ? null : view.key;
  return (
    <OrgTableActions>
      {view.type !== "seat" && (
        <OrgTableAdd
          label={`Add to ${view.name || "the company"}`}
          onOpen={() => grid.opened(view.key, ACTIONS)}
          options={[
            {
              key: "unit",
              label: "Add unit",
              icon: <CreateNewFolderGlyph />,
              ...(api.readOnly ? { disabledReason: REFUSED } : {}),
              onSelect: () => api.openAdd(at, "unit"),
            },
            {
              key: "agent",
              label: "Add agent seat",
              icon: <SmartToyGlyph />,
              ...(api.readOnly ? { disabledReason: REFUSED } : {}),
              onSelect: () => api.openAdd(at, "agent"),
            },
            {
              key: "human",
              label: "Add human seat",
              icon: <PersonGlyph />,
              ...(api.readOnly ? { disabledReason: REFUSED } : {}),
              onSelect: () => api.openAdd(at, "human"),
            },
          ]}
        />
      )}
      <IconButton
        size="sm"
        label={`Edit ${view.name || "the company"}`}
        icon={<EditGlyph />}
        onClick={() => api.openEditor(view.key)}
      />
      {isDeletable(view) && (
        <IconButton
          size="sm"
          variant="ghost-danger"
          label={`Delete ${view.name}`}
          icon={<DeleteGlyph />}
          {...(api.readOnly ? { disabledReason: REFUSED } : {})}
          onClick={() => api.openDelete(view.key)}
        />
      )}
      <Menu
        label="Row actions"
        icon={<MoreVertGlyph />}
        align="end"
        open={grid.menuOpen(view.key)}
        onOpenChange={(up) => grid.setMenuOpen(view.key, up)}
        items={[...nodeMenu(api, view, open), ...moveEntries(reorder, view.key)]}
      />
    </OrgTableActions>
  );
}
