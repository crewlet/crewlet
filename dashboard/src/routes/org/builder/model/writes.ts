/**
 * The settings write of a save: signing it, and settling one whose answer
 * never arrived — and reading the company a draft stands on.
 *
 * THE CHART'S WRITES NEED NONE OF THIS. Each carries an operation id the
 * chart's ledger recognises (`save.ts`), so an unknown outcome is resolved by
 * sending the same step again, and the ledger answers the first arrival's
 * outcome. The settings surface has no such ledger: its write is conditional
 * on a revision and signed in its audit summary, and what follows is how a
 * lost answer is settled from the revision history instead.
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
 * AN UPDATE WAITS FOR THE NODE TO CATCH UP. A settings 409 names the revision
 * the node holds, and the document the draft is rebased onto has to be that
 * revision or a later one. A node behind a load balancer, or one still
 * applying, can answer `GET /config` with an older document, and rebasing onto
 * that would lose the very change the conflict was about. [readyToUpdate]
 * reads the active document and accepts it only when it is the conflict's
 * revision or a descendant of it. The chart needs no such wait: every chart
 * read is linearizable, so the answer is at least as new as anything this
 * node was told.
 */

import type { ChartRead, CompanyDocument, ConfigRevision } from "~/protocol/index.ts";
import { isRecord } from "./json.ts";
import type { KeySource } from "./keys.ts";
import type { CompanyReading } from "./reducer.ts";
import {
  revisionOfEtag,
  type BuilderMode,
  type EngineTransport,
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
  transport: EngineTransport,
  attempt: SaveAttempt,
  currentRevisionId: string | null,
  signal: AbortSignal,
): Promise<Settlement> {
  let current = currentRevisionId;
  if (current === null) {
    const answer = await transport.settings(signal);
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

/** Whether the node can serve the settings a conflicted draft is updated onto. */
export type UpdateReadiness =
  | { readonly kind: "ready"; readonly revisionId: string; readonly document: CompanyDocument }
  /** The node still serves the draft's base or an older revision. */
  | { readonly kind: "behind" }
  | { readonly kind: "unknown"; readonly detail: string };

/**
 * Reads the active settings and accepts them as the base to update a draft
 * onto only when they are `conflictRevisionId` or descend from it. A conflict
 * that named no revision — the chart moved, not the settings — accepts
 * whatever is active.
 */
export async function readyToUpdate(
  transport: EngineTransport,
  conflict: { readonly baseRevision: string; readonly conflictRevisionId: string | null },
  signal: AbortSignal,
): Promise<UpdateReadiness> {
  const answer = await transport.settings(signal);
  if (answer.status !== 200 || !isRecord(answer.body))
    return { kind: "unknown", detail: unanswered(answer) };
  const active = revisionOfEtag(answer.etag);
  if (active === null)
    return { kind: "unknown", detail: "The engine did not name its active revision." };
  const document = answer.body as CompanyDocument;
  const target = conflict.conflictRevisionId;
  if (target === null) return { kind: "ready", revisionId: active, document };
  if (active === conflict.baseRevision) return { kind: "behind" };
  let descends = active === target;

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
  return descends ? { kind: "ready", revisionId: active, document } : { kind: "behind" };
}

/** A chart reading, or why there is none. */
export type ChartReading =
  | { readonly kind: "read"; readonly chart: ChartRead }
  | { readonly kind: "refused"; readonly answer: HttpAnswer };

/**
 * Reads the whole chart, runtime half included where this reader may see it.
 * Any answer but a 200 carrying a chart is handed back as it came, for the
 * caller to say what it means in its own place.
 */
export async function readChart(
  transport: EngineTransport,
  signal: AbortSignal,
): Promise<ChartReading> {
  const answer = await transport.chart(signal);
  if (answer.status === 200 && isRecord(answer.body) && Array.isArray(answer.body.seats)) {
    return { kind: "read", chart: answer.body as unknown as ChartRead };
  }
  return { kind: "refused", answer };
}

/** The company as the engine holds it now, or why it could not be read. */
export type CompanyRead =
  | { readonly kind: "read"; readonly reading: CompanyReading }
  | { readonly kind: "failed"; readonly detail: string };

/**
 * Reads the company whole: the active settings revision (none, in a company
 * that has no settings yet) and the chart. What a save reads back, and what a
 * draft that stopped part way is carried onto.
 */
export async function readCompany(
  transport: EngineTransport,
  signal: AbortSignal,
): Promise<CompanyRead> {
  const [settings, chart] = await Promise.all([
    transport.settings(signal),
    readChart(transport, signal),
  ]);
  if (chart.kind !== "read") return { kind: "failed", detail: unanswered(chart.answer) };
  if (
    settings.status === 404 &&
    isRecord(settings.body) &&
    settings.body.error === "no_active_revision"
  ) {
    return { kind: "read", reading: { settings: null, revision: null, chart: chart.chart } };
  }
  const revision = revisionOfEtag(settings.etag);
  if (settings.status !== 200 || !isRecord(settings.body) || revision === null) {
    return {
      kind: "failed",
      detail:
        settings.status === 200
          ? "The engine did not name its active revision."
          : unanswered(settings),
    };
  }
  return {
    kind: "read",
    reading: { settings: settings.body as CompanyDocument, revision, chart: chart.chart },
  };
}

/** What an update of a draft can be carried onto. */
export type UpdateRead =
  | { readonly kind: "ready"; readonly reading: CompanyReading }
  | { readonly kind: "behind" }
  | { readonly kind: "unknown"; readonly detail: string };

/**
 * The company a conflicted draft is updated onto: the settings once this node
 * serves the revision the conflict named or a later one ([readyToUpdate]),
 * and the chart as it stands.
 */
export async function readUpdate(
  transport: EngineTransport,
  conflict: { readonly baseRevision: string | null; readonly conflictRevisionId: string | null },
  signal: AbortSignal,
): Promise<UpdateRead> {
  // A CREATE DRAFT HAS NO REVISION TO WAIT FOR: whatever the company holds
  // now is what it is carried onto, read whole — the chart once, not twice.
  if (conflict.baseRevision === null) {
    const whole = await readCompany(transport, signal);
    return whole.kind === "read"
      ? { kind: "ready", reading: whole.reading }
      : { kind: "unknown", detail: whole.detail };
  }
  const chart = await readChart(transport, signal);
  if (chart.kind !== "read") return { kind: "unknown", detail: unanswered(chart.answer) };
  const settings = await readyToUpdate(
    transport,
    { baseRevision: conflict.baseRevision, conflictRevisionId: conflict.conflictRevisionId },
    signal,
  );
  if (settings.kind !== "ready") return settings;
  return {
    kind: "ready",
    reading: { settings: settings.document, revision: settings.revisionId, chart: chart.chart },
  };
}
