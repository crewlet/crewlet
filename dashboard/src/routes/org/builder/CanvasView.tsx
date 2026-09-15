/**
 * The org builder's canvas: the draft drawn as a chart on the design system's
 * tree canvas, in two arrangements.
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
 * THE CHART ITSELF IS `TreeCanvas` FROM `@crewlethq/ui`, and this module is
 * what the engine's own model puts into it. The layout, the connectors, the
 * ARIA tree pattern, focus and the reveal of every pointer-only control are
 * the design system's; a card's CONTENT, what a key does to a seat and what an
 * Add offers under a unit are the engine's, because only the engine knows what
 * a seat, a unit and a reporting cycle are. Everything above was a copy of
 * that component living here, which is how the two came to draw different
 * charts in the same product.
 *
 * WHAT THE READER SEES FIRST IS THE ORGANIZATION, not the tools for editing
 * it. A card says its name with the whole card's width, and the controls (the
 * expander, the actions menu, and the Add that hangs on the branch below a
 * card that can take a child) appear when the card is reached, by pointer, by
 * focus or by being the selected node. The keyboard reaches all of them
 * without any of that: Enter edits, Delete or Backspace deletes, the
 * ContextMenu key or Shift+F10 opens the node's menu, and the toolbar mirrors
 * the selected node.
 *
 * The chart's reading of the draft is `chartModel.ts` and every action is
 * `nodeActions.tsx`.
 */

import { useCallback, useMemo, useRef, type ReactNode } from "react";
import { plural } from "~/lib/format.ts";
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
  nodeMenu,
  reportingMenu,
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
import { useOpenScreen, useReporting, useStructure } from "./useCharts.ts";
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
import {
  Avatar,
  cx,
  EmptyState,
  IconButton,
  Menu,
  type MenuEntry,
  Tag,
  TreeCanvas,
  type TreeCardContext,
  type TreeCardInput,
  type TreeInput,
  type TreeItemAction,
  type TreeModel,
  type TreeViewHandle,
  VisuallyHidden,
} from "@crewlethq/ui";

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
  const cardOf = useCallback(
    (id: string) => {
      const view = structure.nodes.get(id);
      return view?.type === "seat" && view.parent !== COMPANY_KEY ? view.parent : id;
    },
    [structure],
  );
  const cards = useCallback(
    (model: TreeModel, expanded: ReadonlySet<string>): TreeCardInput[] => {
      const card = (id: string): TreeCardInput => {
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
  const onNodeKey = useCallback(
    (id: string, action: Exclude<TreeItemAction, "menu">): boolean => {
      const view = structure.nodes.get(id);
      if (!view) return false;
      if (action === "activate") api.openEditor(id);
      else {
        if (!isDeletable(view) || api.readOnly) return false;
        api.openDelete(id);
      }
      return true;
    },
    [api, structure],
  );
  return (
    <Chart
      label="Structure chart"
      nodes={structure.tree}
      cards={cards}
      cardOf={cardOf}
      onNodeKey={onNodeKey}
      isNode={(id) => structure.nodes.has(id)}
      hasNodeMenu={(id) => structure.nodes.has(id)}
      // A HUMAN SEAT WEARS THE DASHED EDGE every human seat on this dashboard
      // wears, and it is the card's frame, so the design system draws it.
      cardOutline={(id) => {
        const view = structure.nodes.get(id);
        return view?.type === "seat" && view.kind === "human";
      }}
      renderCard={(id, card) => (
        <StructureCard api={api} structure={structure} id={id} card={card} open={open} />
      )}
      // THE ADD IS ON THE BRANCH, under the card whose children it makes. It
      // used to be a third button crowding the card's own name, where "add a
      // unit under Engineering" was a menu on Engineering's top right corner
      // rather than a control on the line its units hang from.
      renderUnder={(id) => {
        const view = structure.nodes.get(id);
        if (!view || view.type === "seat" || api.readOnly) return null;
        return <AddButton api={api} view={view} />;
      }}
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
  const cards = useCallback((model: TreeModel, expanded: ReadonlySet<string>): TreeCardInput[] => {
    const card = (id: string): TreeCardInput => ({
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
  const onNodeKey = (id: string, action: Exclude<TreeItemAction, "menu">): boolean => {
    const seat = seatOf(id);
    // Enter on the reporting chart is its menu's first entry, Edit reports.
    if (action === "activate" && seat) {
      api.openEditor(seat.key, "reports");
      return true;
    }
    return false;
  };

  if (!chart.known) {
    return (
      <EmptyState
        icon={<AccountTreeGlyph />}
        title="Reporting lines appear after the check"
        description="Who reports to whom is derived by the engine. It is drawn here once the engine has checked this draft."
      />
    );
  }
  if (chart.roots.length === 0 && chart.cycles.length === 0) {
    return (
      <EmptyState
        icon={<AccountTreeGlyph />}
        title="No seats to report on"
        description="Who reports to whom is drawn here once the organization has a seat."
      />
    );
  }
  const stale = api.state.check.generation !== api.state.generation;
  return (
    <Chart
      label="Reporting chart"
      overlay={
        // Over the canvas rather than above it: a note that came and went
        // with every check would resize the viewport under the operator.
        stale && (
          <p className="bchart-note">
            These reporting lines are from the last check. Changes made since then appear after the
            next one.
          </p>
        )
      }
      nodes={chart.tree}
      cards={cards}
      cardOf={(id) => id}
      onNodeKey={onNodeKey}
      isNode={(id) => seatOf(id) !== undefined}
      hasNodeMenu={(id) => seatOf(id) !== undefined}
      cardOutline={(id) => chart.items.get(id)?.kind === "human"}
      renderCard={(id, card) =>
        id === CYCLE_GROUP ? (
          <CycleGroupCard card={card} count={chart.cycles.length} />
        ) : (
          <ReportingCard
            api={api}
            item={chart.items.get(id)!}
            seat={seatOf(id)}
            card={card}
            open={open}
          />
        )
      }
    />
  );
}

// ---------------------------------------------------------------------------
// The chart: the design system's, wired to the Builder
// ---------------------------------------------------------------------------

/**
 * The design system's tree canvas, holding the Builder's selection, its
 * anchor and its view handle.
 *
 * It is a component of its own rather than four props repeated at each chart,
 * because the three things wired here are the same on both and each was a
 * defect the first time it was written twice: which node a relayout keeps
 * still, which ids a selection may hold, and the handle the toolbar's Expand
 * all, Collapse all and focus-a-node go through.
 */
function Chart({
  label,
  nodes,
  cards,
  cardOf,
  renderCard,
  renderUnder,
  cardOutline,
  onNodeKey,
  hasNodeMenu,
  isNode,
  overlay,
}: {
  label: string;
  nodes: readonly TreeInput[];
  cards: (model: TreeModel, expanded: ReadonlySet<string>) => TreeCardInput[];
  cardOf: (id: string) => string;
  renderCard: (id: string, card: TreeCardContext) => ReactNode;
  renderUnder?: (id: string, card: TreeCardContext) => ReactNode;
  cardOutline?: (id: string) => boolean;
  onNodeKey: (id: string, action: Exclude<TreeItemAction, "menu">) => boolean;
  hasNodeMenu: (id: string) => boolean;
  /** Whether an id names a node of the draft, which is what a selection can hold. */
  isNode: (id: string) => boolean;
  overlay?: ReactNode;
}) {
  const api = useBuilder();
  const view = useRef<TreeViewHandle>(null);

  // ONE IDENTITY FOR THE LENS'S LIFETIME, read through a ref: the Builder
  // registers a view in an effect, and a handle that changed identity would
  // register the mounted view again after every edit and every answer.
  const handle = useMemo(
    () => ({
      focusNode: (key: string) => view.current?.focusNode(key),
      expandAll: () => view.current?.expandAll(),
      collapseAll: () => view.current?.collapseAll(),
    }),
    [],
  );
  useBuilderView(handle);

  return (
    <div className="bchart">
      <TreeCanvas
        className="bchart-canvas"
        ref={view}
        label={label}
        nodes={nodes}
        cards={cards}
        cardOf={cardOf}
        renderCard={renderCard}
        renderUnder={renderUnder}
        cardOutline={cardOutline}
        onNodeKey={onNodeKey}
        hasNodeMenu={hasNodeMenu}
        // SELECTION FOLLOWS FOCUS, for nodes of the draft: the selection is
        // what the toolbar acts on and what the URL names, and a group heading
        // is neither.
        selectedId={api.selection.key}
        onSelect={(id) => {
          if (isNode(id)) api.selection.select(id);
        }}
        // THE NODE THE OPERATOR ACTED ON stays where it was across a relayout.
        anchorNode={api.state.last?.focus ?? null}
        overlay={overlay}
      />
    </div>
  );
}

// ---------------------------------------------------------------------------
// Cards
// ---------------------------------------------------------------------------

function StructureCard({
  api,
  structure,
  id,
  card,
  open,
}: {
  api: BuilderApi;
  structure: Structure;
  id: string;
  card: TreeCardContext;
  open: OpenScreen;
}) {
  const view = structure.nodes.get(id);
  if (!view) return null;
  if (view.type === "seat") {
    return (
      <>
        <div {...card.item(id)} className="bchart-head">
          <SeatBody api={api} view={view} />
        </div>
        <div {...card.actions(id)}>
          <MoreMenu card={card} id={id} label={view.name} items={nodeMenu(api, view, open)} />
        </div>
      </>
    );
  }
  const expanded = card.expanded(id);
  if (view.type === "company") {
    return (
      <>
        <div {...card.item(id)} className="bchart-head">
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
        <div {...card.actions(id)}>
          <ToggleButton card={card} id={id} name={view.name || "the company"} />
          <MoreMenu
            card={card}
            id={id}
            label={view.name || "the company"}
            items={nodeMenu(api, view, open)}
          />
        </div>
      </>
    );
  }
  return (
    <>
      <div {...card.item(id)} className="bchart-head">
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
        <VisuallyHidden>{leadSentence(view)}</VisuallyHidden>
        <span className="bchart-marks">
          <UnitMarks view={view} />
        </span>
      </div>
      <div {...card.actions(id)}>
        <ToggleButton card={card} id={id} name={view.name} />
        <MoreMenu card={card} id={id} label={view.name} items={nodeMenu(api, view, open)} />
      </div>
      <LeadChip api={api} structure={structure} unit={view} card={card} />
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
                <div {...card.item(key)} className="bchart-row-item">
                  <SeatBody api={api} view={seat} />
                </div>
                <div {...card.actions(key)}>
                  <MoreMenu
                    card={card}
                    id={key}
                    label={seat.name}
                    items={nodeMenu(api, seat, open)}
                  />
                </div>
              </div>
            );
          })}
        </div>
      )}
    </>
  );
}

/** A seat's name, kind, handle and marks: the body of a root seat card and of a unit's seat row. */
function SeatBody({ api, view }: { api: BuilderApi; view: SeatView }) {
  return (
    <>
      <span className="bchart-line">
        <Avatar
          name={view.name}
          size="xs"
          variant={view.kind === "human" ? "dashed" : "solid"}
          decorative
        />
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
  card,
  open,
}: {
  api: BuilderApi;
  item: ReportingItem;
  seat: SeatView | undefined;
  card: TreeCardContext;
  open: OpenScreen;
}) {
  return (
    <>
      <div {...card.item(item.id)} className="bchart-head">
        <span className="bchart-line">
          <Avatar
            name={item.name}
            size="xs"
            variant={item.kind === "human" ? "dashed" : "solid"}
            decorative
          />
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
          {item.root && <Tag appearance="outline">No manager</Tag>}
          {item.cycleSize !== undefined && (
            <Tag variant="warning" leadingIcon={<CycleGlyph />}>
              {`In a reporting cycle of ${plural(item.cycleSize, "seat")}`}
            </Tag>
          )}
        </span>
      </div>
      <div {...card.actions(item.id)}>
        {item.reports.length > 0 && <ToggleButton card={card} id={item.id} name={item.name} />}
        {seat && (
          <MoreMenu
            card={card}
            id={item.id}
            label={item.name}
            items={reportingMenu(api, seat, open)}
          />
        )}
      </div>
    </>
  );
}

function CycleGroupCard({ card, count }: { card: TreeCardContext; count: number }) {
  return (
    <>
      <div {...card.item(CYCLE_GROUP)} className="bchart-head">
        <CycleGlyph size="sm" />
        <span className="bchart-text">
          <span className="bchart-name">Reporting cycle</span>
          <span className="bchart-meta">
            {plural(count, "cycle")}. These seats manage each other in a loop, so none of them has a
            top. Each loop is drawn from its first seat in the engine's order.
          </span>
        </span>
      </div>
      <div {...card.actions(CYCLE_GROUP)}>
        <ToggleButton card={card} id={CYCLE_GROUP} name="the reporting cycles" />
      </div>
    </>
  );
}

function ToggleButton({ card, id, name }: { card: TreeCardContext; id: string; name: string }) {
  if (!card.expandable(id)) return null;
  const expanded = card.expanded(id);
  return (
    <IconButton
      label={expanded ? `Collapse ${name}` : `Expand ${name}`}
      icon={expanded ? <KeyboardArrowDownGlyph /> : <ChevronRightGlyph />}
      size="sm"
      tabIndex={-1}
      onClick={(e) => {
        e.stopPropagation();
        card.toggle(id);
      }}
    />
  );
}

/**
 * The Add on a card's branch: the three kinds a company or a unit can take,
 * under the card their children hang from.
 *
 * The same entries the node's own menu and the toolbar offer, because there is
 * one list of what can be added under a parent and three places that ask for
 * it. It is absent on a read-only draft rather than present and refusing.
 *
 * A MARK RATHER THAN A WORD, and it is named after the card it belongs to. A
 * visible "Add" is its own accessible name, so nine of them on one chart are
 * nine controls called "Add" and which card each belongs to is read off the
 * geometry. Named, each says the card it adds to, which is also its tooltip.
 */
function AddButton({ api, view }: { api: BuilderApi; view: NodeView }) {
  return (
    <Menu
      label={`Add to ${view.name || "the company"}`}
      icon={<AddGlyph />}
      items={addMenu(api, view)}
      triggerTabIndex={-1}
    />
  );
}

function MoreMenu({
  card,
  id,
  label,
  items,
}: {
  card: TreeCardContext;
  id: string;
  label: string;
  items: MenuEntry[];
}) {
  return (
    <Menu
      label={`Actions for ${label}`}
      items={items}
      triggerTabIndex={-1}
      open={card.menuOpen(id)}
      onOpenChange={(opened) => card.setMenuOpen(id, opened)}
    />
  );
}

/**
 * A unit's lead, as a chip along the card's bottom edge.
 *
 * IT STAYS DRAWN, unlike the controls that edit the card, because who leads a
 * unit is a FACT ABOUT THE ORGANIZATION rather than a tool for changing it: a
 * chart that hid it until the pointer arrived would be a chart you could not
 * read the leads off. What it opens is still a menu, and the press on it lands
 * on the unit's own node.
 */
function LeadChip({
  api,
  structure,
  unit,
  card,
}: {
  api: BuilderApi;
  structure: Structure;
  unit: UnitView;
  card: TreeCardContext;
}) {
  return (
    <div className="bchart-lead" aria-hidden="true" {...card.press(unit.key)}>
      <CrownGlyph size="xs" />
      {api.readOnly ? (
        <span className="truncate">{leadLabel(unit)}</span>
      ) : (
        <Menu
          label={`Lead of ${unit.name}`}
          icon={<KeyboardArrowDownGlyph />}
          items={leadMenu(api, structure, unit)}
          triggerTabIndex={-1}
          onOpenChange={(opened) => opened && card.activate(unit.key)}
          trigger={leadLabel(unit)}
        />
      )}
    </div>
  );
}
