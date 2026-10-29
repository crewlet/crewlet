/**
 * Settings › Models' writes: the model routes (`internal/api/llmapi`), the
 * removal that repoints what names a model, and the whole-document write the
 * wizard makes.
 *
 * EVERY ANSWER IS A VALUE. A refusal comes back as `refused`, with the
 * engine's code, sentence and fields, so each flow branches on the code it
 * expects (`credential_rejected`, `already_exists`, `not_sitting_out`,
 * `sign_in_in_progress`, a removal's `references`) rather than catching. Only
 * the caller's own abort rejects. A `step_up_required` never reaches here:
 * the transport asks for the second factor and replays the request.
 *
 * EVERY WRITE THAT RE-ACTIVATES THE COMPANY STATES THE REVISION IT EDITED
 * (`If-Match`), so a colleague's save in between is a conflict to re-read
 * rather than an overwrite.
 */

import { answerOf, outcomeOf, type ConfigWriteOutcome } from "~/protocol/configWrite.ts";
import { isAbort, rest, RestError } from "~/protocol/rest.ts";
import type { CompanyDocument } from "~/protocol/types.ts";
import {
  MODEL_CHECK_BUDGET_SECONDS,
  MODEL_CREDENTIAL_BUDGET_SECONDS,
  MODEL_RETRY_BUDGET_SECONDS,
} from "~/contract/models.ts";
import type {
  CheckAnswer,
  CredentialCheck,
  CredentialStored,
  HostLoginAnswer,
  ModelDevice,
  ModelReference,
  ModelRepoint,
  RetryAnswer,
} from "~/protocol/models.ts";

/** What one model route answered: its body, or the engine's refusal. */
export type ModelAnswer<T> =
  | { kind: "ok"; status: number; value: T }
  | {
      kind: "refused";
      status: number;
      /** The engine's code: `credential_rejected`, `already_exists`, `unreachable`, ... */
      code: string;
      /** The engine's sentence for it, what a person is shown by default. */
      sentence: string;
      detail: string;
      hint: string;
      /** Every field the refusal carried (`problems`, `check`, `names`, `running`, `references`). */
      body: Record<string, unknown>;
    };

const path = (key: string, rest_: string) => `/llm/providers/${encodeURIComponent(key)}/${rest_}`;

async function call<T>(
  method: "PUT" | "POST" | "DELETE",
  url: string,
  options: {
    body?: unknown;
    headers?: Record<string, string>;
    query?: Record<string, string>;
    signal?: AbortSignal;
    timeoutMs?: number;
  } = {},
): Promise<ModelAnswer<T>> {
  try {
    const answer = await rest.request(method, url, {
      ...(options.body === undefined ? {} : { body: options.body }),
      ...(options.headers ? { headers: options.headers } : {}),
      ...(options.query ? { query: options.query } : {}),
      ...(options.signal ? { signal: options.signal } : {}),
      ...(options.timeoutMs ? { timeoutMs: options.timeoutMs } : {}),
    });
    return { kind: "ok", status: answer.status, value: answer.body as T };
  } catch (err) {
    if (isAbort(err)) throw err;
    if (err instanceof RestError) {
      return {
        kind: "refused",
        status: err.status,
        code: err.code,
        sentence: err.sentence || err.detail,
        detail: err.detail,
        hint: err.hint,
        body: err.body,
      };
    }
    return {
      kind: "refused",
      status: 0,
      code: "unreachable",
      sentence: "The engine could not be reached.",
      detail: err instanceof Error ? err.message : "",
      hint: "",
      body: {},
    };
  }
}

/**
 * How long the dashboard waits on a route that runs the CLI: the engine's own
 * budget for it (held to the engine by a Go gate) and a margin for the answer
 * to be written and travel back, so the page never abandons a test the engine
 * is still finishing.
 */
const ANSWER_MARGIN_SECONDS = 15;
const waitFor = (budgetSeconds: number) => (budgetSeconds + ANSWER_MARGIN_SECONDS) * 1000;

/** The candidate values a credential write carries, and the draft entry they are for. */
export interface CredentialRequest {
  values: Record<string, string>;
  /** The model as the wizard holds it, for a model not created yet. */
  entity?: Record<string, unknown>;
  /** Store only names nothing holds (`If-None-Match: *`): a new model's own secrets. */
  createOnly: boolean;
}

/** `PUT /llm/providers/{key}/credential?dry_run=true`: the values tested, stored nowhere. */
export function testCredential(
  key: string,
  request: CredentialRequest,
  signal?: AbortSignal,
): Promise<ModelAnswer<CredentialCheck>> {
  return call("PUT", path(key, "credential"), {
    body: { values: request.values, ...(request.entity ? { entity: request.entity } : {}) },
    headers: request.createOnly ? { "If-None-Match": "*" } : {},
    query: { dry_run: "true" },
    timeoutMs: waitFor(MODEL_CREDENTIAL_BUDGET_SECONDS),
    ...(signal ? { signal } : {}),
  });
}

/**
 * `PUT /llm/providers/{key}/credential`: the values tested, sealed only if
 * they sign the model in, and the revision that reads them re-activated
 * against `etag`.
 */
export function storeCredential(
  key: string,
  request: CredentialRequest,
  etag: string,
  signal?: AbortSignal,
): Promise<ModelAnswer<CredentialStored>> {
  return call("PUT", path(key, "credential"), {
    body: { values: request.values, ...(request.entity ? { entity: request.entity } : {}) },
    headers: { "If-Match": etag, ...(request.createOnly ? { "If-None-Match": "*" } : {}) },
    timeoutMs: waitFor(MODEL_CREDENTIAL_BUDGET_SECONDS),
    ...(signal ? { signal } : {}),
  });
}

/** `POST /llm/providers/{key}/check`: the model's test, with a real call when `smoke`. */
export function checkModel(
  key: string,
  options: { smoke: boolean; entity?: Record<string, unknown> },
  signal?: AbortSignal,
): Promise<ModelAnswer<CheckAnswer>> {
  return call("POST", path(key, "check"), {
    body: { smoke: options.smoke, ...(options.entity ? { entity: options.entity } : {}) },
    timeoutMs: waitFor(MODEL_CHECK_BUDGET_SECONDS),
    ...(signal ? { signal } : {}),
  });
}

/**
 * `POST /llm/providers/{key}/retry`: `probe` asks the vendor once and clears
 * the sit-out only if it answers; `clear` clears it for a plan raised outside.
 */
export function retryModel(
  key: string,
  mode: "probe" | "clear",
  signal?: AbortSignal,
): Promise<ModelAnswer<RetryAnswer>> {
  return call("POST", path(key, "retry"), {
    body: { mode },
    timeoutMs: waitFor(MODEL_RETRY_BUDGET_SECONDS),
    ...(signal ? { signal } : {}),
  });
}

/** `POST /llm/providers/{key}/host-login/check`: the CLI's own login on the answering node. */
export function checkHostLogin(
  key: string,
  signal?: AbortSignal,
): Promise<ModelAnswer<HostLoginAnswer>> {
  return call("POST", path(key, "host-login/check"), {
    body: {},
    ...(signal ? { signal } : {}),
  });
}

/** `POST /llm/providers/{key}/device-sign-in`: a device-code sign-in started (201). */
export function startDeviceSignIn(
  key: string,
  signal?: AbortSignal,
): Promise<ModelAnswer<ModelDevice>> {
  return call("POST", path(key, "device-sign-in"), {
    body: {},
    ...(signal ? { signal } : {}),
  });
}

/** `DELETE /llm/device-sign-ins/{id}`: the sign-in cancelled. */
export function cancelDeviceSignIn(
  id: string,
  signal?: AbortSignal,
): Promise<ModelAnswer<ModelDevice>> {
  return call("DELETE", `/llm/device-sign-ins/${encodeURIComponent(id)}`, {
    ...(signal ? { signal } : {}),
  });
}

/** What removing a model does with each place that names it. */
export interface Removal {
  repoint: ModelRepoint[];
  /** Turn the assistant off when the removal leaves its chat with no model. */
  assistantOff: boolean;
}

/** What a removal came to, and every place that names the model where the engine listed them. */
export interface RemovalAnswer {
  outcome: ConfigWriteOutcome;
  /**
   * Every written reference to the model, from a refusal for repoints that do
   * not answer them all (`body.references`): the list the page draws its
   * repoints from. Empty on any other answer.
   */
  references: ModelReference[];
}

/** A refusal body's references, as the engine wrote them; nothing else is read as one. */
export function referencesOf(body: unknown): ModelReference[] {
  if (typeof body !== "object" || body === null) return [];
  const refs = (body as { references?: unknown }).references;
  if (!Array.isArray(refs)) return [];
  return refs.filter(
    (r): r is ModelReference =>
      typeof r === "object" &&
      r !== null &&
      typeof (r as ModelReference).at === "object" &&
      Array.isArray((r as ModelReference).chain),
  );
}

/**
 * `DELETE /config/llm-providers/{key}`: the model removed and every dependant
 * repointed in one revision, or with `dryRun` the same checked and stored
 * nowhere. A refusal for repoints that miss a reference carries every
 * reference, from which the page draws its list.
 */
export async function removeModel(
  key: string,
  removal: Removal,
  options: { etag: string; summary: string; dryRun: boolean },
  signal: AbortSignal,
): Promise<RemovalAnswer> {
  const answer = await answerOf(
    rest.request("DELETE", `/config/llm-providers/${encodeURIComponent(key)}`, {
      body: { repoint: removal.repoint, assistant_off: removal.assistantOff },
      headers: {
        "If-Match": options.etag,
        ...(options.dryRun ? {} : { "X-Summary": options.summary }),
      },
      ...(options.dryRun ? { query: { dry_run: "true" } } : {}),
      signal,
    }),
  );
  return { outcome: outcomeOf(answer), references: referencesOf(answer.body) };
}

/**
 * `PUT /config`: the whole next document, which is how the wizard adds or
 * edits a model and every use it writes in one revision. With `dryRun` it is
 * checked against `etag` and stored nowhere.
 */
export async function writeDocument(
  doc: CompanyDocument,
  options: { etag: string; summary: string; dryRun: boolean },
  signal: AbortSignal,
): Promise<ConfigWriteOutcome> {
  return outcomeOf(
    await answerOf(
      rest.request("PUT", "/config", {
        body: doc,
        headers: {
          "If-Match": options.etag,
          ...(options.dryRun ? {} : { "X-Summary": options.summary }),
        },
        ...(options.dryRun ? { query: { dry_run: "true" } } : {}),
        signal,
      }),
    ),
  );
}
