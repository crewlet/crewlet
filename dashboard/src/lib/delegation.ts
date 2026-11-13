/**
 * Delegated decisions as a person reads them (ADR-0125): who a held call
 * waits on, why it reached people, and an answer a delegate gave.
 *
 * THE ENGINE DECIDES, these only say it. Whether a call waits on its
 * delegates is the row's `delegation` with no `escalated`; why it reached
 * people is the engine's own sentences (`escalated.engine`); an answer a
 * delegate gave is the `delegated_rule` its event carries. Nothing here
 * infers any of it from words.
 */

import type {
  DelegatedAct,
  DelegatedConfidence,
  DelegatedVerdict,
  DelegatedVerdictRow,
  HeldDelegation,
} from "~/contract/delegated.ts";
import type { McpHeldCall } from "~/contract/mcp.ts";
import type { EventRecord, ExecutorAnswerPayload } from "~/protocol/index.ts";
import { possessive } from "./format.ts";
import { STORAGE_KEYS } from "./storage.ts";
import { andList } from "./words.ts";

/** A handle as a person reads it, a seat's name where the chart has one. */
type Name = (handle: string) => string;

/**
 * The delegation of a call that still waits on its delegates alone, or null:
 * such a call is put to nobody until it is escalated.
 */
export function delegationWaiting(
  held: { delegation?: McpHeldCall["delegation"] | undefined } | null | undefined,
): HeldDelegation | null {
  const d = held?.delegation;
  return d && !d.escalated ? d : null;
}

/** The delegation of a call that reached people over its delegates, or null. */
export function delegationEscalated(
  held: { delegation?: McpHeldCall["delegation"] | undefined } | null | undefined,
): HeldDelegation | null {
  const d = held?.delegation;
  return d?.escalated ? d : null;
}

/** "With delegate Security Engineer (rule 1)": who a waiting call is with. */
export function withDelegateWords(d: HeldDelegation, name: Name): string {
  const who = andList(d.delegates.map(name));
  return `With ${d.delegates.length > 1 ? "delegates" : "delegate"} ${who} (rule ${d.rule})`;
}

/**
 * The one delegate whose answer IS the escalation, or null: a single answer,
 * and it says escalate. Its name and confidence are then said once, on the
 * note's heading, and never again on a line of its own.
 */
export function soleEscalation(d: HeldDelegation): HeldDelegation["given"][number] | null {
  const only = d.given.length === 1 ? d.given[0]! : null;
  return only?.verdict === "escalate" ? only : null;
}

/**
 * Who put a call to people, under which rule, and how sure they were: the
 * delegates that answered escalate, or Crewlet, where none did (no answer in
 * time, or the engine overruled their answers). The confidence is said here
 * only for a sole escalation, whose answer has no line of its own.
 */
export function escalatedByWords(d: HeldDelegation, name: Name): string {
  const by = [...new Set(d.given.filter((g) => g.verdict === "escalate").map((g) => g.by))];
  const who = by.length > 0 ? andList(by.map(name)) : "Crewlet";
  const sure = confidenceWords(soleEscalation(d)?.confidence ?? "").toLowerCase();
  return `Escalated by ${who} (rule ${d.rule})${sure ? `, ${sure}` : ""}`;
}

/** A verdict in a word, and the state it is drawn in: done, needs a person, refused. */
export const VERDICT_WORDS: Readonly<
  Record<DelegatedVerdict, { label: string; tone: "success" | "warning" | "danger" }>
> = {
  allow: { label: "Allowed", tone: "success" },
  escalate: { label: "Escalated", tone: "warning" },
  deny: { label: "Denied", tone: "danger" },
};

/** A verdict this build does not know is said as written, untoned. */
export function verdictWords(verdict: string): {
  label: string;
  tone: "success" | "warning" | "danger" | "neutral";
} {
  return Object.hasOwn(VERDICT_WORDS, verdict)
    ? VERDICT_WORDS[verdict as DelegatedVerdict]
    : { label: verdict.replaceAll("_", " "), tone: "neutral" };
}

/** What each act a rule may delegate is called (`delegated.Kind`). */
const KIND_WORDS: Readonly<Record<string, string>> = {
  ssh_operate: "Commands on servers",
  pr_merge: "Merges",
  deploy: "Deploys",
};

/** An act in words; one this build does not name is said as written. */
export function kindWords(kind: DelegatedAct): string {
  return KIND_WORDS[kind] ?? kind;
}

/** "High confidence", "Low confidence"; nothing for a giver that states none. */
export function confidenceWords(confidence: DelegatedConfidence | ""): string {
  return confidence === "high" ? "High confidence" : confidence === "low" ? "Low confidence" : "";
}

/**
 * The engine's closed words for why it changed or made a verdict
 * (`delegated.Overruled*`, a merge's and a deploy's unmet conditions
 * `delegated.Unmet*` and the path classes no delegate allows), as a phrase. A
 * word this list does not carry (an SSH always-escalate class, a newer
 * engine's) is said as written.
 */
const OVERRULED_WORDS: Readonly<Record<string, string>> = {
  self: "the delegate made, asked for or handed on the request",
  leads: "the delegate leads the requesting seat",
  rate_limit: "past the delegate's allows for the hour",
  server_rate_limit: "past the server's commands for the hour",
  low_confidence: "low confidence",
  disagreement: "the panel disagreed",
  deadline: "no delegate answered in time",
  delegation_off: "delegation was turned off or its delegates changed",
  sensitive_label: "its work item is labelled security, privacy or legal",
  facts_unread: "a condition could not be read from the code host",
  head_moved: "the change's head is not the one asked about",
  not_open: "the change is not open",
  no_required_checks: "its branch requires no check and its rule names none",
  checks_not_passed: "a required check has not passed",
  not_mergeable: "the change does not merge cleanly",
  not_ready: "the target's own merge gate is not met",
  no_approval: "nobody but its authors approved the head",
  paths_unclassified: "it touches more paths than Crewlet reads",
  not_merged: "a change it names has not merged",
  not_contained: "the commit does not hold every change, or is not on their branch",
  ci_pipelines: "it changes a CI pipeline",
  code_owners: "it changes who owns and approves the code",
  secret_files: "it touches a file that holds keys or secrets",
  access_policies: "it changes access policies or roles",
  infrastructure: "it changes infrastructure the company runs",
  migrations: "it changes a database migration",
  crewlet_company: "it changes a Crewlet company document",
  rule_path: "it touches a path the rule always puts to people",
};

export function overruledWords(word: string): string {
  return OVERRULED_WORDS[word] ?? word.replaceAll("_", " ");
}

/** Who gave a verdict, in words: the delegate or person by name, or Crewlet. */
export function giverWords(row: Pick<DelegatedVerdictRow, "by" | "by_kind">, name: Name): string {
  return row.by_kind === "engine" || !row.by ? "Crewlet" : name(row.by);
}

/** The acts an agent asks for with a release tool: a merge and a deploy (`release` approvers). */
export function isReleaseAct(act: string | undefined): act is "pr_merge" | "deploy" {
  return act === "pr_merge" || act === "deploy";
}

/** A merge `ask_to_merge` holds: the change, the head it read, how to merge and why. */
export interface MergeRequest {
  act: "pr_merge";
  change: string;
  head: string;
  /** `merge` or `squash`; "" where the call named none, which the engine merges as `merge`. */
  method: string;
  reason: string;
}

/** A deploy `ask_to_deploy` holds: the commit, the changes it ships, where, the way back and why. */
export interface DeployRequest {
  act: "deploy";
  repo: string;
  commit: string;
  /** As the agent named them: `web!41`, `41` or `#41`. */
  changes: string[];
  environment: string;
  rollback: string;
  reason: string;
}

export type ReleaseRequest = MergeRequest | DeployRequest;

const MERGE_KEYS = new Set(["change", "head", "method", "reason"]);
const DEPLOY_KEYS = new Set(["repo", "commit", "changes", "environment", "rollback", "reason"]);

/**
 * A held merge's or deploy's arguments, read whole, or null: for another act,
 * arguments not shown, or any key or value the tool does not take, so a
 * screen draws every argument as sent rather than the ones it knew.
 */
export function releaseRequestOf(
  act: string | undefined,
  args: Record<string, unknown> | null | undefined,
): ReleaseRequest | null {
  if (!args || !isReleaseAct(act)) return null;
  const known = act === "pr_merge" ? MERGE_KEYS : DEPLOY_KEYS;
  if (!Object.keys(args).every((k) => known.has(k))) return null;
  // EVERY VALUE TEXT, `method` the one a call may leave out.
  const text: Record<string, string> = {};
  for (const key of known) {
    if (key === "changes") continue;
    const value = args[key] ?? (key === "method" ? "" : undefined);
    if (typeof value !== "string") return null;
    text[key] = value;
  }
  if (act === "pr_merge") {
    return {
      act,
      change: text.change!,
      head: text.head!,
      method: text.method!,
      reason: text.reason!,
    };
  }
  const changes = args.changes;
  if (!Array.isArray(changes)) return null;
  if (!changes.every((c) => typeof c === "string" || typeof c === "number")) return null;
  return {
    act,
    repo: text.repo!,
    commit: text.commit!,
    changes: changes.map(String),
    environment: text.environment!,
    rollback: text.rollback!,
    reason: text.reason!,
  };
}

/** How a merge is made, in words; a method this build does not know is said as written. */
export function mergeMethodWords(method: string): string {
  if (method === "" || method === "merge") return "A merge commit";
  if (method === "squash") return "One squashed commit";
  return method;
}

/** What Allow once of a held merge or deploy does, for the agent named. */
export function releaseAllowWords(act: "pr_merge" | "deploy", agent: string): string {
  return act === "pr_merge"
    ? `Allow once merges exactly this head, once, as ${agent}. A change pushed to since is not merged, and the target's own rules still apply.`
    : `Allow once tells ${agent} it may deploy exactly this commit there, once. Crewlet does not deploy it.`;
}

/**
 * Who may answer a held merge or deploy (`release.approve`): administrators,
 * the agent's leads and the people the kind's rule escalates to, at a recent
 * step-up. The rule is the delegation's, or unnamed for a call put to people
 * at once.
 */
export function releaseApproverWords(
  act: "pr_merge" | "deploy",
  agent: string,
  rule: number | null,
): string {
  const named = rule
    ? `the people rule ${rule} escalates to`
    : `the people a delegated decisions rule for ${act === "pr_merge" ? "merges" : "deploys"} escalates to`;
  return `Administrators, ${possessive(agent)} leads and ${named} may answer it. Answering asks you to confirm it\u2019s you unless you did recently.`;
}

/**
 * The rule an answer to a held call was given under by a delegate, off its
 * `executor_answered` or `executor_answer_given`; null for a person's answer
 * and every other event.
 */
export function delegatedRuleOf(event: Pick<EventRecord, "type" | "payload">): number | null {
  if (event.type !== "executor_answered" && event.type !== "executor_answer_given") return null;
  const rule = (event.payload as ExecutorAnswerPayload | undefined)?.delegated_rule;
  return typeof rule === "number" && Number.isInteger(rule) && rule > 0 ? rule : null;
}

/** "Decided under delegation, rule 2": the mark an answer a delegate gave carries. */
export function delegatedMarkWords(rule: number): string {
  return `Decided under delegation, rule ${rule}`;
}

/**
 * What the Home notices were put away for: the digest by the company day it
 * covers, and the flagged denials by the newest one, so a later flag brings
 * its notice back.
 */
export interface NoticesDismissed {
  decided?: string;
  flagged?: string;
}

/** The notices this browser put away, or none where storage refuses. */
export function readDismissed(storage: () => Pick<Storage, "getItem">): NoticesDismissed {
  try {
    const raw = storage().getItem(STORAGE_KEYS.delegatedNotices);
    const parsed: unknown = raw ? JSON.parse(raw) : null;
    if (!parsed || typeof parsed !== "object") return {};
    const { decided, flagged } = parsed as Record<string, unknown>;
    return {
      ...(typeof decided === "string" ? { decided } : {}),
      ...(typeof flagged === "string" ? { flagged } : {}),
    };
  } catch {
    return {};
  }
}

/** Put one notice away. NEVER THROWS: storage refused leaves it on screen. */
export function writeDismissed(
  storage: () => Pick<Storage, "getItem" | "setItem">,
  next: NoticesDismissed,
): void {
  try {
    const kept = readDismissed(storage);
    storage().setItem(STORAGE_KEYS.delegatedNotices, JSON.stringify({ ...kept, ...next }));
  } catch {
    // Storage is refused: the notice stays until the day moves on.
  }
}

/** The newest flagged denial's instant, the notice's own key, or "" for none. */
export function newestFlag(rows: readonly Pick<DelegatedVerdictRow, "at">[]): string {
  return rows.reduce((newest, row) => (row.at > newest ? row.at : newest), "");
}
