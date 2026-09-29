/**
 * What the builder core needs from the world, as small injected interfaces,
 * and the requests it sends.
 *
 * NOTHING IN THIS DIRECTORY TOUCHES A GLOBAL. Time, storage, randomness and
 * the network all arrive as arguments, so every rule here runs under a test
 * with a fake clock and a scripted engine, and the one place that binds them
 * to `fetch`, `setTimeout` and `sessionStorage` is the UI that owns their
 * lifetimes. `model/boundary.test.ts` holds the directory to it.
 *
 * TWO SURFACES, BECAUSE THE COMPANY IS TWO THINGS. Its settings are a revision
 * `/config` serves and validates whole; its org chart is a log `/chart` serves
 * and arbitrates per object. The settings requests are built here; the chart's
 * writes are a save's steps (`save.ts`), and its read is [EngineTransport.chart].
 *
 * ONE SETTINGS REQUEST SHAPE PER MODE, for a check and a save alike, because a
 * check that sent something other than what the save will send validates a
 * different write:
 *
 * - EDIT mode is `PATCH /config` with a JSON merge patch of exactly the keys
 *   the builder changed (`settingsPatch`) and `If-Match` naming the base
 *   revision, so a newer revision is a 409 rather than an overwrite.
 * - CREATE mode is `PUT /config` with the whole settings document and
 *   `If-None-Match: *`, so a company that appeared meanwhile, anywhere in the
 *   fleet, is a 412 rather than a replacement.
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
import { settingsPatch } from "./document.ts";

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

/** A write or a dry run, ready for the transport. */
export interface EngineRequest {
  readonly method: "PUT" | "PATCH" | "POST";
  /** `/config`, `/chart/batch`, `/chart/seats/{handle}` or `/chart/units/{key}`, already encoded. */
  readonly path: string;
  readonly query: Readonly<Record<string, string>>;
  readonly contentType: "application/json" | "application/merge-patch+json";
  readonly headers: Readonly<Record<string, string>>;
  readonly body: Readonly<Record<string, unknown>>;
}

/**
 * The network, as the builder uses it. Every method RESOLVES with the answer,
 * refusals included; it rejects only when `signal` aborted it.
 */
export interface EngineTransport {
  send(request: EngineRequest, signal: AbortSignal): Promise<HttpAnswer>;
  /** `GET /config`: the active settings revision. Its entity tag names the revision. */
  settings(signal: AbortSignal): Promise<HttpAnswer>;
  /** `GET /config/revisions/{id}`. */
  revision(id: string, signal: AbortSignal): Promise<HttpAnswer>;
  /**
   * `GET /chart?runtime=true`: the whole org chart, with each object's runtime
   * half where this reader may see it (the answer's `runtime` says whether it
   * was served).
   */
  chart(signal: AbortSignal): Promise<HttpAnswer>;
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

/** What a settings request is built from. */
export interface SettingsInputs {
  readonly mode: BuilderMode;
  /** The active revision the draft was built on; `null` in create mode. */
  readonly baseRevision: string | null;
  /** The settings document of that revision; `null` in create mode. */
  readonly base: CompanyDocument | null;
  /** The draft's settings document. */
  readonly draft: CompanyDocument;
}

/**
 * Whether the draft's settings differ from its base's, so there is a settings
 * write to send at all. Always in create mode: the company's first revision
 * is what makes it exist.
 */
export function settingsChanged(inputs: SettingsInputs): boolean {
  return (
    inputs.mode === "create" || Object.keys(settingsPatch(inputs.base, inputs.draft)).length > 0
  );
}

function settingsRequest(
  inputs: SettingsInputs,
  query: Record<string, string>,
  extra: Record<string, unknown>,
): EngineRequest {
  if (inputs.mode === "create") {
    return {
      method: "PUT",
      path: "/config",
      query,
      contentType: "application/json",
      headers: { "If-None-Match": "*" },
      body: { ...inputs.draft, ...extra },
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
    body: { ...settingsPatch(inputs.base, inputs.draft), ...extra },
  };
}

/** The dry run of the draft's settings: exactly the write a save would send, stored nowhere. */
export function settingsCheckRequest(inputs: SettingsInputs): EngineRequest {
  return settingsRequest(inputs, { dry_run: "true" }, {});
}

/** The save of the draft's settings, carrying its audit summary. */
export function settingsSaveRequest(inputs: SettingsInputs, summary: string): EngineRequest {
  return settingsRequest(inputs, {}, { _summary: summary });
}
