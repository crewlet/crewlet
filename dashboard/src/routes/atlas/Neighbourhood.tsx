/**
 * One entry among its neighbours, in the map's own look: what uses it, the
 * entry, what it depends on, what it runs on and what stands around it, each
 * in the cluster that says how it relates. A still diagram (no zoom bar, no
 * legend), read from one step along every relation in both directions.
 */

import { useMemo } from "react";
import {
  GraphCanvas,
  type GraphBand,
  type GraphEdge,
  type GraphLane,
  type GraphNode,
} from "@crewlethq/ui";
import type { AtlasMapEdge, AtlasSummary, AtlasTraceAnswer } from "~/contract/atlas.ts";
import { MAP_LABELS, cardOf, lineOf } from "./graph.tsx";

type Cluster = "used" | "self" | "uses" | "runs" | "around";

const CLUSTERS: Readonly<Record<Cluster, GraphLane>> = {
  used: { id: "used", label: "Used by" },
  self: { id: "self", label: "This entry" },
  uses: { id: "uses", label: "Depends on" },
  runs: { id: "runs", label: "Runs on", columns: 2 },
  around: { id: "around", label: "Around it", columns: 2 },
};

/** On a wide surface: the entry's path across, what it stands on and what stands around under it. */
const WIDE: readonly (readonly Cluster[])[] = [
  ["used", "self", "uses"],
  ["runs", "around"],
];
/** On a sheet: one cluster under another, each two columns wide. */
const STACKED: readonly (readonly Cluster[])[] = [
  ["used"],
  ["self"],
  ["uses"],
  ["runs"],
  ["around"],
];

/** The cluster a neighbour stands in, read off the relation that joins it to the entry. */
function clusterOf(edge: AtlasMapEdge, self: string): Cluster {
  if (edge.family === "around") return "around";
  if (edge.to === self) return "used";
  return edge.family === "runs_on" ? "runs" : "uses";
}

/** How many neighbours the diagram stands in each of its clusters. */
export interface Touching {
  /** The entries that rely on this one. */
  usedBy: number;
  /** The entries it relies on: what it depends on and what it runs on. */
  reliesOn: number;
  /** The entries around it: its account, its monitors, what it is built from. */
  around: number;
}

/** A diagram's neighbours counted by the cluster each stands in. */
export function touching(nodes: readonly GraphNode[]): Touching {
  const count = (...lanes: Cluster[]) =>
    nodes.filter((node) => (lanes as string[]).includes(node.lane)).length;
  return { usedBy: count("used"), reliesOn: count("uses", "runs"), around: count("around") };
}

/** The diagram of one entry: its cards and its lines. */
export type Near = ReturnType<typeof neighbourhood>;

/** The entry and everything one step from it as the diagram's cards and lines. */
export function neighbourhood(
  self: AtlasSummary,
  down: AtlasTraceAnswer | null,
  up: AtlasTraceAnswer | null,
  now: number,
): { nodes: GraphNode[]; edges: GraphEdge[] } {
  const summaries = new Map<string, AtlasSummary>();
  const lines = new Map<string, AtlasMapEdge>();
  for (const trace of [down, up]) {
    for (const summary of trace?.hops[0]?.entries ?? []) summaries.set(summary.id, summary);
    for (const edge of trace?.edges ?? []) {
      if (edge.from !== self.id && edge.to !== self.id) continue;
      lines.set(`${edge.from}>${edge.to}:${edge.relation}`, edge);
    }
  }
  // A NEIGHBOUR STANDS IN ONE CLUSTER, the one of its first relation: the
  // path's before what it runs on, before what stands around.
  const rank: Cluster[] = ["used", "uses", "runs", "around"];
  const placed = new Map<string, Cluster>();
  for (const edge of lines.values()) {
    const other = edge.from === self.id ? edge.to : edge.from;
    if (!summaries.has(other)) continue;
    const cluster = clusterOf(edge, self.id);
    const held = placed.get(other);
    if (!held || rank.indexOf(cluster) < rank.indexOf(held)) placed.set(other, cluster);
  }
  const nodes: GraphNode[] = [{ ...cardOf(self, now), lane: "self" }];
  for (const [id, cluster] of placed) {
    const summary = summaries.get(id);
    if (summary) nodes.push({ ...cardOf(summary, now), lane: cluster });
  }
  return {
    nodes,
    edges: [...lines.values()]
      .filter((edge) => placed.has(edge.from === self.id ? edge.to : edge.from))
      .map(lineOf),
  };
}

export function Neighbourhood({
  self,
  near,
  stacked = false,
}: {
  self: AtlasSummary;
  /** The entry and what stands one step from it ([neighbourhood]). */
  near: Near;
  /** One cluster under another, for a sheet too narrow for the path across. */
  stacked?: boolean;
}) {
  const { nodes, edges } = near;
  // A CLUSTER NOTHING STANDS IN IS LEFT OUT rather than drawn empty: a still
  // diagram is short of room, and the sections beside it say what is missing.
  const bands = useMemo<GraphBand[]>(() => {
    const filled = new Set(nodes.map((node) => node.lane));
    return (stacked ? STACKED : WIDE)
      .map((row, i) => ({
        id: `row-${i}`,
        lanes: row.filter((id) => filled.has(id)).map((id) => CLUSTERS[id]),
      }))
      .filter((band) => band.lanes.length > 0);
  }, [nodes, stacked]);
  return (
    <div className="atlas-near" data-rows={bands.length}>
      <GraphCanvas
        label={`${self.name} and what it touches`}
        bands={bands}
        nodes={nodes}
        edges={edges}
        labels={MAP_LABELS}
        selectedId={self.id}
        chrome={false}
      />
    </div>
  );
}
