/**
 * Agents › Org chart: the company as it is running, one card per seat.
 *
 * # What the chart draws, and from what
 *
 * The tree is who reports to whom — each card under its primary manager — with
 * a box round the seats of a unit under the lead they report to, and the
 * unit's project key on the box. It is built by `lib/orgchart.ts` from the org
 * projection this node has APPLIED, never the builder's draft: between a save
 * and this node applying it the chart has not moved, and [PreviousRevisionNote]
 * says so rather than letting the chart read as a save that did nothing.
 *
 * # Colour is what a seat is doing, never who it is
 *
 * The one hue on a card is its STATE — the ring round the badge and the dot
 * before the state line — and the words come from the engine's own
 * vocabulary (`activity`, `lib/seats.ts`). Cards carry no hue of their own:
 * a colour per seat reads as a state, and a reader scanning for the amber
 * one that needs them would find a seat that is merely called something.
 * The legend counts the same four words.
 *
 * # A card opens the seat's peek
 *
 * The chart is where a reader looks at the company; a card opens the seat
 * beside it (`peek=seat:{handle}`), which keeps the chart on screen. Enter
 * does the same, the arrows walk the tree (the kit's tree pattern), and while
 * the peek is open it follows the card that has focus. The chart stays fitted
 * and centred as the peek takes and gives back its width ([useChartView]).
 *
 * # On a phone it is rows
 *
 * Below the phone breakpoint the same tree is drawn as the kit's tree grid
 * ([OrgOutline]): no size of card chart can be read on a phone.
 */

import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type MouseEvent,
  type RefObject,
} from "react";
import { flushSync } from "react-dom";
import {
  Callout,
  EmptyState,
  StatusDot,
  Tag,
  TreeCanvas,
  TreeGrid,
  type TreeCanvasGroup,
  type TreeCanvasHandle,
  type TreeCardContext,
  type TreeCardInput,
  type TreeItemAction,
  type TreeModel,
} from "@crewlethq/ui";
import { NetworkGlyph } from "@crewlethq/icons/glyphs";
import { useFillScreen } from "~/app/fill.tsx";
import { peekHref, rowPeekHandler, usePeek, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours, usePeekStep } from "~/app/frame/PeekHost.tsx";
import { usePeekWidth } from "~/app/frame/peekWidth.ts";
import { useNow } from "~/lib/clock.ts";
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
  toneOf,
  type NameOf,
  type Seat,
} from "~/lib/seats.ts";
import { buildOrgChart, placeLine, stateCounts, type StateCounts } from "~/lib/orgchart.ts";
import type { AgentRow } from "~/protocol/types.ts";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { PreviousRevisionNote } from "~/routes/org/builder/AfterSaveStrip.tsx";
import { AgentsHeader, useAgentsCounts } from "./header.tsx";

/** The project page the chart reads its unit keys from: every live project. */
const PROJECT_PAGE = 200;

/** The legend's four words, in the order a reader acts on them. */
const LEGEND: readonly { state: keyof StateCounts; label: string }[] = [
  { state: "working", label: "Working" },
  { state: "needs", label: "Needs you" },
  { state: "stopped", label: "Stopped" },
  { state: "idle", label: "Idle" },
];

export function OrgChart() {
  // BELOW A PHONE'S WIDTH THE CHART IS ROWS — see [OrgOutline] — and rows
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

  // THE KEY ON A UNIT'S BOX is the project its work is filed under, which
  // only the tracker knows. A company on another tracker answers nothing and
  // the boxes carry their names alone.
  const projects = useQuery("work_projects", { limit: PROJECT_PAGE }, { pollMs: 120_000 });
  const projectRows = projects.error ? undefined : projects.data?.projects;
  const chart = useMemo(() => buildOrgChart(index, projectRows ?? []), [index, projectRows]);
  const counts = useMemo(() => stateCounts(index, agents), [index, agents]);
  const byRole = useMemo(() => new Map(agents.map((a) => [a.role, a])), [agents]);

  const view = useRef<TreeCanvasHandle>(null);
  const peek = usePeek();
  const { open, move } = usePeekControls();
  const peeked =
    peek?.kind === "seat" ? (index.byHandle.get(peek.id) ?? index.byName.get(peek.id)) : undefined;

  // `[` AND `]` WALK THE SEATS in the order the tree reads them.
  const order = useMemo(() => {
    const out: Seat[] = [];
    const walk = (nodes: readonly { id: string; children?: readonly unknown[] }[]) => {
      for (const n of nodes) {
        const seat = chart.seats.get(n.id);
        if (seat) out.push(seat);
        if (n.children) walk(n.children as { id: string }[]);
      }
    };
    walk(chart.nodes);
    return out;
  }, [chart]);
  usePeekNeighbours(
    useMemo(() => order.map((s) => ({ kind: "seat" as const, id: s.handle || s.name })), [order]),
  );
  const step = usePeekStep();

  // THE ROOT THE COMPANY HANGS FROM — the root with the most seats under it —
  // which a chart held above its fit with nobody selected is revealed at.
  const anchor = useMemo(() => {
    const size = (n: { children?: readonly unknown[] }): number =>
      1 + (n.children ?? []).reduce<number>((sum, c) => sum + size(c as typeof n), 0);
    let best: { id: string; seats: number } | null = null;
    for (const n of chart.nodes) {
      const seats = size(n);
      if (!best || seats > best.seats) best = { id: n.id, seats };
    }
    return best?.id ?? null;
  }, [chart]);

  // ONE PRESS, ONE NAVIGATION. A pointer press on a card reaches the peek
  // twice — the tree's selection follows the focus the press gave the card,
  // and the card's own click opens it — and both land before the address has
  // moved, so each read the peek as still showing the last seat. What was
  // ASKED FOR is remembered until the address answers, and asking again for
  // the same seat is nothing.
  const asked = useRef<string | null>(null);
  const peekId = peek?.kind === "seat" ? peek.id : null;
  useEffect(() => {
    asked.current = peekId;
  }, [peekId]);
  const peekSeat = useCallback(
    (seat: Seat) => {
      const id = seat.handle || seat.name;
      if (asked.current === id) return;
      asked.current = id;
      const ref = { kind: "seat" as const, id };
      if (peek) move(ref);
      else open(ref);
    },
    [peek, move, open],
  );

  // A STATE RATHER THAN A REF: the chart is not drawn until the org has
  // loaded, and what watches its box has to start when it is.
  const [host, setHost] = useState<HTMLDivElement | null>(null);
  const chartBox = useChartView(host, view, peeked?.key ?? null, anchor);

  const cards = useCallback((model: TreeModel, expanded: ReadonlySet<string>): TreeCardInput[] => {
    const card = (id: string): TreeCardInput => ({
      id,
      children: expanded.has(id) ? (model.children.get(id) ?? []).map(card) : [],
    });
    return model.roots.map(card);
  }, []);
  const groups = useMemo<TreeCanvasGroup[]>(
    () =>
      chart.groups.map((g) => ({
        id: g.id,
        label: g.label,
        memberIds: g.memberIds,
        ...(g.projectKeys.length
          ? {
              lead: (
                <span className="oc-key" title={g.projectKeys.join(", ")}>
                  {g.projectKeys[0]}
                  {g.projectKeys.length > 1 ? ` +${g.projectKeys.length - 1}` : ""}
                </span>
              ),
            }
          : {}),
      })),
    [chart.groups],
  );
  const onNodeKey = useCallback(
    (id: string, action: Exclude<TreeItemAction, "menu">): boolean => {
      const seat = chart.seats.get(id);
      if (!seat || action !== "activate") return false;
      peekSeat(seat);
      return true;
    },
    [chart.seats, peekSeat],
  );
  // `[` AND `]` ON A CARD STEP THE PEEK. The tree reads every printable key a
  // card is sent as type-ahead, so the rail's own binding never heard them
  // from the chart — and pressing a card, then `]`, is the way a reader walks
  // the company. The card claims the two keys and steps the ONE stepper the
  // rail's buttons use; focus follows the peek to the next card, as the
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
      {/* A HIERARCHY NOBODY DERIVED IS NOT A HIERARCHY. Without the engine's
          `derived` block nobody reports to anybody as far as this client can
          say, so every seat is a root and no unit has a lead to box it under. */}
      {!index.hierarchy && (
        <Callout variant="info">
          This engine did not report its derived hierarchy, so the chart cannot say who reports to
          whom: every seat is drawn on its own, and no unit is boxed under its lead.
        </Callout>
      )}
      {phone ? (
        <OrgOutline
          chart={chart}
          byRole={byRole}
          counts={counts}
          now={now}
          nameOf={nameOf}
          peeked={peeked}
          following={peek?.kind === "seat"}
          onOpen={peekSeat}
        />
      ) : (
        <div className="oc-chart" ref={setHost} {...chartBox}>
          <TreeCanvas
            className="oc-canvas"
            ref={view}
            label="Org chart"
            labels={{ fit: FIT_LABEL, zoomIn: ZOOM_IN_LABEL }}
            connector="elbow"
            nodes={chart.nodes}
            cards={cards}
            cardOf={(id) => id}
            groups={groups}
            selectedId={peeked?.key ?? null}
            onSelect={(id) => {
              // THE PEEK FOLLOWS FOCUS while it is open, so the arrows walk the
              // company with the seat beside it; with it shut, focus is focus.
              const seat = chart.seats.get(id);
              if (peek?.kind === "seat" && seat && seat !== peeked) peekSeat(seat);
            }}
            onNodeKey={onNodeKey}
            onNodeKeyDown={onNodeKeyDown}
            controlsPlacement="bottom-right"
            overlay={<OrgLegend counts={counts} />}
            renderCard={(id, card) => {
              const seat = chart.seats.get(id);
              return seat ? (
                <SeatCardNode
                  seat={seat}
                  agent={byRole.get(seat.name)}
                  card={card}
                  now={now}
                  nameOf={nameOf}
                  onOpen={() => peekSeat(seat)}
                />
              ) : null;
            }}
          />
        </div>
      )}
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
  chart: ReturnType<typeof buildOrgChart>;
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
  const ref = { kind: "seat" as const, id: seat.handle || seat.name };
  return (
    <span className="oc-row" data-state={state}>
      <SeatAvatar
        name={seat.name}
        size="sm"
        kind={human ? "human" : "agent"}
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
          {seat.handle && <span className="sr-only"> {handleLabel(seat.handle)}</span>}
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
 * The labels this screen gives the canvas's own Fit and Zoom in controls — and
 * so how it finds them to press (see [canvasControls]).
 */
const FIT_LABEL = "Fit to view";
const ZOOM_IN_LABEL = "Zoom in";

/**
 * The smallest zoom this screen draws the chart at ON ITS OWN.
 *
 * A card's place and state lines are `--font-size-xs`, 12px, the smallest
 * text on it a reader is meant to read; at 0.85 they are drawn at 10.2px,
 * and below it they stop being text. Fitting the whole company into whatever
 * width the peek left used to go as far as the fit went: 71% beside a peek at
 * 1440 and 54% at 1280, where the state lines were seven pixels and the
 * selected card's outline barely a line. A chart that fits above this is
 * fitted whole; one that would not is drawn AT this, with the seat the reader
 * is on revealed in it. Only a Fit the READER presses goes below it — that is
 * a request for the whole company, legible or not.
 */
export const LEGIBLE_ZOOM = 0.85;

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
 * after it — is held at [LEGIBLE_ZOOM] where the fit falls below it, and the
 * selected seat (or, with none, the root the company hangs from) is revealed
 * at that size. The chart past the edge is a pan away; a chart drawn whole at
 * half size was a chart nobody could read any of.
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
    const canvasReady = () =>
      el.querySelector("[data-ready]")?.getAttribute("data-ready") === "true";
    setReady(canvasReady());
    const watch = new MutationObserver(() => setReady(canvasReady()));
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
        if ((event.target as Element).closest(`button[aria-label="${FIT_LABEL}"]`)) remember();
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
   * first layout), held at [LEGIBLE_ZOOM], with the selected seat revealed —
   * or the root, where the chart was held above its fit and nobody is
   * selected. Remembered as chosen once the canvas has drawn it.
   */
  const place = useCallback(
    (refit: boolean) => {
      const el = host;
      if (!el) return;
      const controls = canvasControls(el, pressing);
      if (refit) controls.fit();
      const held = holdLegible(controls);
      const target = latest.current ?? (held ? root.current : null);
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
 * Raise the view to [LEGIBLE_ZOOM] where it is below it, a zoom step at a
 * time — so it comes to rest at the first step at or above the floor, never
 * more than one step over it. True when it had to.
 */
function holdLegible(controls: CanvasControls): boolean {
  let k = controls.zoom();
  let held = false;
  while (k !== null && k < LEGIBLE_ZOOM) {
    controls.zoomIn();
    const next = controls.zoom();
    // The canvas's ceiling: a step that moves nothing ends the climb.
    if (next === null || next <= k) break;
    k = next;
    held = true;
  }
  return held;
}

/** What [canvasControls] offers. */
interface CanvasControls {
  /** The view as the canvas draws it: the transform of the layer it pans. */
  view(): string | null;
  /** The zoom that view is drawn at, or null before there is one. */
  zoom(): number | null;
  /** Press Fit. */
  fit(): void;
  /** Press Zoom in: one step, about the canvas's centre. */
  zoomIn(): void;
}

/**
 * The canvas's own controls, PRESSED, and its view as it draws it — what this
 * screen uses in place of the handle methods the kit's tree canvas does not
 * hand out.
 *
 * THE KIT'S TREE CANVAS OFFERS NO FIT AND NO ZOOM. Its canvas has both on its
 * handle; the tree canvas wraps that canvas and passes on only `focusNode`,
 * `focusRegion` and `restoreView`, and it passes out no ready or view-change
 * event either. So the view is read the way a reader meets it — the viewport
 * is the group announced as a canvas, and the layer it moves is its one child
 * — and the fit and the zoom are the controls the reader has, found by the
 * labels this screen gives them, so there is still exactly one answer to
 * "fitted" and one zoom step: the kit's. A press is flushed, so the view it
 * moved to can be read before the next press decides anything.
 *
 * ALL OF IT IS HERE AND NOWHERE ELSE, so a kit that hands out `fit()`, a zoom
 * and a view-change event is a change to this function and nothing else.
 */
function canvasControls(host: HTMLElement, pressing: { current: boolean }): CanvasControls {
  const layer = () =>
    host.querySelector('[role="group"][aria-roledescription="canvas"]')
      ?.firstElementChild as HTMLElement | null;
  const press = (label: string) => {
    const button = host.querySelector<HTMLButtonElement>(`button[aria-label="${label}"]`);
    if (!button) return;
    pressing.current = true;
    try {
      flushSync(() => button.click());
    } finally {
      pressing.current = false;
    }
  };
  return {
    view: () => layer()?.style.transform ?? null,
    zoom: () => {
      const scale = /scale\(([\d.]+)\)/.exec(layer()?.style.transform ?? "");
      return scale ? Number(scale[1]) : null;
    },
    fit: () => press(FIT_LABEL),
    zoomIn: () => press(ZOOM_IN_LABEL),
  };
}

/**
 * One seat's card: its badge with the state ring and its name, then its kind
 * and where it sits, then the engine's state line.
 */
function SeatCardNode({
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
  const ring = human ? undefined : ringOf(state);
  const item = card.item(seat.key);
  return (
    <div
      {...item}
      className="oc-card"
      data-state={state}
      onClick={(event: MouseEvent<HTMLElement>) => {
        item.onClick(event);
        onOpen();
      }}
    >
      <div className="oc-head">
        <SeatAvatar
          name={seat.name}
          size="sm"
          kind={human ? "human" : "agent"}
          {...(ring ? { ring } : {})}
          decorative
        />
        {/* THE NAME HAS THE LINE TO ITSELF. Beside the kind pill, in the
            approved card's 184px, a name had about sixty pixels and every
            "Agent …" seat read "Agent S…", "Agent C…": the one word that tells
            two seats apart was the word cut. The pill leads the line under it,
            where the unit is the text that gives way. A name longer than the
            card is still cut, and said whole on hover as it is to a screen
            reader (the card's text is its name). */}
        <strong className="oc-name" title={seat.name}>
          {seat.name}
        </strong>
      </div>
      <span className="oc-place">
        <span className="oc-kind">
          <Tag size="xs" appearance="outline">
            {human ? "Human" : "Agent"}
          </Tag>
        </span>
        <span className="oc-place-text">{placeLine(seat)}</span>
        {seat.handle && <span className="sr-only"> {handleLabel(seat.handle)}</span>}
      </span>
      <span className="oc-state">
        <StatusDot tone={human ? "neutral" : toneOf(state)} pulse={state === "working"} />
        <span className="oc-line">{cardLine(seat, agent, now, nameOf)}</span>
      </span>
    </div>
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
      {LEGEND.map(({ state, label }) => (
        <span key={state} className="oc-legend-item">
          <StatusDot tone={toneOf(state)} />
          {label} <span className="t-num">{counts[state]}</span>
        </span>
      ))}
    </div>
  );
}
