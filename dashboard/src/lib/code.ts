/**
 * The code host's values, as the Code workspace says them.
 *
 * Every value here arrives as the engine spells it — a repository's name, an
 * object id, a ref, a command's result — and these are the one place each is
 * turned into what a reader reads, so a repository's grid and its pushes
 * cannot say one thing two ways.
 */

import {
  COMMAND_OUTCOMES,
  REPO_NAME,
  REPO_NAME_MAX_BYTES,
  REPO_STATUSES,
} from "~/contract/code.ts";
import type { CodePushCommand, CodePushCounts, CodeRepoStatus } from "~/protocol/index.ts";

/** The engine's repository grammar, compiled once. */
const REPO_NAME_PATTERN = new RegExp(REPO_NAME);

/**
 * Whether a string is a name a repository can have — the engine's own
 * grammar (`contract/code.ts`): its shape, a project's or a seat's scratch
 * repository's, and its length, so an address naming anything else is said in
 * place rather than asked about. The length is counted in code units, which
 * for a name the shape admits — lowercase ASCII alone — are its bytes.
 */
export function isRepoName(name: string): boolean {
  return name.length <= REPO_NAME_MAX_BYTES && REPO_NAME_PATTERN.test(name);
}

/**
 * How many digits of an object id a reader is shown: SEVEN, git's own default
 * abbreviation (`core.abbrev`) and the fewest an abbreviated id the engine
 * takes back may have (`code.MinAbbrevDigits`), so what is printed is what a
 * person copies into git or into a tool. The whole id is the title beside it.
 */
export const SHORT_OID_DIGITS = 7;

/** An object id as a row prints it: see [SHORT_OID_DIGITS]. */
export function shortOID(oid: string): string {
  return oid.slice(0, SHORT_OID_DIGITS);
}

/**
 * Git's null id: the old side of a ref's creation, the new side of its
 * deletion. Forty digits, the engine's `gitref.ZeroOID`: its repositories are
 * object-format sha1 (`gitref.OIDHexLen`).
 */
const ZERO_OID = "0".repeat(40);

/** A ref as a reader names it: a branch by its name, a tag as a tag, anything else whole. */
export function refName(ref: string): { name: string; kind: "branch" | "tag" | "ref" } {
  if (ref.startsWith("refs/heads/"))
    return { name: ref.slice("refs/heads/".length), kind: "branch" };
  if (ref.startsWith("refs/tags/")) return { name: ref.slice("refs/tags/".length), kind: "tag" };
  return { name: ref, kind: "ref" };
}

/**
 * What one command of a push did, in a word: what became of the ref — made,
 * moved, moved by force, removed — or that it was asked for the value it had,
 * or refused. ONE WORD PER OUTCOME (`contract/code.ts`'s `COMMAND_OUTCOMES`),
 * the order the engine decides them in: a refusal is a refusal whatever it
 * asked, and an unchanged ref moved nowhere.
 */
export function commandWord(c: CodePushCommand): string {
  if (c.result === "ng") return "refused";
  if (c.result === "noop") return "unchanged";
  if (c.result !== "ok") return c.result;
  if (c.old === ZERO_OID) return "created";
  if (c.new === ZERO_OID) return "deleted";
  return c.forced ? "forced" : "moved";
}

/**
 * The order a push's counts are said in: the REFUSALS FIRST, as a push read in
 * brief lists its refused commands first — what did not happen is what a
 * reader acts on — then the rest in the engine's order.
 */
const COUNT_ORDER: readonly string[] = [
  "refused",
  ...COMMAND_OUTCOMES.filter((outcome) => outcome !== "refused"),
];

/** One outcome's count, as a summary says it. */
export interface OutcomeCount {
  outcome: string;
  count: number;
}

/**
 * The commands a push read in brief left out (`omitted`): how many, and each
 * outcome's count in the order a summary says them — an outcome a newer
 * engine counts by, which this build has no word for, after the rest by its
 * own key, never dropped from the sum.
 */
export function omittedOf(counts: CodePushCounts | undefined): {
  total: number;
  outcomes: OutcomeCount[];
} {
  if (!counts) return { total: 0, outcomes: [] };
  const keys = [
    ...COUNT_ORDER.filter((outcome) => outcome in counts),
    ...Object.keys(counts).filter((outcome) => !COUNT_ORDER.includes(outcome)),
  ];
  const outcomes = keys
    .map((outcome) => ({ outcome, count: counts[outcome] ?? 0 }))
    .filter((c) => Number.isSafeInteger(c.count) && c.count > 0);
  return { total: outcomes.reduce((n, c) => n + c.count, 0), outcomes };
}

/** A state the engine knows (`contract/code.ts`). */
type KnownRepoStatus = (typeof REPO_STATUSES)[number];

/**
 * Each state in words: what a row's mark says and what the listing's filter
 * offers. Keyed on the contract's set, so a state the engine grows is a type
 * error here until it has its words.
 */
const REPO_STATUS_WORDS: Readonly<Record<KnownRepoStatus, string>> = {
  active: "Active",
  archived: "Archived",
  deleted: "Deleted",
  purging: "Being purged",
};

function knownStatus(status: CodeRepoStatus): status is KnownRepoStatus {
  return (REPO_STATUSES as readonly string[]).includes(status);
}

/**
 * A repository's state as its row marks it, or "" for an active one: the
 * state a repository is in when nothing needs saying. A state a newer engine
 * names is shown as it is, never as active.
 */
export function repoStatusWord(status: CodeRepoStatus): string {
  if (status === "active") return "";
  return knownStatus(status) ? REPO_STATUS_WORDS[status] : status;
}

/**
 * The listing's states a reader picks from, as the `status` the engine takes:
 * no value first, which is the engine's own default — the repositories in use
 * — and then every state on its own, in the engine's order.
 */
export const REPO_STATUS_FILTERS: readonly { value: string; label: string }[] = [
  { value: "", label: "Active and archived" },
  ...REPO_STATUSES.map((status) => ({ value: status, label: REPO_STATUS_WORDS[status] })),
];
