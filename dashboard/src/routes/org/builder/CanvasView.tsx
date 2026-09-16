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
 * AND A NODE IS ADDED IN THE CHART. Picking a kind from the Add on a branch
 * does not open a dialog over the picture: the chart makes room in the rank
 * the new node will land in, reaches a dashed branch into that place, eases
 * onto it and draws the add form THERE, in the ghost of the node about to
 * exist. It is the design system's `composing` (a card the layout knows about
 * and the draft does not), so the tree's counts, keys and focus are untouched
 * by it, and it is the console chart's own gesture. The form is the engine's,
 * in `AddNodeDialog.tsx`, and so is the one case that cannot be drawn this
 * way: an add asked while the TABLE or the REPORTING chart is on screen, where
 * there is no place in the drawing for a unit's next child, still opens the
 * dialog (`Builder.tsx` decides which, because only it knows which view is
 * mounted).
 *
 * The chart's reading of the draft is `chartModel.ts`, every action is
 * `nodeActions.tsx`, and which hue a seat takes is `nodeTone.ts`.
 */

import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import { plural } from "~/lib/format.ts";
import {
  useBuilder,
  useBuilderView,
  type AddKind,
  type BuilderApi,
  type ChartKind,
} from "./BuilderContext.tsx";
import {
  CYCLE_GROUP,
  type NodeView,
  type ReportingItem,
  type SeatView,
  type Structure,
  type UnitView,
} from "./chartModel.ts";
import { AddNodeGhostForm } from "./AddNodeDialog.tsx";
import { COMPANY_KEY, type NodeKey } from "./model/keys.ts";
import {
  addSections,
  cardMenu,
  isDeletable,
  leadChipLabel,
  leadMenu,
  leadSentence,
  moveKey,
  reportingMenu,
  type OpenScreen,
} from "./nodeActions.tsx";
import {
  LiveState,
  NodeGlyph,
  ProblemCount,
  ReportingMarks,
  SeatMarks,
  UnitMarks,
  handleLabel,
  nodeGlyphKind,
  seatKindLabel,
  unitTypeLabel,
} from "./nodeMarks.tsx";
import { nodeTone, seatTone } from "./nodeTone.ts";
import { useReorder, type Reorder } from "./reorder.ts";
import { useOpenScreen, useReporting, useStructure } from "./useCharts.ts";
import {
  AccountTreeGlyph,
  ChevronRightGlyph,
  CloseGlyph,
  CycleGlyph,
  DeleteGlyph,
  EditGlyph,
  KeyboardArrowDownGlyph,
} from "@crewlethq/icons/glyphs";
import {
  AddPill,
  EmptyState,
  IconButton,
  Kbd,
  Menu,
  type MenuEntry,
  OrgNodeDisclosure,
  OrgNodeLabel,
  OrgNodeLead,
  TreeCanvas,
  type TreeCanvasHandle,
  type TreeCardContext,
  type TreeCardInput,
  type TreeCardTone,
  type TreeComposing,
  type TreeInput,
  type TreeItemAction,
  type TreeModel,
  VisuallyHidden,
} from "@crewlethq/ui";

/**
 * The canvas of the Builder lens.
 *
 * WHICH CHART IS THE LENS'S. The Builder owns the `chart` section param (its
 * toolbar is where the chart is chosen) and hands the answer in, so this view
 * never reads the URL a second time, where the two readings could disagree.
 */
export function CanvasView({
  chart,
  chrome = {},
  about = null,
  adding = null,
}: {
  chart: ChartKind;
  chrome?: Chrome;
  /**
   * The node a surface opened OVER the chart is about: the parent an add will
   * hang from, the node an editor is editing. Null while nothing is open.
   *
   * WHAT IT IS FOR. A dialog that asks for the name of a child says nothing
   * about WHERE that child will go, and the chart is the only thing that can
   * say it. Named here, the chart pushes itself back behind the surface and
   * eases onto the node it is about, and the reader is put back exactly where
   * they were when it closes. It is the chart this is drawn from's own gesture
   * (it saves its viewBox, eases onto the ghost of the node being added, and
   * restores it on close), and it is drawn by the design system's canvas, so a
   * reader who asked for less motion is moved without an animation.
   */
  about?: string | null;
  /**
   * An add the Builder has handed to the chart to draw, rather than opening a
   * dialog for it. Only the structure chart takes one: see [Adding].
   */
  adding?: Adding | null;
}) {
  const api = useBuilder();
  const open = useOpenScreen();
  const structure = useStructure(api.state);
  if (chart === "reporting") {
    return (
      <ReportingChart api={api} structure={structure} open={open} chrome={chrome} about={about} />
    );
  }
  return (
    <StructureChart
      api={api}
      structure={structure}
      open={open}
      chrome={chrome}
      about={about}
      adding={adding}
    />
  );
}

/**
 * An add drawn in the chart: which parent it hangs from, which kind it opened
 * on, which opening of it this is, and how it ends.
 *
 * ONE MOUNT PER OPENING, keyed on `opening`, for the reason the Builder's own
 * dialog host keys on it: every opening builds its form afresh (another node,
 * or the same node again), while the node it is about may be re-keyed under it
 * by the first check without the form being torn down and its focus thrown
 * away.
 */
export interface Adding {
  /** A unit's key, or `null` for the company root. */
  parent: NodeKey | null;
  kind?: AddKind;
  opening: number;
  onClose: () => void;
}

/** The ghost's card id. No key of the draft can collide with it: `model/keys.ts`
 *  mints `company`, `seat:`, `unit:`, `seat@`, `unit@` and `new:` and nothing else. */
const ADD_GHOST = "adding:";

/**
 * What the PAGE hangs in the canvas's own control group.
 *
 * WHY THE PAGE STILL OWNS THEM. Both of these act on the canvas, so both
 * belong beside it: the chart this is drawn from keeps its zoom bar, its key
 * hint and its chart switch in one column in the canvas's top right corner,
 * and measured on this build the switch and the fullscreen toggle had ended up
 * in the page's toolbar 800px from the bar they belong to. But the fullscreen
 * toggle acts on the builder's whole container and the switch writes the
 * lens's own section param, and neither of those is this view's to hold: the
 * Builder hands them in, and this view says WHERE they are drawn.
 */
export interface Chrome {
  /** Drawn at the end of the zoom bar: the fullscreen toggle. */
  controls?: ReactNode;
  /** Its own bar under the zoom bar: the switch between the two charts. */
  switcher?: ReactNode;
}

// ---------------------------------------------------------------------------
// The two charts
// ---------------------------------------------------------------------------

function StructureChart({
  api,
  structure,
  open,
  chrome,
  about,
  adding,
}: {
  api: BuilderApi;
  structure: Structure;
  open: OpenScreen;
  chrome: Chrome;
  about: string | null;
  adding: Adding | null;
}) {
  const reorder = useReorder(api, structure);
  /*
   * THE GHOST OF THE NODE ABOUT TO EXIST. The parent is a card of this chart,
   * so a ghost is only asked for where the parent is still in the draft: an
   * add left open across a delete of its own parent has nothing to hang from,
   * and the form then falls back to nothing rather than to a card floating at
   * the origin. The refusal a reader needs in that case is in the form itself
   * ("That unit is no longer in the draft"), which is why it stays in the
   * fields rather than in the dialog shell.
   */
  const composing: TreeComposing | null = useMemo(() => {
    if (adding === null) return null;
    const parent = adding.parent ?? COMPANY_KEY;
    const view = structure.nodes.get(parent);
    if (!view) return null;
    const where = view.name || "the company";
    return {
      id: ADD_GHOST,
      parent,
      label: `Add to ${where}`,
      onCancel: adding.onClose,
      render: () => (
        <AddNodeGhostForm
          key={adding.opening}
          parent={adding.parent}
          {...(adding.kind ? { kind: adding.kind } : {})}
          onClose={adding.onClose}
        />
      ),
    };
  }, [adding, structure]);
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
      chrome={chrome}
      about={about}
      composing={composing}
      nodes={structure.tree}
      cards={cards}
      cardOf={(id) => id}
      onNodeKey={onNodeKey}
      /*
       * ALT WITH AN ARROW MOVES A NODE among the siblings it is drawn beside,
       * which is the key the outline already binds. This is the view where a
       * reader reaches for it first: a chart draws siblings left to right in
       * exactly the order the move changes, and passing one can change which
       * seat manages this one (`reorder.ts`).
       */
      onNodeKeyDown={(id, event) => !api.readOnly && moveKey(reorder, id as NodeKey, event)}
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
        <StructureCard
          api={api}
          structure={structure}
          id={id}
          card={card}
          open={open}
          reorder={reorder}
        />
      )}
      // THE ADD IS ON THE BRANCH, under the node whose children it makes, and
      // it is ALONE there. It used to be a third button crowding the node's
      // own name, where "add a unit under Engineering" was a menu on
      // Engineering's top right corner rather than a control on the line its
      // units hang from; then the expander shared the strip with it, and an
      // add pill splitting open covered the expander. The expander is on the
      // node's leading edge now (`leading` on every label below), which is
      // where every hierarchy a reader has used puts a disclosure.
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
  chrome,
  about,
}: {
  api: BuilderApi;
  structure: Structure;
  open: OpenScreen;
  chrome: Chrome;
  about: string | null;
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
      chrome={chrome}
      about={about}
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
  chrome,
  about,
  composing = null,
  nodes,
  cards,
  cardOf,
  renderCard,
  renderUnder,
  cardOutline,
  cardTone,
  onNodeKey,
  onNodeKeyDown,
  hasNodeMenu,
  isNode,
  overlay,
}: {
  label: string;
  chrome: Chrome;
  about: string | null;
  /** A node being composed in the chart: the design system draws the ghost. */
  composing?: TreeComposing | null;
  nodes: readonly TreeInput[];
  cards: (model: TreeModel, expanded: ReadonlySet<string>) => TreeCardInput[];
  cardOf: (id: string) => string;
  renderCard: (id: string, card: TreeCardContext) => ReactNode;
  renderUnder?: (id: string, card: TreeCardContext) => ReactNode;
  cardOutline?: (id: string) => boolean;
  cardTone?: (id: string) => TreeCardTone | undefined;
  onNodeKey: (id: string, action: Exclude<TreeItemAction, "menu">) => boolean;
  onNodeKeyDown?: (id: string, event: KeyboardEvent<HTMLElement>) => boolean;
  hasNodeMenu: (id: string) => boolean;
  /** Whether an id names a node of the draft, which is what a selection can hold. */
  isNode: (id: string) => boolean;
  overlay?: ReactNode;
}) {
  const api = useBuilder();
  const view = useRef<TreeCanvasHandle>(null);

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

  /*
   * THE CHART GOES TO THE NODE A SURFACE IS ABOUT, and comes back when it
   * closes. Nothing else moves the view: an add that changes the draft is a
   * relayout, which the chart tweens and anchors on its own.
   */
  useEffect(() => {
    if (about === null) {
      view.current?.restoreView();
      return;
    }
    view.current?.focusRegion(about);
  }, [about]);

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
        onNodeKeyDown={onNodeKeyDown}
        hasNodeMenu={hasNodeMenu}
        // PUSHED BACK BEHIND A SURFACE ABOUT ONE OF ITS NODES, so what is
        // being decided has the part of the chart it is about behind it rather
        // than a veil over a picture nobody can see any more. An add is NOT
        // such a surface any more: it is drawn IN the chart, so the chart
        // stays sharp and pannable behind the form and the reader can read the
        // organization they are adding to while they fill it in.
        dimmed={about !== null}
        composing={composing}
        // THE CHART'S OWN CHROME, in the chart's own corner: the design system
        // puts the zoom bar at the top right in this appearance and stacks
        // what the page hands in under it, which is the arrangement the chart
        // this is drawn from keeps.
        controlsExtra={chrome.controls}
        controlsBelow={chrome.switcher}
        hint={<FullscreenHint />}
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
  reorder,
}: {
  api: BuilderApi;
  structure: Structure;
  id: string;
  card: TreeCardContext;
  open: OpenScreen;
  reorder: Reorder;
}) {
  const view = structure.nodes.get(id);
  if (!view) return null;
  if (view.type === "seat") {
    return (
      <>
        <div {...card.item(id)} title={seatTitle(view)}>
          <OrgNodeLabel
            icon={<NodeGlyph kind={nodeGlyphKind(view)} />}
            iconRing={view.kind === "human" ? "dashed" : "none"}
            // AN AGENT SEAT WEARS THE LARGE MARK. The chart this is drawn from
            // sizes a mark by what it stands for: a container at half its icon
            // zone and the thing the chart is ABOUT at three quarters of it,
            // which is what tells an agent seat from a unit at the far end of
            // a chart. A human seat keeps the small figure inside its dashed
            // ring, which is the boundary that says what it is.
            iconSize={view.kind === "human" ? "md" : "lg"}
            name={view.name}
            caption={seatKindLabel(view)}
            captionMarks={<SeatMarks view={view} />}
            trailing={
              <>
                <LiveState api={api} view={view} compact />
                <ProblemCount api={api} nodeKey={view.key} />
              </>
            }
          />
          <VisuallyHidden>{handleLabel(view.handle)}</VisuallyHidden>
        </div>
        <ToggleButton card={card} id={id} name={view.name} />
        <NodeActions
          api={api}
          card={card}
          id={id}
          label={view.name}
          view={view}
          open={open}
          reorder={reorder}
        />
      </>
    );
  }
  if (view.type === "company") {
    const counts = `${plural(view.seats.length, "root seat")}, ${plural(view.units.length, "unit")}`;
    return (
      <>
        <div {...card.item(id)} title={counts}>
          <OrgNodeLabel
            icon={<NodeGlyph kind="company" />}
            name={view.name || "Unnamed company"}
            caption="Company"
            trailing={<ProblemCount api={api} nodeKey={id} />}
          />
          <VisuallyHidden>{counts}</VisuallyHidden>
        </div>
        <ToggleButton card={card} id={id} name={view.name || "the company"} />
        <NodeActions
          api={api}
          card={card}
          id={id}
          label={view.name || "the company"}
          view={view}
          open={open}
          reorder={reorder}
        />
      </>
    );
  }
  const counts = plural(view.seats.length, "seat");
  return (
    <>
      <div {...card.item(id)} title={counts}>
        <OrgNodeLabel
          icon={<NodeGlyph kind="unit" />}
          name={view.name}
          caption={unitTypeLabel(view)}
          captionMarks={<UnitMarks view={view} />}
          trailing={<ProblemCount api={api} nodeKey={id} />}
        />
        <VisuallyHidden>{counts}</VisuallyHidden>
        <VisuallyHidden>{leadSentence(view)}</VisuallyHidden>
      </div>
      <ToggleButton card={card} id={id} name={view.name} />
      <NodeActions
        api={api}
        card={card}
        id={id}
        label={view.name}
        view={view}
        open={open}
        reorder={reorder}
      />
      <LeadChip api={api} structure={structure} unit={view} card={card} />
    </>
  );
}

/**
 * What a seat's node says beyond its name and its kind.
 *
 * ITS HANDLE IS HERE RATHER THAN ON THE CAPTION, which says what kind of thing
 * the node is and nothing else. A node is one rank tall and as wide as its
 * name: a caption carrying the kind and the handle would be truncated on the
 * node of every seat whose name is longer than its handle. It is the node's
 * tooltip, and it is a column of the table beside this chart.
 *
 * AND IT IS A TOOLTIP RATHER THAN A SENTENCE READ AFTER THE NAME. The caption
 * above it already says what kind of seat this is, so the hidden sentence
 * carries the HANDLE alone: measured, a card announced itself as "SRE Lead,
 * Agent seat, idle, Agent seat, @sre-lead", and a screen reader heard the kind
 * twice on every seat of both charts. A `title` is not read after the caption,
 * so the whole sentence still stands where a pointer asks for it.
 */
function seatTitle(view: Pick<SeatView, "kind" | "handle">): string {
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
  const title = seatTitle(item);
  const standing = item.root
    ? "No manager."
    : item.cycleSize !== undefined
      ? `In a reporting cycle of ${plural(item.cycleSize, "seat")}.`
      : "";
  return (
    <>
      <div {...card.item(item.id)} title={title}>
        <OrgNodeLabel
          icon={<NodeGlyph kind={item.kind === "human" ? "human" : "agent"} />}
          iconRing={item.kind === "human" ? "dashed" : "none"}
          iconSize={item.kind === "human" ? "md" : "lg"}
          name={item.name}
          caption={seatKindLabel(item)}
          captionMarks={<ReportingMarks item={item} />}
          trailing={
            <>
              {seat && <LiveState api={api} view={seat} compact />}
              {seat && <ProblemCount api={api} nodeKey={seat.key} />}
            </>
          }
        />
        <VisuallyHidden>{handleLabel(item.handle)}</VisuallyHidden>
        {standing !== "" && <VisuallyHidden>{standing}</VisuallyHidden>}
      </div>
      <ToggleButton card={card} id={item.id} name={item.name} />
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
      <ToggleButton card={card} id={CYCLE_GROUP} name="the reporting cycles" />
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
 * TWO ACTIONS, where the node's whole menu has eight. Edit and Delete are what
 * the console chart puts on a node and what this chart's own keys already do
 * (Enter, Delete), so they are the pair a pointer gets without opening
 * anything, and two is also what a node one rank tall can split into cells a
 * finger can hit. Everything else (Add, Move to, Open seat, Edit reports,
 * Change kind, Move up and Move down) is one list, in `nodeActions`, and it is
 * reached three ways that are all still here: the ContextMenu key or Shift+F10
 * on the node, the toolbar, which mirrors the selected node, and the Add on
 * the branch below.
 *
 * THE EXPANDER IS NOT HERE EITHER, it is on the node's LEADING edge. In this
 * column it was a third cell, which on one rank is 16px a pointer cannot land
 * on; on the branch below, beside the Add, it was covered whenever the add
 * pill split open into its three kinds, which is the one gesture a reader
 * makes right next to it. The card's leading edge is where every hierarchy a
 * reader has used puts a disclosure, and it leaves the branch to the Add
 * alone, which is what the chart this is drawn from draws there.
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
  reorder,
}: {
  api: BuilderApi;
  card: TreeCardContext;
  id: string;
  label: string;
  view: NodeView;
  open: OpenScreen;
  reorder: Reorder;
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
      {/*
        THE MENU IS WHAT THIS CARD DOES NOT ALREADY REACH (`cardMenu`), which
        is the question the table's row asks through the same subtraction.
        Written out as `nodeMenu` itself, it opened with an Edit drawn two
        inches to its left and ended with the Delete beside it, and dropped the
        Move up and Move down the row offered: one node, two menus, on two
        views of one draft. The three ADDS stay, because this is a tree and a
        tree item may hold no tab stop: the pill on the branch is pointer-only,
        so its kinds have nowhere else a key reaches. The toolbar draws no
        control of its own and keeps the whole list.
      */}
      <KeyboardMenu card={card} id={id} label={label} items={cardMenu(api, view, open, reorder)} />
    </div>
  );
}

/**
 * The way out of fullscreen, drawn under the canvas's controls while the
 * builder is in it.
 *
 * A READER WHO CANNOT LEAVE A SCREEN IS STUCK ON IT. Fullscreen takes the
 * browser's own chrome away with it, so the only way back is a key, and the
 * builder said nothing about which: searched on the running build, no element
 * anywhere on the page matched "press esc". The chart this is drawn from draws
 * this hint under its toolbar for exactly that reason.
 *
 * READ FROM THE DOCUMENT rather than handed in, because the state is the
 * DOCUMENT's: whichever control took the builder fullscreen, and whatever
 * takes it out again (Escape, the browser's own gesture, a second press), this
 * is the one fact that says whether a reader needs the way out. The canvas
 * draws it as a `status` region, so entering fullscreen announces it too.
 */
function FullscreenHint() {
  const [active, setActive] = useState(false);
  useEffect(() => {
    // TRUTHINESS, not a comparison with null: a browser with no element in
    // fullscreen answers null and one with no support for it at all answers
    // undefined, and the hint must be absent for both.
    const onChange = () => setActive(Boolean(document.fullscreenElement));
    onChange();
    document.addEventListener("fullscreenchange", onChange);
    return () => document.removeEventListener("fullscreenchange", onChange);
  }, []);
  if (!active) return null;
  return (
    <>
      <span>Press</span>
      <Kbd keys={["Esc"]} />
      <span>to leave fullscreen</span>
    </>
  );
}

/**
 * The control that opens and closes what hangs under a node, on its leading
 * edge.
 *
 * BESIDE THE TREEITEM, NEVER INSIDE IT, which is what `OrgNodeDisclosure` is
 * for: a tree's items hold nothing focusable, so the design system draws this
 * as a sibling of the node's own item, exactly where the actions strip goes,
 * and places it on the card's boundary from there. It used to hang on the
 * BRANCH beside the Add, where the add pill splitting open into its three
 * kinds covered it, which is the one gesture a reader makes right next to it.
 *
 * A NODE WITH NOTHING UNDER IT DRAWS NONE, so a leaf of the chart is a card
 * with no disclosure rather than one with an empty slot.
 *
 * AND THE PRESS LANDS ON THE NODE, never in the control, which is why
 * `card.press` is spread on the region rather than on the button inside it:
 * it is the same rule the actions strip gets from `card.actions`, and the
 * design system asks a caller for it here because only the chart knows which
 * node this belongs to. Focus in a subtree hidden from assistive technology is
 * focus nowhere, so a press that left it on the expander would leave the chart
 * with no announced position at all.
 */
function ToggleButton({ card, id, name }: { card: TreeCardContext; id: string; name: string }) {
  if (!card.expandable(id)) return null;
  const expanded = card.expanded(id);
  return (
    <OrgNodeDisclosure {...card.press(id)}>
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
    </OrgNodeDisclosure>
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
 *
 * AND ONE PRESS TAKES IT AWAY. Clearing a lead through the menu is a press, a
 * list and a choice, for the one answer a reader is most likely to want after
 * setting the wrong one; the chart this is drawn from puts a small X inside
 * the pill for exactly that. It is quiet until the node is reached, like every
 * other control on a node, and it is named after the unit it belongs to.
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
  // THE PILL SAYS THE WORD "LEAD" ITSELF (`leadChipLabel`), as the chart this
  // is drawn from does: "Lead: VP Engineering" set, "Lead" not. The table's
  // own column keeps `leadLabel`, which says the name alone, because there the
  // word is already the column heading and the pill's wording would double it.
  //
  // THE PILL IS DRAWN EMPTY where the unit has no lead, and it still SAYS
  // which of the two nothings it is. The chart this is drawn from writes
  // "Lead" in every pill with nothing in it; this builder has a third answer
  // that chart has no idea of, because the lead a unit inherits is derived by
  // the engine: "No lead" and "Lead after the check" are different facts, and
  // a pill reading "Lead" for both would hide a check that has not answered
  // behind a unit that declares nothing.
  const none = unit.lead === null || unit.lead === undefined;
  // WHAT THE X CLEARS IS A DECLARED LEAD. A lead the engine derived from an
  // ancestor is not written on this unit, so there is nothing here to take
  // away: the control is drawn where a press would change the draft and
  // nowhere else, rather than drawn everywhere and refusing on two thirds of
  // the units in the chart.
  const declared = unit.lead !== null && unit.lead !== undefined && !unit.lead.inherited;
  return (
    <div aria-hidden="true" {...card.press(unit.key)}>
      <OrgNodeLead
        empty={none}
        {...(declared && !api.readOnly
          ? {
              clear: (
                <IconButton
                  label={`Clear the lead of ${unit.name}`}
                  icon={<CloseGlyph />}
                  size="sm"
                  variant="ghost"
                  tabIndex={-1}
                  onClick={(event) => {
                    event.stopPropagation();
                    card.activate(unit.key);
                    api.dispatch({
                      type: "record",
                      intent: { type: "setLead", target: unit.key, lead: undefined },
                    });
                  }}
                />
              ),
            }
          : {})}
      >
        {api.readOnly ? (
          <span className="truncate">{leadChipLabel(unit)}</span>
        ) : (
          <Menu
            label={`Lead of ${unit.name}`}
            items={leadMenu(api, structure, unit)}
            triggerTabIndex={-1}
            onOpenChange={(opened) => opened && card.activate(unit.key)}
            trigger={leadChipLabel(unit)}
          />
        )}
      </OrgNodeLead>
    </div>
  );
}
