/**
 * Settings › Models & Fleets' writes: the model routes (`internal/api/llmapi`), the
 * removal that repoints what names a model, and the whole-document write the
 * wizard makes.
 *
 * EVERY ANSWER IS A VALUE. A refusal comes back as `refused`, with the
 * engine's code, sentence and fields, so each flow branches on the code it
 * expects (`credential_rejected`, `already_exists`, `not_sitting_out`,
 * `sign_in_in_progress`, a removal's `references`) rather than catching. Only
 * two things reject: the caller's own abort, and a name that cannot be sent
 * as one path segment (`pathSegment`'s RangeError, before anything is sent;
 * no name the engine mints is one). Every route is `async`, so both arrive as
 * a rejection, never as a throw from the call. A `step_up_required` never
 * reaches here: the transport asks for the second factor and replays the
 * request.
 *
 * EVERY WRITE THAT RE-ACTIVATES THE COMPANY STATES THE REVISION IT EDITED
 * (`If-Match`), so a colleague's save in between is a conflict to re-read
 * rather than an overwrite.
 *
 * A WRITE THAT HAS STARTED IS SENT WITHOUT A SIGNAL, and so runs to its
 * answer (or the transport's own deadline) whatever happens to the screen:
 * stopped part way, a save could seal a secret no document names, or a
 * removal leave the person unsure whether it happened. The signal a write
 * takes is for a dry run or a check the screen may abandon; what a screen does
 * with a write's answer is what its lifetime gates (`useLifetimeSignal`).
 */

import {
  answerOf,
  outcomeOf,
  type ConfigCheckOutcome,
  type ConfigWriteOutcome,
} from "~/protocol/configWrite.ts";
import { isAbort, pathSegment, rest, RestError, type RestResponse } from "~/protocol/rest.ts";
import { QueryError, queryErrorCode } from "~/protocol/index.ts";
import type { CompanyDocument } from "~/protocol/types.ts";
import { MODEL_CREDENTIAL_BUDGET_SECONDS, MODEL_RETRY_BUDGET_SECONDS } from "~/contract/models.ts";
import type { RemovalSecrets, SecretDeletion } from "~/contract/config.ts";
import type {
  CheckAnswer,
  CredentialCheck,
  CredentialStored,
  HostLoginAnswer,
  ModelCheck,
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

/**
 * One model route's answer as a value: its body, or the engine's refusal.
 * Each route names its own path at its call (a literal under `/llm`, which
 * the dev server's proxy gate reads there); this settles what came back.
 */
async function settle<T>(request: Promise<RestResponse>): Promise<ModelAnswer<T>> {
  try {
    const answer = await request;
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
 * How long the dashboard waits on a route that runs the harness: the engine's own
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
export async function testCredential(
  key: string,
  request: CredentialRequest,
  signal?: AbortSignal,
): Promise<ModelAnswer<CredentialCheck>> {
  return settle(
    rest.request("PUT", `/llm/providers/${pathSegment(key)}/credential`, {
      body: { values: request.values, ...(request.entity ? { entity: request.entity } : {}) },
      headers: request.createOnly ? { "If-None-Match": "*" } : {},
      query: { dry_run: "true" },
      timeoutMs: waitFor(MODEL_CREDENTIAL_BUDGET_SECONDS),
      ...(signal ? { signal } : {}),
    }),
  );
}

/**
 * `PUT /llm/providers/{key}/credential`: the values tested, sealed only if
 * they sign the model in, and the revision that reads them re-activated
 * against `etag`.
 */
export async function storeCredential(
  key: string,
  request: CredentialRequest,
  etag: string,
  signal?: AbortSignal,
): Promise<ModelAnswer<CredentialStored>> {
  return settle(
    rest.request("PUT", `/llm/providers/${pathSegment(key)}/credential`, {
      body: { values: request.values, ...(request.entity ? { entity: request.entity } : {}) },
      headers: { "If-Match": etag, ...(request.createOnly ? { "If-None-Match": "*" } : {}) },
      timeoutMs: waitFor(MODEL_CREDENTIAL_BUDGET_SECONDS),
      ...(signal ? { signal } : {}),
    }),
  );
}

/**
 * How often a running test is asked about (`llm_check`): two seconds, the
 * engine's own hold before it answers a test running, against a test's 3 to
 * 80 seconds, so a person learns of the end within two seconds of it.
 */
const CHECK_POLL_MS = 2000;

/**
 * The latest test of a model as a screen asks it: the `llm_check` question,
 * which any node answers ({@link useCheckModel} supplies it).
 */
export type AskCheck = (key: string) => Promise<ModelCheck>;

/** Query failures a poll asks again after: the node could not answer yet. */
const PASSING_QUERY_FAILURES = ["unavailable", "timeout"];

/**
 * `POST /llm/providers/{key}/check`: the model's test, with a real call when
 * `smoke`. The test runs on the answering node's own time, so the route
 * answers it running (`202`) and this asks `ask` every {@link CHECK_POLL_MS}
 * until it is not: what the test found, or a refusal carrying why it found
 * nothing — it ended without an answer, its node stopped, its record is gone,
 * or another test of the model replaced it first. Such a refusal is the
 * test's own, never the route's, so its `code` is the question's failure or
 * the empty string, which no flow branches on.
 */
export async function checkModel(
  key: string,
  options: { smoke: boolean; entity?: Record<string, unknown> },
  ask: AskCheck,
  signal?: AbortSignal,
): Promise<ModelAnswer<CheckAnswer>> {
  const started = await settle<ModelCheck>(
    rest.request("POST", `/llm/providers/${pathSegment(key)}/check`, {
      body: { smoke: options.smoke, ...(options.entity ? { entity: options.entity } : {}) },
      ...(signal ? { signal } : {}),
    }),
  );
  if (started.kind === "refused") return started;
  let check = started.value;
  while (check.state === "running") {
    await pause(CHECK_POLL_MS, signal);
    let polled: ModelCheck;
    try {
      polled = await ask(key);
    } catch (err) {
      if (signal?.aborted) throw signal.reason;
      const code = (err instanceof QueryError && queryErrorCode(err.message)) || "";
      if (PASSING_QUERY_FAILURES.includes(code)) continue;
      return testRefusal(started.status, code, unreadSentence(code), check);
    }
    if (signal?.aborted) throw signal.reason;
    if (polled.id !== check.id) {
      return testRefusal(
        started.status,
        "",
        "Another test of this model began before this one's outcome was read. Test it again.",
        polled,
      );
    }
    check = polled;
  }
  if (check.state === "ended" && check.check) {
    return { kind: "ok", status: started.status, value: check.check };
  }
  return testRefusal(
    started.status,
    "",
    check.failure || "The test ended without saying what it found. Test it again.",
    check,
  );
}

/** Why a running test's outcome could not be read, in words. */
function unreadSentence(code: string): string {
  return code === "not_found"
    ? "The test's outcome is gone: the model was removed, or its result is no longer kept. Test it again."
    : `The test's outcome could not be read (${code || "no answer"}). Test it again.`;
}

/** A test that found nothing, as a refusal of its own ({@link checkModel}). */
function testRefusal(
  status: number,
  code: string,
  sentence: string,
  check: ModelCheck,
): ModelAnswer<CheckAnswer> {
  return {
    kind: "refused",
    status,
    code,
    sentence,
    detail: sentence,
    hint: "",
    body: check as unknown as Record<string, unknown>,
  };
}

/** Waits `ms`, or rejects at once as the caller's abort does. */
function pause(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(signal.reason);
      return;
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", stop);
      resolve();
    }, ms);
    const stop = () => {
      clearTimeout(timer);
      reject(signal?.reason);
    };
    signal?.addEventListener("abort", stop, { once: true });
  });
}

/**
 * `POST /llm/providers/{key}/retry`: `probe` asks the vendor once and clears
 * the sit-out only if it answers; `clear` clears it for a plan raised outside.
 */
export async function retryModel(
  key: string,
  mode: "probe" | "clear",
  signal?: AbortSignal,
): Promise<ModelAnswer<RetryAnswer>> {
  return settle(
    rest.request("POST", `/llm/providers/${pathSegment(key)}/retry`, {
      body: { mode },
      timeoutMs: waitFor(MODEL_RETRY_BUDGET_SECONDS),
      ...(signal ? { signal } : {}),
    }),
  );
}

/** `POST /llm/providers/{key}/host-login/check`: the harness's own login on the answering node. */
export async function checkHostLogin(
  key: string,
  signal?: AbortSignal,
): Promise<ModelAnswer<HostLoginAnswer>> {
  return settle(
    rest.request("POST", `/llm/providers/${pathSegment(key)}/host-login/check`, {
      body: {},
      ...(signal ? { signal } : {}),
    }),
  );
}

/** `POST /llm/providers/{key}/device-sign-in`: a device-code sign-in started (201). */
export async function startDeviceSignIn(
  key: string,
  signal?: AbortSignal,
): Promise<ModelAnswer<ModelDevice>> {
  return settle(
    rest.request("POST", `/llm/providers/${pathSegment(key)}/device-sign-in`, {
      body: {},
      ...(signal ? { signal } : {}),
    }),
  );
}

/** `DELETE /llm/device-sign-ins/{id}`: the sign-in cancelled. */
export async function cancelDeviceSignIn(
  id: string,
  signal?: AbortSignal,
): Promise<ModelAnswer<ModelDevice>> {
  return settle(
    rest.request("DELETE", `/llm/device-sign-ins/${pathSegment(id)}`, {
      ...(signal ? { signal } : {}),
    }),
  );
}

/** What removing a model does with each place that names it. */
export interface Removal {
  /** Every reference's next model; Ask Crewlet's pointed at none turns it off. */
  repoint: ModelRepoint[];
  /** The stored values the model read to delete once it is removed. */
  deleteSecrets: readonly string[];
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
  /** What became of each value asked to delete, on a written removal; empty otherwise. */
  secrets: SecretDeletion[];
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
 * `GET /config/llm-providers/{key}/references`: every written reference to the
 * model, read with no write asked, from which a removal draws its list. Asked
 * as a dry run with no repoint, the list came back as a 422 that every
 * browser logs as a failed request.
 */
export async function modelReferences(
  key: string,
  signal?: AbortSignal,
): Promise<
  { references: ModelReference[]; values: RemovalSecrets } | { refused: ConfigCheckOutcome }
> {
  const answer = await answerOf(
    rest.request("GET", `/config/llm-providers/${pathSegment(key)}/references`, {
      ...(signal ? { signal } : {}),
    }),
  );
  if (answer.status === 200)
    return {
      references: referencesOf(answer.body),
      values: (answer.body as { dependants: RemovalSecrets }).dependants,
    };
  return { refused: outcomeOf(answer) };
}

/**
 * `DELETE /config/llm-providers/{key}`: the model removed and every dependant
 * repointed in one revision, then the values asked for deleted, or with
 * `dryRun` the same checked and stored nowhere. A refusal for repoints that
 * miss a reference carries every reference, from which the page draws its list.
 */
export async function removeModel(
  key: string,
  removal: Removal,
  options: { etag: string; summary: string; dryRun: boolean },
  signal?: AbortSignal,
): Promise<RemovalAnswer> {
  const answer = await answerOf(
    rest.request("DELETE", `/config/llm-providers/${pathSegment(key)}`, {
      body: { repoint: removal.repoint, delete_secrets: removal.deleteSecrets },
      headers: {
        "If-Match": options.etag,
        ...(options.dryRun ? {} : { "X-Summary": options.summary }),
      },
      ...(options.dryRun ? { query: { dry_run: "true" } } : {}),
      ...(signal ? { signal } : {}),
    }),
  );
  const outcome = outcomeOf(answer);
  return {
    outcome,
    references: referencesOf(answer.body),
    secrets: outcome.kind === "saved" ? (answer.body as { secrets: SecretDeletion[] }).secrets : [],
  };
}

/**
 * `PUT /config`: the whole next document, which is how the wizard adds or
 * edits a model and every use it writes in one revision. With `dryRun` it is
 * checked against `etag` and stored nowhere.
 */
export async function writeDocument(
  doc: CompanyDocument,
  options: { etag: string; summary: string; dryRun: boolean },
  signal?: AbortSignal,
): Promise<ConfigCheckOutcome> {
  return outcomeOf(
    await answerOf(
      rest.request("PUT", "/config", {
        body: doc,
        headers: {
          "If-Match": options.etag,
          ...(options.dryRun ? {} : { "X-Summary": options.summary }),
        },
        ...(options.dryRun ? { query: { dry_run: "true" } } : {}),
        ...(signal ? { signal } : {}),
      }),
    ),
  );
}
