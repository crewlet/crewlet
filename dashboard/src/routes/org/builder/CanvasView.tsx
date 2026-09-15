/**
 * The org builder's canvas: the draft drawn as a chart on a pannable,
 * zoomable surface, in two arrangements.
 *
 * - STRUCTURE: the company card at the root, root seats as cards beneath it,
 *   units as cards with their seats stacked inside as rows and their child
 *   units as tree children. A root seat the engine placed in a unit by its
 *   `unit:` reference is a row of that unit, marked; one whose reference names
 *   no unit stays at the root, marked with the engine's warning.
 * - REPORTING: who each seat reports to, as the engine derived it: the seats
 *   with no manager as the tops of a forest, and the seats that manage each
 *   other in a loop under a "Reporting cycle" group. Read-only, because a
 *   reporting line is not written anywhere as such: it follows from `manages`,
 *   `lead` and the unit tree, which a seat's editor changes ("Edit reports").
 *
 * The chart's reading of the draft is `chartModel.ts` and every action is
 * `nodeActions.tsx`; this module is the wiring from them to the canvas, the
 * tree pattern and the layout.
 *
 * THE KEYBOARD PATTERN IS A TREE, AND ITS ITEMS HOLD NOTHING FOCUSABLE. Each
 * card's header and each seat row is a `treeitem` with its level, position
 * and expansion, and focus moves among them by a roving tab stop: arrows
 * walk the visible order, Right and Left open, close and climb, Home and End
 * jump, and typing finds a node by name (`ui/treeModel.ts`). A treeitem's own
 * keys act on it: Enter edits, Delete or Backspace deletes, the ContextMenu
 * key or Shift+F10 opens its menu. The buttons a pointer uses (expand, add,
 * more, the lead chip) sit BESIDE the treeitem in a strip hidden from
 * assistive technology and out of the tab order, because a control inside a
 * treeitem is a control a screen reader cannot reach and a keyboard user
 * would Tab through by the hundred; the menu they open is the same one the
 * ContextMenu key opens, and the toolbar mirrors the focused node. A press on
 * that strip never leaves focus in it: the treeitem takes the focus, so what
 * holds it is always a node a screen reader can announce and the arrows can
 * move from.
 *
 * FOCUS NEVER SCROLLS BEHIND THE TRANSFORM'S BACK. A node is focused with
 * `preventScroll` and then revealed by panning the canvas, and a node inside
 * a collapsed unit has its ancestors opened first. The Builder decides which
 * node is focused after an operation, an undo or a redo; this view performs
 * it through the handle it registers (`useBuilderView`).
 *
 * THE LAYOUT IS MEASURED. Every card is rendered at the card-width token and
 * measured (`ui/useMeasuredSizes.ts`), the gaps between cards are read from a
 * probe drawn with spacing tokens (so density scales them with everything
 * else), and the tidy tree layout (`ui/tidytree.ts`) places them. A relayout
 * keeps the node the operator acted on still on screen. Live state is not an
 * input to any of it: a push changes a badge's text inside a slot that never
 * changes size, and nothing is laid out again.
 */

import {
  useCallback,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
  type MouseEvent,
  type ReactNode,
} from "react";
import { plural } from "~/lib/format.ts";
import { Canvas, useCanvasOverlay, type CanvasHandle } from "~/ui/Canvas.tsx";
import { Menu, type MenuEntry } from "~/ui/Menu.tsx";
import { Avatar, Badge, Button, Empty, cx } from "~/ui/primitives.tsx";
import { layoutForest, type Layout, type TreeNode } from "~/ui/tidytree.ts";
import {
  ancestors,
  isExpandable,
  level,
  posInSet,
  setSize,
  type TreeInput,
  type TreeModel,
} from "~/ui/treeModel.ts";
import { useLayoutAnchor, useMeasuredSizes } from "~/ui/useMeasuredSizes.ts";
import type { Point, Rect } from "~/ui/viewport.ts";
import { useBuilder, useBuilderView, type BuilderApi, type ChartKind } from "./BuilderContext.tsx";
import {
  CYCLE_GROUP,
  type NodeView,
  type ReportingItem,
  type SeatView,
  type Structure,
  type UnitView,
} from "./chartModel.ts";
import { COMPANY_KEY } from "./model/keys.ts";
import {
  addMenu,
  isDeletable,
  leadLabel,
  leadMenu,
  leadSentence,
  nodeKeyAction,
  nodeMenu,
  reportingMenu,
  type NodeKeyAction,
  type OpenScreen,
} from "./nodeActions.tsx";
import {
  LiveState,
  ProblemCount,
  SeatMarks,
  UnitMarks,
  handleLabel,
  seatKindLabel,
} from "./nodeMarks.tsx";
import { treeStep, useOpenScreen, useReporting, useStructure, useTreeState } from "./useCharts.ts";
import {
  AccountTreeGlyph,
  AddGlyph,
  ApartmentGlyph,
  ChevronRightGlyph,
  CrownGlyph,
  CycleGlyph,
  FolderGlyph,
  KeyboardArrowDownGlyph,
} from "@crewlethq/icons/glyphs";

/**
 * The canvas of the Builder lens.
 *
 * WHICH CHART IS THE LENS'S. The Builder owns the `chart` section param (its
 * toolbar is where the chart is chosen) and hands the answer in, so this view
 * never reads the URL a second time, where the two readings could disagree.
 */
export function CanvasView({ chart }: { chart: ChartKind }) {
  const api = useBuilder();
  const open = useOpenScreen();
  const structure = useStructure(api.state);
  if (chart === "reporting") {
    return <ReportingChart api={api} structure={structure} open={open} />;
  }
  return <StructureChart api={api} structure={structure} open={open} />;
}

// ---------------------------------------------------------------------------
// The two charts
// ---------------------------------------------------------------------------

function StructureChart({
  api,
  structure,
  open,
}: {
  api: BuilderApi;
  structure: Structure;
  open: OpenScreen;
}) {
  // A seat drawn inside a unit is a ROW of that unit's card; every other node
  // is a card of its own.
  const boxOf = useCallback(
    (id: string) => {
      const view = structure.nodes.get(id);
      return view?.type === "seat" && view.parent !== COMPANY_KEY ? view.parent : id;
    },
    [structure],
  );
  const boxes = useCallback(
    (model: TreeModel, expanded: ReadonlySet<string>): BoxInput[] => {
      const card = (id: string): BoxInput => {
        const view = structure.nodes.get(id);
        const shown = expanded.has(id);
        if (view?.type === "company") {
          return { id, children: shown ? [...view.seats, ...view.units].map(card) : [] };
        }
        if (view?.type === "unit") return { id, children: shown ? view.units.map(card) : [] };
        return { id, children: [] };
      };
      return model.roots.map(card);
    },
    [structure],
  );
  const act = useCallback(
    (id: string, action: NodeKeyAction): boolean => {
      const view = structure.nodes.get(id);
      if (!view) return false;
      if (action === "edit") api.openEditor(id);
      else if (action === "delete") {
        if (!isDeletable(view) || api.readOnly) return false;
        api.openDelete(id);
      }
      return true;
    },
    [api, structure],
  );
  return (
    <TreeCanvas
      label="Structure chart"
      tree={structure.tree}
      boxes={boxes}
      boxOf={boxOf}
      act={act}
      isNode={(id) => structure.nodes.has(id)}
      hasMenu={(id) => structure.nodes.has(id)}
      render={(id, ctx) => (
        <StructureCard api={api} structure={structure} id={id} ctx={ctx} open={open} />
      )}
    />
  );
}

function ReportingChart({
  api,
  structure,
  open,
}: {
  api: BuilderApi;
  structure: Structure;
  open: OpenScreen;
}) {
  const chart = useReporting(api.state);
  const boxes = useCallback((model: TreeModel, expanded: ReadonlySet<string>): BoxInput[] => {
    const card = (id: string): BoxInput => ({
      id,
      children: expanded.has(id) ? (model.children.get(id) ?? []).map(card) : [],
    });
    return model.roots.map(card);
  }, []);
  const seatOf = (id: string): SeatView | undefined => {
    const item = chart.items.get(id);
    const view = item?.key ? structure.nodes.get(item.key) : undefined;
    return view?.type === "seat" ? view : undefined;
  };
  const act = (id: string, action: NodeKeyAction): boolean => {
    const seat = seatOf(id);
    // Enter on the reporting chart is its menu's first entry, Edit reports.
    if (action === "edit" && seat) {
      api.openEditor(seat.key, "reports");
      return true;
    }
    return false;
  };

  if (!chart.known) {
    return (
      <Empty
        icon={AccountTreeGlyph}
        title="Reporting lines appear after the check"
        hint="Who reports to whom is derived by the engine. It is drawn here once the engine has checked this draft."
      />
    );
  }
  if (chart.roots.length === 0 && chart.cycles.length === 0) {
    return (
      <Empty
        icon={AccountTreeGlyph}
        title="No seats to report on"
        hint="Who reports to whom is drawn here once the organization has a seat."
      />
    );
  }
  const stale = api.state.check.generation !== api.state.generation;
  return (
    <TreeCanvas
      label="Reporting chart"
      note={
        // Over the canvas rather than above it: a note that came and went
        // with every check would resize the viewport under the operator.
        stale && (
          <p className="bchart-note">
            These reporting lines are from the last check. Changes made since then appear after the
            next one.
          </p>
        )
      }
      tree={chart.tree}
      boxes={boxes}
      boxOf={(id) => id}
      act={act}
      isNode={(id) => seatOf(id) !== undefined}
      hasMenu={(id) => seatOf(id) !== undefined}
      render={(id, ctx) =>
        id === CYCLE_GROUP ? (
          <CycleGroupCard ctx={ctx} count={chart.cycles.length} />
        ) : (
          <ReportingCard
            api={api}
            item={chart.items.get(id)!}
            seat={seatOf(id)}
            ctx={ctx}
            open={open}
          />
        )
      }
    />
  );
}

// ---------------------------------------------------------------------------
// Cards
// ---------------------------------------------------------------------------

function StructureCard({
  api,
  structure,
  id,
  ctx,
  open,
}: {
  api: BuilderApi;
  structure: Structure;
  id: string;
  ctx: CardContext;
  open: OpenScreen;
}) {
  const view = structure.nodes.get(id);
  if (!view) return null;
  if (view.type === "seat") {
    return (
      <div className={cx("bchart-card", "seat", view.kind === "human" && "human")}>
        <div {...ctx.item(id)} className="bchart-head">
          <SeatBody api={api} view={view} />
        </div>
        <CardActions ctx={ctx} id={id}>
          <MoreMenu ctx={ctx} id={id} label={view.name} items={nodeMenu(api, view, open)} />
        </CardActions>
      </div>
    );
  }
  const expanded = ctx.expanded(id);
  if (view.type === "company") {
    return (
      <div className="bchart-card company">
        <div {...ctx.item(id)} className="bchart-head">
          <span className="bchart-line">
            <ApartmentGlyph size="sm" />
            <span className="bchart-text">
              <span className="bchart-name truncate">{view.name || "Unnamed company"}</span>
              <span className="bchart-meta truncate">
                Company, {plural(view.seats.length, "root seat")},{" "}
                {plural(view.units.length, "unit")}
              </span>
            </span>
            <ProblemCount api={api} nodeKey={id} />
          </span>
        </div>
        <CardActions ctx={ctx} id={id}>
          <ToggleButton ctx={ctx} id={id} name={view.name || "the company"} />
          <AddMenu ctx={ctx} view={view} api={api} />
          <MoreMenu
            ctx={ctx}
            id={id}
            label={view.name || "the company"}
            items={nodeMenu(api, view, open)}
          />
        </CardActions>
      </div>
    );
  }
  return (
    <div className="bchart-card unit">
      <div {...ctx.item(id)} className="bchart-head">
        <span className="bchart-line">
          <FolderGlyph size="sm" />
          <span className="bchart-text">
            <span className="bchart-name truncate">{view.name}</span>
            <span className="bchart-meta truncate">
              {view.unitType || "Unit"}, {plural(view.seats.length, "seat")}
            </span>
          </span>
          <ProblemCount api={api} nodeKey={id} />
        </span>
        <span className="sr-only">{leadSentence(view)}</span>
        <span className="bchart-marks">
          <UnitMarks view={view} />
        </span>
      </div>
      <CardActions ctx={ctx} id={id}>
        <ToggleButton ctx={ctx} id={id} name={view.name} />
        <AddMenu ctx={ctx} view={view} api={api} />
        <MoreMenu ctx={ctx} id={id} label={view.name} items={nodeMenu(api, view, open)} />
      </CardActions>
      <LeadChip api={api} structure={structure} unit={view} ctx={ctx} />
      {expanded && view.seats.length > 0 && (
        <div role="none" className="bchart-rows">
          {view.seats.map((key) => {
            const seat = structure.nodes.get(key);
            if (seat?.type !== "seat") return null;
            return (
              <div
                role="none"
                key={key}
                className={cx("bchart-row", seat.kind === "human" && "human")}
              >
                <div {...ctx.item(key)} className="bchart-row-item">
                  <SeatBody api={api} view={seat} />
                </div>
                <CardActions ctx={ctx} id={key}>
                  <MoreMenu
                    ctx={ctx}
                    id={key}
                    label={seat.name}
                    items={nodeMenu(api, seat, open)}
                  />
                </CardActions>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

/** A seat's name, kind, handle and marks: the body of a root seat card and of a unit's seat row. */
function SeatBody({ api, view }: { api: BuilderApi; view: SeatView }) {
  return (
    <>
      <span className="bchart-line">
        <Avatar name={view.name} size="sm" human={view.kind === "human"} />
        <span className="bchart-text">
          <span className="bchart-name truncate">{view.name}</span>
          <span className="bchart-meta truncate">
            {seatKindLabel(view)}, <span className="mono">{handleLabel(view.handle)}</span>
          </span>
        </span>
        <LiveState api={api} view={view} />
        <ProblemCount api={api} nodeKey={view.key} />
      </span>
      <span className="bchart-marks">
        <SeatMarks view={view} />
      </span>
    </>
  );
}

function ReportingCard({
  api,
  item,
  seat,
  ctx,
  open,
}: {
  api: BuilderApi;
  item: ReportingItem;
  seat: SeatView | undefined;
  ctx: CardContext;
  open: OpenScreen;
}) {
  return (
    <div className={cx("bchart-card", "seat", item.kind === "human" && "human")}>
      <div {...ctx.item(item.id)} className="bchart-head">
        <span className="bchart-line">
          <Avatar name={item.name} size="sm" human={item.kind === "human"} />
          <span className="bchart-text">
            <span className="bchart-name truncate">{item.name}</span>
            <span className="bchart-meta truncate">
              {seatKindLabel(item)}, <span className="mono">{handleLabel(item.handle)}</span>
            </span>
          </span>
          {seat && <LiveState api={api} view={seat} />}
          {seat && <ProblemCount api={api} nodeKey={seat.key} />}
        </span>
        <span className="bchart-marks">
          {item.root && <Badge outline>No manager</Badge>}
          {item.cycleSize !== undefined && (
            <Badge tone="caution" icon={CycleGlyph}>
              {`In a reporting cycle of ${plural(item.cycleSize, "seat")}`}
            </Badge>
          )}
        </span>
      </div>
      <CardActions ctx={ctx} id={item.id}>
        {item.reports.length > 0 && <ToggleButton ctx={ctx} id={item.id} name={item.name} />}
        {seat && (
          <MoreMenu
            ctx={ctx}
            id={item.id}
            label={item.name}
            items={reportingMenu(api, seat, open)}
          />
        )}
      </CardActions>
    </div>
  );
}

function CycleGroupCard({ ctx, count }: { ctx: CardContext; count: number }) {
  return (
    <div className="bchart-card group">
      <div {...ctx.item(CYCLE_GROUP)} className="bchart-head">
        <CycleGlyph size="sm" />
        <span className="bchart-text">
          <span className="bchart-name">Reporting cycle</span>
          <span className="bchart-meta">
            {plural(count, "cycle")}. These seats manage each other in a loop, so none of them has a
            top. Each loop is drawn from its first seat in the engine's order.
          </span>
        </span>
      </div>
      <CardActions ctx={ctx} id={CYCLE_GROUP}>
        <ToggleButton ctx={ctx} id={CYCLE_GROUP} name="the reporting cycles" />
      </CardActions>
    </div>
  );
}

/**
 * The pointer's buttons for a card or row: beside the treeitem, out of the
 * tab order and hidden from assistive technology, whose way to the same
 * actions is the treeitem's own keys (see the module doc).
 *
 * THE PRESS LANDS ON THE NODE, NOT ON THE BUTTON. A press is stopped from
 * focusing the button it lands on and focuses the treeitem instead, because
 * focus inside a subtree hidden from assistive technology is focus nowhere:
 * a screen reader would have nothing to announce, and the menu the press
 * opens would hand focus back there when it closed. Focused on the node, the
 * arrows carry on from the card the operator pressed.
 */
function CardActions({ ctx, id, children }: { ctx: CardContext; id: string; children: ReactNode }) {
  return (
    <div className="bchart-actions" aria-hidden="true" {...ctx.press(id)}>
      {children}
    </div>
  );
}

function ToggleButton({ ctx, id, name }: { ctx: CardContext; id: string; name: string }) {
  if (!ctx.expandable(id)) return null;
  const expanded = ctx.expanded(id);
  return (
    <Button
      size="sm"
      variant="ghost"
      icon={expanded ? KeyboardArrowDownGlyph : ChevronRightGlyph}
      title={expanded ? `Collapse ${name}` : `Expand ${name}`}
      tabIndex={-1}
      onClick={(e) => {
        e.stopPropagation();
        ctx.toggle(id);
      }}
    />
  );
}

function AddMenu({ ctx, view, api }: { ctx: CardContext; view: NodeView; api: BuilderApi }) {
  const layer = useCanvasOverlay();
  return (
    <Menu
      label={`Add to ${view.name || "the company"}`}
      icon={AddGlyph}
      items={addMenu(api, view)}
      layer={layer}
      triggerTabIndex={-1}
      onOpenChange={(opened) => opened && ctx.activate(view.key)}
    />
  );
}

function MoreMenu({
  ctx,
  id,
  label,
  items,
}: {
  ctx: CardContext;
  id: string;
  label: string;
  items: MenuEntry[];
}) {
  const layer = useCanvasOverlay();
  return (
    <Menu
      label={`Actions for ${label}`}
      items={items}
      layer={layer}
      triggerTabIndex={-1}
      open={ctx.menuOpen(id)}
      onOpenChange={(opened) => ctx.setMenuOpen(id, opened)}
    />
  );
}

/** A unit's lead, as a chip that opens the lead choice in place. */
function LeadChip({
  api,
  structure,
  unit,
  ctx,
}: {
  api: BuilderApi;
  structure: Structure;
  unit: UnitView;
  ctx: CardContext;
}) {
  const layer = useCanvasOverlay();
  return (
    <div className="bchart-lead" aria-hidden="true" {...ctx.press(unit.key)}>
      <CrownGlyph size="xs" />
      {api.readOnly ? (
        <span className="truncate">{leadLabel(unit)}</span>
      ) : (
        <Menu
          label={`Lead of ${unit.name}`}
          icon={KeyboardArrowDownGlyph}
          items={leadMenu(api, structure, unit)}
          layer={layer}
          triggerTabIndex={-1}
          onOpenChange={(opened) => opened && ctx.activate(unit.key)}
        >
          {leadLabel(unit)}
        </Menu>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// The tree on a canvas
// ---------------------------------------------------------------------------

/** A card of the layout: its id and its child cards. */
interface BoxInput {
  id: string;
  children: BoxInput[];
}

/** What a card needs from the tree it is drawn in. */
interface CardContext {
  /** The props that make an element the treeitem of `id`. */
  item(id: string): TreeItemProps;
  expanded(id: string): boolean;
  expandable(id: string): boolean;
  toggle(id: string): void;
  /** Makes `id` the tree's current node without moving focus, as a pointer action on its card does. */
  activate(id: string): void;
  /** The props that make a press anywhere in a hidden strip land on `id` rather than on the button. */
  press(id: string): { onMouseDown(e: MouseEvent<HTMLElement>): void };
  menuOpen(id: string): boolean;
  setMenuOpen(id: string, open: boolean): void;
}

interface TreeItemProps {
  role: "treeitem";
  tabIndex: number;
  "aria-level": number;
  "aria-setsize": number;
  "aria-posinset": number;
  "aria-expanded": boolean | undefined;
  "aria-selected": boolean;
  "data-tree-id": string;
  ref: (el: HTMLElement | null) => void;
  onClick: (e: MouseEvent<HTMLElement>) => void;
  onDoubleClick: () => void;
}

/** The id of the probe the gaps between cards are measured from. */
const GAP_PROBE = "bchart:gap";

function TreeCanvas({
  label,
  tree: forest,
  boxes,
  boxOf,
  act,
  isNode,
  hasMenu,
  render,
  note,
}: {
  label: string;
  /** Drawn over the canvas without the transform. */
  note?: ReactNode;
  tree: readonly TreeInput[];
  boxes: (model: TreeModel, expanded: ReadonlySet<string>) => BoxInput[];
  /** The card a node is drawn in: itself, or the card holding its row. */
  boxOf: (id: string) => string;
  /** Carries out Enter or Delete on a node; false when the node has no such action. */
  act: (id: string, action: NodeKeyAction) => boolean;
  /** Whether an id names a node of the draft, which is what a selection can hold. */
  isNode: (id: string) => boolean;
  hasMenu: (id: string) => boolean;
  render: (id: string, ctx: CardContext) => ReactNode;
}) {
  const api = useBuilder();
  const tree = useTreeState(forest, api.selection.key);
  const { model, expanded, active, setActive, toggle } = tree;
  const [menuFor, setMenuFor] = useState<string | null>(null);

  // ---- measuring and layout -----------------------------------------------
  const { measure, sizes, measured } = useMeasuredSizes();
  const boxForest = useMemo(() => boxes(model, expanded), [boxes, model, expanded]);
  const boxIds = useMemo(() => {
    const out: string[] = [];
    const walk = (b: BoxInput) => {
      out.push(b.id);
      b.children.forEach(walk);
    };
    boxForest.forEach(walk);
    return out;
  }, [boxForest]);

  const previousLayout = useRef<Layout | null>(null);
  const layout = useMemo(() => {
    if (!measured([...boxIds, GAP_PROBE])) return previousLayout.current;
    const size = (id: string) => sizes.get(id)!;
    const gap = size(GAP_PROBE);
    const toTree = (b: BoxInput): TreeNode => ({
      id: b.id,
      width: size(b.id).width,
      height: size(b.id).height,
      children: b.children.map(toTree),
    });
    return layoutForest(boxForest.map(toTree), { gapX: gap.width, gapY: gap.height });
  }, [boxForest, boxIds, measured, sizes]);
  previousLayout.current = layout;

  const positions = useMemo(
    () =>
      layout ? new Map<string, Point>(layout.nodes.map((n) => [n.id, { x: n.x, y: n.y }])) : null,
    [layout],
  );

  const canvas = useRef<CanvasHandle>(null);
  const shownPositions = useRef<ReadonlyMap<string, Point> | null>(null);
  // THE NODE THE OPERATOR ACTED ON stays where it was: the last operation's
  // node, else the node with focus. A node that did not exist before the
  // relayout (one just added) has no position to keep, so its nearest
  // ancestor's card is kept still instead.
  const anchorId = useMemo(() => {
    const was = shownPositions.current;
    if (!was) return null;
    for (const candidate of [api.state.last?.focus, active]) {
      if (!candidate || !model.parent.has(candidate)) continue;
      for (const id of [candidate, ...ancestors(model, candidate).reverse()]) {
        const box = boxOf(id);
        if (was.has(box)) return box;
      }
    }
    return null;
  }, [api.state.last, active, model, boxOf]);
  useLayoutAnchor(positions, anchorId, (before, after) => canvas.current?.anchor(before, after));
  useLayoutEffect(() => {
    shownPositions.current = positions;
  }, [positions]);

  // ---- focus ----------------------------------------------------------------
  const items = useRef(new Map<string, HTMLElement>());
  const itemRefs = useRef(new Map<string, (el: HTMLElement | null) => void>());
  const refFor = (id: string) => {
    let callback = itemRefs.current.get(id);
    if (!callback) {
      callback = (el) => {
        if (el) items.current.set(id, el);
        else if (items.current.get(id)?.isConnected === false) items.current.delete(id);
      };
      itemRefs.current.set(id, callback);
    }
    return callback;
  };

  const pendingFocus = useRef<string | null>(null);
  const focusNow = useCallback(
    (id: string): boolean => {
      const el = items.current.get(id);
      const placed = layout?.byId.get(boxOf(id));
      if (!el || !el.isConnected || !placed) return false;
      el.focus({ preventScroll: true });
      // A row's rectangle is the card's, moved down to the row: `offsetTop`
      // is a layout value, unaffected by the canvas's transform.
      const box = el.closest<HTMLElement>(".bchart-box");
      const top = box ? offsetWithin(el, box) : 0;
      const rect: Rect = {
        x: placed.x,
        y: placed.y + top,
        width: placed.width,
        height: el.offsetHeight > 0 && top > 0 ? el.offsetHeight : placed.height,
      };
      canvas.current?.reveal(rect);
      return true;
    },
    [layout, boxOf],
  );

  const focusNode = useCallback(
    (id: string) => {
      if (!model.parent.has(id)) return;
      const opened = tree.open(id);
      setActive(id);
      if (opened || !focusNow(id)) pendingFocus.current = id;
    },
    [model, tree, setActive, focusNow],
  );

  // A node that was not on screen yet (inside a unit just opened, or a card
  // not measured yet) takes focus as soon as it is laid out.
  useLayoutEffect(() => {
    const id = pendingFocus.current;
    if (id !== null && focusNow(id)) pendingFocus.current = null;
  });

  const focusRef = useRef(focusNode);
  focusRef.current = focusNode;
  const { expandAll, collapseAll } = tree;
  const handle = useMemo(
    () => ({ focusNode: (key: string) => focusRef.current(key), expandAll, collapseAll }),
    [expandAll, collapseAll],
  );
  useBuilderView(handle);

  // SELECTION FOLLOWS FOCUS, for nodes of the draft: the selection is what the
  // toolbar acts on and what the URL names, and a group heading is neither.
  const select = (id: string) => {
    if (isNode(id)) api.selection.select(id);
  };
  const moveTo = (id: string | null) => {
    if (id === null) return;
    select(id);
    focusNode(id);
  };

  function onKeyDown(e: KeyboardEvent<HTMLDivElement>) {
    const target = e.target as HTMLElement;
    const id = target.getAttribute("data-tree-id");
    if (id === null || !model.parent.has(id)) return;
    const action = nodeKeyAction(e);
    if (action === "menu") {
      if (hasMenu(id)) {
        e.preventDefault();
        setMenuFor(id);
      }
      return;
    }
    if (action !== null) {
      if (act(id, action)) e.preventDefault();
      return;
    }
    const step = treeStep(tree, id, e);
    if (step === undefined) return;
    e.preventDefault();
    if (step === null) return;
    if ("toggle" in step) toggle(step.toggle);
    else moveTo(step.focus);
  }

  const selectedId = api.selection.key;
  const ctx: CardContext = {
    item: (id) => ({
      role: "treeitem",
      tabIndex: id === active ? 0 : -1,
      "aria-level": level(model, id),
      "aria-setsize": setSize(model, id),
      "aria-posinset": posInSet(model, id),
      "aria-expanded": isExpandable(model, id) ? expanded.has(id) : undefined,
      "aria-selected": id === selectedId,
      "data-tree-id": id,
      ref: refFor(id),
      onClick: (e) => {
        select(id);
        setActive(id);
        (e.currentTarget as HTMLElement).focus({ preventScroll: true });
      },
      onDoubleClick: () => act(id, "edit"),
    }),
    expanded: (id) => expanded.has(id),
    expandable: (id) => isExpandable(model, id),
    toggle,
    activate: (id) => setActive(id),
    press: (id) => ({
      onMouseDown: (e) => {
        // The press must not focus the button it landed on: see CardActions.
        e.preventDefault();
        select(id);
        setActive(id);
        items.current.get(id)?.focus({ preventScroll: true });
      },
    }),
    menuOpen: (id) => menuFor === id,
    setMenuOpen: (id, open) => {
      if (open) setActive(id);
      setMenuFor((was) => (open ? id : was === id ? null : was));
    },
  };

  const links = useMemo(() => (layout ? connectors(layout) : []), [layout]);

  return (
    <div className="bchart">
      <Canvas label={label} content={layout?.bounds ?? null} ref={canvas} overlay={note}>
        <div className="bchart-gap-probe" ref={measure(GAP_PROBE)} aria-hidden="true" />
        {layout && (
          <svg
            className="bchart-links"
            width={layout.bounds.width}
            height={layout.bounds.height}
            aria-hidden="true"
          >
            {links.map((d, i) => (
              <path key={i} d={d} />
            ))}
          </svg>
        )}
        <div role="tree" aria-label={label} className="bchart-tree" onKeyDown={onKeyDown}>
          {boxIds.map((id) => {
            const placed = layout?.byId.get(id);
            return (
              <div
                key={id}
                role="none"
                ref={measure(id)}
                className="bchart-box"
                style={
                  placed
                    ? { transform: `translate(${placed.x}px, ${placed.y}px)` }
                    : { visibility: "hidden" }
                }
              >
                {render(id, ctx)}
              </div>
            );
          })}
        </div>
      </Canvas>
    </div>
  );
}

/** How far down `el` sits inside `container`, in layout pixels. */
function offsetWithin(el: HTMLElement, container: HTMLElement): number {
  let top = 0;
  for (
    let at: HTMLElement | null = el;
    at && at !== container;
    at = at.offsetParent as HTMLElement | null
  ) {
    top += at.offsetTop;
  }
  return top;
}

/**
 * The connector from each card to its parent, as SVG paths: down from the
 * parent's bottom centre to halfway through the gap, across, and down into
 * the child's top centre.
 */
function connectors(layout: Layout): string[] {
  const out: string[] = [];
  for (const node of layout.nodes) {
    if (node.parent === null) continue;
    const parent = layout.byId.get(node.parent);
    if (!parent) continue;
    const fromX = parent.x + parent.width / 2;
    const fromY = parent.y + parent.height;
    const toX = node.x + node.width / 2;
    const toY = node.y;
    const midY = fromY + (toY - fromY) / 2;
    out.push(`M${fromX} ${fromY}V${midY}H${toX}V${toY}`);
  }
  return out;
}
