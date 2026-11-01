/**
 * The code host's checks and merge queues in this dashboard's words, and the
 * arithmetic every screen that draws them shares: Code › Queue and Checks, a
 * change's and a repository's checks tabs, and the Inbox card.
 *
 * WORDS FOR THE ENGINE'S CLOSED SETS, never the sets themselves: those are
 * `contract/checks.ts`'s, held both ways by `internal/code`. A value a newer
 * engine names that this build has no word for is drawn as the engine sent
 * it, never as a known one.
 *
 * COLOUR IS STATE: a check that passed is the success tone, one that failed
 * the danger tone, and everything that measured nothing — errored, cancelled,
 * skipped, or not ended yet — is no verdict, so it wears none of those.
 * Durations are what a check measured; a check runs no model, so nothing here
 * is ever tokens or money.
 */

import type { CHECK_PURPOSES, CHECK_STATES, QUEUE_STATES } from "~/contract/checks.ts";
import type {
  CodeCandidateRow,
  CodeCheckRow,
  CodeEjectedRow,
  CodeQueueRow,
  CodeReadiness,
  CodeRevisionFacts,
} from "~/protocol/index.ts";

type CheckState = (typeof CHECK_STATES)[number];
type CheckPurpose = (typeof CHECK_PURPOSES)[number];
type QueueState = (typeof QUEUE_STATES)[number];

const CHECK_STATE_WORDS: Readonly<Record<CheckState, string>> = {
  queued: "queued",
  running: "running",
  passed: "passed",
  failed: "failed",
  errored: "errored",
  cancelled: "cancelled",
  skipped: "skipped",
};

/** A check's state in a word — one a newer engine names, as it is. */
export function checkStateWord(state: string): string {
  return CHECK_STATE_WORDS[state as CheckState] ?? state;
}

/**
 * The tone a check's state wears: success for a pass, danger for a failure,
 * info for one running, and neutral for everything that measured nothing.
 */
export function checkStateTone(state: string): "success" | "danger" | "info" | "neutral" {
  switch (state) {
    case "passed":
      return "success";
    case "failed":
      return "danger";
    case "running":
      return "info";
    default:
      return "neutral";
  }
}

const PURPOSE_WORDS: Readonly<Record<CheckPurpose, string>> = {
  head: "a change's head",
  candidate: "a queue's candidate",
  tip: "a branch's tip",
  bisect: "a landing, bisected",
};

/** What an attempt was checking, in words. */
export function checkPurposeWord(purpose: string | undefined): string {
  if (!purpose) return "";
  return PURPOSE_WORDS[purpose as CheckPurpose] ?? purpose;
}

const QUEUE_STATE_WORDS: Readonly<Record<QueueState, string>> = {
  waiting: "waiting",
  testing: "testing",
  landed: "landed",
  ejected: "ejected",
  dequeued: "dequeued",
};

/** Where a queue entry stands, in a word. */
export function queueStateWord(state: string): string {
  return QUEUE_STATE_WORDS[state as QueueState] ?? state;
}

/**
 * Why a queue put an entry out (`code.QueueEndReason`'s ejections), in a
 * sentence's words: what the engine found, never a guess at who is to blame.
 */
const EJECT_WORDS: Readonly<Record<string, string>> = {
  check_failed: "a required check failed",
  conflict: "its merge conflicted",
  gate_unmet: "it no longer met the branch's gate",
  head_moved: "its head moved",
  not_fast_forward: "the branch moved under the queue",
};

/** Why an entry was ejected, in words — a reason a newer engine names, as it sent it. */
export function ejectReasonWord(reason: string): string {
  return EJECT_WORDS[reason] ?? reason;
}

/**
 * The attempt that stands for each check: its latest, by name — an earlier
 * attempt was rerun, and what it said is history, not the check's state.
 * In the order the checks were first met.
 */
export function latestAttempts(rows: readonly CodeCheckRow[]): CodeCheckRow[] {
  const latest = new Map<string, CodeCheckRow>();
  for (const row of rows) {
    const key = `${row.target}\u0000${row.name}`;
    const held = latest.get(key);
    if (!held || row.attempt > held.attempt) latest.set(key, row);
  }
  return [...latest.values()];
}

/** One part of a meter of checks: how many stand in one state. */
export interface MeterPart {
  id: "passed" | "failed" | "running";
  label: string;
  value: number;
  tone: "success" | "danger" | "info";
}

/**
 * A meter of a commit's REQUIRED checks — the ones its branch's gate waits
 * on — by their latest attempts: how many passed, how many failed, how many
 * are running, of how many. The rest (queued, or ended measuring nothing) is
 * the meter's remainder: not yet a verdict.
 */
export function requiredMeter(rows: readonly CodeCheckRow[]): {
  parts: MeterPart[];
  total: number;
} {
  const required = latestAttempts(rows).filter((r) => r.required);
  const count = (state: string) => required.filter((r) => r.state === state).length;
  return {
    parts: [
      { id: "passed", label: "passed", value: count("passed"), tone: "success" },
      { id: "failed", label: "failed", value: count("failed"), tone: "danger" },
      { id: "running", label: "running", value: count("running"), tone: "info" },
    ],
    total: required.length,
  };
}

/**
 * The longest a commit's required checks may run, in seconds: the largest
 * timeout their definitions declare — what an entry's elapsed time is read
 * against, since the queue waits for its slowest required check. 0 where none
 * declares one.
 */
export function longestTimeout(rows: readonly CodeCheckRow[]): number {
  return latestAttempts(rows)
    .filter((r) => r.required)
    .reduce((most, r) => Math.max(most, r.timeout_seconds ?? 0), 0);
}

/** One queue entry as a band draws it: the entry, and the train base it is tested on. */
export interface QueuePlace {
  row: CodeQueueRow;
  /** The candidate testing it, where one is. */
  candidate: CodeCandidateRow | null;
}

/** One band of the Queue section: a repository's branch, its live entries in queue order, and what it put out. */
export interface QueueBand {
  key: string;
  repo: string;
  target: string;
  places: QueuePlace[];
  ejected: CodeEjectedRow[];
}

/**
 * The live entries of merge queues as the Queue section draws them: a BAND
 * per repository and branch, in the order the engine first named each, its
 * entries in queue order — the engine's — each beside the candidate testing
 * it (the train whose entries name it), and the band's ejections beside its
 * live ones. A branch whose queue holds nothing live but ejected an entry
 * lately still gets its band: why the queue shrank is the question.
 */
export function queueBands(
  queue: readonly CodeQueueRow[],
  trains: readonly CodeCandidateRow[],
  ejected: readonly CodeEjectedRow[],
): QueueBand[] {
  const bands = new Map<string, QueueBand>();
  const band = (repo: string, target: string): QueueBand => {
    const key = `${repo}\u0000${target}`;
    let held = bands.get(key);
    if (!held) {
      held = { key, repo, target, places: [], ejected: [] };
      bands.set(key, held);
    }
    return held;
  };
  for (const row of queue) {
    const candidate =
      trains.find(
        (c) =>
          c.target_ref === row.target_ref &&
          (c.candidate_id === row.candidate_id ||
            c.entries.some((e) => e.entry_id === row.entry_id)),
      ) ?? null;
    band(row.repo, row.target_ref).places.push({ row, candidate });
  }
  for (const out of ejected) band(out.repo, out.target_ref).ejected.push(out);
  for (const b of bands.values()) b.places.sort((x, y) => x.row.place - y.row.place);
  return [...bands.values()];
}

/** One required check of a change's head, as the Inbox card says what it measured. */
export interface Measured {
  name: string;
  state: string;
  /** How long it ran, where it ran to an ending; null before. */
  durationMs: number | null;
}

/**
 * What a change's head revision's REQUIRED checks measured, by their latest
 * attempts, in the order the engine listed them: each one's state and how
 * long it ran. Never tokens: a check runs no model.
 */
export function measuredChecks(rows: readonly CodeCheckRow[], head: string): Measured[] {
  return latestAttempts(rows.filter((r) => r.oid === head && r.required)).map((r) => ({
    name: r.name,
    state: r.state,
    durationMs: r.duration_ms ?? null,
  }));
}

/** A revision's change classes, counted: how many touched paths change only whitespace, only comments. */
export interface ClassCount {
  whitespaceOnly: number;
  commentOnly: number;
  /** How many paths the revision touches, or null where it touches everything (no list). */
  touched: number | null;
  /** Every touched path was classified: one in neither list changes code. */
  complete: boolean;
}

/**
 * A revision's change classes as counts over its touched paths, or null
 * where its facts are not recorded yet or carry no class at all — a revision
 * no lexer classified says nothing, which is not "every path is code".
 */
export function classCount(facts: CodeRevisionFacts | undefined): ClassCount | null {
  const classes = facts?.classes;
  if (!facts || !classes) return null;
  const whitespaceOnly = classes.whitespace_only?.length ?? 0;
  const commentOnly = classes.comment_only?.length ?? 0;
  if (whitespaceOnly === 0 && commentOnly === 0 && !classes.complete) return null;
  return {
    whitespaceOnly,
    commentOnly,
    touched: facts.touches_all ? null : facts.touched.length,
    complete: Boolean(classes.complete),
  };
}

/**
 * The human paths that asked for THIS person: the gate's human_approval
 * precondition where it is unmet and waits on the reader — its sentence,
 * which names the globs the revision touches and the branch that holds them
 * to a person's approval. Null where no human path asks the reader: the gate
 * holds none, the revision touches none, a person already approved, or it
 * waits on somebody else.
 */
export function humanPathsAsking(
  readiness: CodeReadiness | undefined,
  handle: string,
): string | null {
  if (!readiness || !handle) return null;
  const pre = readiness.preconditions.find(
    (p) =>
      p.kind === "human_approval" &&
      p.state === "unmet" &&
      (p.waits_on ?? []).some((w) => w.handle === handle),
  );
  return pre ? pre.detail : null;
}

/**
 * Whether a change's target holds it to checks at all: its readiness judges
 * a `check` precondition. A change into an ungated branch has none, and its
 * card says nothing of checks rather than "none measured".
 */
export function gatedByChecks(readiness: CodeReadiness | undefined): boolean {
  return Boolean(readiness?.preconditions.some((p) => p.kind === "check"));
}
