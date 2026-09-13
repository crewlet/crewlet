/**
 * Saving, and settling a save whose answer never arrived.
 *
 * A WRITE CAN LAND WITHOUT ITS ANSWER. A PATCH that activated a revision and
 * then lost its connection is, to the browser, indistinguishable from one
 * that never arrived: both are status 0. Pressing Save again then meets the
 * operator's OWN revision as a 409, and a builder that took every 409 as
 * "somebody else saved" would rebase the draft onto its own save and replay
 * every operation a second time, producing duplicate seats or dropping the
 * work as already applied.
 *
 * SO EVERY SAVE IS SIGNED. It carries a write id, minted by the event handler
 * and appended to its audit summary, and after an unanswered attempt the
 * builder reads the revision that is now active. The write landed exactly when
 * that revision's parent is the draft's base and its summary carries the write
 * id: a revision with that parent and some other summary is a colleague's
 * save, and one with this id and another parent cannot exist. A 409 or a 412
 * that follows an unanswered attempt is settled the same way, before it is
 * believed.
 *
 * AN UPDATE WAITS FOR THE NODE TO CATCH UP. A 409 names the revision the node
 * holds, and the document the draft is rebased onto has to be that revision
 * or a later one. A node behind a load balancer, or one still applying, can
 * answer `GET /config` with an older document, and rebasing onto that would
 * lose the very change the conflict was about. [readyToUpdate] reads the
 * active document and accepts it only when it is the conflict's revision or a
 * descendant of it.
 */

import type {
  ConfigRevision,
  ConfigWarning,
  Derived,
  CompanyDocument,
  WriteResult,
} from "~/protocol/index.ts";
import { isRecord } from "./json.ts";
import type { KeySource } from "./keys.ts";
import { fromDocument, toDocument } from "./document.ts";
import { classifyCheck, type CheckOutcome } from "./scheduler.ts";
import {
  checkRequest,
  revisionOfEtag,
  type BuilderMode,
  type ConfigTransport,
  type HttpAnswer,
} from "./transport.ts";

/** A write id: letters, digits, `_` and `-`, bounded like a minted key. */
const WRITE_ID = /^[A-Za-z0-9_-]{8,64}$/;

/** A fresh write id. Call it in the event handler that saves. */
export function newWriteId(source: KeySource): string {
  const id = source.next();
  if (!WRITE_ID.test(id)) {
    throw new RangeError(
      `newWriteId: the key source produced an unusable token: ${JSON.stringify(id)}`,
    );
  }
  return id;
}

/**
 * The audit summary a save sends: the operator's sentence, then the write id
 * in a form no sentence produces by accident.
 */
export function signedSummary(summary: string, writeId: string): string {
  return `${summary.trim()} (write ${writeId})`;
}

/** Whether a summary is signed with a write id. */
export function summaryCarries(summary: string, writeId: string): boolean {
  return summary.endsWith(`(write ${writeId})`);
}

/** One save as it was sent. */
export interface SaveAttempt {
  readonly writeId: string;
  readonly mode: BuilderMode;
  /** The revision the draft was built on; `null` in create mode. */
  readonly baseRevision: string | null;
}

/** What a save's answer means. */
export type SaveOutcome =
  | {
      readonly kind: "saved";
      readonly revisionId: string;
      readonly epoch: number;
      readonly warnings: readonly ConfigWarning[];
      readonly derived: Derived | null;
    }
  /** Refused, with the same meaning a check's answer would have. */
  | { readonly kind: "refused"; readonly outcome: CheckOutcome }
  /** Whether it landed is not known yet: settle it with [settleUnknownWrite]. */
  | { readonly kind: "unknown"; readonly currentRevisionId: string | null };

/**
 * Classifies a save's answer. `afterUnknown` says the previous attempt of this
 * draft was never answered, which is what makes a 409 or 412 ambiguous.
 */
export function classifySave(
  answer: HttpAnswer,
  attempt: SaveAttempt,
  afterUnknown: boolean,
): SaveOutcome {
  const body = isRecord(answer.body) ? answer.body : {};
  if (answer.status === 201) {
    const result = body as Partial<WriteResult>;
    return {
      kind: "saved",
      revisionId: typeof result.revision_id === "string" ? result.revision_id : "",
      epoch: typeof result.epoch === "number" ? result.epoch : 0,
      warnings: Array.isArray(result.warnings) ? result.warnings : [],
      derived: isRecord(result.derived) ? (result.derived as unknown as Derived) : null,
    };
  }
  if (answer.status === 0) return { kind: "unknown", currentRevisionId: null };
  if (afterUnknown && (answer.status === 409 || answer.status === 412)) {
    const current = body.current_revision_id;
    return {
      kind: "unknown",
      currentRevisionId: typeof current === "string" && current !== "" ? current : null,
    };
  }
  return { kind: "refused", outcome: classifyCheck(answer, attempt.mode, attempt.baseRevision) };
}

/** Whether a revision is the one a save wrote. */
export function isRevisionOfWrite(
  revision: Pick<ConfigRevision, "parent_revision_id" | "summary">,
  attempt: SaveAttempt,
): boolean {
  const parent = revision.parent_revision_id ? revision.parent_revision_id : null;
  return (
    parent === attempt.baseRevision &&
    typeof revision.summary === "string" &&
    summaryCarries(revision.summary, attempt.writeId)
  );
}

/** How an unanswered save settled. */
export type Settlement =
  /** It landed, as this revision. */
  | { readonly kind: "landed"; readonly revisionId: string }
  /** It did not land. `currentRevisionId` is what is active instead (`null` for nothing). */
  | { readonly kind: "not_landed"; readonly currentRevisionId: string | null }
  /** Still unknown: the engine could not be asked, or this node does not hold the revision yet. */
  | { readonly kind: "unknown"; readonly detail: string };

/**
 * Settles a save whose answer never arrived, by reading what is active.
 *
 * `currentRevisionId` is the revision a 409 or 412 named, when one did;
 * without it the active revision is read from `GET /config`.
 */
export async function settleUnknownWrite(
  transport: ConfigTransport,
  attempt: SaveAttempt,
  currentRevisionId: string | null,
  signal: AbortSignal,
): Promise<Settlement> {
  let current = currentRevisionId;
  if (current === null) {
    const answer = await transport.current(signal);
    if (
      answer.status === 404 &&
      isRecord(answer.body) &&
      answer.body.error === "no_active_revision"
    ) {
      return { kind: "not_landed", currentRevisionId: null };
    }
    if (answer.status !== 200) return { kind: "unknown", detail: unanswered(answer) };
    current = revisionOfEtag(answer.etag);
    if (current === null)
      return { kind: "unknown", detail: "The engine did not name its active revision." };
  }
  if (current === attempt.baseRevision) return { kind: "not_landed", currentRevisionId: current };

  const answer = await transport.revision(current, signal);
  if (answer.status !== 200 || !isRecord(answer.body)) {
    return {
      kind: "unknown",
      detail:
        answer.status === 404
          ? "This node does not hold the active revision yet."
          : unanswered(answer),
    };
  }
  const revision = answer.body as unknown as ConfigRevision;
  return isRevisionOfWrite(revision, attempt)
    ? { kind: "landed", revisionId: current }
    : { kind: "not_landed", currentRevisionId: current };
}

function unanswered(answer: HttpAnswer): string {
  if (answer.status === 0) return "The engine could not be reached.";
  const detail =
    isRecord(answer.body) && typeof answer.body.detail === "string" ? answer.body.detail : "";
  return detail || `The engine answered with status ${answer.status}.`;
}

/**
 * How many revisions [readyToUpdate] follows back from the active one looking
 * for the conflict's revision.
 *
 * Each step is one request. The walk only has to cover the writes activated
 * between the conflict being reported and the operator choosing to update
 * their draft, which is seconds to minutes of a fleet's activity; twenty-five
 * covers far more writes than any fleet activates in that time while bounding
 * what one click can send. Past it the node is reported as not caught up,
 * which a second attempt resolves.
 */
export const UPDATE_ANCESTRY_LIMIT = 25;

/** Whether the node can serve the document a conflicted draft is updated onto. */
export type UpdateReadiness =
  | {
      readonly kind: "ready";
      readonly revisionId: string;
      readonly document: CompanyDocument;
      /**
       * The engine's derivation of that document, from a dry run of it with no
       * changes. The draft is rebased onto nodes keyed by the engine's handles,
       * and without it a seat declaring no handle could only be keyed by its
       * path, which names nothing the log recorded.
       */
      readonly derived: Derived;
    }
  /** The node still serves the draft's base or an older revision. */
  | { readonly kind: "behind" }
  | { readonly kind: "unknown"; readonly detail: string };

/**
 * Reads the active document and accepts it as the base to update a draft onto
 * only when it is `conflictRevisionId` or descends from it. A conflict that
 * named no revision accepts any active revision other than the draft's base.
 */
export async function readyToUpdate(
  transport: ConfigTransport,
  conflict: { readonly baseRevision: string; readonly conflictRevisionId: string | null },
  signal: AbortSignal,
): Promise<UpdateReadiness> {
  const answer = await transport.current(signal);
  if (answer.status !== 200 || !isRecord(answer.body))
    return { kind: "unknown", detail: unanswered(answer) };
  const active = revisionOfEtag(answer.etag);
  if (active === null)
    return { kind: "unknown", detail: "The engine did not name its active revision." };
  const document = answer.body as CompanyDocument;
  if (active === conflict.baseRevision) return { kind: "behind" };
  const target = conflict.conflictRevisionId;
  let descends = target === null || active === target;

  let at: string | null = active;
  for (let step = 0; !descends && step < UPDATE_ANCESTRY_LIMIT && at !== null; step++) {
    const revision = await transport.revision(at, signal);
    if (revision.status !== 200 || !isRecord(revision.body))
      return { kind: "unknown", detail: unanswered(revision) };
    const parent = revision.body.parent_revision_id;
    at = typeof parent === "string" && parent !== "" ? parent : null;
    if (at === target) descends = true;
    else if (at === conflict.baseRevision) return { kind: "behind" };
  }
  if (!descends) return { kind: "behind" };

  const request = checkRequest({
    mode: "edit",
    baseRevision: active,
    base: document,
    sent: toDocument(fromDocument(document, null)),
  });
  const checked = classifyCheck(await transport.send(request, signal), "edit", active);
  if ((checked.status === "clean" || checked.status === "problems") && checked.derived) {
    return { kind: "ready", revisionId: active, document, derived: checked.derived };
  }
  if (checked.status === "conflict") {
    return {
      kind: "unknown",
      detail: "The configuration changed again while it was being read. Try again.",
    };
  }
  return {
    kind: "unknown",
    detail:
      checked.status === "unreachable"
        ? checked.detail || "The engine could not be reached."
        : "The engine did not describe the organization.",
  };
}
