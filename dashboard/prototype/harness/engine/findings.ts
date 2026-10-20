/**
 * What a reconcile pass's findings mean, as the engine's `integration`
 * package says it (`finding.go`): each kind's verdict and its sentence, which
 * finding a report is about, and the report a pass's findings fold into.
 *
 * ONE PORT for every reader in the demo: the loop folding a pass, the
 * integrations answer marking each finding with its verdict, and the roll-up
 * reading a finding's sentence. A second copy of any of these tables is how
 * a card and the row beneath it come to disagree.
 */

import type { ReconcileFinding } from "~/contract/integrations.ts";

/** The phase and the actor each kind implies (`FindingKind.Verdict`). */
const VERDICTS: Record<string, readonly [phase: string, actor: string]> = {
  credential_missing: ["unconfigured", "operator"],
  credential_rejected: ["unconfigured", "operator"],
  credential_expiring: ["ready", "operator"],
  approval_required: ["awaiting_admin", "admin"],
  ingress_blocked: ["degraded", "admin"],
  ingress_pending: ["activating", "engine"],
  identity_missing: ["provisioning", "engine"],
  identity_failed: ["degraded", "admin"],
  grant_pending: ["activating", "provider"],
  unknown_tier: ["degraded", "operator"],
  grant_short: ["degraded", "admin"],
  grant_excess: ["ready", "admin"],
  registration_orphaned: ["ready", "admin"],
  coverage_partial: ["ready", "operator"],
};

/** A kind's phase and actor; a kind this build does not know is degraded and the operator's. */
export function verdictOf(kind: string): { phase: string; actor: string } {
  const [phase, actor] = VERDICTS[kind] ?? ["degraded", "operator"];
  return { phase, actor };
}

/** A kind's sentence where a finding carries no detail (`FindingKind.sentence`). */
export function sentenceOf(kind: string, subject?: string): string {
  const about = subject ? ` for ${subject}` : "";
  const sentences: Record<string, string> = {
    credential_missing: "this integration has no usable credential",
    credential_rejected: "the third-party app refused this integration's credential",
    credential_expiring: "this integration's credential expires soon",
    approval_required: "an administrator must approve this integration at the third-party app",
    ingress_blocked: "events cannot be delivered to this engine",
    ingress_pending: "still pointing the third-party app at this engine",
    identity_missing: "creating agent identities",
    identity_failed: "an agent identity could not be created",
    grant_pending: "the third-party app is applying agent access",
    unknown_tier: "the company names an access tier this third-party app does not have",
    grant_short: "an agent holds less access than its role asks for",
    grant_excess: "an agent holds more access than its role asks for",
    registration_orphaned: "this engine registered something that it no longer manages",
  };
  return (sentences[kind] ?? `this integration reported ${kind}`) + about;
}

/**
 * How serious a kind is, smaller first (`FindingKind.severity`). A kind this
 * build does not know sits above the advisories and below every real problem.
 */
const SEVERITY: Record<string, number> = {
  credential_missing: 0,
  credential_rejected: 1,
  approval_required: 2,
  ingress_blocked: 3,
  ingress_pending: 4,
  identity_missing: 5,
  identity_failed: 6,
  grant_pending: 7,
  unknown_tier: 8,
  grant_short: 9,
  credential_expiring: 11,
  grant_excess: 12,
  registration_orphaned: 13,
  coverage_partial: 14,
};
const severityOf = (kind: string) => SEVERITY[kind] ?? 10;

/** The finding a report is about and where it sits: the worst, the first among equals (`worstOf`). */
function worstOf(findings: readonly ReconcileFinding[]): [ReconcileFinding, number] {
  let at = 0;
  for (let i = 1; i < findings.length; i++) {
    if (severityOf(findings[i]!.kind) < severityOf(findings[at]!.kind)) at = i;
  }
  return [findings[at]!, at];
}

/** The findings with the one the report is about first, the rest in the pass's order (`Promote`). */
export function promote(findings: readonly ReconcileFinding[]): ReconcileFinding[] {
  if (findings.length < 2) return [...findings];
  const [worst, at] = worstOf(findings);
  return [worst, ...findings.slice(0, at), ...findings.slice(at + 1)];
}

/** The report a pass's findings fold into, which the loop records (`integration.Report`). */
export interface Report {
  phase: string;
  actor: string;
  detail: string;
  action_url: string;
}

/**
 * Fold a pass's findings into the one report a person reads (`Classify`):
 * none is ready; otherwise the worst finding speaks, counting the others of
 * its kind, and carries its link only where a person has to act.
 */
export function classify(findings: readonly ReconcileFinding[]): Report {
  if (findings.length === 0) return { phase: "ready", actor: "", detail: "", action_url: "" };
  const [worst] = worstOf(findings);
  const same = findings.filter((f) => f.kind === worst.kind).length;
  let detail = worst.detail || sentenceOf(worst.kind, worst.subject);
  if (same > 1) detail = `${detail} (and ${same - 1} more)`;
  const { phase, actor } = verdictOf(worst.kind);
  const person = actor === "admin" || actor === "operator";
  return { phase, actor, detail, action_url: person ? (worst.action_url ?? "") : "" };
}

/** A quantity and its noun, agreeing (`integration.Count`). */
export function count(n: number, noun: string): string {
  return n === 1 ? `1 ${noun}` : `${n} ${noun}s`;
}
