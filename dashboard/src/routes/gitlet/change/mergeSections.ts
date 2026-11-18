/**
 * The merge box's sections: one per question its gate asks, then one for each condition none
 * holds, so the box adds up to the readiness meter's count.
 */

import { CHECK_HISTORY_DAYS } from "~/lib/checks.ts";
import type {
  CodeChangeDetail,
  CodePrecondition,
  CodeReadiness,
  CodeReviewRow,
  CodeReviewerRow,
  CodeRevisionRow,
} from "~/protocol/index.ts";
import { preconditionTone } from "../ChangeRow.tsx";
import type { DiscTone } from "./Timeline.tsx";

export type SectionKey = "reviews" | "checks" | "findings" | "facts" | "mergeable" | "other";

/** Where a section stands: `none` where no condition backs it. */
export type SectionState = "met" | "unmet" | "computing" | "none";

export interface MergeSection {
  key: SectionKey;
  state: SectionState;
  preconditions: CodePrecondition[];
}

/** Which of the five sections holds each kind, in the box's order. */
const HOLDS: readonly (readonly [Exclude<SectionKey, "other">, readonly string[]])[] = [
  ["reviews", ["approval", "human_approval", "owners_approval"]],
  ["checks", ["check"]],
  ["findings", ["blocker"]],
  ["facts", ["facts"]],
  ["mergeable", ["mergeable"]],
];

/** A section's standing from its conditions: unmet before computing before met. */
export function sectionState(preconditions: readonly CodePrecondition[]): SectionState {
  if (preconditions.length === 0) return "none";
  if (preconditions.some((p) => p.state === "unmet")) return "unmet";
  if (preconditions.some((p) => p.state === "computing")) return "computing";
  return "met";
}

/** The box's sections in order, then one for each condition the five do not hold. */
export function mergeSections(readiness: CodeReadiness | undefined): MergeSection[] {
  const pre = readiness?.preconditions ?? [];
  const held = new Set(HOLDS.flatMap(([, kinds]) => kinds));
  const five = HOLDS.map(([key, kinds]) => {
    const preconditions = pre.filter((p) => kinds.includes(p.kind));
    return { key, state: sectionState(preconditions), preconditions };
  });
  const other = pre
    .filter((p) => !held.has(p.kind))
    .map((p) => ({ key: "other" as const, state: sectionState([p]), preconditions: [p] }));
  return [...five, ...other];
}

/** The meter's tones by urgency, most urgent first. */
const URGENCY: readonly DiscTone[] = ["danger", "warning", "info", "success"];

/** A section's disc: its most urgent condition's tone on the readiness meter, neutral where none backs it. */
export function sectionTone(section: Pick<MergeSection, "preconditions">): DiscTone {
  const tones = new Set<DiscTone>(section.preconditions.map((p) => preconditionTone(p)));
  return URGENCY.find((t) => tones.has(t)) ?? "neutral";
}

/** The box's own disc: the first section not met's tone, in the box's order; success when all are met. */
export function boxTone(readiness: CodeReadiness | undefined): DiscTone {
  const sections = mergeSections(readiness);
  const next =
    sections.find((s) => s.state === "unmet") ?? sections.find((s) => s.state === "computing");
  if (next) return sectionTone(next);
  return readiness?.preconditions.length ? "success" : "neutral";
}

/** Whether its head's checks are still kept: pushed within the checks' horizon. */
export function checksKept(
  change: Pick<CodeChangeDetail, "revision" | "opened_at">,
  revisions: readonly Pick<CodeRevisionRow, "n" | "at">[],
  now: number,
): boolean {
  const pushed = revisions.find((r) => r.n === change.revision)?.at ?? change.opened_at;
  return now - Date.parse(pushed) < CHECK_HISTORY_DAYS * 86_400_000;
}

/** A reviewer's standing verdict and the revision it judged. */
export interface Standing {
  handle: string;
  kind: string;
  verdict: string;
  revision: number;
}

/**
 * Each reviewer's standing verdict: the participants' (the verdict the gate
 * counts), then the loaded reviews' latest for a reviewer the participants do
 * not hold (past their bound, or a fleet that does not answer them).
 */
export function reviewerStandings(
  change: Pick<CodeChangeDetail, "participants">,
  reviews: readonly CodeReviewRow[],
): Standing[] {
  const rows = change.participants?.rows ?? [];
  const out: Standing[] = rows
    .filter((p) => p.verdict !== undefined && p.verdict_revision !== undefined)
    .map((p) => ({
      handle: p.handle,
      kind: p.kind,
      verdict: p.verdict!,
      revision: p.verdict_revision!,
    }));
  const partial = !change.participants || (change.participants.omitted ?? 0) > 0;
  if (!partial) return out;
  const known = new Set(rows.map((p) => p.handle));
  const latest = new Map<string, CodeReviewRow>();
  // A COMMENT JUDGES NOTHING: the engine's standing verdict moves only on a review that does.
  for (const r of reviews) {
    if (!known.has(r.reviewer) && r.verdict !== "comment") latest.set(r.reviewer, r);
  }
  for (const r of latest.values()) {
    out.push({
      handle: r.reviewer,
      kind: r.reviewer_kind,
      verdict: r.verdict,
      revision: r.revision,
    });
  }
  return out;
}

/**
 * Whom a re-request asks: each standing verdict not an approval of the head, not asked now, no
 * author, and never the reader, whose own review is theirs to give again, not to ask of themselves.
 */
export function reRequestable(
  change: Pick<CodeChangeDetail, "revision" | "authors">,
  standings: readonly Standing[],
  asked: readonly CodeReviewerRow[],
  me: string,
): string[] {
  const authors = new Set(change.authors.map((a) => a.author));
  const askedNow = new Set(asked.filter((a) => a.state === "requested").map((a) => a.reviewer));
  return standings
    .filter((s) => s.verdict !== "approve" || s.revision !== change.revision)
    .map((s) => s.handle)
    .filter((h) => !askedNow.has(h) && !authors.has(h) && h !== me);
}
