/**
 * The atlas as the kit's map reads it: an entry as a card, a relation as a
 * line in its family, and the words the map says on its own. The page's map
 * and an entry's neighbourhood diagram are built from here, so the two never
 * draw one entry two ways.
 */

import { VendorMark } from "@crewlethq/icons";
import type { GraphAttentionTone, GraphCanvasLabels, GraphEdge, GraphNode } from "@crewlethq/ui";
import type { AtlasMapEdge, AtlasSummary } from "~/contract/atlas.ts";
import { relTime } from "~/lib/format.ts";
import { KindGlyph } from "./parts.tsx";
import { FAMILIES, HEALTH, KINDS, RELATIONS, VENDORS, entries } from "./words.ts";

/** The words the map says on its own, in the atlas's word for one item. */
export const MAP_LABELS: Partial<GraphCanvasLabels> = {
  lane: (label, count) => `${label}, ${entries(count)}`,
  entries: (count) => entries(count),
  openGroup: (label, count) => `${label}, ${entries(count)}, open`,
  closedGroup: (label, count) => `${label}, ${entries(count)}, closed`,
  keys: "Arrow keys move between entries and lanes, Enter selects an entry or opens a group, and Escape clears the selection. Plus and minus zoom, and zero fits the map in view.",
};

/** The word of each tone the map keys: the engine's two healths that are not quiet. */
export const MAP_TONES: Partial<Record<GraphAttentionTone, string>> = {
  danger: HEALTH.alerting.label,
  warning: HEALTH.needs_look.label,
};

/**
 * One entry as a card: its kind's glyph in the ruled cell, its vendor's mark
 * on the corner, its name, and a notch with its health word when it is not
 * quiet. Its key, or how long since it was seen, is read after the name.
 */
export function cardOf(summary: AtlasSummary, now: number): Omit<GraphNode, "lane"> {
  const dot = HEALTH[summary.health].dot;
  const mark = summary.vendor ? VENDORS[summary.vendor].mark : undefined;
  return {
    id: summary.id,
    name: summary.name,
    // THE WHOLE NAME ON HOVER, where the card's one line cuts it.
    title: summary.name,
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
    mark: mark ? <VendorMark vendor={mark} size="xs" /> : undefined,
    attention: dot ? { tone: dot, label: HEALTH[summary.health].label } : undefined,
    unconfirmed: summary.word === "unconfirmed",
  };
}

/** One relation as a line, in the family the engine says it is drawn in. */
export function lineOf(edge: AtlasMapEdge): GraphEdge {
  return {
    id: `${edge.from}>${edge.to}:${edge.relation}`,
    from: edge.from,
    to: edge.to,
    label: RELATIONS[edge.relation].toLowerCase(),
    family: FAMILIES[edge.family],
  };
}

/** The lines a failure travels along: calls and what things run on, never what stands around. */
export function carrying(edges: readonly GraphEdge[]): GraphEdge[] {
  return edges.filter((edge) => edge.family !== "context");
}
