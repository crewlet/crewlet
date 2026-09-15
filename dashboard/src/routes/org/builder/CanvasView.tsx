/**
 * The org builder's canvas: the draft drawn as a chart on the design system's
 * tree canvas, in two arrangements.
 *
 * - STRUCTURE: the company at the root, its root seats and units hanging off
 *   it, each unit's seats and child units hanging off that. A root seat the
 *   engine placed in a unit by its `unit:` reference hangs off that unit,
 *   marked; one whose reference names no unit stays at the root, marked with
 *   the engine's warning.
 * - REPORTING: who each seat reports to, as the engine derived it: the seats
 *   with no manager as the tops of a forest, and the seats that manage each
 *   other in a loop under a "Reporting cycle" group. Read-only, because a
 *   reporting line is not written anywhere as such: it follows from `manages`,
 *   `lead` and the unit tree, which a seat's editor changes ("Edit reports").
 *
 * THE CHART ITSELF IS `TreeCanvas` FROM `@crewlethq/ui`, drawn in its `node`
 * appearance, which is the console org chart's own language: a node one rank
 * tall and as wide as its own name between a fixed icon zone and a fixed
 * actions column, branches that are single cubics from a parent's bottom into
 * a child's top, and a hue on the agent seats that reaches the node's fill,
 * its edge, the halo outside it, its name and the branch arriving at it. This
 * module is what the engine's own model puts into that: the layout, the
 * connectors, the ARIA tree pattern, focus and the reveal of every
 * pointer-only control are the design system's; a node's CONTENT, what a key
 * does to a seat and what an Add offers under a unit are the engine's,
 * because only the engine knows what a seat, a unit and a reporting cycle are.
 *
 * EVERY SEAT IS A NODE OF ITS OWN. A unit's seats used to be drawn as ROWS
 * stacked inside the unit's card, which made a unit as tall as its membership
 * and drew no branch between a unit and the seats in it. They hang off the
 * unit like its child units do now, which is the one arrangement the reporting
 * chart, the outline and the console chart all already used, so a reader
 * learns one shape rather than three.
 *
 * WHAT THE READER SEES FIRST IS THE ORGANIZATION, not the tools for editing
 * it. A node says its name and what kind of thing it is; the controls (the
 * expander, Edit, Delete, and the Add that hangs on the branch below a node
 * that can take a child) appear when the node is reached, by pointer, by focus
 * or by being the selected node. The keyboard reaches all of them without any
 * of that: Enter edits, Delete or Backspace deletes, the ContextMenu key or
 * Shift+F10 opens the node's whole menu, and the toolbar mirrors the selected
 * node.
 *
 * WHAT NEVER MOVES A NODE. The canvas lays nodes out by measuring them, so
 * everything that arrives from a push or a check sits in a slot whose room is
 * kept: the live state and the problem count in the label's trailing slot, the
 * wiring marks as fixed-size glyphs on the caption. A name that would wrap
 * truncates instead.
 *
 * The chart's reading of the draft is `chartModel.ts`, every action is
 * `nodeActions.tsx`, and which hue a seat takes is `nodeTone.ts`.
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
  addSections,
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
  ReportingMarks,
  SeatMarks,
  UnitMarks,
  handleLabel,
  seatKindLabel,
} from "./nodeMarks.tsx";
import { nodeTone, seatTone } from "./nodeTone.ts";
import { useOpenScreen, useReporting, useStructure } from "./useCharts.ts";
import { CrewletIcon } from "@crewlethq/icons";
import {
  AccountTreeGlyph,
  ApartmentGlyph,
  ChevronRightGlyph,
  CycleGlyph,
  DeleteGlyph,
  EditGlyph,
  KeyboardArrowDownGlyph,
  PersonGlyph,
} from "@crewlethq/icons/glyphs";
import {
  AddPill,
  EmptyState,
  IconButton,
  Menu,
  type MenuEntry,
  OrgNodeLabel,
  OrgNodeLead,
  TreeCanvas,
  type TreeCardContext,
  type TreeCardInput,
  type TreeCardTone,
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
  const cards = useCallback(
    (model: TreeModel, expanded: ReadonlySet<string>): TreeCardInput[] => {
      const card = (id: string): TreeCardInput => {
        const view = structure.nodes.get(id);
        if (!expanded.has(id) || view === undefined || view.type === "seat") {
          return { id, children: [] };
        }
        return { id, children: [...view.seats, ...view.units].map(card) };
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
      cardOf={(id) => id}
      onNodeKey={onNodeKey}
      isNode={(id) => structure.nodes.has(id)}
      hasNodeMenu={(id) => structure.nodes.has(id)}
      // A HUMAN SEAT WEARS THE DASHED EDGE every human seat on this dashboard
      // wears, and it is the node's frame, so the design system draws it.
      cardOutline={(id) => {
        const view = structure.nodes.get(id);
        return view?.type === "seat" && view.kind === "human";
      }}
      // AN AGENT SEAT CARRIES A HUE, derived from its key: see `nodeTone.ts`.
      cardTone={(id) => nodeTone(structure.nodes.get(id))}
      renderCard={(id, card) => (
        <StructureCard api={api} structure={structure} id={id} card={card} open={open} />
      )}
      // THE ADD IS ON THE BRANCH, under the node whose children it makes. It
      // used to be a third button crowding the node's own name, where "add a
      // unit under Engineering" was a menu on Engineering's top right corner
      // rather than a control on the line its units hang from.
      renderUnder={(id, card) => {
        const view = structure.nodes.get(id);
        if (!view) return null;
        return (
          <>
            <ToggleButton card={card} id={id} name={view.name || "the company"} />
            {view.type !== "seat" && !api.readOnly && <AddButton api={api} view={view} />}
          </>
        );
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
      cardTone={(id) => {
        const item = chart.items.get(id);
        return item ? seatTone(item.kind, item.key ?? undefined) : undefined;
      }}
      renderUnder={(id, card) => <ToggleButton card={card} id={id} name={nameOfItem(chart, id)} />}
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
 * all, Collapse all and focus-a-node go through. The appearance and the
 * connector shape are here for the same reason: two charts of one organization
 * drawn in two languages is the defect this whole module exists to stop.
 */
function Chart({
  label,
  nodes,
  cards,
  cardOf,
  renderCard,
  renderUnder,
  cardOutline,
  cardTone,
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
  cardTone?: (id: string) => TreeCardTone | undefined;
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
        appearance="node"
        connector="curve"
        nodes={nodes}
        cards={cards}
        cardOf={cardOf}
        renderCard={renderCard}
        renderUnder={renderUnder}
        cardOutline={cardOutline}
        cardTone={cardTone}
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
// Nodes
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
        <div {...card.item(id)} title={seatTitle(view)}>
          <OrgNodeLabel
            icon={view.kind === "human" ? <PersonGlyph size="sm" /> : <CrewletIcon />}
            iconRing={view.kind === "human" ? "dashed" : "none"}
            name={view.name}
            caption={seatKindLabel(view)}
            captionMarks={<SeatMarks view={view} />}
            trailing={
              <>
                <LiveState api={api} view={view} />
                <ProblemCount api={api} nodeKey={view.key} />
              </>
            }
          />
          <VisuallyHidden>{seatTitle(view)}</VisuallyHidden>
        </div>
        <NodeActions api={api} card={card} id={id} label={view.name} view={view} open={open} />
      </>
    );
  }
  if (view.type === "company") {
    const counts = `${plural(view.seats.length, "root seat")}, ${plural(view.units.length, "unit")}`;
    return (
      <>
        <div {...card.item(id)} title={counts}>
          <OrgNodeLabel
            icon={<ApartmentGlyph />}
            name={view.name || "Unnamed company"}
            caption="Company"
            trailing={<ProblemCount api={api} nodeKey={id} />}
          />
          <VisuallyHidden>{counts}</VisuallyHidden>
        </div>
        <NodeActions
          api={api}
          card={card}
          id={id}
          label={view.name || "the company"}
          view={view}
          open={open}
        />
      </>
    );
  }
  const counts = plural(view.seats.length, "seat");
  return (
    <>
      <div {...card.item(id)} title={counts}>
        <OrgNodeLabel
          icon={<AccountTreeGlyph />}
          name={view.name}
          caption={unitCaption(view)}
          captionMarks={<UnitMarks view={view} />}
          trailing={<ProblemCount api={api} nodeKey={id} />}
        />
        <VisuallyHidden>{counts}</VisuallyHidden>
        <VisuallyHidden>{leadSentence(view)}</VisuallyHidden>
      </div>
      <NodeActions api={api} card={card} id={id} label={view.name} view={view} open={open} />
      <LeadChip api={api} structure={structure} unit={view} card={card} />
    </>
  );
}

/** A unit's caption: the type it writes, capitalised, or the word for one that writes none. */
function unitCaption(view: UnitView): string {
  const type = view.unitType.trim();
  if (type === "") return "Unit";
  return type.charAt(0).toUpperCase() + type.slice(1);
}

/**
 * What a seat's node says beyond its name and its kind.
 *
 * ITS HANDLE IS HERE RATHER THAN ON THE CAPTION, which says what kind of thing
 * the node is and nothing else. A node is one rank tall and as wide as its
 * name: a caption carrying the kind and the handle would be truncated on the
 * node of every seat whose name is longer than its handle. It is the node's
 * tooltip and the sentence read after its name, and it is a column of the
 * table beside this chart.
 */
function seatTitle(view: SeatView): string {
  return `${seatKindLabel(view)}, ${handleLabel(view.handle)}`;
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
  const title = `${seatKindLabel(item)}, ${handleLabel(item.handle)}`;
  const standing = item.root
    ? "No manager."
    : item.cycleSize !== undefined
      ? `In a reporting cycle of ${plural(item.cycleSize, "seat")}.`
      : "";
  return (
    <>
      <div {...card.item(item.id)} title={title}>
        <OrgNodeLabel
          icon={item.kind === "human" ? <PersonGlyph size="sm" /> : <CrewletIcon />}
          iconRing={item.kind === "human" ? "dashed" : "none"}
          name={item.name}
          caption={seatKindLabel(item)}
          captionMarks={<ReportingMarks item={item} />}
          trailing={
            <>
              {seat && <LiveState api={api} view={seat} />}
              {seat && <ProblemCount api={api} nodeKey={seat.key} />}
            </>
          }
        />
        <VisuallyHidden>{title}</VisuallyHidden>
        {standing !== "" && <VisuallyHidden>{standing}</VisuallyHidden>}
      </div>
      <div {...card.actions(item.id)}>
        {seat && (
          <KeyboardMenu
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
  const note =
    "These seats manage each other in a loop, so none of them has a top. Each loop is drawn from its first seat in the engine's order.";
  return (
    <>
      <div {...card.item(CYCLE_GROUP)} title={note}>
        <OrgNodeLabel
          icon={<CycleGlyph />}
          name="Reporting cycle"
          caption={plural(count, "cycle")}
        />
        <VisuallyHidden>{note}</VisuallyHidden>
      </div>
    </>
  );
}

/** What a reporting chart node is called, for the branch control below it. */
function nameOfItem(chart: { items: ReadonlyMap<string, ReportingItem> }, id: string): string {
  return id === CYCLE_GROUP ? "the reporting cycles" : (chart.items.get(id)?.name ?? "");
}

/**
 * The column down a node's right edge: what it can be expanded into, Edit, and
 * Delete.
 *
 * TWO ACTIONS, where the node's whole menu has six. Edit and Delete are what
 * the console chart puts on a node and what this chart's own keys already do
 * (Enter, Delete), so they are the pair a pointer gets without opening
 * anything, and two is also what a node one rank tall can split into cells a
 * finger can hit. Everything else (Add, Move to, Open seat, Edit reports,
 * Change kind) is one list, in `nodeActions.nodeMenu`, and it is reached three
 * ways that are all still here: the ContextMenu key or Shift+F10 on the node,
 * the toolbar, which mirrors the selected node, and the Add on the branch
 * below.
 *
 * THE EXPANDER IS NOT HERE, it is on the branch beside the Add. Both are about
 * a node's CHILDREN rather than about the node, and the branch below the node
 * is where its children hang from; in the column they were a third cell, which
 * on one rank is 16px a pointer cannot land on.
 *
 * THE MENU IS STILL MOUNTED even though nothing visible opens it: a menu the
 * key can open has to have somewhere to be, and `KeyboardMenu` is that anchor,
 * drawn nowhere and taking no cell.
 */
function NodeActions({
  api,
  card,
  id,
  label,
  view,
  open,
}: {
  api: BuilderApi;
  card: TreeCardContext;
  id: string;
  label: string;
  view: NodeView;
  open: OpenScreen;
}) {
  const deletable = isDeletable(view) && !api.readOnly;
  return (
    <div {...card.actions(id)}>
      <IconButton
        label={`Edit ${label}`}
        icon={<EditGlyph />}
        size="sm"
        variant="ghost"
        tabIndex={-1}
        onClick={(event) => {
          event.stopPropagation();
          api.openEditor(id);
        }}
      />
      {deletable && (
        <IconButton
          label={`Delete ${label}`}
          icon={<DeleteGlyph />}
          size="sm"
          variant="ghost-danger"
          tabIndex={-1}
          onClick={(event) => {
            event.stopPropagation();
            api.openDelete(id);
          }}
        />
      )}
      <KeyboardMenu card={card} id={id} label={label} items={nodeMenu(api, view, open)} />
    </div>
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
      variant="ghost"
      tabIndex={-1}
      onClick={(e) => {
        e.stopPropagation();
        card.toggle(id);
      }}
    />
  );
}

/**
 * The Add on a node's branch: the three kinds a company or a unit can take,
 * under the node their children hang from.
 *
 * THE MARK SPLITS INTO THE THREE rather than opening a menu of them. What can
 * go under a node is three things and the choice between them is the whole of
 * the decision, so the design system's `AddPill` asks it in place: the mark the
 * pointer arrived at grows into the kinds, each one within a few pixels of
 * where the mark was. It is the console chart's own gesture, which this chart
 * is drawn to match.
 *
 * The same list the node's own menu and the toolbar offer (`addSections` and
 * `addMenu` are one list in `nodeActions.tsx`), because there is one answer to
 * what can be added under a parent and three places that ask for it. It is
 * absent on a read-only draft rather than present and refusing.
 *
 * A MARK RATHER THAN A WORD, and both it and each of its kinds are named after
 * the node they belong to. A visible "Add" is its own accessible name, so nine
 * of them on one chart are nine controls called "Add" and which node each
 * belongs to is read off the geometry.
 *
 * THE KEYBOARD NEVER REACHES IT, and does not need to: the strip it sits in is
 * pointer-only and out of the tab order, and the same three kinds are in the
 * node's own menu, which the ContextMenu key and Shift+F10 open, and in the
 * toolbar, which mirrors the selected node.
 */
function AddButton({ api, view }: { api: BuilderApi; view: NodeView }) {
  return (
    <AddPill
      label={`Add to ${view.name || "the company"}`}
      sections={addSections(api, view)}
      tabIndex={-1}
    />
  );
}

/**
 * The node's whole menu, with nothing to press.
 *
 * It exists because the ContextMenu key and Shift+F10 open a node's menu, and
 * a menu has to be anchored to something on the node to be placed. Marked so
 * the chart's actions column neither draws it nor gives it a cell.
 */
function KeyboardMenu({
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
    <span data-keyboard-only="true">
      <Menu
        label={`Actions for ${label}`}
        items={items}
        triggerTabIndex={-1}
        open={card.menuOpen(id)}
        onOpenChange={(opened) => card.setMenuOpen(id, opened)}
      />
    </span>
  );
}

/**
 * A unit's lead, along the bottom edge of its node.
 *
 * IT STAYS DRAWN, unlike the controls that edit the node, because who leads a
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
  // THE PILL IS DRAWN EMPTY where the unit has no lead, and it still SAYS
  // which of the two nothings it is. The chart this is drawn from writes
  // "Lead" in every pill with nothing in it; this builder has a third answer
  // that chart has no idea of, because the lead a unit inherits is derived by
  // the engine: "No lead" and "Lead after the check" are different facts, and
  // a pill reading "Lead" for both would hide a check that has not answered
  // behind a unit that declares nothing.
  const none = unit.lead === null || unit.lead === undefined;
  return (
    <div aria-hidden="true" {...card.press(unit.key)}>
      <OrgNodeLead empty={none}>
        {api.readOnly ? (
          <span className="truncate">{leadLabel(unit)}</span>
        ) : (
          <Menu
            label={`Lead of ${unit.name}`}
            items={leadMenu(api, structure, unit)}
            triggerTabIndex={-1}
            onOpenChange={(opened) => opened && card.activate(unit.key)}
            trigger={leadLabel(unit)}
          />
        )}
      </OrgNodeLead>
    </div>
  );
}
