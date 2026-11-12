/**
 * The atlas as a map: the kit's GraphCanvas, its lanes the engine's, read left
 * to right the way a request travels, with code, monitoring, tools and models
 * in the band beneath. Every lane is drawn; an empty one says what in this
 * build would fill it.
 *
 * The selection is the peek (`peek=atlas:<id>`), so a reload, Back or a copied
 * link lands on the same picture. A selected entry also lights the relations
 * the map does not draw at rest, read from its own answer.
 */

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  Callout,
  GraphCanvas,
  Link,
  SegmentedControl,
  StatusLegend,
  graphReach,
  type GraphBand,
  type GraphCanvasHandle,
  type GraphCanvasLabels,
  type GraphEdge,
  type GraphGroup,
  type GraphNode,
  type GraphTraceDirection,
  type StatusLegendItem,
} from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import type {
  AtlasEntryAnswer,
  AtlasHealth,
  AtlasLane,
  AtlasMapAnswer,
  AtlasSummary,
} from "~/contract/atlas.ts";
import { holds } from "~/lib/access.ts";
import { relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { useNarrowerThan } from "~/lib/useNarrowerThan.ts";
import type { TraceChoice } from "./filters.ts";
import { KindGlyph, VendorLead } from "./parts.tsx";
import { BAND_LABEL, HEALTH, KINDS, LANES, RELATIONS, VENDORS, entries } from "./words.ts";

/**
 * Groups start closed past this many entries drawn. About nineteen cards a
 * lane read whole at half size (the least a name stays words) in a 1440 by
 * 900 window, over eight lanes, is about 150.
 */
export const ATLAS_OPEN_MAX = 150;

/**
 * The narrowest map, in px, that holds the trace switch beside the zoom bar
 * (about 190 and 150 with the canvas's insets); narrower, the switch goes
 * under the zoom bar rather than over it.
 */
const TRACE_BESIDE_ZOOM = 400;

/** The words the map says on its own, in the atlas's word for one item. */
const MAP_LABELS: Partial<GraphCanvasLabels> = {
  lane: (label, count) => `${label}, ${entries(count)}`,
  entries: (count) => entries(count),
  openGroup: (label, count) => `${label}, ${entries(count)}, open`,
  closedGroup: (label, count) => `${label}, ${entries(count)}, closed`,
  keys: "Arrow keys move between entries and lanes, Enter selects an entry or opens a group, and Escape clears the selection. Plus and minus zoom, and zero fits the map in view.",
};

const TRACE_OPTIONS: readonly { value: TraceChoice | "off"; label: string }[] = [
  { value: "off", label: "Off" },
  { value: "down", label: "Depends on" },
  { value: "up", label: "Used by" },
  { value: "both", label: "Both" },
];

/** The healths the legend keys, the most urgent first. */
const LEGEND: readonly AtlasHealth[] = ["alerting", "needs_look", "quiet"];

/** The group an entry stands in: its vendor's in its lane, or the lane's entries no vendor names (the engine's key). */
function groupOf(summary: Pick<AtlasSummary, "lane" | "vendor">): string {
  return summary.vendor ? `${summary.lane}/${summary.vendor}` : summary.lane;
}

/** What a group of entries no vendor names is called. */
const NO_VENDOR = "No vendor";

/** One entry as a card: its name, its key or how long since it was seen, its dot. */
export function cardOf(summary: AtlasSummary, now: number): GraphNode {
  const dot = HEALTH[summary.health].dot;
  return {
    id: summary.id,
    lane: summary.lane,
    group: groupOf(summary),
    name: summary.name,
    // THE KEY WHERE IT SAYS MORE THAN THE NAME: a repository named by its path
    // would read its path twice.
    caption:
      summary.state === "stale"
        ? `Last seen ${relTime(summary.last_seen, now)}`
        : summary.key === summary.name
          ? undefined
          : summary.key,
    kind: KINDS[summary.kind].word,
    glyph: <KindGlyph kind={summary.kind} />,
    attention: dot ? { tone: dot, label: HEALTH[summary.health].label } : undefined,
    unconfirmed: summary.word === "unconfirmed",
  };
}

/** The lanes as the map draws them, each saying what would fill it when it is empty. */
function bandsOf(
  lanes: AtlasMapAnswer["lanes"],
  emptyOf: (lane: AtlasLane) => ReactNode,
): GraphBand[] {
  const lane = (one: AtlasMapAnswer["lanes"][number]) => ({
    id: one.lane,
    label: LANES[one.lane].label,
    empty: emptyOf(one.lane),
  });
  return [
    { id: "request", lanes: lanes.filter((one) => !one.band).map(lane) },
    { id: "around", label: BAND_LABEL, lanes: lanes.filter((one) => one.band).map(lane) },
  ];
}

/**
 * The relations the map draws at rest, and the selected entry's others (an
 * account it belongs to, a check it is built from), drawn only while it is
 * selected.
 */
export function edgesOf(answer: AtlasMapAnswer, selected: AtlasEntryAnswer | null): GraphEdge[] {
  const drawn = new Set(answer.nodes.map((node) => node.id));
  const out = new Map<string, GraphEdge>();
  for (const edge of answer.edges) {
    const id = `${edge.from}>${edge.to}:${edge.relation}`;
    out.set(id, {
      id,
      from: edge.from,
      to: edge.to,
      label: RELATIONS[edge.relation].toLowerCase(),
    });
  }
  if (selected) {
    const from = selected.entry.id;
    const more = [
      ...selected.entry.relations.flatMap((edge) =>
        edge.to ? [{ from, to: edge.to, relation: edge.relation }] : [],
      ),
      ...selected.used_by.map((link) => ({
        from: link.entry.id,
        to: from,
        relation: link.relation,
      })),
    ];
    for (const edge of more) {
      const id = `${edge.from}>${edge.to}:${edge.relation}`;
      if (out.has(id) || !drawn.has(edge.from) || !drawn.has(edge.to)) continue;
      out.set(id, {
        id,
        from: edge.from,
        to: edge.to,
        label: RELATIONS[edge.relation].toLowerCase(),
        atRest: false,
      });
    }
  }
  return [...out.values()];
}

export function AtlasMap({
  answer,
  selected,
  selectedId,
  onSelect,
  trace,
  onTrace,
  narrowed,
  held,
  fitKey,
}: {
  answer: AtlasMapAnswer;
  /** The selected entry's own answer, for the relations the map draws only on selection. */
  selected: AtlasEntryAnswer | null;
  selectedId: string | null;
  onSelect: (id: string | null) => void;
  trace: TraceChoice;
  onTrace: (trace: TraceChoice) => void;
  /** A filter narrows the map, so an empty lane says nothing matched rather than what fills it. */
  narrowed: boolean;
  /** The reader's grants, for the links an empty lane offers. */
  held: readonly string[];
  fitKey: string;
}) {
  const now = useNow();
  const [legend, setLegend] = useState<string | null>(null);
  const box = useRef<HTMLDivElement>(null);
  const narrow = useNarrowerThan(box, TRACE_BESIDE_ZOOM);
  const canvas = useRef<GraphCanvasHandle>(null);

  const nodes = useMemo(() => answer.nodes.map((summary) => cardOf(summary, now)), [answer, now]);
  const healthOf = useMemo(
    () => new Map(answer.nodes.map((summary) => [summary.id, summary.health])),
    [answer],
  );
  const edges = useMemo(() => edgesOf(answer, selected), [answer, selected]);
  const groups = useMemo<GraphGroup[]>(
    () =>
      answer.groups.map((group) => ({
        id: group.key,
        lane: group.lane,
        label: group.vendor ? VENDORS[group.vendor].name : NO_VENDOR,
        lead: <VendorLead vendor={group.vendor} />,
      })),
    [answer],
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

  const items: StatusLegendItem[] = LEGEND.map((health) => ({
    key: health,
    label: HEALTH[health].label,
    tone: HEALTH[health].tone,
    count: answer.nodes.filter((node) => node.health === health).length,
  }));
  const cardDimmed = useCallback(
    (id: string) => legend !== null && healthOf.get(id) !== legend,
    [legend, healthOf],
  );

  // A SELECTION THE ADDRESS ARRIVED WITH IS BROUGHT INTO VIEW, once: a link to
  // an entry on the map lands on it, where the map would open on its start.
  const arrived = useRef(selectedId);
  useEffect(() => {
    const id = arrived.current;
    if (id === null || !healthOf.has(id)) return;
    arrived.current = null;
    canvas.current?.focusNode(id);
  }, [healthOf]);

  const traced =
    trace === ""
      ? null
      : { direction: trace as GraphTraceDirection, depth: Number.POSITIVE_INFINITY };
  const hint = useMemo(() => {
    if (!traced || selectedId === null || !healthOf.has(selectedId)) return undefined;
    const reached = graphReach(edges, selectedId, traced.direction, traced.depth).nodes.size - 1;
    const name = answer.nodes.find((node) => node.id === selectedId)?.name ?? "";
    if (traced.direction === "up") {
      return `${entries(reached)} ${reached === 1 ? "depends" : "depend"} on ${name}`;
    }
    if (traced.direction === "down") return `${name} depends on ${entries(reached)}`;
    return `${entries(reached)} lit from ${name}`;
  }, [traced, selectedId, healthOf, edges, answer]);

  const traceSwitch = (
    <SegmentedControl
      label="Trace"
      semantics="radio"
      size="sm"
      options={TRACE_OPTIONS}
      value={trace === "" ? "off" : trace}
      onValueChange={(value) => onTrace(value === "off" ? "" : value)}
    />
  );

  return (
    <>
      {answer.collapsed.length > 0 && <Collapsed answer={answer} />}
      <div className="atlas-map" ref={box}>
        <GraphCanvas
          ref={canvas}
          label="Atlas map"
          bands={bands}
          nodes={nodes}
          edges={edges}
          groups={groups}
          labels={MAP_LABELS}
          collapseAbove={ATLAS_OPEN_MAX}
          selectedId={selectedId}
          onSelect={onSelect}
          trace={traced}
          cardDimmed={legend === null ? undefined : cardDimmed}
          controlsPlacement="top-right"
          controlsStart={narrow ? undefined : traceSwitch}
          controlsBelow={narrow ? traceSwitch : undefined}
          overlayEnd={
            answer.nodes.length > 0 ? (
              <StatusLegend
                label="Entry health"
                items={items}
                value={legend}
                onValueChange={setLegend}
                countLabel={entries}
              />
            ) : undefined
          }
          hint={hint}
          fitKey={fitKey}
        />
      </div>
    </>
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
 * draws at once: named, with the way to read each in the list, so the map is
 * never taken for the whole.
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
      The atlas holds more entries than the map draws at once, so the engine closed these groups:{" "}
      {closed.map((group, i) => (
        <span key={group.key}>
          {i > 0 && (i === closed.length - 1 ? " and " : ", ")}
          <Link
            href={href(["company", "atlas"], {
              shape: "list",
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
