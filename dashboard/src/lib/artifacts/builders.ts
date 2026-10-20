/**
 * Which agents a create page offers to build an artifact, and which it
 * suggests first.
 *
 * EVERY AGENT SEAT CAN BUILD ANY KIND: the artifact tools are built in, and a
 * seat's grants are an admin's to read. So the offer is every agent, and the
 * order is the only judgement made here: a seat whose name, goal or
 * responsibilities speak the kind's work ("design", "dashboard", "frontend")
 * comes first, the closest match first, then everybody else in the chart's
 * order. A page that knows better (the agent the reader last asked) names its
 * own suggestion, which outranks the words.
 */

import type { ArtifactKind } from "~/protocol/index.ts";
import type { OrgIndex, Seat } from "~/lib/seats.ts";

/**
 * The words a seat that does each kind's work describes itself with, matched
 * against its name, goal and responsibilities as whole word starts.
 */
const KIND_WORK: Readonly<Record<ArtifactKind, readonly string[]>> = {
  doc: ["writ", "doc", "content", "editor", "product", "pm", "research"],
  slides: ["present", "deck", "slide", "marketing", "pitch", "strategy", "product"],
  sheet: ["analy", "data", "finance", "operations", "ops", "spreadsheet", "report"],
  design: ["design", "ux", "ui", "brand", "visual", "creative", "mockup"],
  dashboard: ["dashboard", "metric", "analytic", "data", "insight", "report"],
  html: ["frontend", "front-end", "web", "engineer", "developer", "prototype", "interactive"],
};

/** How strongly a seat speaks a kind's work: one point per word it uses, its name counting double. */
export function builderScore(seat: Seat, kind: ArtifactKind): number {
  const words = KIND_WORK[kind];
  const said = (text: string) =>
    words.filter((w) => new RegExp(`(^|[^a-z])${w}`, "i").test(text)).length;
  return 2 * said(seat.name) + said(seat.goal) + said(seat.responsibilities.join(" "));
}

/** One agent a create page offers, and whether it is the one suggested. */
export interface Builder {
  seat: Seat;
  suggested: boolean;
}

/**
 * The agents that can build `kind`, the suggested one first: `suggested` when
 * it names an agent on the chart, else the seat that speaks the kind's work
 * best, else the chart's first agent.
 */
export function artifactBuilders(
  index: OrgIndex,
  kind: ArtifactKind,
  suggested?: string,
): Builder[] {
  const agents = index.seats.filter((s) => s.kind === "agent");
  const scored = agents
    .map((seat, at) => ({ seat, at, score: builderScore(seat, kind) }))
    .sort((a, b) => b.score - a.score || a.at - b.at)
    .map((s) => s.seat);
  const named = suggested ? scored.find((s) => s.handle === suggested) : undefined;
  const ordered = named ? [named, ...scored.filter((s) => s !== named)] : scored;
  return ordered.map((seat, i) => ({ seat, suggested: i === 0 }));
}
