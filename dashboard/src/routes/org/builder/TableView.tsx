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
 * EVERY ACTION IS THE CHART'S OWN LIST (`nodeMenu`), AND EACH HAS ONE OWNER.
 * The console's row draws an add pill, a pencil and a trash, so those three
 * are the row's own controls here; the menu at the end of the row carries what
 * has no button of its own (Open seat, Edit reports, Change kind, Move to, and
 * the two moves among the siblings), and a row whose menu would be empty draws
 * none. They were drawn BOTH ways once: on the company's row every entry of
 * the menu was already a button 32 pixels to its left. Read-only disables all
 * of them and never hides them, exactly as on the canvas.
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
import { addSections, isDeletable, leadLabel, rowMenu, type OpenScreen } from "./nodeActions.tsx";
import {
  LiveState,
  NodeGlyph,
  SeatMarks,
  UnitMarks,
  handleLabel,
  managerLabel,
  nodeGlyphKind,
  seatKindLabel,
  unitTypeLabel,
} from "./nodeMarks.tsx";
import { nodeTone } from "./nodeTone.ts";
import { useReorder, type Reorder } from "./reorder.ts";
import { useOpenScreen, useStructure } from "./useCharts.ts";
import { DeleteGlyph, EditGlyph, MoreVertGlyph } from "@crewlethq/icons/glyphs";
import {
  EmptyValue,
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
  // left for the word a reader is scanning for. It takes the Kind column's
  // share as well: what a row IS is the word under its name, which is where
  // the console writes it and where the chart's own cards write it, and a
  // column repeating that word cost 145px of a 1269px table to say it twice.
  { key: "name", header: "Name", width: "minmax(0, 5.25fr)" },
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
 *
 * The add pill's own kinds carry the refusal WITHOUT this sentence, because
 * the design system's pill takes a flag: they are drawn, announced as
 * unavailable and refuse the press, and the reason is the one part that does
 * not travel. It belongs on `AddPillSection`, where both surfaces would get
 * it, rather than in a second pill drawn here.
 */
const REFUSED = "This draft is read-only here.";

/** Which column is which, so a cell is never drawn by its number alone. */
const NAME = 1;
const HANDLE = 2;
const LEAD = 3;
const PROBLEMS = 4;
const ACTIONS = 5;

/**
 * "Company", the unit's own type, or which kind of seat: the word a row writes
 * under its name. Each of the three words comes from `nodeMarks`, which is
 * where the chart's cards take the same words from.
 */
function kindLabel(view: NodeView): string {
  if (view.type === "company") return "Company";
  if (view.type === "seat") return seatKindLabel(view);
  return unitTypeLabel(view);
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
              icon={NodeGlyph({ kind: nodeGlyphKind(view) })}
              iconRing={view.type === "seat" && view.kind === "human" ? "dashed" : "none"}
              name={<span className="btable-label">{view.name || "Unnamed company"}</span>}
              caption={kindLabel(view)}
              // THE SAME GLYPHS THE CHART DRAWS, for the same reason: a row is
              // a rank tall. As tags the marks were the row's own height (one
              // measured tag took the caption from 13.2px to 20 and the row
              // from 36 to 36.6), so a seat's height depended on its wiring
              // and two of them would have burst the name column. Each glyph
              // carries its sentence as its own name and its tooltip.
              captionMarks={
                <>
                  {view.type === "unit" && <UnitMarks view={view} />}
                  {view.type === "seat" && <SeatMarks view={view} />}
                </>
              }
              // The live state of a saved agent seat, in the slot that keeps
              // its room: a push twice a tool-loop round changes a word here
              // and never the row.
              trailing={view.type === "seat" ? <LiveState api={api} view={view} /> : undefined}
              tone={nodeTone(view)}
            />
          );
        /*
         * A CELL THAT CANNOT HOLD A VALUE SAYS SO ONCE, in the one mark the
         * design system draws for it: the company's row read "Not a seat" in
         * one column and "Not a seat or unit" in the next, two phrasings of
         * one idea written out as sentences on the first row a reader meets.
         * The dash is drawn and the meaning is spoken, and it stays distinct
         * from the different fact that no check has answered yet.
         */
        case HANDLE:
          if (view.type !== "seat") return <EmptyValue label="Not applicable" />;
          return view.handle ? (
            handleLabel(view.handle)
          ) : (
            <span className="muted">{handleLabel(undefined)}</span>
          );
        case LEAD:
          if (view.type === "unit") return leadLabel(view);
          if (view.type !== "seat") return <EmptyValue label="Not applicable" />;
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
      /*
       * THE PAIR LIVES IN THE PAGE TOOLBAR, on both views. The design system
       * draws its own Expand all and Collapse all over this table, and the
       * lens draws the same two in its toolbar: one action pair, two
       * implementations, each hidden on the view where the other was drawn,
       * so the control moved 800px when a reader changed view.
       */
      controls={false}
      columns={COLUMNS}
      rows={structure.tree}
      ref={grid}
      tone={(id) => nodeTone(structure.nodes.get(id as NodeKey))}
      renderCell={cell}
      cellHasControl={(_id, column) => column === ACTIONS}
      onRowKey={onRowKey}
      onRowKeyDown={onRowKeyDown}
      // A REAL PREDICATE, because the row draws the actions the console draws
      // and the menu carries the rest: the company's would be empty, and an
      // empty menu is a control that opens onto nothing for a pointer and a
      // ContextMenu key that answers with a blank surface.
      hasRowMenu={(id) => {
        const node = structure.nodes.get(id as NodeKey);
        return node !== undefined && rowMenu(api, node, open, reorder).length > 0;
      }}
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
 * The controls at the end of a row: what the console puts there, and the rest
 * of what the engine can do to a node behind one menu.
 *
 * ONE OWNER PER ACTION. The console's row is an add pill, a pencil and a
 * trash; the engine can do five more things to a node, so those five are the
 * row's menu. Every one of the eight was offered TWICE before: the strip drew
 * Edit and Delete and the menu beside it opened with Edit and ended with
 * Delete, and on the company's row the whole menu was the three adds and Edit,
 * every one of them already a button 32 pixels to its left.
 *
 * TAKEN FROM `nodeMenu` RATHER THAN LISTED AGAIN, because a second list is how
 * the chart and the table come to offer different things: the entries the
 * strip draws are subtracted from the one list, so an action added there
 * arrives here with no edit at all.
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
  const name = view.name || "the company";
  const menu = rowMenu(api, view, open, reorder);
  return (
    <>
      {/* OUTSIDE THE STRIP, because it is not one of the quiet controls: the
          console draws the plus on every row that can take a child and keeps
          only the pencil and the trash behind the reveal. */}
      {view.type !== "seat" && (
        <OrgTableAdd
          label={`Add to ${name}`}
          onOpen={() => grid.opened(view.key, ACTIONS)}
          sections={addSections(api, view)}
        />
      )}
      <OrgTableActions>
        <IconButton
          size="sm"
          label={`Edit ${name}`}
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
        {menu.length > 0 && (
          <Menu
            /* NAMED AFTER THE NODE IT ACTS ON, exactly as the chart names the
               same control: "Row actions" on every row is several identically
               named controls, and none of them says which row it would act
               on. */
            label={`Actions for ${name}`}
            icon={<MoreVertGlyph />}
            align="end"
            open={grid.menuOpen(view.key)}
            onOpenChange={(up) => grid.setMenuOpen(view.key, up)}
            items={menu}
          />
        )}
      </OrgTableActions>
    </>
  );
}
