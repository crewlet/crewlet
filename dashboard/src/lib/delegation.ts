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
 * Who put a call to people: the delegates that answered escalate, or Crewlet,
 * where none did (no answer in time, or the engine overruled their answers).
 */
export function escalatedByWords(d: HeldDelegation, name: Name): string {
  const by = [...new Set(d.given.filter((g) => g.verdict === "escalate").map((g) => g.by))];
  return by.length > 0
    ? `Escalated by ${andList(by.map(name))} (rule ${d.rule})`
    : `Escalated by Crewlet (rule ${d.rule})`;
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
 * (`delegated.Overruled*`), as a phrase. A word this list does not carry (an
 * SSH always-escalate class, a newer engine's) is said as written.
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
};

export function overruledWords(word: string): string {
  return OVERRULED_WORDS[word] ?? word.replaceAll("_", " ");
}

/** Who gave a verdict, in words: the delegate or person by name, or Crewlet. */
export function giverWords(row: Pick<DelegatedVerdictRow, "by" | "by_kind">, name: Name): string {
  return row.by_kind === "engine" || !row.by ? "Crewlet" : name(row.by);
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
