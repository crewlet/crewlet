/**
 * The org builder's outline: the structure of the draft as rows and columns,
 * the view the lens opens on at narrow widths and the one a keyboard or a
 * screen reader user works fastest in.
 *
 * A TREEGRID, NOT A TREE. Every row says its level, position and expansion
 * like a treeitem, and its cells are navigable like a grid's: Name, Kind or
 * type, Handle, Lead or reports to, Problems, and the row's actions. A row
 * holds focus by default and its keys act on the node (Enter edits, Delete or
 * Backspace deletes, the ContextMenu key or Shift+F10 opens its actions);
 * Right opens a closed row or steps into its cells, Left steps back out,
 * closes the row or climbs to its parent, Up and Down keep the column. A cell
 * holding a control (a unit's lead choice, the actions menu, an add button)
 * focuses the control itself, so what the cell does is one Enter away. The
 * grid's one tab stop is wherever focus is: a press that opens a cell's menu
 * moves it to that cell, and the row's chevron, which is pointer-only and
 * hidden from assistive technology, takes no focus at all.
 *
 * AN INLINE ADD ROW closes every unit's rows and the company's: Add agent
 * seat, Add human seat, Add unit, each asking the Builder's Add dialog for
 * that kind under that parent. It is a row of the grid like any other, so it
 * counts in its siblings' position and set size, and it is absent while the
 * draft is read-only.
 *
 * ALT WITH UP OR DOWN MOVES A ROW among its siblings of the same kind (a seat
 * among its unit's seats, a unit among its parent's units). A reorder can
 * change who a seat reports to, because the engine's primary manager is the
 * first seat that lists it; that is the engine's to derive, so when the check
 * of the reordered draft answers, the reporting lines it reports are compared
 * with the ones before the reorder (`model/changes.ts`) and any change is
 * announced. A root seat drawn in a unit by its unit reference lives in the
 * company's list, not the unit's, so it is not reordered here and the
 * operator is told how to place it. A row moves past the row drawn beside it,
 * so a root seat steps over the root seats drawn inside units rather than
 * making a move nobody can see.
 */

import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent,
  type MouseEvent,
  type ReactNode,
} from "react";
import type { Derived } from "~/protocol/index.ts";
import { Icon } from "~/ui/Icon.tsx";
import { Menu } from "~/ui/Menu.tsx";
import { Avatar, Button } from "~/ui/primitives.tsx";
import { VIEW_CHANGE_EVENT } from "~/ui/viewport.ts";
import {
  isExpandable,
  level,
  nextVisible,
  posInSet,
  previousVisible,
  setSize,
} from "~/ui/treeModel.ts";
import type { TreeInput } from "~/ui/treeModel.ts";
import { useBuilder, useBuilderView, type AddKind, type BuilderApi } from "./BuilderContext.tsx";
import type { NodeView, SeatView, Structure, UnitView } from "./chartModel.ts";
import { deriveChanges } from "./model/changes.ts";
import { locate, type Draft } from "./model/draft.ts";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import {
  isDeletable,
  leadLabel,
  leadMenu,
  nodeKeyAction,
  nodeMenu,
  type OpenScreen,
} from "./nodeActions.tsx";
import {
  LiveState,
  ProblemCount,
  SeatMarks,
  UnitMarks,
  handleLabel,
  managerLabel,
  seatKindLabel,
} from "./nodeMarks.tsx";
import { treeStep, useOpenScreen, useStructure, useTreeState } from "./useCharts.ts";

/** The columns, in order. Their positions are the cells' `aria-colindex`. */
const COLUMNS = ["Name", "Kind or type", "Handle", "Lead or reports to", "Problems", "Actions"];

/** The add row's buttons, in order: one cell each. */
const ADD_BUTTONS: readonly { kind: AddKind; label: string }[] = [
  { kind: "agent", label: "Add agent seat" },
  { kind: "human", label: "Add human seat" },
  { kind: "unit", label: "Add unit" },
];

const ADD_ROW = "add:";
const addRowId = (parent: NodeKey) => `${ADD_ROW}${parent}`;
/** The parent an add row adds to, or `null` for a node's row. */
const addParentOf = (id: string): NodeKey | null =>
  id.startsWith(ADD_ROW) ? id.slice(ADD_ROW.length) : null;

/** The structure's forest with an add row closing the company's children and each unit's. */
function withAddRows(structure: Structure): TreeInput[] {
  const walk = (input: TreeInput): TreeInput => {
    const children = (input.children ?? []).map(walk);
    const view = structure.nodes.get(input.id);
    if (view?.type === "company" || view?.type === "unit") {
      // An empty label: type-ahead finds rows by the name a person reads,
      // and an add row names nothing.
      children.push({ id: addRowId(input.id), label: "" });
    }
    return { ...input, children };
  };
  return structure.tree.map(walk);
}

/** A reorder whose effect on the reporting lines is waiting for the check of its draft. */
interface PendingReorder {
  /** The generation the reorder was recorded on. */
  readonly from: number;
  /** The generation it produced, once it was recorded. */
  produced?: number;
  readonly target: NodeKey;
  readonly before: { readonly draft: Draft; readonly derived: Derived };
}

export function OutlineView() {
  const api = useBuilder();
  const open = useOpenScreen();
  const structure = useStructure(api.state);
  const forest = useMemo(
    () => (api.readOnly ? structure.tree : withAddRows(structure)),
    [structure, api.readOnly],
  );
  const tree = useTreeState(forest, api.selection.key);
  const { model, expanded, rows, active, setActive, toggle } = tree;

  /** The column of the active row that holds focus; `null` when the row itself does. */
  const [column, setColumn] = useState<number | null>(null);
  const [menuFor, setMenuFor] = useState<string | null>(null);

  // ---- focus -----------------------------------------------------------------
  const rowEls = useRef(new Map<string, HTMLElement>());
  const refs = useRef(new Map<string, (el: HTMLElement | null) => void>());
  const rowRef = (id: string) => {
    let callback = refs.current.get(id);
    if (!callback) {
      callback = (el) => {
        if (el) rowEls.current.set(id, el);
        else rowEls.current.delete(id);
      };
      refs.current.set(id, callback);
    }
    return callback;
  };

  // A FOCUS REQUEST IS TAKEN BY THE RENDER IT CAUSES, AND BY NO OTHER. Focus
  // moves once the row it names is rendered (a row inside a unit just opened,
  // a node an operation just added, a row a reorder just moved, which a
  // browser blurs as it moves it), so the request is state: each one is a
  // new value, so even a request for the row already active causes a render,
  // and the effect acts on that value once. A request held anywhere else
  // would move no focus when nothing else changed (an undo from the toolbar
  // asking for its node), then pull focus back to that row on whatever render
  // came next, a live push twice per tool-loop round, wherever the operator
  // had gone since.
  const [request, setRequest] = useState<{
    readonly id: string;
    readonly column: number | null;
  } | null>(null);
  const focusAt = useCallback(
    (id: string, col: number | null) => {
      if (addParentOf(id) === null) api.selection.select(id);
      setActive(id);
      setColumn(col);
      setRequest({ id, column: col });
    },
    [api.selection, setActive],
  );
  useLayoutEffect(() => {
    const row = request ? rowEls.current.get(request.id) : undefined;
    if (!request || !row) return;
    if (request.column === null) {
      row.focus();
      return;
    }
    const cell = row.querySelector<HTMLElement>(`[aria-colindex="${request.column}"]`);
    const widget = cell?.querySelector<HTMLElement>("[data-cell-widget] button");
    (widget ?? cell ?? row).focus();
  }, [request]);

  const focusRef = useRef<(key: NodeKey) => void>(() => {});
  focusRef.current = (key: NodeKey) => {
    if (!model.parent.has(key)) return;
    tree.open(key);
    focusAt(key, null);
  };
  const { expandAll, collapseAll } = tree;
  const handle = useMemo(
    () => ({ focusNode: (key: NodeKey) => focusRef.current(key), expandAll, collapseAll }),
    [expandAll, collapseAll],
  );
  useBuilderView(handle);

  // ---- reorder, and what it did to the reporting lines ------------------------
  const reordering = useRef<PendingReorder | null>(null);
  const { state, announce } = api;
  useEffect(() => {
    const p = reordering.current;
    if (!p) return;
    if (p.produced === undefined) {
      const last = state.last;
      if (
        state.generation === p.from + 1 &&
        last?.kind === "applied" &&
        last.op.type === "reorder" &&
        last.op.target === p.target
      ) {
        p.produced = state.generation;
      } else {
        // Refused, or something else moved the draft first.
        reordering.current = null;
        return;
      }
    }
    if (state.generation !== p.produced) {
      // Another change landed before the check answered. Generations only
      // move forward, so no check of the reordered draft alone will ever
      // answer now (the next one describes both changes, and the review
      // lists them): the record is released rather than kept waiting.
      reordering.current = null;
      return;
    }
    if (state.check.generation !== p.produced) return;
    reordering.current = null;
    if (!state.check.derived) return;
    const changes = deriveChanges({
      base: p.before,
      next: { draft: state.draft, derived: state.check.derived },
      ops: [],
      reports: [],
    });
    const lines = changes.reportsTo.map(
      (r) =>
        `${r.ref.name} now reports to ${r.after?.name ?? "no one"} instead of ${r.before?.name ?? "no one"}.`,
    );
    if (lines.length > 0) announce(`The new order changes a primary manager. ${lines.join(" ")}`);
  }, [state, announce]);

  const reorder = (id: NodeKey, direction: -1 | 1) => {
    const view = structure.nodes.get(id);
    if (!view || view.type === "company" || api.readOnly) return;
    if (view.type === "seat" && view.placedByRef) {
      announce(
        `${view.name} is placed in this unit by its unit reference. Move it into the unit to reorder it.`,
      );
      return;
    }
    const found = locate(state.draft, id);
    if (!found) return;
    // A ROW MOVES PAST THE ROW THE OPERATOR SEES BESIDE IT. The company's list
    // of root seats also holds the ones the engine placed in units by
    // reference, drawn inside those units, so counting in the document's list
    // would step over an invisible sibling: a keypress that changes nothing
    // on screen. The step is counted among the siblings as drawn and written
    // as a place in the document's list.
    const parentView = structure.nodes.get(found.parent);
    const drawn: readonly NodeKey[] =
      found.kind === "seat" && parentView?.type === "company"
        ? parentView.seats
        : found.siblings.map((s) => s.key);
    const past = drawn[drawn.indexOf(id) + direction];
    if (past === undefined) {
      const kind = found.kind === "seat" ? "seat" : "unit";
      const where = found.parent === COMPANY_KEY ? "the company" : nameOf(structure, found.parent);
      announce(
        `${view.name} is already the ${direction < 0 ? "first" : "last"} ${kind} in ${where}.`,
      );
      return;
    }
    // Down: straight after the row it passes. Up: straight before it, which
    // the document writes as after whatever precedes that row there.
    const before = found.siblings.findIndex((s) => s.key === past) - 1;
    const after = direction > 0 ? past : before < 0 ? null : found.siblings[before]!.key;
    const current = state.check.generation === state.generation ? state.check.derived : null;
    reordering.current = current
      ? { from: state.generation, target: id, before: { draft: state.draft, derived: current } }
      : null;
    api.dispatch({
      type: "record",
      intent: { type: "reorder", target: id, to: { parent: found.parent, after } },
    });
    focusAt(id, null);
  };

  // ---- keys ------------------------------------------------------------------
  // CAPTURED, so a navigation key reaches the grid before the control that
  // holds focus in a cell: ArrowDown on a menu button would otherwise open
  // the menu instead of moving to the next row. An open menu keeps its keys
  // because it is in no row: it renders in the layer over the grid (see
  // below), and React carries its keys up through this handler with a target
  // no row contains.
  function onKeyDownCapture(e: KeyboardEvent<HTMLDivElement>) {
    const target = e.target as HTMLElement;
    const row = target.closest<HTMLElement>("[data-row-id]");
    if (!row) return;
    const id = row.getAttribute("data-row-id")!;
    const onRow = target === row;
    const cellIndex = target.closest("[role='gridcell']")?.getAttribute("aria-colindex");
    const col = onRow || !cellIndex ? null : Number(cellIndex);
    const adding = addParentOf(id) !== null;
    const cells = adding ? ADD_BUTTONS.length : COLUMNS.length;
    const handled = () => {
      e.preventDefault();
      e.stopPropagation();
    };

    if (e.altKey && !e.ctrlKey && !e.metaKey && (e.key === "ArrowUp" || e.key === "ArrowDown")) {
      if (onRow && !adding) {
        handled();
        reorder(id, e.key === "ArrowUp" ? -1 : 1);
      }
      return;
    }

    if (onRow && !adding) {
      const action = nodeKeyAction(e);
      const view = structure.nodes.get(id);
      if (action === "menu") {
        handled();
        setMenuFor(id);
        return;
      }
      if (action === "edit") {
        handled();
        api.openEditor(id);
        return;
      }
      if (action === "delete") {
        if (view && isDeletable(view) && !api.readOnly) {
          handled();
          api.openDelete(id);
        }
        return;
      }
    }

    if (e.ctrlKey || e.metaKey || e.altKey) return;
    const sameColumn = (next: string | null) => {
      if (next === null) return;
      const width = addParentOf(next) !== null ? ADD_BUTTONS.length : COLUMNS.length;
      focusAt(next, col === null ? null : Math.min(col, width));
    };

    if (col !== null) {
      switch (e.key) {
        case "ArrowRight":
          handled();
          if (col < cells) focusAt(id, col + 1);
          return;
        case "ArrowLeft":
          handled();
          focusAt(id, col > 1 ? col - 1 : null);
          return;
        case "Home":
          handled();
          focusAt(id, 1);
          return;
        case "End":
          handled();
          focusAt(id, cells);
          return;
        case "ArrowDown":
          handled();
          sameColumn(nextVisible(model, expanded, id));
          return;
        case "ArrowUp":
          handled();
          sameColumn(previousVisible(model, expanded, id));
          return;
        default:
          // Enter and Space belong to the control in the cell.
          return;
      }
    }

    // A row: Right opens a closed row, and steps into an open or leaf row's cells.
    if (e.key === "ArrowRight" && !(isExpandable(model, id) && !expanded.has(id))) {
      handled();
      focusAt(id, 1);
      return;
    }
    if (adding && e.key === "Enter") {
      handled();
      focusAt(id, 1);
      return;
    }
    const step = treeStep(tree, id, e);
    if (step === undefined) return;
    handled();
    if (step === null) return;
    if ("toggle" in step) toggle(step.toggle);
    else focusAt(step.focus, null);
  }

  // ---- rows ------------------------------------------------------------------
  const rowProps = (id: string, extra: { selected?: boolean; label?: string }) => ({
    role: "row",
    "data-row-id": id,
    ref: rowRef(id),
    tabIndex: id === active && column === null ? 0 : -1,
    "aria-level": level(model, id),
    "aria-setsize": setSize(model, id),
    "aria-posinset": posInSet(model, id),
    "aria-expanded": isExpandable(model, id) ? expanded.has(id) : undefined,
    "aria-selected": extra.selected,
    "aria-label": extra.label,
    style: { "--depth": level(model, id) - 1 } as CSSProperties,
    onClick: (event: MouseEvent<HTMLElement>) => {
      // A press on a control in the row is the control's; a press on the row
      // itself selects it and gives it focus.
      if ((event.target as HTMLElement).closest("button, [role='menu']")) return;
      focusAt(id, null);
    },
  });
  /** Whether the active cell of `id` is column `col`: its control is then the tab stop. */
  const stop = (id: string, col: number) => id === active && column === col;
  // A POINTER OPENING A CELL'S CONTROL MOVES THE TAB STOP TO IT. The control
  // keeps the focus the press gave it and hands it back when the menu closes,
  // so the grid's one tab stop has to be that cell rather than the row it sits
  // in. Focus is not moved here: the menu is taking it.
  const markCell = (id: string, col: number) => {
    setActive(id);
    setColumn(col);
  };

  // THE MENUS OPEN OVER THE GRID, NOT INSIDE IT. The grid scrolls sideways
  // at narrow widths, and a box that scrolls on one axis clips on both, so a
  // menu drawn under its trigger in the last rows would be cut off and would
  // scroll the grid down instead of opening. They render in a layer over the
  // frame instead, placed from the trigger's rectangle, and a sideways scroll
  // tells the layer so an open menu follows its trigger or closes with it.
  const [layer, setLayer] = useState<HTMLElement | null>(null);
  const onScroll = () => layer?.dispatchEvent(new CustomEvent(VIEW_CHANGE_EVENT));

  return (
    <div className="boutline-frame">
      <div className="boutline-wrap" onScroll={onScroll}>
        <div
          role="treegrid"
          aria-label="Organization outline"
          aria-colcount={COLUMNS.length}
          aria-readonly={api.readOnly || undefined}
          className="boutline"
          onKeyDownCapture={onKeyDownCapture}
        >
          <div role="rowgroup" className="boutline-head">
            <div role="row" className="boutline-row">
              {COLUMNS.map((title, i) => (
                <div role="columnheader" aria-colindex={i + 1} key={title}>
                  {i === COLUMNS.length - 1 ? <span className="sr-only">{title}</span> : title}
                </div>
              ))}
            </div>
          </div>
          <div role="rowgroup">
            {rows.map((id) => {
              const parent = addParentOf(id);
              if (parent !== null) {
                const where = parent === COMPANY_KEY ? "the company" : nameOf(structure, parent);
                return (
                  <div
                    key={id}
                    className="boutline-row add"
                    {...rowProps(id, { label: `Add to ${where}` })}
                  >
                    {ADD_BUTTONS.map((b, i) => (
                      <div
                        role="gridcell"
                        aria-colindex={i + 1}
                        key={b.kind}
                        className="boutline-add-cell"
                      >
                        <span data-cell-widget="">
                          <Button
                            size="sm"
                            variant="ghost"
                            icon={b.kind === "unit" ? "folderPlus" : "userPlus"}
                            tabIndex={stop(id, i + 1) ? 0 : -1}
                            onClick={() =>
                              api.openAdd(parent === COMPANY_KEY ? null : parent, b.kind)
                            }
                          >
                            {b.label}
                          </Button>
                        </span>
                      </div>
                    ))}
                  </div>
                );
              }
              const view = structure.nodes.get(id);
              if (!view) return null;
              return (
                <div
                  key={id}
                  className="boutline-row"
                  {...rowProps(id, { selected: api.selection.key === id })}
                >
                  <NameCell
                    api={api}
                    view={view}
                    expanded={expanded.has(id)}
                    expandable={isExpandable(model, id)}
                    tabStop={stop(id, 1)}
                    onToggle={() => toggle(id)}
                    onPress={() => focusAt(id, null)}
                  />
                  <Cell col={2} tabStop={stop(id, 2)}>
                    {view.type === "seat" ? (
                      <>
                        <span className="truncate">{seatKindLabel(view)}</span>
                        <LiveState api={api} view={view} />
                      </>
                    ) : view.type === "unit" ? (
                      <span className="truncate">{view.unitType || "Unit"}</span>
                    ) : (
                      "Company"
                    )}
                  </Cell>
                  <Cell col={3} tabStop={stop(id, 3)}>
                    {view.type === "seat" && (
                      <span className="mono truncate">{handleLabel(view.handle)}</span>
                    )}
                  </Cell>
                  <Cell col={4} tabStop={stop(id, 4) && !leadWidget(api, view)}>
                    <LeadOrManager
                      api={api}
                      structure={structure}
                      view={view}
                      layer={layer}
                      tabStop={stop(id, 4)}
                      onOpen={() => markCell(id, 4)}
                    />
                  </Cell>
                  <Cell col={5} tabStop={stop(id, 5)}>
                    <ProblemCount api={api} nodeKey={id} />
                  </Cell>
                  <Cell col={6} tabStop={false} className="boutline-actions">
                    <span data-cell-widget="">
                      <Menu
                        label={`Actions for ${view.name || "the company"}`}
                        items={nodeMenu(api, view, open)}
                        layer={layer}
                        triggerTabIndex={stop(id, 6) ? 0 : -1}
                        open={menuFor === id}
                        onOpenChange={(opened) => {
                          // Only a press reports an opening: the ContextMenu key
                          // sets `menuFor` itself, and focus goes back to the row.
                          if (opened) markCell(id, 6);
                          setMenuFor((was) => (opened ? id : was === id ? null : was));
                        }}
                      />
                    </span>
                  </Cell>
                </div>
              );
            })}
          </div>
        </div>
      </div>
      <div className="popup-layer" ref={setLayer} />
    </div>
  );
}

function nameOf(structure: Structure, key: NodeKey): string {
  return structure.nodes.get(key)?.name ?? "";
}

/**
 * A cell. It is the tab stop while it is the active cell and holds no
 * control; a cell with a control hands the stop to the control instead.
 */
function Cell({
  col,
  tabStop,
  className,
  children,
}: {
  col: number;
  tabStop: boolean;
  className?: string;
  children?: ReactNode;
}) {
  return (
    <div role="gridcell" aria-colindex={col} tabIndex={tabStop ? 0 : -1} className={className}>
      {children}
    </div>
  );
}

function NameCell({
  api,
  view,
  expanded,
  expandable,
  tabStop,
  onToggle,
  onPress,
}: {
  api: BuilderApi;
  view: NodeView;
  expanded: boolean;
  expandable: boolean;
  tabStop: boolean;
  onToggle: () => void;
  /** Takes the press the hidden chevron must not take: see the toggle below. */
  onPress: () => void;
}) {
  const name = view.name || (view.type === "company" ? "Unnamed company" : "");
  return (
    <div role="gridcell" aria-colindex={1} tabIndex={tabStop ? 0 : -1} className="boutline-name">
      {/* THE PRESS LANDS ON THE ROW. The chevron is a pointer-only control
          hidden from assistive technology (the keyboard opens and closes a row
          with Right and Left), and focus inside a hidden subtree is focus
          nowhere, so the press is stopped from focusing it and the row takes
          the focus instead. */}
      <span
        className="boutline-toggle"
        aria-hidden="true"
        onMouseDown={(e) => {
          e.preventDefault();
          onPress();
        }}
      >
        {expandable && (
          <Button
            size="sm"
            variant="ghost"
            icon={expanded ? "chevronDown" : "chevronRight"}
            title={expanded ? `Collapse ${name}` : `Expand ${name}`}
            tabIndex={-1}
            onClick={onToggle}
          />
        )}
      </span>
      {view.type === "seat" ? (
        <Avatar name={view.name} size="sm" human={view.kind === "human"} />
      ) : (
        <Icon name={view.type === "company" ? "flag" : "folder"} size="sm" />
      )}
      <span className="truncate">{name}</span>
      {view.type === "seat" && <SeatMarks view={view as SeatView} />}
      {view.type === "unit" && <UnitMarks view={view as UnitView} />}
    </div>
  );
}

/** Whether the lead column of a row holds a control: a unit's lead choice, while the draft can change. */
function leadWidget(api: BuilderApi, view: NodeView): boolean {
  return view.type === "unit" && !api.readOnly;
}

/** A unit's lead, chosen in place; a seat's primary manager as the engine derived it. */
function LeadOrManager({
  api,
  structure,
  view,
  layer,
  tabStop,
  onOpen,
}: {
  api: BuilderApi;
  structure: Structure;
  view: NodeView;
  /** The layer the lead choice opens in (see `OutlineView`). */
  layer: HTMLElement | null;
  tabStop: boolean;
  /** Says a press opened the lead choice, so the tab stop follows the focus into this cell. */
  onOpen: () => void;
}) {
  if (view.type === "company") return null;
  if (view.type === "seat") return <span className="truncate">{managerLabel(view.manager)}</span>;
  const unit = view as UnitView;
  if (!leadWidget(api, unit)) return <span className="truncate">{leadLabel(unit)}</span>;
  return (
    <span data-cell-widget="">
      <Menu
        label={`Lead of ${unit.name}`}
        icon="crown"
        items={leadMenu(api, structure, unit)}
        layer={layer}
        triggerTabIndex={tabStop ? 0 : -1}
        onOpenChange={(opened) => opened && onOpen()}
      >
        {leadLabel(unit)}
      </Menu>
    </span>
  );
}
