/**
 * A change's readiness in a PERSON'S words, composed from what the engine
 * judged rather than printed from its sentence.
 *
 * The engine writes one sentence per precondition (`detail`, and `first`) for
 * the seats that call its tools: handles quoted with their kind (`person
 * "ada"`), branches as full refs, lowercase. Those are a model's words. The
 * readiness also carries each precondition's kind, where it stands and whom
 * it waits on, so every surface that draws a readiness for a person (the
 * merge panel, the peek, the "Needs a person" band, the listing's meter)
 * words it here, once: parties by the name the dashboard shows for them, the
 * target by its branch name, one term for the count.
 */

import { PRECONDITION_KINDS } from "~/contract/code.ts";
import { plural } from "~/lib/format.ts";
import { refName } from "~/lib/code.ts";
import type {
  CodeChangeRow,
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
}

/** Names in a sentence: "Ada", "Ada and Bo", "Ada, Bo and Cy". */
function namesOf(parties: readonly CodeParty[], nameOf: (handle: string) => string): string {
  const names = parties.map((p) => nameOf(p.handle));
  if (names.length <= 1) return names[0] ?? "";
  return `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}

function waitingOn(p: CodePrecondition, nameOf: (handle: string) => string): string {
  const who = namesOf(p.waits_on ?? [], nameOf);
  return who ? ` Waiting on ${who}.` : "";
}

type Kind = (typeof PRECONDITION_KINDS)[number];

/**
 * One precondition in a person's words, or null for one that is met: its
 * checklist word and the Met tag already say all there is.
 */
export function preconditionSentence(
  p: CodePrecondition,
  { change, nameOf }: ReadinessSubject,
): string | null {
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
      const waits = p.waits_on ?? [];
      const ownerOnly =
        waits.length === 1 &&
        waits[0]!.handle === change.owner &&
        waits[0]!.kind === change.owner_kind;
      return ownerOnly
        ? `No approval counts on revision ${n}, and nobody who may approve it has been asked. ${owner} asks a reviewer.`
        : `No approval counts on revision ${n} yet.${waitingOn(p, nameOf)}`;
    }
    case "human_approval":
      return computing
        ? `Which of ${target}'s person-approved paths revision ${n} touches is still being read.`
        : `Revision ${n} touches paths of ${target} that take a person's approval.${waitingOn(p, nameOf)}`;
    case "owners_approval":
      return computing
        ? `Which path owners revision ${n} touches is still being read.`
        : `Revision ${n} touches paths whose owners have not all approved.${waitingOn(p, nameOf)}`;
    case "blocker": {
      const count = (p.blockers?.length ?? 0) + (p.blockers_omitted ?? 0);
      const standing =
        count > 0
          ? `${plural(count, "blocking finding")} ${count === 1 ? "stands" : "stand"}`
          : "A blocking finding stands";
      return `${standing}. Its raiser, a maintainer of the repository or a person clears it.${waitingOn(p, nameOf)}`;
    }
    case "check":
      return computing
        ? `The checks ${target} requires are still running.`
        : `The checks ${target} requires have not all passed.${waitingOn(p, nameOf)}`;
    case "mergeable":
      return computing
        ? `Whether revision ${n} merges cleanly into ${target} is still being read.`
        : `Revision ${n} conflicts with ${target}. ${owner} merges ${target} into it.`;
    case "merge_authority":
      return `Merging into ${target} takes access you do not hold.${waitingOn(p, nameOf)}`;
    case "queue":
      return `It is in ${target}'s merge queue, which lands it.`;
    default:
      return computing ? "Still being judged." : `Not met yet.${waitingOn(p, nameOf)}`;
  }
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
