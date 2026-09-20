/**
 * Saving, and settling a save whose answer never arrived.
 *
 * A WRITE CAN LAND WITHOUT ITS ANSWER. A PATCH that activated a revision and
 * then lost its connection is, to the browser, indistinguishable from one
 * that never arrived: both are status 0. So is a 5xx: a gateway that gave up
 * waiting (502, 504) says nothing about the engine behind it, and the engine
 * itself stores the revision before it activates it, so a failure reported
 * after that point leaves a revision that may be active. The one 5xx that is
 * certain is `503 draining`, which the drain gate refuses before the handler
 * runs. Pressing Save again then meets the
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
 * THE WRITE MAY NO LONGER BE THE ACTIVE REVISION. A colleague who saved in
 * the seconds between the lost answer and the settling read built on it, so
 * the active revision is theirs and its parent is this write. Settling reads
 * back from the active revision to the draft's base, one parent at a time,
 * and finds the write wherever it sits in that line. Reading only the active
 * revision would call such a write not landed, and the conflict flow would
 * then replay every operation onto a document that already holds them.
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

/** Whether a value is shaped like a write id, as one read back from storage must be. */
export function isWriteId(value: unknown): value is string {
  return typeof value === "string" && WRITE_ID.test(value);
}

/** A fresh write id. Call it in the event handler that saves. */
export function newWriteId(source: KeySource): string {
  const id = source.next();
  if (!isWriteId(id)) {
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
  /**
   * Whether it landed is not known yet: settle it with [settleUnknownWrite].
   * `detail` is what the answer said, if it said anything, for a failure
   * that settles as not landed and would otherwise be reported with no cause.
   */
  | {
      readonly kind: "unknown";
      readonly currentRevisionId: string | null;
      readonly detail: string;
    };

/**
 * Classifies a save's answer. `afterUnknown` says the outcome of the previous
 * attempt of this draft was unknown, which is what makes a 409 or 412
 * ambiguous.
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
  const detail =
    typeof body.detail === "string" && body.detail !== ""
      ? body.detail
      : typeof body.error === "string"
        ? body.error
        : "";
  // THE ONE 5xx THAT IS CERTAIN. The drain gate refuses a write before the
  // handler runs (internal/api's drainGate wraps the mux), so a `503
  // draining` stored nothing and needs no settling read — where every other
  // 5xx may have been raised after the revision was stored.
  const refusedBeforeStoring = answer.status === 503 && body.error === "draining";
  if (answer.status === 0 || (answer.status >= 500 && !refusedBeforeStoring)) {
    return { kind: "unknown", currentRevisionId: null, detail };
  }
  if (afterUnknown && (answer.status === 409 || answer.status === 412)) {
    const current = body.current_revision_id;
    return {
      kind: "unknown",
      currentRevisionId: typeof current === "string" && current !== "" ? current : null,
      detail,
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
  /**
   * It landed, as `revisionId`. `activeRevisionId` is what is active now:
   * the write itself, or a later revision built on it.
   */
  | { readonly kind: "landed"; readonly revisionId: string; readonly activeRevisionId: string }
  /** It did not land. `currentRevisionId` is what is active instead (`null` for nothing). */
  | { readonly kind: "not_landed"; readonly currentRevisionId: string | null }
  /** Still unknown: the engine could not be asked, or this node does not hold the revision yet. */
  | { readonly kind: "unknown"; readonly detail: string };

/**
 * Settles a save whose answer never arrived, by reading what is active and
 * the line of revisions it descends by.
 *
 * `currentRevisionId` is the revision a 409 or 412 named, when one did;
 * without it the active revision is read from `GET /config`. The walk back
 * stops at the first revision whose parent is the draft's base (in create
 * mode, the first revision of all), which is the only place this write can
 * sit, and gives up as unknown past [UPDATE_ANCESTRY_LIMIT].
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

  let at = current;
  for (let step = 0; step < UPDATE_ANCESTRY_LIMIT; step++) {
    const answer = await transport.revision(at, signal);
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
    if (isRevisionOfWrite(revision, attempt)) {
      return { kind: "landed", revisionId: at, activeRevisionId: current };
    }
    const parent = revision.parent_revision_id ? revision.parent_revision_id : null;
    if (parent === null || parent === attempt.baseRevision) {
      return { kind: "not_landed", currentRevisionId: current };
    }
    at = parent;
  }
  return {
    kind: "unknown",
    detail:
      "The configuration has moved on by more revisions than the builder reads back. Check the revision history for this save before saving again.",
  };
}

function unanswered(answer: HttpAnswer): string {
  if (answer.status === 0) return "The engine could not be reached.";
  const detail =
    isRecord(answer.body) && typeof answer.body.detail === "string" ? answer.body.detail : "";
  return detail || `The engine answered with status ${answer.status}.`;
}

/**
 * How many revisions [readyToUpdate] and [settleUnknownWrite] follow back from
 * the active one, looking for the conflict's revision or for the write.
 *
 * Each step is one request. A walk only has to cover the writes activated
 * between the moment it is about (a conflict reported, an answer lost) and
 * the read, which is seconds to minutes of a fleet's activity; twenty-five
 * covers far more writes than any fleet activates in that time while bounding
 * what one click can send. Past it an update reports the node as not caught
 * up, which a second attempt resolves, and a settlement stays unknown.
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
