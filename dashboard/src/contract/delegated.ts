/**
 * Delegated decisions (ADR-0125): a request that waits on a person, decided
 * by an agent seat the company names, within rules the engine enforces.
 *
 * The shapes here are the engine's: a held call's `delegation` on its run's
 * row (`queries.delegationOf`), and the `delegated_decisions` answer
 * (`queries.delegatedDecisions`), the rules, the limits and what delegates
 * decided. Every word a delegate wrote (a reason, a finding) is a model's text
 * and is drawn as plain text, never as Markdown.
 */

/**
 * What a request is for: its act, set by the engine from the structured data
 * that made it (`delegated.Kind`). `other` is never delegated. CLOSED: the
 * engine sends only the acts it declares and a gate holds this list to
 * `delegated.Kinds()`; a reader still says an act it does not know as written.
 */
export type DelegatedAct = "ssh_operate" | "other";

/** A delegate's answer, or what a request's answers came to (`delegated.Verdict`). */
export type DelegatedVerdict = "allow" | "escalate" | "deny";

/** How sure a delegate is of its verdict (`delegated.Confidence`). */
export type DelegatedConfidence = "high" | "low";

/** One delegate's answer to a held call, in the order given (`sandbox.HeldVerdict`). */
interface HeldDelegateAnswer {
  /** The delegate seat, by handle. */
  by: string;
  verdict: DelegatedVerdict;
  confidence: DelegatedConfidence;
  /** The delegate's own words: a model's, drawn as plain text. */
  reason: string;
  /** What it found, most serious first, as the delegate gave them. */
  findings: string[];
  at: string;
}

/** Why a delegated call went to people (`sandbox.HeldEscalation`). */
interface HeldEscalation {
  /** The engine's closed words for why it overruled or made the outcome. */
  overruled: string[];
  /** The engine's sentences for the people it is put to. */
  engine: string[];
  at: string;
}

/**
 * A held call's delegation: the rule that routed it, its delegates, their
 * answers so far, and, once it reached people, why. A call whose
 * `escalated` is null waits on its delegates and is put to nobody.
 */
export interface HeldDelegation {
  /** The rule's 1-based number, as it stood when the call was held. */
  rule: number;
  /** One delegate seat, or a panel of two that must both allow. */
  delegates: string[];
  given: HeldDelegateAnswer[];
  escalated: HeldEscalation | null;
}

/** One rule of the company document's `delegated_decisions` block, as the read numbers it. */
export interface DelegatedRule {
  rule: number;
  kinds: DelegatedAct[];
  delegates: string[];
  escalate_to: string[];
  /** One decision in this many is shown for a spot-check after the fact. */
  sample_one_in: number;
}

/** The limits the engine holds every delegate to, whatever the document says. */
interface DelegatedLimits {
  /** A request no delegate answered in this long goes to people. */
  deadline_minutes: number;
  /** Allows per kind and delegate in one clock hour. */
  decisions_per_hour: number;
  /** Allows per server in one clock hour. */
  server_allows_per_hour: number;
}

/** Who gave a verdict (`accessdomain.VerdictBy`). */
type DelegatedGiver = "agent" | "person" | "engine";

/** The path a verdict was given on (`accessdomain.VerdictThrough`). */
type DelegatedPath = "delegation" | "decision";

/** One verdict on a delegated request, as the access log records it. */
export interface DelegatedVerdictRow {
  request: string;
  kind: DelegatedAct;
  /** The rule that routed it, 0 for a person's approval through a decision. */
  rule: number;
  /** The seat whose request it is. */
  seat: string;
  /** The delegate's or the approving person's seat, "" for the engine. */
  by: string;
  by_kind: DelegatedGiver;
  through: DelegatedPath;
  /** What a delegate answered, "" for anybody else. */
  asked: DelegatedVerdict | "";
  /** How sure a delegate was, "" for anybody else. */
  confidence: DelegatedConfidence | "";
  /** What took effect. */
  verdict: DelegatedVerdict;
  /** The engine's closed words for why it changed or made the verdict. */
  overruled: string[];
  /** Chosen for a person's spot-check after the fact. */
  sampled: boolean;
  /** The server, for an `ssh_operate`. */
  host: string;
  /** The work item the request is on, "" where none. */
  item: string;
  at: string;
  /** What was decided, in clear: a command. Absent where it did not open on this node. */
  subject?: string;
  /** The giver's words. Absent where they did not open. */
  reason?: string;
  findings?: string[];
  /** Why what was decided is not shown, where it did not open. */
  subject_unopened?: string;
}

/**
 * `GET /delegated-decisions?day=`: the rules, the limits, what delegates
 * decided on one company day (the digest), and the spot-checks and flagged
 * denials of the week before. Admitted for an administrator and the people a
 * rule names in `escalate_to`; refused for everybody else.
 */
export interface DelegatedDecisionsAnswer {
  enabled: boolean;
  rules: DelegatedRule[];
  limits: DelegatedLimits;
  /** The company day the digest covers, `2026-11-09`. */
  day: string;
  since: string;
  until: string;
  decided: DelegatedVerdictRow[];
  /** The log holds more than the page for that day. */
  decided_more: boolean;
  spot_checks: DelegatedVerdictRow[];
  spot_checks_more: boolean;
  /** Denials a delegate gave with high confidence: what it judged harmful. */
  flagged: DelegatedVerdictRow[];
  flagged_more: boolean;
}
