/**
 * The dashboard's one path onto the company document: `/config`.
 *
 * A change to the COMPANY — a provider, the company's budget ceiling, an MCP
 * server — is a new configuration revision, not an act: it is validated whole,
 * stored, and activated by the fleet at a new epoch. So it does not go through
 * `protocol/act.ts`, and every screen that makes one goes through here:
 *
 *  - [getConfig] reads the active document and the entity tag that names its
 *    revision;
 *  - [dryRunPatch] validates a merge patch against that revision and stores
 *    nothing;
 *  - [savePatch] stores it, carrying the audit `_summary` every revision needs;
 *  - [getEntity] reads one addressable entity (a provider or an MCP server)
 *    with the tag of its revision, [dryRunEntity] checks a replacement of it,
 *    and [putEntity] stores one — each with the same validation of the whole
 *    document behind it;
 *  - [dryRunCreate] and [createEntity] ADD an MCP server or a provider: the
 *    same PUT at the new entity's own address, create-only.
 *
 * EVERY WRITE STATES THE REVISION IT EDITED (`If-Match`), so a colleague's
 * save in between is a `conflict` to re-read rather than an overwrite. And
 * every answer is a VALUE — a success, or one of the refusals
 * `configAnswer.ts` reads — never a throw, except the caller's own abort.
 *
 * `configTransport` is the same requests at their lowest level, for the org
 * builder, whose model builds its own requests and settles its own unknown
 * saves (`routes/org/builder/model/`).
 */

import { classifyConfigRefusal, type ConfigAnswer, type ConfigRefusal } from "./configAnswer.ts";
import { isAbort, rest, RestError, type QueryValue, type RestResponse } from "./rest.ts";
import type { CompanyDocument, ConfigWarning, Derived } from "./types.ts";
import type { ENTITY_KINDS } from "../contract/config.ts";

/**
 * Runs one REST call and resolves with the answer, refusal or not. An abort
 * rejects, because a superseded request is not an engine that answered.
 */
export async function answerOf(call: Promise<RestResponse>): Promise<ConfigAnswer> {
  try {
    const { status, body, etag } = await call;
    return { status, body, etag };
  } catch (err) {
    if (isAbort(err)) throw err;
    if (err instanceof RestError) return { status: err.status, body: err.body, etag: null };
    return {
      status: 0,
      body: {
        error: "unreachable",
        detail: err instanceof Error ? err.message : "The engine could not be reached.",
      },
      etag: null,
    };
  }
}

/**
 * One whole-document write or dry run, as a caller built it. Always to
 * `/config` itself: an entity write is [putEntity]'s, with its own path.
 */
export interface ConfigWriteRequest {
  readonly method: "PUT" | "PATCH";
  readonly query: Readonly<Record<string, QueryValue>>;
  readonly contentType: string;
  readonly headers: Readonly<Record<string, string>>;
  readonly body: unknown;
}

/**
 * The configuration requests at their lowest level. Every method RESOLVES
 * with the answer, refusals included, and rejects only on an abort.
 */
export const configTransport = {
  send: (request: ConfigWriteRequest, signal: AbortSignal): Promise<ConfigAnswer> =>
    answerOf(
      rest.request(request.method, "/config", {
        query: request.query,
        contentType: request.contentType,
        headers: request.headers,
        body: request.body,
        signal,
      }),
    ),
  /** `GET /config`. Its entity tag names the active revision. */
  current: (signal: AbortSignal): Promise<ConfigAnswer> =>
    answerOf(rest.request("GET", "/config", { signal })),
  /** `GET /config/revisions/{id}`. */
  revision: (id: string, signal: AbortSignal): Promise<ConfigAnswer> =>
    answerOf(rest.request("GET", `/config/revisions/${encodeURIComponent(id)}`, { signal })),
};

/** The active document, or why there is none to read. */
export type ConfigRead =
  | {
      readonly kind: "document";
      readonly document: CompanyDocument;
      /** The entity tag, verbatim, to hand back as `If-Match`. */
      readonly etag: string;
    }
  /** No company has been configured yet. */
  | { readonly kind: "none" }
  | ConfigRefusal;

/** What a write or a dry run came to. */
export type ConfigWriteOutcome =
  /** A dry run: valid against `baseRevisionId`, stored nowhere. */
  | {
      readonly kind: "valid";
      readonly baseRevisionId: string;
      readonly warnings: readonly ConfigWarning[];
      readonly derived: Derived | null;
    }
  /** A save: stored as `revisionId` and activated at `epoch`. */
  | {
      readonly kind: "saved";
      readonly revisionId: string;
      readonly epoch: number;
      readonly warnings: readonly ConfigWarning[];
      readonly derived: Derived | null;
    }
  | ConfigRefusal;

/** Read the active company document and the tag naming its revision. */
export async function getConfig(signal: AbortSignal): Promise<ConfigRead> {
  const answer = await configTransport.current(signal);
  if (answer.status === 200 && isRecord(answer.body) && answer.etag) {
    return {
      kind: "document",
      document: answer.body as unknown as CompanyDocument,
      etag: answer.etag,
    };
  }
  if (answer.status === 404) return { kind: "none" };
  if (answer.status >= 200 && answer.status < 300) {
    // A SUCCESS WITH NO TAG is one no write can be conditional on, and a
    // write that is not conditional is an overwrite of whatever landed
    // in between. Read as a refusal of the request rather than a document.
    return {
      kind: "problems",
      problems: [
        {
          path: "",
          segments: null,
          kind: "invalid",
          message: "The engine answered the configuration without naming its revision.",
        },
      ],
      derived: null,
      code: "",
      hint: "",
    };
  }
  return classifyConfigRefusal(answer);
}

/** Validate a merge patch against the revision `etag` names. Stores nothing. */
export async function dryRunPatch(
  patch: Record<string, unknown>,
  etag: string,
  signal: AbortSignal,
): Promise<ConfigWriteOutcome> {
  // NO SUMMARY ON A CHECK, deliberately: a node that predates dry runs
  // ignores the parameter and refuses every write without a summary, so a
  // check carrying one would be stored by that node during a rolling upgrade.
  return outcomeOf(
    await configTransport.send(
      {
        method: "PATCH",
        query: { dry_run: "true" },
        contentType: "application/merge-patch+json",
        headers: { "If-Match": etag },
        body: patch,
      },
      signal,
    ),
  );
}

/** Store a merge patch onto the revision `etag` names, with its audit summary. */
export async function savePatch(
  patch: Record<string, unknown>,
  etag: string,
  summary: string,
  signal: AbortSignal,
): Promise<ConfigWriteOutcome> {
  return outcomeOf(
    await configTransport.send(
      {
        method: "PATCH",
        query: {},
        contentType: "application/merge-patch+json",
        headers: { "If-Match": etag },
        body: { ...patch, _summary: summary },
      },
      signal,
    ),
  );
}

/**
 * The collections `PUT /config/{kind}/{id}` addresses — read off
 * `contract/config.ts`'s list rather than spelled again here, because that
 * list is the one the engine's own test holds against `configapi.EntityKinds`,
 * and a second spelling is how this type went on naming `roles` and `units`
 * after the org chart took both off the company document.
 */
export type EntityPath = (typeof ENTITY_KINDS)[number]["kind"];

/** One entity of the active revision, or why there is none to read. */
export type EntityRead =
  | {
      readonly kind: "entity";
      /** The entity itself, redacted — the body the write takes back. */
      readonly entity: Record<string, unknown>;
      /** The DOCUMENT's tag: an entity is a slice of one revision. */
      readonly etag: string;
    }
  /** Nothing in the active revision carries this id, or nothing is active. */
  | { readonly kind: "missing" }
  | ConfigRefusal;

/**
 * Read one entity of the active revision, with the tag of the revision it was
 * read from — the pair [putEntity] and [dryRunEntity] take back.
 */
export async function getEntity(
  kind: EntityPath,
  id: string,
  signal: AbortSignal,
): Promise<EntityRead> {
  const answer = await answerOf(
    rest.request("GET", `/config/${kind}/${encodeURIComponent(id)}`, { signal }),
  );
  if (answer.status === 200 && isRecord(answer.body) && answer.etag) {
    return { kind: "entity", entity: answer.body, etag: answer.etag };
  }
  if (answer.status === 404) return { kind: "missing" };
  if (answer.status >= 200 && answer.status < 300) {
    // The same reading as [getConfig]'s: a success no write can be
    // conditional on is refused rather than taken as the entity.
    return {
      kind: "problems",
      problems: [
        {
          path: "",
          segments: null,
          kind: "invalid",
          message: "The engine answered the entity without naming its revision.",
        },
      ],
      derived: null,
      code: "",
      hint: "",
    };
  }
  return classifyConfigRefusal(answer);
}

/**
 * Validate a replacement of one entity against the revision `etag` names —
 * the whole company, as [putEntity] would store it. Stores nothing, and
 * carries no summary for the reason [dryRunPatch] gives.
 */
export async function dryRunEntity(
  kind: EntityPath,
  id: string,
  entity: Record<string, unknown>,
  etag: string,
  signal: AbortSignal,
): Promise<ConfigWriteOutcome> {
  return outcomeOf(
    await answerOf(
      rest.request("PUT", `/config/${kind}/${encodeURIComponent(id)}`, {
        query: { dry_run: "true" },
        headers: { "If-Match": etag },
        body: entity,
        signal,
      }),
    ),
  );
}

/**
 * Replace one entity of the revision `etag` names. The engine splices it in
 * and validates the whole document, so an entity that is fine on its own and
 * breaks a reference elsewhere is refused like any other invalid write.
 */
export async function putEntity(
  kind: EntityPath,
  id: string,
  entity: Record<string, unknown>,
  etag: string,
  summary: string,
  signal: AbortSignal,
): Promise<ConfigWriteOutcome> {
  return outcomeOf(
    await answerOf(
      rest.request("PUT", `/config/${kind}/${encodeURIComponent(id)}`, {
        headers: { "If-Match": etag },
        body: { ...entity, _summary: summary },
        signal,
      }),
    ),
  );
}

/**
 * The create-only condition: `If-None-Match: *` at the new entity's address.
 *
 * NOT `If-Match` BESIDE IT — the engine refuses the pair. The document tag
 * names a revision an EDIT was read from, and an address with nothing at it
 * has no representation for that tag to describe; the create lands on the
 * revision active when it commits, compare-and-set, and is validated whole
 * against it. A taken name is `412 entity_exists` — a conflict whose reason
 * says so — rather than a replacement of an entity the form never showed.
 */
const CREATE_ONLY = { "If-None-Match": "*" } as const;

/**
 * Check that adding `entity` under `id` would be accepted — the whole company
 * validated with it in. Stores nothing, carries no summary.
 */
export async function dryRunCreate(
  kind: EntityPath,
  id: string,
  entity: Record<string, unknown>,
  signal: AbortSignal,
): Promise<ConfigWriteOutcome> {
  return outcomeOf(
    await answerOf(
      rest.request("PUT", `/config/${kind}/${encodeURIComponent(id)}`, {
        query: { dry_run: "true" },
        headers: CREATE_ONLY,
        body: entity,
        signal,
      }),
    ),
  );
}

/** Add `entity` under `id`, with its audit summary. */
export async function createEntity(
  kind: EntityPath,
  id: string,
  entity: Record<string, unknown>,
  summary: string,
  signal: AbortSignal,
): Promise<ConfigWriteOutcome> {
  return outcomeOf(
    await answerOf(
      rest.request("PUT", `/config/${kind}/${encodeURIComponent(id)}`, {
        headers: CREATE_ONLY,
        body: { ...entity, _summary: summary },
        signal,
      }),
    ),
  );
}

/** What one answer to a write or a dry run means. */
export function outcomeOf(answer: ConfigAnswer): ConfigWriteOutcome {
  const body = isRecord(answer.body) ? answer.body : {};
  const warnings = Array.isArray(body.warnings) ? (body.warnings as ConfigWarning[]) : [];
  const derived = isRecord(body.derived) ? (body.derived as unknown as Derived) : null;
  if (answer.status === 201) {
    return {
      kind: "saved",
      revisionId: typeof body.revision_id === "string" ? body.revision_id : "",
      epoch: typeof body.epoch === "number" ? body.epoch : 0,
      warnings,
      derived,
    };
  }
  if (answer.status === 200) {
    return {
      kind: "valid",
      baseRevisionId: typeof body.base_revision_id === "string" ? body.base_revision_id : "",
      warnings,
      derived,
    };
  }
  return classifyConfigRefusal(answer);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
