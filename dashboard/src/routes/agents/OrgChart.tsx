/**
 * Agents › Org chart: the company as it is running, one node per seat.
 *
 * # One drawing with the builder
 *
 * The chart is drawn as Agents › Edit org draws a draft: the design system's
 * tree canvas in its `node` appearance, elbow connectors on the dotted field,
 * an agent seat toned and marked with the Crewlet figure, a person inside a
 * dashed ring, each unit's lead in a pill along its bottom edge, and the
 * switch between the two arrangements under the zoom bar. What a node LOOKS
 * like is shared (`ui/orgNodes.tsx`); what it is drawn FROM is not. The
 * STRUCTURE is the company, its units and the seats in each; the REPORTING
 * chart is who each seat reports to. Both are built by `lib/orgchart.ts` from
 * the org projection this node has APPLIED, never the builder's draft: between
 * a save and this node applying it the chart has not moved, and
 * [PreviousRevisionNote] says so rather than letting the chart read as a save
 * that did nothing.
 *
 * # A seat's caption is what it is doing
 *
 * Under a seat's name is the engine's word for its state behind the state's
 * dot (`lib/seats.ts`), and the legend counts the same four words. The dot is
 * the one mark on the chart that says what a seat is DOING; the tone and the
 * figure say what it IS. The word is one of a few short ones on purpose: a node
 * is as wide as its name, so a caption that grew with every push would relay
 * the whole chart twice a tool-loop round. The engine's full state line is the
 * node's title, and it is read after the node's name.
 *
 * # A node opens its peek
 *
 * A seat opens the seat beside the chart (`peek=seat:{handle}`) and a unit
 * opens the unit (`peek=unit:{name}`), which keeps the chart on screen. Enter
 * does the same, the arrows walk the tree (the kit's tree pattern), and while
 * the peek is open it follows the node that has focus. The chart stays fitted
 * and centred as the peek takes and gives back its width ([useChartView]).
 *
 * # On a phone it is rows
 *
 * Below the phone breakpoint the reporting tree is drawn as the kit's tree grid
 * ([OrgOutline]): no size of chart can be read on a phone.
 */

import {
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type MouseEvent,
  type RefObject,
} from "react";
import {
  EmptyState,
  OrgNodeLabel,
  OrgNodeLead,
  StatusDot,
  Tag,
  TreeCanvas,
  TreeGrid,
  VisuallyHidden,
  type TreeCanvasHandle,
  type TreeCanvasProps,
  type TreeCardContext,
  type TreeCardInput,
  type TreeInput,
  type TreeItemAction,
  type TreeModel,
} from "@crewlethq/ui";
import { NetworkGlyph } from "@crewlethq/icons/glyphs";
import { useFillScreen } from "~/app/fill.tsx";
import { peekHref, rowPeekHandler, usePeek, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { refToken, type ObjectRef } from "~/app/frame/objects.ts";
import { usePeekNeighbours, usePeekStep } from "~/app/frame/PeekHost.tsx";
import { usePeekWidth } from "~/app/frame/peekWidth.ts";
import { useParam } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { plural } from "~/lib/format.ts";
import { useAgents, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useMediaQuery } from "~/lib/media.ts";
import { CANVAS_PEEK_WIDTH, PHONE_BREAKPOINT } from "~/app/layout.ts";
import {
  activityOf,
  handleLabel,
  indexOrg,
  nameOfIn,
  ringOf,
  stateLine,
  stateWord,
  toneOf,
  type NameOf,
  type OrgIndex,
  type Seat,
  type Unit,
} from "~/lib/seats.ts";
import {
  COMPANY_NODE,
  buildReportingChart,
  buildStructureChart,
  placeLine,
  projectsByUnit,
  stateCounts,
  unitNodeId,
  type OrgChartModel,
  type StateCounts,
} from "~/lib/orgchart.ts";
import type { AgentRow } from "~/protocol/types.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import {
  CANVAS_LABELS,
  canvasControls,
  canvasReady,
  holdLegible,
  largestRoot,
} from "~/ui/canvasView.ts";
import {
  ChartSwitch,
  NodeGlyph,
  NodeToggle,
  chartKindOf,
  leadPill,
  leadSentenceOf,
  seatKindLabel,
  seatMark,
  seatTone,
  unitTypeLabel,
  type LeadName,
} from "~/ui/orgNodes.tsx";
import { PreviousRevisionNote } from "~/routes/org/builder/AfterSaveStrip.tsx";
import { AgentsHeader, useAgentsCounts } from "./header.tsx";

/** The project page the chart reads its unit keys from: every live project. */
const PROJECT_PAGE = 200;

/** The legend's four states, in the order a reader acts on them. */
const LEGEND: readonly (keyof StateCounts)[] = ["working", "needs", "stopped", "idle"];

export function OrgChart() {
  // BELOW A PHONE'S WIDTH THE CHART IS ROWS (see [OrgOutline]), and rows
  // scroll with the page as any list does; the canvas takes the height the
  // page leaves.
  const phone = useMediaQuery(`(width < ${PHONE_BREAKPOINT}px)`);
  useFillScreen(!phone);
  // THE PEEK RESTS AT THE CANVAS WIDTH beside a chart, as the approved one
  // draws it: every pixel it takes is one the whole company is shrunk into.
  // A phone's outline is a list, and its drawer keeps the frame's width.
  usePeekWidth(phone ? null : CANVAS_PEEK_WIDTH);
  const org = useOrg();
  const agents = useAgents();
  const now = useNow();
  const index = useMemo(() => indexOrg(org), [org]);
  const nameOf = useMemo(() => nameOfIn(index), [index]);
  useAgentsCounts(index);

  // WHICH ARRANGEMENT, chosen as the builder's is: the same section param
  // and the same switch, so a link names the same chart on either screen.
  const [chartParam, setChart] = useParam("chart", "structure", "section");
  const kind = chartKindOf(chartParam);
  const panelId = useId();

  // THE KEY ON A UNIT'S NODE is the project its work is filed under, which
  // only the tracker knows. A company on another tracker answers nothing and
  // a unit's node carries its type alone.
  const projects = useQuery("work_projects", { limit: PROJECT_PAGE }, { pollMs: 120_000 });
  const projectRows = projects.error ? undefined : projects.data?.projects;
  const keysOf = useMemo(() => projectsByUnit(projectRows ?? []), [projectRows]);
  const company = org?.name ?? "";
  const structure = useMemo(() => buildStructureChart(index, company), [index, company]);
  const reporting = useMemo(() => buildReportingChart(index), [index]);
  const chart = kind === "reporting" ? reporting : structure;
  const counts = useMemo(() => stateCounts(index, agents), [index, agents]);
  const byRole = useMemo(() => new Map(agents.map((a) => [a.role, a])), [agents]);

  const view = useRef<TreeCanvasHandle>(null);
  const peek = usePeek();
  const { open, move } = usePeekControls();
  const peeked =
    peek?.kind === "seat" ? (index.byHandle.get(peek.id) ?? index.byName.get(peek.id)) : undefined;
  const peekedUnit =
    peek?.kind === "unit" ? index.units.find((u) => u.name === peek.id) : undefined;
  // THE NODE THE PEEK IS ABOUT, where the chart on screen draws one: a unit
  // is a node of the structure only.
  const peekedUnitNode = peekedUnit ? unitNodeId(peekedUnit) : null;
  const selected =
    peeked?.key ?? (peekedUnitNode && chart.units.has(peekedUnitNode) ? peekedUnitNode : null);

  // `[` AND `]` WALK THE SEATS in the order the tree reads them.
  const order = useMemo(() => {
    const out: Seat[] = [];
    const walk = (nodes: readonly TreeInput[]) => {
      for (const n of nodes) {
        const seat = chart.seats.get(n.id);
        if (seat) out.push(seat);
        if (n.children) walk(n.children);
      }
    };
    walk(chart.nodes);
    return out;
  }, [chart]);
  usePeekNeighbours(
    useMemo(() => order.map((s) => ({ kind: "seat" as const, id: s.handle })), [order]),
  );
  const step = usePeekStep();

  // THE ROOT THE COMPANY HANGS FROM: the company on the structure, and on the
  // reporting chart the root with the most seats under it. A chart held above
  // its fit with nothing selected is anchored on it.
  const anchor = useMemo(() => largestRoot(chart.nodes), [chart]);

  // ONE PRESS, ONE NAVIGATION. A pointer press on a node reaches the peek
  // twice (the tree's selection follows the focus the press gave the node,
  // and the node's own click opens it), and both land before the address has
  // moved, so each read the peek as still showing the last node. What was
  // ASKED FOR is remembered until the address answers, and asking again for
  // the same node is nothing.
  const asked = useRef<string | null>(null);
  const peekToken = peek ? refToken(peek) : null;
  useEffect(() => {
    asked.current = peekToken;
  }, [peekToken]);
  const peekTo = useCallback(
    (ref: ObjectRef) => {
      const token = refToken(ref);
      if (asked.current === token) return;
      asked.current = token;
      if (peek) move(ref);
      else open(ref);
    },
    [peek, move, open],
  );
  // WHAT A NODE OPENS: a seat, a unit, or nothing for the company.
  const subjectOf = useCallback(
    (id: string): ObjectRef | null => {
      const seat = chart.seats.get(id);
      if (seat) return { kind: "seat", id: seat.handle };
      const unit = chart.units.get(id);
      return unit ? { kind: "unit", id: unit.name } : null;
    },
    [chart],
  );

  const cards = useCallback((model: TreeModel, expanded: ReadonlySet<string>): TreeCardInput[] => {
    const card = (id: string): TreeCardInput => ({
      id,
      children: expanded.has(id) ? (model.children.get(id) ?? []).map(card) : [],
    });
    return model.roots.map(card);
  }, []);
  const onNodeKey = useCallback(
    (id: string, action: Exclude<TreeItemAction, "menu">): boolean => {
      const subject = subjectOf(id);
      if (!subject || action !== "activate") return false;
      peekTo(subject);
      return true;
    },
    [subjectOf, peekTo],
  );
  // `[` AND `]` ON A NODE STEP THE PEEK. The tree reads every printable key a
  // node is sent as type-ahead, so the rail's own binding never heard them
  // from the chart, and pressing a node, then `]`, is the way a reader walks
  // the company. The node claims the two keys and steps the ONE stepper the
  // rail's buttons use; focus follows the peek to the next node, as the
  // selection follows focus.
  const onNodeKeyDown = useCallback(
    (_id: string, event: ReactKeyboardEvent<HTMLElement>): boolean => {
      const delta = event.key === "]" ? 1 : event.key === "[" ? -1 : 0;
      if (!delta || !step || event.altKey || event.ctrlKey || event.metaKey) return false;
      step(delta);
      return true;
    },
    [step],
  );
  const renderCard = (id: string, card: TreeCardContext) => {
    const seat = chart.seats.get(id);
    if (seat) {
      return (
        <SeatNode
          seat={seat}
          agent={byRole.get(seat.name)}
          card={card}
          now={now}
          nameOf={nameOf}
          onOpen={() => peekTo({ kind: "seat", id: seat.handle })}
        />
      );
    }
    const unit = chart.units.get(id);
    if (unit) {
      return (
        <UnitNode
          id={id}
          unit={unit}
          keys={keysOf.get(unit.name) ?? []}
          card={card}
          onOpen={() => peekTo({ kind: "unit", id: unit.name })}
        />
      );
    }
    return id === COMPANY_NODE ? (
      <CompanyNode name={structure.nodes[0]?.label ?? ""} index={index} card={card} />
    ) : null;
  };

  if (!index.seats.length) {
    return (
      <>
        <AgentsHeader index={index} />
        <EmptyState
          icon={<NetworkGlyph size={32} />}
          title="No organisation is loaded"
          description="The org chart comes from the active company configuration."
        />
      </>
    );
  }

  return (
    // THE CANVAS RUNS TO THE SHEET'S EDGES, as the approved chart's dotted
    // field does: the screen's padding and the canvas's own frame were a
    // border and forty pixels round a chart that is shrunk into whatever width
    // it is given. A phone's outline is a list and keeps the page's padding.
    <div className="oc" data-shape={phone ? "outline" : "canvas"}>
      <AgentsHeader index={index} onFound={(seat) => view.current?.focusNode(seat.key)} />
      {/* A SAVE IS NOT AN APPLY: until this node applies the revision the
          builder saved, the projection the chart draws is the previous one. */}
      <PreviousRevisionNote />
      {phone ? (
        <OrgOutline
          chart={reporting}
          byRole={byRole}
          counts={counts}
          now={now}
          nameOf={nameOf}
          peeked={peeked}
          following={peek?.kind === "seat"}
          onOpen={(seat) => peekTo({ kind: "seat", id: seat.handle })}
        />
      ) : (
        <ChartCanvas
          // ONE CANVAS PER ARRANGEMENT, as the builder mounts one per chart:
          // the two draw different trees, so each is fitted and held legible
          // from its own first layout rather than inheriting the other's view.
          key={kind}
          view={view}
          panelId={panelId}
          selected={selected}
          anchor={anchor}
          className="oc-canvas"
          label={kind === "reporting" ? "Reporting chart" : "Structure chart"}
          labels={CANVAS_LABELS}
          // THE BUILDER'S DRAWING (`ui/orgNodes.tsx`): nodes as wide as their
          // names on the dotted field, joined by the design system's elbows.
          appearance="node"
          ground="dotted"
          connector="elbow"
          // AN AGENT SEAT IS TONED, as the builder draws one, and every other
          // node keeps the chart's neutral surface.
          cardTone={(id) => {
            const seat = chart.seats.get(id);
            return seat ? seatTone(seat.kind, seat.avatar) : undefined;
          }}
          nodes={chart.nodes}
          cards={cards}
          cardOf={(id) => id}
          onSelect={(id) => {
            // THE PEEK FOLLOWS FOCUS while it is open, so the arrows walk the
            // company with the node beside it; with it shut, focus is focus.
            const subject = subjectOf(id);
            if (!peek || (peek.kind !== "seat" && peek.kind !== "unit") || !subject) return;
            if (refToken(subject) !== peekToken) peekTo(subject);
          }}
          onNodeKey={onNodeKey}
          onNodeKeyDown={onNodeKeyDown}
          // THE SWITCH UNDER THE ZOOM BAR, in the chart's own corner, where
          // the builder keeps it.
          controlsBelow={<ChartSwitch value={kind} onValueChange={setChart} panelId={panelId} />}
          overlay={<OrgLegend counts={counts} />}
          renderCard={renderCard}
        />
      )}
    </div>
  );
}

/**
 * The canvas one arrangement is drawn on, with the view rules of
 * [useChartView] held for as long as that arrangement is on screen.
 */
function ChartCanvas({
  view,
  panelId,
  selected,
  anchor,
  ...canvas
}: {
  view: RefObject<TreeCanvasHandle | null>;
  /** The element the chart switch controls. */
  panelId: string;
  /** The node the peek is about, or `null`. */
  selected: string | null;
  anchor: string | null;
} & Omit<TreeCanvasProps, "ref" | "selectedId">) {
  // A STATE RATHER THAN A REF: the chart is not drawn until the org has
  // loaded, and what watches its box has to start when it is.
  const [host, setHost] = useState<HTMLDivElement | null>(null);
  const chartBox = useChartView(host, view, selected, anchor);
  return (
    <div className="oc-chart" id={panelId} ref={setHost} {...chartBox}>
      <TreeCanvas ref={view} selectedId={selected} {...canvas} />
    </div>
  );
}

/**
 * The chart below a phone's width: the same tree, as ROWS.
 *
 * # Why a phone does not get the canvas
 *
 * A company fitted into 360 pixels is drawn at a third of its size, where no
 * card can be read; eased onto its root instead, the root sat in the middle of
 * the canvas under an empty band, still at barely half size, and every other
 * seat was a pan away. There is no size at which a chart of cards is read on a
 * phone. The kit's own answer for a narrow screen is its tree grid — "the view
 * a keyboard or a screen reader user works fastest in, and the one a narrow
 * screen falls back to" — so a phone reads the chart as that: every seat a
 * row, indented under its manager in the same order the canvas draws, with the
 * same badge, ring, name, kind, place and state line a card carries, at full
 * size. The legend heads it.
 *
 * THE SAME TREE AND THE SAME PEEK. The rows are the chart's own nodes, so a
 * phone and a desktop can never disagree about who reports to whom; a row's
 * name opens the seat's peek, Enter on a row does the same, and while the
 * peek is open it follows the row that has focus, as it follows a card.
 */
function OrgOutline({
  chart,
  byRole,
  counts,
  now,
  nameOf,
  peeked,
  following,
  onOpen,
}: {
  chart: OrgChartModel;
  byRole: ReadonlyMap<string, AgentRow>;
  counts: StateCounts;
  now: number;
  nameOf: NameOf;
  peeked: Seat | undefined;
  /** The peek is open on a seat, so focus moving to a row moves it too. */
  following: boolean;
  onOpen: (seat: Seat) => void;
}) {
  const columns = useMemo(() => [{ key: "seat", header: "Seat" }], []);
  return (
    <div className="oc-outline">
      <OrgLegend counts={counts} inline />
      <TreeGrid
        label="Org chart"
        columns={columns}
        rows={chart.nodes}
        readOnly
        selectedId={peeked?.key ?? null}
        onSelect={(id) => {
          const seat = chart.seats.get(id);
          if (following && seat && seat !== peeked) onOpen(seat);
        }}
        onRowKey={(id, action) => {
          const seat = chart.seats.get(id);
          if (!seat || action !== "activate") return false;
          onOpen(seat);
          return true;
        }}
        cellHasControl={() => true}
        renderCell={(id, column, grid) => {
          const seat = chart.seats.get(id);
          return seat ? (
            <SeatRowCell
              seat={seat}
              agent={byRole.get(seat.name)}
              now={now}
              nameOf={nameOf}
              tabIndex={grid.tabStop(id, column) ? 0 : -1}
              onOpen={() => onOpen(seat)}
            />
          ) : null;
        }}
      />
    </div>
  );
}

/**
 * One seat as an outline row: what its card says, on two lines — the badge,
 * the name (the link that opens the seat) and the kind, then where it sits and
 * the engine's state line.
 */
function SeatRowCell({
  seat,
  agent,
  now,
  nameOf,
  tabIndex,
  onOpen,
}: {
  seat: Seat;
  agent: AgentRow | undefined;
  now: number;
  nameOf: NameOf;
  tabIndex: number;
  onOpen: () => void;
}) {
  const human = seat.kind === "human";
  const state = human ? undefined : activityOf(agent);
  const ring = human ? undefined : ringOf(state);
  const ref = { kind: "seat" as const, id: seat.handle };
  return (
    <span className="oc-row" data-state={state}>
      <SeatAvatar
        name={seat.name}
        size="sm"
        kind={human ? "human" : "agent"}
        avatar={seat.avatar}
        {...(ring ? { ring } : {})}
        decorative
      />
      <span className="oc-row-text">
        <span className="oc-head">
          <a
            className="oc-name"
            href={peekHref(ref)}
            tabIndex={tabIndex}
            onClick={rowPeekHandler(onOpen)}
          >
            {seat.name}
          </a>
          <Tag size="xs" appearance="outline">
            {human ? "Human" : "Agent"}
          </Tag>
        </span>
        <span className="oc-place">
          {placeLine(seat)}
          <span className="sr-only"> {handleLabel(seat.handle)}</span>
        </span>
        <span className="oc-state">
          <StatusDot tone={human ? "neutral" : toneOf(state)} pulse={state === "working"} />
          <span className="oc-line">{cardLine(seat, agent, now, nameOf)}</span>
        </span>
      </span>
    </span>
  );
}

/**
 * The canvas under a chart whose frame changes: the whole chart stays fitted
 * and centred in whatever width the canvas has — never below [LEGIBLE_ZOOM] —
 * and the seat the peek is about stays on screen.
 *
 * # A new width is fitted again
 *
 * The canvas fits the chart ONCE, on its first layout, and never moves a
 * reader's view on its own after that (the kit's rule, so a push never yanks
 * the chart away). But the canvas's WIDTH moves under the chart whenever the
 * peek opens or closes or the window is resized, and the view fitted to the
 * old width then describes nothing: opening a peek left the chart cut at one
 * side, and closing it left the chart cut there with a third of the canvas
 * empty on the other. So whenever the canvas's width changes after that first
 * fit, the chart is fitted again, as the approved chart draws it beside a
 * peek: all of it, in the middle. A change of HEIGHT alone is not one, so a
 * browser bar sliding away moves nothing.
 *
 * # But never shrunk past reading
 *
 * Every view this screen chooses — the canvas's own first fit and every refit
 * after it — is drawn at exactly [LEGIBLE_ZOOM] where the fit falls below it,
 * centred across on the selected seat (or, with none, the root the company
 * hangs from) and down the canvas where the kit's own fit would put the chart
 * (`holdLegible`). The chart past the edge is a pan away; a chart drawn whole
 * at half size was a chart nobody could read any of.
 *
 * # A reader's own view is theirs
 *
 * Only a chart still at the view this screen last chose is fitted again. A
 * reader who zoomed in to read the cards and then pressed one would otherwise
 * be thrown back out by the peek they opened; a view the reader moved stays
 * theirs, and the pressed card is revealed in it. The reader's own Fit (the
 * control or the canvas's `0` key) is remembered as a chosen view too, so
 * pressing it hands the chart back to this rule.
 *
 * # And the selected card is revealed
 *
 * Whenever the selected seat changes — pressed, found, or stepped to with `[`
 * and `]` — and after every refit, its card is brought into view: the least
 * pan that shows it, at the view's zoom, which is the kit's reveal.
 *
 * THE KIT REVEALS BY FOCUSING. `focusNode` is the handle's one reveal, and it
 * moves focus to the card as well. A reader already in the chart is where the
 * focus belongs; a reader in the peek (stepping with its buttons) or anywhere
 * else keeps the focus they had, or the next key they press would be read by
 * the tree's type-ahead instead of by what they were using.
 */
function useChartView(
  host: HTMLDivElement | null,
  view: RefObject<TreeCanvasHandle | null>,
  selected: string | null,
  anchor: string | null,
): {
  onClick: (event: MouseEvent<HTMLElement>) => void;
  onKeyDown: (event: ReactKeyboardEvent<HTMLElement>) => void;
} {
  const [width, setWidth] = useState(0);
  const [ready, setReady] = useState(false);
  // THE VIEW THIS SCREEN LAST CHOSE — the canvas's panned layer's transform —
  // and whether it was still the view on screen when the canvas's width last
  // moved: see "A reader's own view is theirs" above.
  const fitted = useRef<string | null>(null);
  const untouched = useRef(true);
  // Set while this screen presses the canvas's controls itself, so its own
  // presses are not mistaken for the reader's.
  const pressing = useRef(false);
  useEffect(() => {
    const el = host;
    if (!el) return;
    // The one element under this chart that says whether it is ready is the
    // kit's canvas, inside the TreeCanvas this screen draws.
    setReady(canvasReady(el));
    const watch = new MutationObserver(() => setReady(canvasReady(el)));
    watch.observe(el, { attributes: true, attributeFilter: ["data-ready"], subtree: true });
    const size = new ResizeObserver((entries) => {
      // READ BEFORE THE CANVAS ANSWERS THE NEW SIZE: its answer clamps the
      // pan, and a view the canvas clamped is not one the reader moved.
      untouched.current =
        fitted.current === null || canvasControls(el, pressing).view() === fitted.current;
      const rect = entries[entries.length - 1]?.contentRect;
      if (rect) setWidth(Math.round(rect.width));
    });
    size.observe(el);
    return () => {
      watch.disconnect();
      size.disconnect();
    };
  }, [host]);

  // THE VIEW ON SCREEN, remembered as chosen — once the canvas has drawn it.
  const remember = useCallback(() => {
    const el = host;
    if (el) requestAnimationFrame(() => (fitted.current = canvasControls(el, pressing).view()));
  }, [host]);
  // THE READER'S OWN FIT — the Fit control and the canvas's `0` key — heard as
  // the press and the key bubble out of the canvas to the chart's own box, so
  // nothing here binds a key: the canvas's `0` is the canvas's, and this only
  // notices it was pressed.
  const handlers = useMemo(
    () => ({
      onClick: (event: MouseEvent<HTMLElement>) => {
        if (pressing.current) return;
        if ((event.target as Element).closest(`button[aria-label="${CANVAS_LABELS.fit}"]`))
          remember();
      },
      onKeyDown: (event: ReactKeyboardEvent<HTMLElement>) => {
        if (event.key === "0" && !(event.target as Element).closest("input, textarea")) {
          remember();
        }
      },
    }),
    [remember],
  );

  const reveal = useCallback(
    (id: string) => {
      const el = host;
      if (!el) return;
      const had = document.activeElement;
      view.current?.focusNode(id);
      if (had instanceof HTMLElement && had !== document.body && !el.contains(had)) {
        had.focus({ preventScroll: true });
      } else if (!(had instanceof HTMLElement) || had === document.body) {
        (document.activeElement as HTMLElement | null)?.blur();
      }
    },
    [host, view],
  );

  const latest = useRef(selected);
  latest.current = selected;
  const root = useRef(anchor);
  root.current = anchor;

  /**
   * Choose the view: the fit (pressed, when `refit`; the canvas's own, on its
   * first layout), held at [LEGIBLE_ZOOM] centred across on the selected seat
   * — or on the root, where nobody is selected — and the selected seat
   * revealed. Remembered as chosen once the canvas has drawn it.
   */
  const place = useCallback(
    (refit: boolean) => {
      const el = host;
      if (!el) return;
      const controls = canvasControls(el, pressing);
      if (refit) controls.fit();
      const chosen = latest.current;
      const hold = holdLegible(controls, chosen ?? root.current);
      // A HELD VIEW THAT COULD NOT PLACE ITS ANCHOR was zoomed about the
      // centre, and the root is revealed in it; a selected seat is revealed
      // whatever the hold did — the least pan, which is none once placed.
      const target = chosen ?? (hold === "centred" ? root.current : null);
      if (target) reveal(target);
      remember();
    },
    [host, reveal, remember],
  );

  // THE CANVAS'S OWN FIRST FIT, held at the floor where it fell below it — on
  // the next frame, so the canvas has drawn the fit it says it is ready with.
  const placed = useRef(false);
  useEffect(() => {
    if (!ready || !host || placed.current) return;
    placed.current = true;
    const frame = requestAnimationFrame(() => place(false));
    return () => cancelAnimationFrame(frame);
  }, [host, ready, place]);

  useEffect(() => {
    if (ready && selected) reveal(selected);
  }, [ready, selected, reveal]);

  // THE WIDTH THE CHART WAS LAST LAID INTO: the canvas's own first fit, then
  // every width change after it.
  const fittedAt = useRef<number | null>(null);
  useEffect(() => {
    if (!ready || !host || width <= 0) return;
    if (fittedAt.current === null) {
      fittedAt.current = width;
      return;
    }
    if (width === fittedAt.current) return;
    fittedAt.current = width;
    // ON THE NEXT FRAME, so the canvas has read its new size before it fits.
    const frame = requestAnimationFrame(() => {
      if (untouched.current) place(true);
      else if (latest.current) reveal(latest.current);
    });
    return () => cancelAnimationFrame(frame);
  }, [host, ready, width, place, reveal]);

  return handlers;
}

/**
 * One seat's node: its mark and its name, then what it is doing. A person's
 * node says it is a person, as the builder's does.
 */
function SeatNode({
  seat,
  agent,
  card,
  now,
  nameOf,
  onOpen,
}: {
  seat: Seat;
  agent: AgentRow | undefined;
  card: TreeCardContext;
  now: number;
  nameOf: NameOf;
  onOpen: () => void;
}) {
  const human = seat.kind === "human";
  const state = human ? undefined : activityOf(agent);
  const line = cardLine(seat, agent, now, nameOf);
  const handle = handleLabel(seat.handle);
  const item = card.item(seat.key);
  return (
    <>
      <div
        {...item}
        data-state={state}
        title={[line, handle].filter(Boolean).join(" · ")}
        onClick={(event: MouseEvent<HTMLElement>) => {
          item.onClick(event);
          onOpen();
        }}
      >
        <OrgNodeLabel
          {...seatMark(seat.kind, seat.avatar)}
          name={seat.name}
          caption={
            state === undefined ? (
              seatKindLabel(seat)
            ) : (
              <StatusDot tone={toneOf(state)} pulse={state === "working"}>
                {stateWord(state)}
              </StatusDot>
            )
          }
        />
        <VisuallyHidden>{[handle, line].filter(Boolean).join(". ")}</VisuallyHidden>
      </div>
      <NodeToggle card={card} id={seat.key} name={seat.name} />
    </>
  );
}

/**
 * One unit's node: its mark, its name, its type and the project its work is
 * filed under, with its lead in a pill along the bottom edge.
 *
 * THE LEAD STAYS DRAWN, as it does on the builder's node: who leads a unit is
 * a fact about the organization, and a chart that hid it would be a chart you
 * could not read the leads off. The press on the pill lands on the unit's own
 * node.
 */
function UnitNode({
  id,
  unit,
  keys,
  card,
  onOpen,
}: {
  id: string;
  unit: Unit;
  /** The tracker projects this unit's work is filed under, by key. */
  keys: readonly string[];
  card: TreeCardContext;
  onOpen: () => void;
}) {
  const lead: LeadName | null = unit.effectiveLead
    ? { name: unit.effectiveLead.name, inherited: unit.leadInherited }
    : null;
  const key = keys.length ? `${keys[0]}${keys.length > 1 ? ` +${keys.length - 1}` : ""}` : "";
  const caption = [unitTypeLabel({ unitType: unit.type }), key].filter(Boolean).join(" · ");
  const seats = plural(unit.seats.length, "seat");
  const item = card.item(id);
  return (
    <>
      <div
        {...item}
        title={keys.length ? `${seats}. Projects: ${keys.join(", ")}` : seats}
        onClick={(event: MouseEvent<HTMLElement>) => {
          item.onClick(event);
          onOpen();
        }}
      >
        <OrgNodeLabel icon={<NodeGlyph kind="unit" />} name={unit.name} caption={caption} />
        <VisuallyHidden>{seats}</VisuallyHidden>
        <VisuallyHidden>{leadSentenceOf(lead)}</VisuallyHidden>
      </div>
      <NodeToggle card={card} id={id} name={unit.name} />
      <div aria-hidden="true" {...card.press(id)}>
        <OrgNodeLead empty={lead === null}>
          <span className="truncate">{leadPill(lead)}</span>
        </OrgNodeLead>
      </div>
    </>
  );
}

/** The structure's root: the company, with how many seats and units hang off it. */
function CompanyNode({
  name,
  index,
  card,
}: {
  name: string;
  index: OrgIndex;
  card: TreeCardContext;
}) {
  const counts = `${plural(index.rootSeats.length, "root seat")}, ${plural(index.topUnits.length, "unit")}`;
  return (
    <>
      <div {...card.item(COMPANY_NODE)} title={counts}>
        <OrgNodeLabel icon={<NodeGlyph kind="company" />} name={name} caption="Company" />
        <VisuallyHidden>{counts}</VisuallyHidden>
      </div>
      <NodeToggle card={card} id={COMPANY_NODE} name={name} />
    </>
  );
}

/**
 * The state line a card carries: the engine's, or for a person the one thing
 * a card has room to say — that it is a person, and when they are around.
 * The peek and the profile say the rest.
 */
export function cardLine(
  seat: Seat,
  agent: AgentRow | undefined,
  now: number,
  nameOf: NameOf,
): string {
  if (seat.kind === "human") return seat.availability ? `Human · ${seat.availability}` : "Human";
  return stateLine(agent, { now, seat, nameOf });
}

/**
 * How many seats are in each state: the engine's words, counted. Over the
 * canvas's corner on a wide screen; `inline` at the head of a phone's outline,
 * where there is no canvas to float over.
 */
export function OrgLegend({ counts, inline = false }: { counts: StateCounts; inline?: boolean }) {
  return (
    <div
      className="oc-legend"
      data-inline={inline ? "" : undefined}
      role="group"
      aria-label="Seats by what they are doing"
    >
      {LEGEND.map((state) => (
        <span key={state} className="oc-legend-item">
          <StatusDot tone={toneOf(state)} />
          {stateWord(state)} <span className="t-num">{counts[state]}</span>
        </span>
      ))}
    </div>
  );
}
