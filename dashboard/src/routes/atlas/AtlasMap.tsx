/**
 * The atlas as a map: the kit's GraphCanvas, read the way a request travels.
 * What the world reaches, the services and the data come first, what those run
 * on next, and what stands around them (code, monitoring, tools and models)
 * last, arranged into rows and columns for the canvas the map has
 * (`arrange.ts`). Every lane is drawn; an empty one says what in this build
 * would fill it.
 *
 * The map draws every relation in its family: calls at rest, what things run
 * on as trunks, and the rest for the entry a reader stands on. A selection
 * lights its whole path both ways, and the catalogue under the map shares the
 * selection, the pointed-at entry and the search with it.
 */

import {
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type RefObject,
} from "react";
import { XGlyph } from "@crewlethq/icons/glyphs";
import {
  Button,
  Callout,
  GRAPH_CANVAS_NARROW_WIDTH,
  type GraphBand,
  GraphCanvas,
  type GraphEdge,
  type GraphGroup,
  type GraphNode,
  graphReach,
  IconButton,
  Link,
  NoticeList,
  StatusDot,
  Text,
} from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import type { AtlasLane, AtlasMapAnswer, AtlasSummary } from "~/contract/atlas.ts";
import { holds } from "~/lib/access.ts";
import { useNow } from "~/lib/clock.ts";
import { leaveFullscreen } from "~/lib/fullscreen.ts";
import { useNarrowerThan } from "~/lib/useNarrowerThan.ts";
import { arrangeMap, mapMetrics, mapRoom, type LaneLoad, type MapPlan } from "./arrange.ts";
import type { TraceChoice } from "./filters.ts";
import { HEALTH_TONES, MAP_LABELS, MAP_TONES, cardOf, carrying, lineOf } from "./graph.tsx";
import { EntryDisc, HealthWord, VendorLead } from "./parts.tsx";
import {
  HEALTH,
  KINDS,
  LANES,
  MAP_ROWS,
  VENDORS,
  entries,
  openTasks,
  reliedOn,
  reliedOnBy,
} from "./words.ts";

/**
 * Entries stand in vendor groups, closed, past this many drawn. About
 * nineteen cards a lane read whole at half size (the least a name stays
 * words) in a 1440 by 900 window, over eight lanes, is about 150. Under it
 * every entry is its own card, with its vendor as the mark on its corner.
 */
export const ATLAS_OPEN_MAX = 150;

/** What a group of entries no vendor names is called. */
const NO_VENDOR = "No vendor";

/** The letters of a lane's name that say which it is, where its title is cut. */
const TITLE_LETTERS = 5;

/** What an empty lane says on a narrowed map. */
const NOTHING_MATCHES = "Nothing in this lane matches the filters.";

/** The group an entry stands in: its vendor's in its lane, or the lane's entries no vendor names (the engine's key). */
function groupOf(summary: Pick<AtlasSummary, "lane" | "vendor">): string {
  return summary.vendor ? `${summary.lane}/${summary.vendor}` : summary.lane;
}

/**
 * The column each service on a call chain stands in: one right of the deepest
 * service that calls it, so a request reads left to right inside the lane, and
 * a chain deeper than the lane is wide ends in its last column. A cycle stops
 * at the entry it started from.
 *
 * A SERVICE ON NO CHAIN NAMES NO COLUMN: it neither calls nor is called by
 * another service, so no column means anything for it, and the map flows it
 * across the lane's columns as it flows every other lane. Named depth 0, every
 * one stood in the first column: 64 of 78 services in one column set the fit
 * of the whole map at a size no name reads.
 */
export function serviceColumns(answer: AtlasMapAnswer): Map<string, number> {
  const inLane = new Set(
    answer.nodes.filter((node) => node.lane === "services").map((node) => node.id),
  );
  const callers = new Map<string, string[]>();
  const chained = new Set<string>();
  for (const edge of answer.edges) {
    if (edge.family !== "calls" || !inLane.has(edge.from) || !inLane.has(edge.to)) continue;
    callers.set(edge.to, [...(callers.get(edge.to) ?? []), edge.from]);
    chained.add(edge.from);
    chained.add(edge.to);
  }
  const depths = new Map<string, number>();
  const depthOf = (id: string, walking: Set<string>): number => {
    const known = depths.get(id);
    if (known !== undefined) return known;
    if (walking.has(id)) return 0;
    walking.add(id);
    const depth = Math.max(
      0,
      ...(callers.get(id) ?? []).map((caller) => depthOf(caller, walking) + 1),
    );
    walking.delete(id);
    depths.set(id, depth);
    return depth;
  };
  return new Map([...chained].map((id) => [id, depthOf(id, new Set())]));
}

/**
 * The lanes the map draws, in reading order, each with its row of `MAP_ROWS`.
 *
 * A NARROWED MAP DRAWS THE LANES THAT HOLD SOMETHING (`onlyFilled`): every
 * lane drawn, empty ones included, set the fit, so the four entries a filter
 * left opened on a phone as four marks with no name. While nothing at all
 * matches, every lane is drawn, each saying so.
 */
function lanesOf(
  lanes: AtlasMapAnswer["lanes"],
  onlyFilled: boolean,
): { id: AtlasLane; row: number }[] {
  const filled = lanes.filter((one) => one.count > 0);
  const answered = new Set(
    (onlyFilled && filled.length > 0 ? filled : lanes).map((one) => one.lane),
  );
  const placed = new Set(MAP_ROWS.flatMap((row) => row.lanes));
  return MAP_ROWS.flatMap((row, i) =>
    [
      ...row.lanes.filter((id) => answered.has(id)),
      // A LANE THE ENGINE ANSWERS AND NO ROW NAMES joins the last one, drawn rather than dropped.
      ...(i === MAP_ROWS.length - 1 ? [...answered].filter((id) => !placed.has(id)) : []),
    ].map((id) => ({ id, row: i })),
  );
}

/**
 * What each drawn lane holds, as the arrangement weighs it: the words on each
 * card (an entry's name, or a closed group's label and count), the services'
 * call depths, and an empty lane's message.
 */
function loadsOf(
  lanes: readonly { id: AtlasLane; row: number }[],
  answer: AtlasMapAnswer,
  grouped: boolean,
  depths: ReadonlyMap<string, number> | null,
  work: ReadonlyMap<string, number> | null,
  emptyWords: (lane: AtlasLane) => number,
): LaneLoad[] {
  return lanes.map(({ id, row }) => {
    const inLane = answer.nodes.filter((node) => node.lane === id);
    const named: number[] = [];
    for (const node of inLane) {
      const depth = depths?.get(node.id);
      if (depth !== undefined) named[depth] = (named[depth] ?? 0) + 1;
    }
    const groups = new Map<string, number>();
    for (const node of inLane) groups.set(groupOf(node), (groups.get(groupOf(node)) ?? 0) + 1);
    const label = (key: string) => {
      const vendor = inLane.find((node) => groupOf(node) === key)?.vendor;
      return vendor ? VENDORS[vendor].name : NO_VENDOR;
    };
    return {
      id,
      row,
      cards: grouped
        ? [...groups].map(([key, count]) => `${label(key)} ${entries(count)}`.length)
        : inLane.map((node) => node.name.length),
      named: named.length > 0 ? Array.from(named, (count) => count ?? 0) : undefined,
      empty: inLane.length === 0 ? emptyWords(id) : 0,
      title: titleWords(LANES[id].label, inLane, work),
    };
  });
}

/**
 * How many characters of a lane's title must show for it to read: its name's
 * first letters (the kit cuts a long name, never its figures), its count, and a
 * figure for each tone it tallies.
 */
function titleWords(
  label: string,
  inLane: readonly AtlasSummary[],
  work: ReadonlyMap<string, number> | null,
): number {
  const tallies = [
    inLane.filter((node) => node.health === "alerting").length,
    inLane.filter((node) => node.health === "needs_look").length,
    work
      ? inLane.filter((node) => node.health === "quiet" && (work.get(node.id) ?? 0) > 0).length
      : 0,
  ].filter((count) => count > 0);
  const figures = (count: number) => String(count).length;
  const name = label.length > TITLE_LETTERS ? TITLE_LETTERS + 1 : label.length;
  return name + 2 + figures(inLane.length) + tallies.reduce((sum, n) => sum + figures(n) + 3, 0);
}

/**
 * The box the map is drawn in, or the screen while the map holds it, with the
 * reader's root font size and density; null until it is laid out.
 */
function useMapBox(box: RefObject<HTMLDivElement | null>) {
  const [size, setSize] = useState<{
    width: number;
    height: number;
    rem: number;
    density: number;
  } | null>(null);
  useLayoutEffect(() => {
    const el = box.current;
    if (!el) return;
    const measure = () => {
      const holding = document.fullscreenElement;
      const shown = holding && el.contains(holding) ? holding : el;
      const next = {
        width: shown.clientWidth,
        height: shown.clientHeight,
        rem: Number.parseFloat(getComputedStyle(document.documentElement).fontSize) || 16,
        density: Number.parseFloat(getComputedStyle(el).getPropertyValue("--density")) || 1,
      };
      setSize((was) =>
        was &&
        was.width === next.width &&
        was.height === next.height &&
        was.rem === next.rem &&
        was.density === next.density
          ? was
          : next,
      );
    };
    measure();
    const watch = new ResizeObserver(measure);
    watch.observe(el);
    document.addEventListener("fullscreenchange", measure);
    window.addEventListener("resize", measure);
    return () => {
      watch.disconnect();
      document.removeEventListener("fullscreenchange", measure);
      window.removeEventListener("resize", measure);
    };
  }, [box]);
  return size;
}

/** A box this much larger or smaller is arranged again; less is a scrollbar or a rounding. */
const REARRANGE = 0.03;

/**
 * The path a selection lights, counted over every hop: how many entries rely
 * on it however far up, and how many it relies on however far down.
 */
export function pathSentence(up: number, down: number): string {
  return `Over every hop, ${reliedOnBy(up)} and it relies on ${reliedOn(down)}`;
}

export function AtlasMap({
  answer,
  work,
  matched,
  selectedId,
  onSelect,
  onDetails,
  pointedId,
  onPoint,
  trace,
  narrowed,
  held,
  fitKey,
}: {
  answer: AtlasMapAnswer;
  /** How many open tasks name each entry, by its id, or null where no work is read for the reader. */
  work: ReadonlyMap<string, number> | null;
  /** The entries the search leaves, or null with no search: the rest are pushed back, never removed. */
  matched: ReadonlySet<string> | null;
  selectedId: string | null;
  onSelect: (id: string | null) => void;
  /** Open the selected entry's sheet. */
  onDetails: (id: string) => void;
  /** The entry the catalogue points at, lit as the pointer lights one. */
  pointedId: string | null;
  onPoint: (id: string | null) => void;
  trace: TraceChoice;
  /** A filter narrows the map, so an empty lane says nothing matched rather than what fills it. */
  narrowed: boolean;
  /** The reader's grants, for the links an empty lane offers. */
  held: readonly string[];
  /** What `answer` was read with: the map fits again when it moves. */
  fitKey: string;
}) {
  const now = useNow();
  const box = useRef<HTMLDivElement>(null);
  // THE CARD BESIDE AN ENTRY ANSWERS THE READER'S OWN GESTURE on the map: a
  // selection a link arrived with is brought into view and opens none. A
  // narrow map draws none either: there it would cover the entry it is about,
  // and the dock under the picture already names the selection.
  const [engaged, setEngaged] = useState(false);
  const engage = engaged ? undefined : () => setEngaged(true);
  const narrow = useNarrowerThan(box, GRAPH_CANVAS_NARROW_WIDTH);
  const grouped = answer.nodes.length > ATLAS_OPEN_MAX;

  const byId = useMemo(() => new Map(answer.nodes.map((node) => [node.id, node])), [answer]);
  const depths = useMemo(() => (grouped ? null : serviceColumns(answer)), [answer, grouped]);
  const lanes = useMemo(() => lanesOf(answer.lanes, narrowed), [answer, narrowed]);

  // THE ARRANGEMENT FOLLOWS THE BOX, held while an entry is selected: a sheet
  // opening beside the page for it never moves the entries under the reader.
  const measured = useMapBox(box);
  const [basis, setBasis] = useState(measured);
  if (
    measured &&
    measured !== basis &&
    (basis === null ||
      (selectedId === null &&
        (Math.abs(measured.width - basis.width) > REARRANGE * basis.width ||
          Math.abs(measured.height - basis.height) > REARRANGE * basis.height ||
          measured.rem !== basis.rem ||
          measured.density !== basis.density)))
  ) {
    setBasis(measured);
  }
  const plan = useMemo<MapPlan | null>(() => {
    if (!basis) return null;
    const loads = loadsOf(lanes, answer, grouped, depths, work, (lane) =>
      narrowed
        ? NOTHING_MATCHES.length
        : LANES[lane].empty.length + (LANES[lane].link?.label.length ?? 0),
    );
    return arrangeMap(
      loads,
      mapRoom(basis.width, basis.height, basis.rem),
      mapMetrics(basis.rem, basis.density),
    );
  }, [basis, lanes, answer, grouped, depths, work, narrowed]);

  const spans = useMemo(
    () => new Map(plan?.rows.flatMap((row) => row.lanes.map((lane) => [lane.id, lane.columns]))),
    [plan],
  );
  const nodes = useMemo<GraphNode[]>(() => {
    // A CHAIN DEEPER THAN THE LANE IS WIDE ends in its last column.
    const last = (spans.get("services") ?? 1) - 1;
    return answer.nodes.map((summary) => {
      const depth = depths?.get(summary.id);
      return {
        ...cardOf(summary, now, work?.get(summary.id)),
        lane: summary.lane,
        group: grouped ? groupOf(summary) : undefined,
        column: depth === undefined ? undefined : Math.min(depth, last),
      };
    });
  }, [answer, now, grouped, work, depths, spans]);
  const edges = useMemo<GraphEdge[]>(() => answer.edges.map(lineOf), [answer]);
  const groups = useMemo<GraphGroup[]>(
    () =>
      grouped
        ? answer.groups.map((group) => ({
            id: group.key,
            lane: group.lane,
            label: group.vendor ? VENDORS[group.vendor].name : NO_VENDOR,
            // A LEAD ONLY WHERE THERE IS A MARK: an element drawing nothing
            // took the card's cell and left it a blank box (No vendor, and
            // vendors the kit has no mark for); with none, the card draws its
            // own open-me glyph there.
            ...(group.vendor && VENDORS[group.vendor].mark
              ? { lead: <VendorLead vendor={group.vendor} /> }
              : {}),
          }))
        : [],
    [answer, grouped],
  );
  const bands = useMemo<GraphBand[]>(
    () =>
      (plan?.rows ?? []).map((row) => ({
        id: row.lanes.map((lane) => lane.id).join("+"),
        lanes: row.lanes.map((lane) => ({
          id: lane.id,
          label: LANES[lane.id as AtlasLane].label,
          columns: lane.columns,
          empty: narrowed ? NOTHING_MATCHES : <EmptyLane lane={lane.id as AtlasLane} held={held} />,
        })),
      })),
    [plan, narrowed, held],
  );
  // THE MAP OPENS ON THE SELECTION THE ADDRESS ARRIVED WITH, in the middle at
  // a size its card reads, where it otherwise opens fitted: a link to an entry
  // (its sheet, or "Show it on the map") lands on it. Read once, when the map
  // is first placed, so a later selection never moves the reader's view.
  const [arrived] = useState(selectedId);

  const selected = selectedId === null ? undefined : byId.get(selectedId);
  // THE PATH THE MAP LIGHTS, COUNTED OVER EVERY HOP and said so, in the verb
  // the entry's own counts use one step out ("1 entry relies on it"), so the
  // dock and the sheet never read as two answers to one question.
  const impact = useMemo(() => {
    if (!selected) return "";
    const lines = carrying(edges);
    const up = graphReach(lines, selected.id, "up", Number.POSITIVE_INFINITY).nodes.size - 1;
    const down = graphReach(lines, selected.id, "down", Number.POSITIVE_INFINITY).nodes.size - 1;
    return pathSentence(up, down);
  }, [selected, edges]);

  return (
    <>
      {answer.collapsed.length > 0 && <Collapsed answer={answer} />}
      <div
        className="atlas-map"
        ref={box}
        onPointerMove={engage}
        onKeyDown={engage}
        style={
          plan
            ? ({ "--crewlet-graph-canvas-card-width": `${plan.card}px` } as CSSProperties)
            : undefined
        }
      >
        {plan && (
          <GraphCanvas
            className="atlas-map__graph"
            label="Atlas map: everything the company runs on and how it relates"
            bands={bands}
            nodes={nodes}
            edges={edges}
            groups={groups}
            collapseAbove={grouped ? ATLAS_OPEN_MAX : undefined}
            labels={MAP_LABELS}
            tones={work ? MAP_TONES : HEALTH_TONES}
            selectedId={selectedId}
            onSelect={onSelect}
            trace={trace === "" ? undefined : { direction: trace, depth: Number.POSITIVE_INFINITY }}
            hoveredId={pointedId}
            onHover={onPoint}
            cardDimmed={matched === null ? undefined : (id) => !matched.has(id)}
            controlsPlacement="top-right"
            renderCard={
              engaged && !narrow
                ? (id) => {
                    const one = byId.get(id);
                    return one ? <HoverCard entry={one} open={work?.get(id) ?? 0} /> : null;
                  }
                : undefined
            }
            dock={
              selected ? (
                <div className="atlas-dock">
                  <EntryDisc entry={selected} size="sm" />
                  <div className="atlas-dock__words">
                    <Text variant="body" truncate>
                      {selected.name}
                    </Text>
                    <Text variant="caption" tone="secondary">
                      {[KINDS[selected.kind].word, HEALTH[selected.health].label, impact].join(
                        " · ",
                      )}
                    </Text>
                  </div>
                  <Button
                    variant="primary"
                    size="small"
                    onClick={() => {
                      // THE SHEET IS THE SHELL'S, outside a fullscreen map.
                      leaveFullscreen();
                      onDetails(selected.id);
                    }}
                  >
                    Details
                  </Button>
                  <IconButton
                    label="Clear the selection"
                    icon={<XGlyph />}
                    size="sm"
                    onClick={() => onSelect(null)}
                  />
                </div>
              ) : undefined
            }
            // A NEW ARRANGEMENT IS FITTED ON THE SAME CANVAS, never a new one, which
            // would drop the screen a fullscreen map holds; the kit fits it again
            // once its cards are measured at the new width.
            fitKey={`${fitKey}|${plan.key}`}
            initialNode={arrived}
          />
        )}
      </div>
    </>
  );
}

/**
 * The card beside the entry the pointer or focus rests on: what it is, whose
 * it is, how it is, and how many open tasks name it, which an entry that is
 * not quiet has no notch left to say.
 */
function HoverCard({ entry, open }: { entry: AtlasSummary; open: number }) {
  return (
    <div className="atlas-hover">
      <EntryDisc entry={entry} size="md" />
      <div className="atlas-hover__words">
        <Text variant="body" truncate>
          {entry.name}
        </Text>
        <Text variant="caption" tone="secondary">
          {[KINDS[entry.kind].word, entry.vendor ? VENDORS[entry.vendor].name : null]
            .filter(Boolean)
            .join(" · ")}
        </Text>
        <HealthWord health={entry.health} />
        {entry.reason && (
          <Text variant="caption" tone="secondary">
            {entry.reason}
          </Text>
        )}
        {open > 0 && (
          <StatusDot tone="info">
            {openTasks(open)} {open === 1 ? "names" : "name"} it
          </StatusDot>
        )}
      </div>
    </div>
  );
}

/** What an empty lane says, with the way to fill it for a reader who may take it. */
function EmptyLane({ lane, held }: { lane: AtlasLane; held: readonly string[] }) {
  const { empty, link } = LANES[lane];
  return (
    <>
      <span>{empty}</span>
      {link && holds(held, link.grant) && (
        <Link href={href(link.path)} size="inherit">
          {link.label}
        </Link>
      )}
    </>
  );
}

/**
 * The groups the engine closed because the atlas holds more than the map
 * draws at once: named, with the way to read each in the catalogue, so the
 * map is never taken for the whole.
 */
function Collapsed({ answer }: { answer: AtlasMapAnswer }) {
  const byKey = new Map(answer.groups.map((group) => [group.key, group]));
  const closed = answer.collapsed.flatMap((key) => {
    const group = byKey.get(key);
    return group ? [group] : [];
  });
  const total = closed.reduce((sum, group) => sum + group.count, 0);
  return (
    <Callout variant="info" layout="banner" title={`${entries(total)} are not drawn on the map`}>
      The map draws only so many entries at once, so these groups are closed. Each opens whole in
      the catalogue.
      <NoticeList
        notices={closed.map((group) => ({
          key: group.key,
          title: `${group.vendor ? VENDORS[group.vendor].name : NO_VENDOR} in ${LANES[group.lane].label} (${group.count.toLocaleString()})`,
          action: (
            <Link
              href={href(["company", "atlas"], {
                lane: group.lane,
                ...(group.vendor ? { vendor: group.vendor } : {}),
              })}
            >
              Open in the catalogue
            </Link>
          ),
        }))}
      />
    </Callout>
  );
}
