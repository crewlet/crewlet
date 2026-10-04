/**
 * What the builder core needs from the world, as small injected interfaces,
 * and the configuration requests it sends.
 *
 * NOTHING IN THIS DIRECTORY TOUCHES A GLOBAL. Time, storage, randomness and
 * the network all arrive as arguments, so every rule here runs under a test
 * with a fake clock and a scripted engine, and the one place that binds them
 * to `fetch`, `setTimeout` and `sessionStorage` is the UI that owns their
 * lifetimes. `model/boundary.test.ts` holds the directory to it.
 *
 * ONE REQUEST SHAPE PER MODE, for a check and a save alike, because a check
 * that sent something other than what the save will send validates a
 * different write:
 *
 * - EDIT mode is `PATCH /config` with a JSON merge patch of exactly the keys
 *   the builder changed (`buildPatch`) and `If-Match` naming the base
 *   revision, so a newer revision is a 409 rather than an overwrite.
 * - CREATE mode is `PUT /config` with the whole document and
 *   `If-None-Match: *`, so a company that appeared meanwhile, anywhere in the
 *   fleet, is a 412 rather than a replacement.
 * - A LEAD's draft (`scope`, `scope.ts`) is one unit they lead, and goes as
 *   `PUT /config/units/{key}` with that unit whole — its seats and child units
 *   included — and `If-Match` naming the base revision: a lead may not read
 *   the company document, so they cannot patch it, and the engine admits this
 *   write when everything it changes is inside what they lead. Unchanged, the
 *   draft is checked all the same: the engine refuses a lead the write, which
 *   would re-publish the company, and answers its dry run.
 *
 * A check adds `dry_run=true` and no summary (the engine lifts one out but
 * does not require it on a dry run); a save adds `_summary` to the body.
 *
 * THE MISSING SUMMARY IS WHAT KEEPS A CHECK A CHECK on a node that predates
 * dry runs. Such a build ignores a query parameter it does not know, so it
 * would take `?dry_run=true` for a real write, and it refuses every write
 * without an audit summary before doing anything else (`400
 * summary_required`). A check that carried a summary, in the body or the
 * `X-Summary` header, would be stored and activated by that node on every
 * edit during a rolling upgrade.
 */

import type { CompanyDocument } from "~/protocol/index.ts";
import { buildPatch, type IndexedDocument } from "./document.ts";

/** Whether the builder edits the active company or creates the first one. */
export type BuilderMode = "edit" | "create";

/**
 * What the engine answered: its status, its parsed body and its entity tag.
 * Status 0 is a request that was never fully answered (unreachable, timed
 * out, or a body that broke part way through).
 */
export interface HttpAnswer {
  readonly status: number;
  readonly body: unknown;
  readonly etag?: string | null;
}

/** A configuration write or dry run, or a lead's read of their unit, ready for the transport. */
export interface ConfigRequest {
  readonly method: "GET" | "PUT" | "PATCH";
  /** `/config`, or a lead's unit ([unitPath]). */
  readonly path: string;
  readonly query: Readonly<Record<string, string>>;
  readonly contentType: "application/json" | "application/merge-patch+json";
  readonly headers: Readonly<Record<string, string>>;
  /** Absent on a read. */
  readonly body?: Readonly<Record<string, unknown>>;
}

/** Where one unit is read and written: `/config/units/{key}`. */
export function unitPath(key: string): string {
  return `/config/units/${encodeURIComponent(key)}`;
}

/**
 * The unit a lead's draft is about, out of the document that stands for it
 * (`scope.ts`): its one top-level unit, whole.
 */
export function scopedUnit(document: CompanyDocument | null): Readonly<Record<string, unknown>> {
  return (document?.units?.[0] ?? {}) as Readonly<Record<string, unknown>>;
}

/**
 * The network, as the builder uses it. Every method RESOLVES with the answer,
 * refusals included; it rejects only when `signal` aborted it.
 */
export interface ConfigTransport {
  send(request: ConfigRequest, signal: AbortSignal): Promise<HttpAnswer>;
  /** `GET /config`. Its entity tag names the active revision. */
  current(signal: AbortSignal): Promise<HttpAnswer>;
  /** `GET /config/revisions/{id}`. */
  revision(id: string, signal: AbortSignal): Promise<HttpAnswer>;
}

/** A timer handle's cancel function. */
export type CancelTimer = () => void;

/** Time, as the builder uses it. */
export interface Clock {
  /** Milliseconds on a clock that only moves forward. */
  now(): number;
  /** Runs `callback` once after `ms`; the result cancels it. */
  setTimer(callback: () => void, ms: number): CancelTimer;
}

/** The entity tag the engine writes for a revision. */
export function etagOfRevision(revision: string): string {
  return `"${revision}"`;
}

/**
 * The revision an entity tag names, or `null` for no tag. Accepts the weak
 * prefix and a bare id, as the engine's own `matchesTag` does.
 */
export function revisionOfEtag(etag: string | null | undefined): string | null {
  if (!etag) return null;
  const bare = etag
    .trim()
    .replace(/^W\//, "")
    .replace(/^"(.*)"$/, "$1");
  return bare === "" ? null : bare;
}

/** What a request is built from. */
export interface RequestInputs {
  readonly mode: BuilderMode;
  /** The active revision the draft was built on; `null` in create mode. */
  readonly baseRevision: string | null;
  /** The document of that revision; `null` in create mode. */
  readonly base: CompanyDocument | null;
  /** The draft's document, with the path index problems will be placed through. */
  readonly sent: IndexedDocument;
  /**
   * The unit a lead's draft is about, by key; `null` for the whole company.
   * See the module doc.
   */
  readonly scope: string | null;
}

function request(
  inputs: RequestInputs,
  query: Record<string, string>,
  extra: Record<string, unknown>,
): ConfigRequest {
  if (inputs.scope !== null) {
    if (inputs.mode !== "edit" || inputs.baseRevision === null) {
      throw new RangeError("a lead's request edits a unit of a company that exists");
    }
    return {
      method: "PUT",
      path: unitPath(inputs.scope),
      query,
      contentType: "application/json",
      headers: { "If-Match": etagOfRevision(inputs.baseRevision) },
      body: { ...scopedUnit(inputs.sent.document), ...extra },
    };
  }
  if (inputs.mode === "create") {
    return {
      method: "PUT",
      path: "/config",
      query,
      contentType: "application/json",
      headers: { "If-None-Match": "*" },
      body: { ...inputs.sent.document, ...extra },
    };
  }
  if (inputs.baseRevision === null || inputs.base === null) {
    throw new RangeError("an edit-mode request needs the base revision and its document");
  }
  return {
    method: "PATCH",
    path: "/config",
    query,
    contentType: "application/merge-patch+json",
    headers: { "If-Match": etagOfRevision(inputs.baseRevision) },
    body: { ...buildPatch(inputs.base, inputs.sent.document), ...extra },
  };
}

/** The dry run of a draft: exactly the write a save would send, stored nowhere. */
export function checkRequest(inputs: RequestInputs): ConfigRequest {
  return request(inputs, { dry_run: "true" }, {});
}

/** The save of a draft, carrying its audit summary. */
export function saveRequest(inputs: RequestInputs, summary: string): ConfigRequest {
  return request(inputs, {}, { _summary: summary });
}
