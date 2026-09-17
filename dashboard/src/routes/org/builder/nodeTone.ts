/**
 * Which of the design system's six node hues an agent seat is drawn in.
 *
 * DERIVED, BECAUSE A COMPANY DOCUMENT HAS NO COLOUR IN IT. The chart this is
 * drawn to match lets a founder pick a seat's hue and stores it beside the
 * seat; a Crewlet company configuration has no such field, and inventing one
 * would put a decoration into the document the engine reads, the schema, the
 * operations log and every dry run. So the hue is a function of the seat's own
 * identity instead.
 *
 * FROM THE KEY, WHICH OUTLIVES A RENAME. A node key is the seat's stable
 * identity in the draft, so a seat keeps its hue while it is renamed, moved
 * between units, or has its handle chosen; hashing the NAME would repaint the
 * chart on every keystroke in the editor. The same rule the design system's
 * seeded avatar tints follow, and the same reason.
 *
 * ONLY AGENT SEATS. A human seat wears the dashed boundary instead, which is
 * the mark this dashboard gives every seat standing for somebody outside the
 * system, and a unit or the company is the chart's own neutral surface: a hue
 * on those would turn a decoration into a taxonomy the reader has to decode.
 */

import type { NodeView } from "./chartModel.ts";
import type { SeatKind } from "./model/operations.ts";
import type { TreeCardTone } from "@crewlethq/ui";

/** The six, in the design system's own order. */
export const NODE_TONES: readonly TreeCardTone[] = [
  "purple",
  "cyan",
  "green",
  "amber",
  "rose",
  "blue",
];

/**
 * A small deterministic hash: the same key always gives the same hue, and
 * neighbouring keys spread rather than landing together.
 *
 * FNV-1a over the key's code units, which is what the design system's own
 * seeded tint uses. It is not a security question and it is not a
 * distribution question either: six hues over any number of seats means
 * repeats, and a repeat says nothing, because this palette is a mark rather
 * than a series.
 */
function hash(seed: string): number {
  let value = 0x811c9dc5;
  for (let at = 0; at < seed.length; at++) {
    value ^= seed.charCodeAt(at);
    value = Math.imul(value, 0x01000193) >>> 0;
  }
  return value;
}

/** The hue of a seat key, whatever kind of seat it turns out to be. */
export function toneOfKey(key: string): TreeCardTone {
  return NODE_TONES[hash(key) % NODE_TONES.length]!;
}

/** The hue a node of the structure chart is drawn in, or nothing for a neutral one. */
export function nodeTone(view: NodeView | undefined): TreeCardTone | undefined {
  if (view?.type !== "seat" || view.kind !== "agent") return undefined;
  return toneOfKey(view.key);
}

/** The hue of a reporting chart item, which knows its kind and the seat key it stands for. */
export function seatTone(kind: SeatKind, key: string | undefined): TreeCardTone | undefined {
  if (kind !== "agent" || key === undefined) return undefined;
  return toneOfKey(key);
}
