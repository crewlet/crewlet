/**
 * A change's readiness in a PERSON'S words, composed from what the engine
 * judged rather than printed from its sentence.
 *
 * The engine writes one sentence per precondition (`detail`, and `first`) for
 * the seats that call its tools: handles quoted with their kind (`person
 * "ada"`), branches as full refs, lowercase. Those are a model's words. The
 * readiness also carries each precondition's kind, where it stands and whom
 * it waits on, so every surface that draws a readiness for a person (the
 * merge panel, the peek, Waiting on you, the listing's meter)
 * words it here, once: parties by the name the dashboard shows for them, the
 * target by its branch name, one term for the count.
 */

import { PRECONDITION_KINDS } from "~/contract/code.ts";
import { CHECK_HISTORY_DAYS, checkStateWord } from "~/lib/checks.ts";
import { plural } from "~/lib/format.ts";
import { refName } from "~/lib/code.ts";
import { namesSaid } from "./owners.ts";
import type {
  CodeChangeRow,
  CodeCheckRow,
  CodeParty,
  CodePrecondition,
  CodeReadiness,
} from "~/protocol/index.ts";

/** What the count of a readiness is called, on every surface that draws one. */
export function readinessCount(readiness: CodeReadiness): string {
  return `${readiness.met} of ${plural(readiness.total, "condition")} to merge met`;
}

/** What a sentence about one change needs to know of it. */
export interface ReadinessSubject {
  change: Pick<CodeChangeRow, "state" | "owner" | "owner_kind" | "revision" | "target_ref">;
  /** The name the dashboard draws for a seat or a person, by handle. */
  nameOf: (handle: string) => string;
  /** The reader's handle, whom "Waiting on" calls "you"; unset says every party by name. */
  me?: string;
}

/** Names in a sentence: "Ada", "Ada and Bo", "Ada, Bo and Cy". */
function namesOf(parties: readonly CodeParty[], nameOf: (handle: string) => string): string {
  const names = parties.map((p) => nameOf(p.handle));
  if (names.length <= 1) return names[0] ?? "";
  return `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}

function waitingOn(p: CodePrecondition, { nameOf, me }: ReadinessSubject): string {
  const who = namesOf(p.waits_on ?? [], (h) => (me && h === me ? "you" : nameOf(h)));
  return who ? ` Waiting on ${who}.` : "";
}

type Kind = (typeof PRECONDITION_KINDS)[number];

/** An approval that waits on the owner alone: nobody who may approve it has been asked. */
function ownerOnly(p: CodePrecondition, change: ReadinessSubject["change"]): boolean {
  const waits = p.waits_on ?? [];
  return (
    p.kind === "approval" &&
    waits.length === 1 &&
    waits[0]!.handle === change.owner &&
    waits[0]!.kind === change.owner_kind
  );
}

/** Names in a sentence where any one will do: "Ada", "Ada or Bo", "Ada, Bo or Cy". */
function namesOr(names: readonly string[]): string {
  if (names.length <= 1) return names[0] ?? "";
  return `${names.slice(0, -1).join(", ")} or ${names[names.length - 1]}`;
}

/**
 * One precondition in a person's words, or null for one that is met: its
 * checklist word and the Met tag already say all there is.
 */
export function preconditionSentence(
  p: CodePrecondition,
  subject: ReadinessSubject,
): string | null {
  const { change, nameOf } = subject;
  if (p.state === "met") return null;
  const n = change.revision;
  const target = refName(change.target_ref).name;
  const owner = nameOf(change.owner);
  const computing = p.state === "computing";
  switch (p.kind as Kind) {
    case "draft":
      return change.state === "draft"
        ? `It is a draft. ${owner}, a maintainer of the repository or any person marks it ready for review.`
        : `It is ${change.state}, and only an open change merges.`;
    case "facts":
      return `Revision ${n} is still being measured against ${target}.`;
    case "approval": {
      if (computing) {
        return `An approval of an earlier revision waits for revision ${n} to be measured, which says whether it still counts.`;
      }
      return ownerOnly(p, change)
        ? `No approval counts on revision ${n}, and nobody who may approve it has been asked. ${owner} asks a reviewer.`
        : `No approval counts on revision ${n} yet.${waitingOn(p, subject)}`;
    }
    case "human_approval":
      return computing
        ? `Which of ${target}'s person-approved paths revision ${n} touches is still being read.`
        : `Revision ${n} touches paths of ${target} that take a person's approval.${waitingOn(p, subject)}`;
    case "owners_approval":
      return computing
        ? `Which path owners revision ${n} touches is still being read.`
        : `Revision ${n} touches paths whose owners have not all approved.${waitingOn(p, subject)}`;
    case "blocker": {
      const count = (p.blockers?.length ?? 0) + (p.blockers_omitted ?? 0);
      const standing =
        count > 0
          ? `${plural(count, "blocking finding")} ${count === 1 ? "stands" : "stand"}`
          : "A blocking finding stands";
      return `${standing}. Its raiser, a maintainer of the repository or a person clears it.${waitingOn(p, subject)}`;
    }
    case "check":
      return computing
        ? `The checks ${target} requires are still running.`
        : `The checks ${target} requires have not all passed.${waitingOn(p, subject)}`;
    case "mergeable":
      return computing
        ? `Whether revision ${n} merges cleanly into ${target} is still being read.`
        : `Revision ${n} conflicts with ${target}. ${owner} merges ${target} into it.`;
    case "merge_authority":
      return `Merging into ${target} takes access you do not hold.${waitingOn(p, subject)}`;
    case "queue":
      return `It is in ${target}'s merge queue, which lands it.`;
    default:
      return computing ? "Still being judged." : `Not met yet.${waitingOn(p, subject)}`;
  }
}

/** A met condition the merge box draws on its own (a draft, merge authority, a newer kind). */
export function metSentence(p: CodePrecondition, subject: ReadinessSubject): string {
  const target = refName(subject.change.target_ref).name;
  switch (p.kind as Kind) {
    case "draft":
      return "It is open for review, not a draft.";
    case "merge_authority":
      return `Your access lets you merge into ${target}.`;
  }
  return "It is met.";
}

/**
 * The sentence a reader acts on next: the first unmet precondition's, else
 * the first still being judged, else that every condition is met.
 */
export function readinessNext(readiness: CodeReadiness, subject: ReadinessSubject): string {
  const next =
    readiness.preconditions.find((p) => p.state === "unmet") ??
    readiness.preconditions.find((p) => p.state === "computing");
  return (next && preconditionSentence(next, subject)) ?? "Every condition to merge is met.";
}

const APPROVAL_KINDS = new Set(["approval", "human_approval", "owners_approval"]);

/** A check that has not passed yet: its latest attempt neither passed nor was skipped. */
export function checkWaits(row: Pick<CodeCheckRow, "state">): boolean {
  return row.state !== "passed" && row.state !== "skipped";
}

/**
 * What a change still waits on, as a sentence: "It waits on approvals from
 * Code Reviewer and you, and on e2e." One phrase per kind of condition, in the
 * engine's order; `lead` is the sentence's start ("It waits on" or "Waits on").
 */
export function readinessWaits(
  readiness: CodeReadiness,
  subject: ReadinessSubject,
  checks: readonly CodeCheckRow[] | null,
  lead = "It waits on",
): string {
  const target = refName(subject.change.target_ref).name;
  const waiting = readiness.preconditions.filter((p) => p.state !== "met");
  const phrases: string[] = [];
  let approvals = false;
  for (const p of waiting) {
    if (APPROVAL_KINDS.has(p.kind)) {
      if (approvals) continue;
      approvals = true;
      phrases.push(
        approvalsPhrase(
          waiting.filter((q) => APPROVAL_KINDS.has(q.kind)),
          subject,
        ),
      );
      continue;
    }
    phrases.push(waitPhrase(p, target, checks));
  }
  if (phrases.length === 0)
    return lead === "It waits on" ? "It is ready to merge." : "Ready to merge";
  const said =
    phrases.length === 1
      ? phrases[0]!
      : phrases.length === 2
        ? `${phrases[0]}, and on ${phrases[1]}`
        : `${phrases.slice(0, -1).join(", on ")} and on ${phrases[phrases.length - 1]}`;
  return `${lead} ${said}${lead === "It waits on" ? "." : ""}`;
}

/**
 * The approval conditions' phrase after "waits on": one approval meets each, so a
 * condition's candidates are joined with "or", and an unasked one names its asker.
 */
function approvalsPhrase(pre: readonly CodePrecondition[], subject: ReadinessSubject): string {
  const { change, nameOf, me } = subject;
  const name = (h: string) => (me && h === me ? "you" : nameOf(h));
  const parts = [
    ...new Set(
      pre.map((p) => {
        if (ownerOnly(p, change)) return `a reviewer asked by ${name(change.owner)}`;
        const who = (p.waits_on ?? []).filter((w) => w.handle !== change.owner);
        return namesOr(who.map((w) => name(w.handle)));
      }),
    ),
  ].filter((part) => part !== "");
  if (parts.length === 0) return "an approval";
  if (parts.length === 1 && parts[0]!.startsWith("a reviewer asked by")) return parts[0]!;
  return parts.length === 1 ? `an approval from ${parts[0]}` : `approvals from ${namesSaid(parts)}`;
}

/** One condition's phrase after "waits on". */
function waitPhrase(p: CodePrecondition, target: string, checks: readonly CodeCheckRow[] | null) {
  switch (p.kind as Kind) {
    case "check": {
      const names = (checks ?? []).filter((r) => r.required && checkWaits(r)).map((r) => r.name);
      return names.length > 0 ? namesSaid(names) : "its required checks";
    }
    case "blocker":
      return plural((p.blockers?.length ?? 0) + (p.blockers_omitted ?? 0) || 1, "blocking finding");
    case "facts":
      return "its facts being measured";
    case "mergeable":
      return p.state === "unmet"
        ? `a conflict with ${target} being resolved`
        : `its merge onto ${target} being measured`;
    case "draft":
      return "being marked ready for review";
    case "merge_authority":
      return `somebody who may merge into ${target}`;
    case "queue":
      return "its queue";
  }
  return `its ${p.kind.replaceAll("_", " ")} condition`;
}

/**
 * The merge box's Reviews section, in words: its title and its sentence. One
 * approval meets each condition, so the title counts conditions, not parties.
 */
export function reviewsWords(
  pre: readonly CodePrecondition[],
  subject: ReadinessSubject,
  standings: readonly { handle: string; verdict: string; revision: number }[],
  authors: readonly string[] = [],
): { title: string; sentence: string } {
  const { change, nameOf, me } = subject;
  const target = refName(change.target_ref).name;
  const name = (h: string) => (me && h === me ? "you" : nameOf(h));
  const n = change.revision;
  const notApproving = standings.filter((s) => s.verdict !== "approve" || s.revision !== n);
  const said = notApproving.map(
    (s) => `${name(s.handle)} ${verdictPhrase(s.verdict)} revision ${s.revision}`,
  );
  const unmet = pre.filter((p) => p.state === "unmet");
  if (unmet.length > 0) {
    // NEVER THE OWNER OR AN AUTHOR as a reviewer: an unasked condition says who asks.
    const seen = new Set([change.owner, ...authors, ...standings.map((s) => s.handle)]);
    const parts = [...said];
    let unasked: string | null = null;
    for (const p of unmet) {
      if (ownerOnly(p, change)) {
        unasked ??= preconditionSentence(p, subject);
        continue;
      }
      const silent = (p.waits_on ?? []).map((w) => w.handle).filter((h) => !seen.has(h));
      for (const h of silent) seen.add(h);
      if (silent.length > 0) {
        const have = silent.length > 1 || silent[0] === me ? "have" : "has";
        parts.push(`${namesSaid(silent.map(name))} ${have} not reviewed yet`);
      }
    }
    const sentence = [parts.length > 0 ? `${capital(parts.join("; "))}.` : "", unasked ?? ""]
      .filter((x) => x !== "")
      .join(" ");
    return {
      title: unmet.length > 1 ? `Waits on ${unmet.length} approvals` : "Waits on an approval",
      sentence: sentence || (preconditionSentence(unmet[0]!, subject) ?? ""),
    };
  }
  const computing = pre.find((p) => p.state === "computing");
  if (computing) {
    return {
      title: "Its approvals are being judged",
      sentence: preconditionSentence(computing, subject) ?? "",
    };
  }
  if (pre.length > 0) {
    const approvers = standings.filter((s) => s.verdict === "approve" && s.revision === n);
    return {
      title: "Approved",
      sentence:
        approvers.length > 0
          ? `${capital(namesSaid(approvers.map((s) => name(s.handle))))} approved revision ${n}.`
          : `An approval of an earlier revision carries to revision ${n}.`,
    };
  }
  return {
    title: "No approval is required",
    sentence: `${target}'s gate asks for none.${said.length > 0 ? ` ${capital(said.join("; "))}.` : ""}`,
  };
}

/** A standing verdict, as a sentence says it before "revision N". */
export function verdictPhrase(verdict: string): string {
  switch (verdict) {
    case "approve":
      return "approved";
    case "request_changes":
      return "requested changes on";
    case "comment":
      return "commented on";
  }
  return `${verdict.replaceAll("_", " ")} on`;
}

function capital(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/** The merge box's Checks section, in words, over each check's latest attempt. */
export function checksWords(
  rows: readonly CodeCheckRow[],
  target: string,
  kept: boolean,
): { title: string; sentence: string } {
  const m = rows.length;
  if (m === 0) {
    return kept
      ? {
          title: "No check has run on its head",
          sentence: `${target}'s .crewlet/checks.yaml defines the checks a head runs.`,
        }
      : {
          title: "No check of its head is kept",
          sentence: `Check attempts are kept ${CHECK_HISTORY_DAYS} days.`,
        };
  }
  // NOT PASSING: every finished state but passed and skipped (failed, errored,
  // cancelled, or one a newer engine names), as the engine's gate counts them.
  const running = rows.filter((r) => r.state === "running" || r.state === "queued");
  const failing = rows.filter((r) => checkWaits(r) && !running.includes(r));
  const passed = rows.filter((r) => !checkWaits(r));
  const required = rows.filter((r) => r.required).length;
  const requiredClause =
    required === 0
      ? "None is required to merge."
      : required === m
        ? m === 1
          ? "It is required to merge."
          : m === 2
            ? "Both are required to merge."
            : `All ${m} are required to merge.`
        : `${required} of them ${required === 1 ? "is" : "are"} required to merge.`;
  const named = (list: readonly CodeCheckRow[]) => {
    const names = list.slice(0, 3).map((r) => `${r.name} ${checkStateWord(r.state)}`);
    return namesSaid(names, Math.max(0, list.length - 3));
  };
  const failedOnly = failing.every((r) => r.state === "failed" || r.state === "errored");
  const notPassed = failedOnly ? "failed" : "did not pass";
  if (failing.some((r) => r.required)) {
    return {
      title: `${failing.length} of ${plural(m, "check")} ${notPassed}`,
      sentence: `${named(rows)}. ${requiredClause}`,
    };
  }
  if (running.length > 0) {
    const others = m - running.length;
    return {
      title: `${running.length} of ${plural(m, "check")} ${running.length === 1 ? "is" : "are"} running`,
      sentence: `${others > 0 ? `${passed.length} passed and ` : ""}${running.length} ${running.length === 1 ? "is" : "are"} running. ${requiredClause}`,
    };
  }
  if (failing.length > 0) {
    return {
      title: `${plural(m, "check")} ran, ${failing.length} ${notPassed}`,
      sentence: `${named(failing)}, and ${failing.length === 1 ? "is" : "are"} not required.`,
    };
  }
  return {
    title: m === 1 ? "Its check passed" : m === 2 ? "Both checks passed" : `All ${m} checks passed`,
    sentence: requiredClause,
  };
}

/** The merge box's Findings section, in words. `severities` counts the loaded open findings by weight, null where not all are loaded. */
export function findingsWords(
  pre: readonly CodePrecondition[],
  subject: ReadinessSubject,
  open: number,
  severities: ReadonlyMap<string, number> | null,
): { title: string; sentence: string } {
  const unmet = pre.find((p) => p.state !== "met");
  if (unmet) {
    const count = (unmet.blockers?.length ?? 0) + (unmet.blockers_omitted ?? 0);
    return {
      title: count > 1 ? `${count} blockers stand` : "A blocker stands",
      sentence: preconditionSentence(unmet, subject) ?? "",
    };
  }
  if (open === 0) return { title: "No blocker stands", sentence: "No finding is open." };
  if (!severities) {
    return {
      title: "No blocker stands",
      sentence: `None of its ${plural(open, "open finding")} is a blocker.`,
    };
  }
  const weights = [...severities].map(([severity, n]) =>
    n === 1 ? `${/^[aeiou]/.test(severity) ? "an" : "a"} ${severity}` : `${n} ${severity}s`,
  );
  return {
    title: "No blocker stands",
    sentence:
      open === 1
        ? `Its open finding is ${weights[0]}. Only a blocker holds the merge.`
        : `Its ${open} open findings are ${namesSaid(weights)}. Only a blocker holds the merge.`,
  };
}
