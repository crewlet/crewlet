/**
 * The atlas as a map: the kit's GraphCanvas, read the way a request travels.
 * What the world reaches, the services and the data stand in the first row,
 * what those run on in the second, and what stands around them (code,
 * monitoring, tools and models) in the third. Every lane is drawn; an empty
 * one says what in this build would fill it.
 *
 * The map draws every relation in its family: calls at rest, what things run
 * on as trunks, and the rest for the entry a reader stands on. A selection
 * lights its whole path both ways, and the catalogue under the map shares the
 * selection, the pointed-at entry and the search with it.
 */

import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { XGlyph } from "@crewlethq/icons/glyphs";
import {
  Button,
  Callout,
  GRAPH_CANVAS_NARROW_WIDTH,
  GraphCanvas,
  IconButton,
  Link,
  Text,
  graphReach,
  type GraphBand,
  type GraphCanvasHandle,
  type GraphEdge,
  type GraphGroup,
  type GraphNode,
} from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import type { AtlasLane, AtlasMapAnswer, AtlasSummary } from "~/contract/atlas.ts";
import { holds } from "~/lib/access.ts";
import { useNow } from "~/lib/clock.ts";
import { leaveFullscreen } from "~/lib/fullscreen.ts";
import { useNarrowerThan } from "~/lib/useNarrowerThan.ts";
import type { TraceChoice } from "./filters.ts";
import { MAP_LABELS, MAP_TONES, cardOf, carrying, lineOf } from "./graph.tsx";
import { EntryDisc, HealthWord, VendorLead } from "./parts.tsx";
import { HEALTH, KINDS, LANES, MAP_ROWS, VENDORS, entries } from "./words.ts";

/**
 * Entries stand in vendor groups, closed, past this many drawn. About
 * nineteen cards a lane read whole at half size (the least a name stays
 * words) in a 1440 by 900 window, over eight lanes, is about 150. Under it
 * every entry is its own card, with its vendor as the mark on its corner.
 */
export const ATLAS_OPEN_MAX = 150;

/** What a group of entries no vendor names is called. */
const NO_VENDOR = "No vendor";

/** The group an entry stands in: its vendor's in its lane, or the lane's entries no vendor names (the engine's key). */
function groupOf(summary: Pick<AtlasSummary, "lane" | "vendor">): string {
  return summary.vendor ? `${summary.lane}/${summary.vendor}` : summary.lane;
}

/**
 * The column each service stands in: one right of the deepest service that
 * calls it, so a request reads left to right inside the lane. A cycle stops
 * at the entry it started from.
 */
export function serviceColumns(answer: AtlasMapAnswer): Map<string, number> {
  const inLane = new Set(
    answer.nodes.filter((node) => node.lane === "services").map((node) => node.id),
  );
  const callers = new Map<string, string[]>();
  for (const edge of answer.edges) {
    if (edge.family !== "calls" || !inLane.has(edge.from) || !inLane.has(edge.to)) continue;
    callers.set(edge.to, [...(callers.get(edge.to) ?? []), edge.from]);
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
  const last = LANES.services.columns - 1;
  return new Map([...inLane].map((id) => [id, Math.min(last, depthOf(id, new Set()))]));
}

/** The lanes as the map draws them, row by row, each saying what would fill it when it is empty. */
function bandsOf(
  lanes: AtlasMapAnswer["lanes"],
  emptyOf: (lane: AtlasLane) => ReactNode,
): GraphBand[] {
  const answered = new Set(lanes.map((one) => one.lane));
  const placed = new Set(MAP_ROWS.flatMap((row) => row.lanes));
  const lane = (id: AtlasLane) => ({
    id,
    label: LANES[id].label,
    columns: LANES[id].columns,
    empty: emptyOf(id),
  });
  return MAP_ROWS.map((row, i) => ({
    id: row.id,
    lanes: [
      ...row.lanes.filter((id) => answered.has(id)),
      // A LANE THE ENGINE ANSWERS AND NO ROW NAMES joins the last one, drawn rather than dropped.
      ...(i === MAP_ROWS.length - 1 ? [...answered].filter((id) => !placed.has(id)) : []),
    ].map(lane),
  })).filter((band) => band.lanes.length > 0);
}

export function AtlasMap({
  answer,
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
  fitKey: string;
}) {
  const now = useNow();
  const canvas = useRef<GraphCanvasHandle>(null);
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
  const nodes = useMemo<GraphNode[]>(() => {
    const columns = grouped ? null : serviceColumns(answer);
    return answer.nodes.map((summary) => ({
      ...cardOf(summary, now),
      lane: summary.lane,
      group: grouped ? groupOf(summary) : undefined,
      column: columns?.get(summary.id),
    }));
  }, [answer, now, grouped]);
  const edges = useMemo<GraphEdge[]>(() => answer.edges.map(lineOf), [answer]);
  const groups = useMemo<GraphGroup[]>(
    () =>
      grouped
        ? answer.groups.map((group) => ({
            id: group.key,
            lane: group.lane,
            label: group.vendor ? VENDORS[group.vendor].name : NO_VENDOR,
            lead: <VendorLead vendor={group.vendor} />,
          }))
        : [],
    [answer, grouped],
  );
  const bands = useMemo(
    () =>
      bandsOf(answer.lanes, (lane) =>
        narrowed ? (
          "Nothing in this lane matches the filters."
        ) : (
          <EmptyLane lane={lane} held={held} />
        ),
      ),
    [answer, narrowed, held],
  );

  // A SELECTION THE ADDRESS ARRIVED WITH IS BROUGHT INTO VIEW, once: a link to
  // an entry on the map lands on it, where the map would open on its start.
  const arrived = useRef(selectedId);
  useEffect(() => {
    const id = arrived.current;
    if (id === null || !byId.has(id)) return;
    arrived.current = null;
    canvas.current?.focusNode(id);
  }, [byId]);

  const selected = selectedId === null ? undefined : byId.get(selectedId);
  const impact = useMemo(() => {
    if (!selected) return "";
    const lines = carrying(edges);
    const up = graphReach(lines, selected.id, "up", Number.POSITIVE_INFINITY).nodes.size - 1;
    const down = graphReach(lines, selected.id, "down", Number.POSITIVE_INFINITY).nodes.size - 1;
    return `${entries(up)} ${up === 1 ? "depends" : "depend"} on it, and it depends on ${down.toLocaleString()}`;
  }, [selected, edges]);

  return (
    <>
      {answer.collapsed.length > 0 && <Collapsed answer={answer} />}
      <div className="atlas-map" ref={box} onPointerMove={engage} onKeyDown={engage}>
        <GraphCanvas
          ref={canvas}
          label="Atlas map: everything the company runs on and how it relates"
          bands={bands}
          nodes={nodes}
          edges={edges}
          groups={groups}
          labels={MAP_LABELS}
          tones={MAP_TONES}
          collapseAbove={grouped ? ATLAS_OPEN_MAX : undefined}
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
                  return one ? <HoverCard entry={one} /> : null;
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
                    {[KINDS[selected.kind].word, HEALTH[selected.health].label, impact].join(" · ")}
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
          fitKey={fitKey}
        />
      </div>
    </>
  );
}

/** The card beside the entry the pointer or focus rests on: what it is, whose it is, and how it is. */
function HoverCard({ entry }: { entry: AtlasSummary }) {
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
      The atlas holds more entries than the map draws at once, so the engine closed these groups.
      Each is read whole in the catalogue:{" "}
      {closed.map((group, i) => (
        <span key={group.key}>
          {i > 0 && (i === closed.length - 1 ? " and " : ", ")}
          <Link
            href={href(["company", "atlas"], {
              lane: group.lane,
              ...(group.vendor ? { vendor: group.vendor } : {}),
            })}
            size="inherit"
          >
            {group.vendor ? VENDORS[group.vendor].name : NO_VENDOR} in {LANES[group.lane].label} (
            {group.count.toLocaleString()})
          </Link>
        </span>
      ))}
      .
    </Callout>
  );
}
