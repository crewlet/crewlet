/**
 * Settings › Models & keys, in words and in values: every `providers.llm`
 * entry the company document holds, the CLI it drives and how it signs in,
 * which seats run on it, and the edit form's rules.
 *
 * THE DOCUMENT IS WHAT AN OPERATOR WROTE; WHAT AN UNSET FIELD MEANS IS THE
 * ENGINE'S. An entry that names no `agent` or `auth.mode` still runs as
 * something, and what it runs as — and whether its CLI can run a seat's
 * executor at all — is read from `contract/models.ts`, which a Go gate holds to
 * what the engine applies. Which seats run on a model is
 * the engine's too — the org projection carries each seat's resolved chain per
 * phase. Nothing here decides either: it names them and builds the one write
 * the screen makes.
 *
 * NAMES, NEVER VALUES. The document is served redacted: a credential written
 * as one whole `${NAME}` arrives as that reference, and anything else as the
 * mask. So a credential is drawn by the variable it names, or as written
 * inline, and there is no member a value could travel in.
 */

import {
  DEFAULT_CLI_AGENT,
  DEFAULT_CLI_AUTH_MODE,
  DEFAULT_CLI_MAX_CONCURRENT,
  DEFAULT_CLI_TIMEOUT_SECONDS,
  EXECUTOR_CLI_AGENTS,
  type CLIAuthMode,
} from "~/contract/models.ts";
import type { Seat } from "~/lib/seats.ts";
import { configValueKind, fmtSetting, REDACTED } from "~/lib/format.ts";
import { wholeReferenceName } from "~/lib/references.ts";
import type { CompanyDocument, ConfigProblem } from "~/protocol/types.ts";

// ---------------------------------------------------------------------------
// Reading the document
// ---------------------------------------------------------------------------

/**
 * A credential field as the redacted document holds it: the variable a whole
 * `${NAME}` names, or a value written into the document, which reaches this
 * page as the mask and so has no name to show.
 */
export type Credential = { kind: "reference"; name: string } | { kind: "inline" };

/** A credential field's value as a [Credential], or null where it is unset. */
export function credentialOf(value: unknown): Credential | null {
  if (typeof value !== "string") return null;
  // A CREDENTIAL FIELD SHOWS A WHOLE REFERENCE OR NOTHING: a literal is
  // `hidden` there even where an engine sent it unmasked.
  switch (configValueKind(value, { secret: true })) {
    case "empty":
      return null;
    case "reference":
      return { kind: "reference", name: wholeReferenceName(value) ?? "" };
    default:
      return { kind: "inline" };
  }
}

/** One `providers.llm` entry, as written and as the engine will run it. */
export interface ModelEntry {
  /** The entry's config key — what a seat's `llm:` names. */
  key: string;
  /** The CLI it drives, the engine's default applied. */
  agent: string;
  /** Whether the entry leaves `agent` out and so drives the default CLI. */
  agentDefault: boolean;
  /** The model, as written, in the CLI's own grammar; a `${VAR}` stays its reference. */
  model: string;
  /**
   * Whether its CLI can run a seat's executor ([EXECUTOR_CLI_AGENTS]). One
   * that cannot serves the text phases alone, and an executor chain naming it
   * skips it at launch.
   */
  executor: boolean;
  /** The sandbox cell an executor run on this entry happens in; "" takes `providers.sandbox`'s default. */
  runIn: string;
  /** The wall-clock cap on one stretch of the CLI's own work with no tool call in flight, in seconds, the default applied. */
  timeoutSeconds: number;
  /** Whether the entry names its own `timeout_seconds`. */
  timeoutSet: boolean;
  /** How many CLI processes run at once, the default applied. */
  maxConcurrent: number;
  /** Whether the entry names its own `max_concurrent`. */
  concurrencySet: boolean;
  /** Where its login and per-seat homes live, as written; "" is derived per key on each node. */
  stateDir: string;
  /** `auth.mode`, the default applied. */
  authMode: string;
  /** Whether the entry leaves `auth.mode` out. */
  authDefault: boolean;
  token: Credential | null;
  apiKey: Credential | null;
  bundle: Credential | null;
  /** The extra variables the CLI is handed, by name, sorted; each value is a credential. */
  env: { name: string; value: Credential | null }[];
}

/**
 * The order the company's models are declared in — the order a seat that names
 * none falls back through — as the engine's `Providers.ProviderOrder` reads it.
 *
 * `llm_order` is how the document carries it, because a JSON object's keys
 * are sorted on the way out; it is reconciled against the map rather than
 * trusted, since the two travel separately: a key it names that the map lacks
 * is dropped, and a key the map has that it omits comes after, sorted.
 */
export function providerOrder(llm: Record<string, unknown>, order: unknown): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const key of Array.isArray(order) ? order : []) {
    if (typeof key !== "string" || !Object.hasOwn(llm, key) || seen.has(key)) continue;
    seen.add(key);
    out.push(key);
  }
  return [
    ...out,
    ...Object.keys(llm)
      .filter((key) => !seen.has(key))
      .sort(),
  ];
}

/** Every model the document configures, in declaration order ([providerOrder]). */
export function modelEntries(doc: CompanyDocument | null | undefined): ModelEntry[] {
  const providers = isRecord(doc?.providers) ? doc.providers : {};
  const llm = isRecord(providers.llm) ? providers.llm : {};
  return providerOrder(llm, providers.llm_order).map((key) =>
    modelEntry(key, isRecord(llm[key]) ? llm[key] : {}),
  );
}

/** One entry, read off the document. */
export function modelEntry(key: string, raw: Record<string, unknown>): ModelEntry {
  const auth = isRecord(raw.auth) ? raw.auth : {};
  const env = isRecord(raw.env) ? raw.env : {};
  const agent = nonEmpty(raw.agent);
  const cli = agent ?? DEFAULT_CLI_AGENT;
  const authMode = nonEmpty(auth.mode);
  const timeout = positive(raw.timeout_seconds);
  const concurrency = positive(raw.max_concurrent);
  return {
    key,
    agent: cli,
    agentDefault: agent === undefined,
    model: typeof raw.model === "string" ? raw.model : "",
    executor: (EXECUTOR_CLI_AGENTS as readonly string[]).includes(cli),
    runIn: nonEmpty(raw.run_in) ?? "",
    timeoutSeconds: timeout ?? DEFAULT_CLI_TIMEOUT_SECONDS,
    timeoutSet: timeout !== undefined,
    maxConcurrent: concurrency ?? DEFAULT_CLI_MAX_CONCURRENT,
    concurrencySet: concurrency !== undefined,
    stateDir: nonEmpty(raw.state_dir) ?? "",
    authMode: authMode ?? DEFAULT_CLI_AUTH_MODE,
    authDefault: authMode === undefined,
    token: credentialOf(auth.token),
    apiKey: credentialOf(auth.api_key),
    bundle: credentialOf(auth.credential_bundle),
    env: Object.keys(env)
      .sort()
      .map((name) => ({ name, value: credentialOf(env[name]) })),
  };
}

// ---------------------------------------------------------------------------
// Words
// ---------------------------------------------------------------------------

/**
 * What the engine runs an entry's CLI for, in words: a seat's executor, as the
 * CLI's own agentic run in a box, or — for a CLI with no in-box runner — the
 * text phases alone.
 */
export function roleWords(entry: Pick<ModelEntry, "executor">): { label: string; hint: string } {
  return entry.executor
    ? {
        label: "Executor",
        hint: "Runs a seat’s executor as the CLI’s own agentic run in a box, with the seat’s tools over MCP; also serves the text phases.",
      }
    : {
        label: "Text phases",
        hint: "Serves the reviewer, workers, onboarding and side passes; an executor chain skips it at launch.",
      };
}

/** How an entry's CLI signs in, in words. Exhaustive for the reason above. */
export const AUTH_WORDS: Record<CLIAuthMode, { label: string; hint: string }> = {
  subscription: {
    label: "Subscription",
    hint: "Signs in with the CLI’s own login: a headless token, or what crewlet llm login wrote.",
  },
  "api-key": {
    label: "API key",
    hint: "Signs in with one API key in the CLI’s key variable, billed to the metered account.",
  },
  "inherit-env": {
    label: "Engine environment",
    hint: "Forwards the CLI’s token or key variable from the engine’s own environment.",
  },
};

/** An auth mode's words, or the value itself for one this build does not know. */
export function authWords(mode: string): { label: string; hint: string } {
  return Object.hasOwn(AUTH_WORDS, mode)
    ? AUTH_WORDS[mode as CLIAuthMode]
    : { label: mode, hint: `“${mode}” is a sign-in this dashboard does not know.` };
}

/**
 * The credential an entry's sign-in reads, and what it is called: the token
 * under `subscription`, the API key under `api-key`. `inherit-env` reads
 * neither from the document, and so has none.
 */
export function signInCredential(
  entry: ModelEntry,
): { label: "Token" | "API key"; value: Credential | null } | null {
  switch (entry.authMode) {
    case "subscription":
      return { label: "Token", value: entry.token };
    case "api-key":
      return { label: "API key", value: entry.apiKey };
    default:
      return null;
  }
}

/** A credential in a few words: the variable it names, or that it is inline. */
export function credentialWords(value: Credential): string {
  return value.kind === "reference" ? value.name : "written inline";
}

/**
 * Where an entry's sign-in comes from, in a few words for its row: the
 * variable its credential names, or what the CLI reads when the entry names
 * none. NEVER A VALUE — a credential written inline is said to be inline.
 */
export function signInSource(entry: ModelEntry): string {
  const read = signInCredential(entry);
  if (read?.value) {
    return read.value.kind === "reference"
      ? read.value.name
      : `${read.label === "Token" ? "token" : "key"} written inline`;
  }
  switch (entry.authMode) {
    case "subscription":
      return entry.bundle ? "CLI login, from a bundle" : "CLI login";
    case "api-key":
      return "the CLI’s own key variable";
    case "inherit-env":
      return "the engine’s own variables";
    default:
      return "";
  }
}

/**
 * What a credential the entry does not name is read from, as a sentence for
 * the model's page — the engine's own fallback for each mode.
 */
export function signInFallback(entry: ModelEntry): string {
  switch (entry.authMode) {
    case "subscription":
      return "None named. The CLI reads its own token variable, or the login crewlet llm login wrote.";
    case "api-key":
      return "None named. The CLI’s own key variable is read from the secret store, then the engine’s environment.";
    default:
      return "";
  }
}

/** "claude-code · sonnet": the CLI and the model it runs, for a row's second line. */
export function modelLine(entry: { agent: string; model: string }): string {
  return entry.model.trim() ? `${entry.agent} · ${entry.model}` : entry.agent;
}

/** A cap as a setting: "10 minutes", "4 at once". */
export function timeoutWords(seconds: number): string {
  return fmtSetting(seconds);
}

// ---------------------------------------------------------------------------
// Who runs on it
// ---------------------------------------------------------------------------

/**
 * The phases a seat's chain names, in the engine's own order (the executor
 * first, because it is what the seat IS running on), as words. A phase this
 * build has no word for is drawn as its wire name, after the known ones.
 */
const PHASE_WORDS: ReadonlyArray<readonly [string, string]> = [
  ["execute", "executor"],
  ["review", "reviewer"],
  ["subagent", "workers"],
  ["auxiliary", "auxiliary"],
  ["judge", "judge"],
  ["onboarding", "onboarding"],
];

/** One seat's use of one model. */
export interface ModelUse {
  seat: Seat;
  /** Phases whose chain STARTS with this model — what the seat runs them on. */
  runs: string[];
  /** Phases that fall back to it after another model. */
  fallback: string[];
  /** Every phase the seat has runs on this model first: said as one fact, not a list of seven. */
  every: boolean;
}

/**
 * Which seats run on a model, and for which phases — read off the chain the
 * engine resolved for each seat (`llm` on the org projection), never worked out
 * again here. A seat whose projection carries no chain (a human, or a company
 * with no providers) uses nothing.
 */
export function usesOf(seats: readonly Seat[], key: string): ModelUse[] {
  const out: ModelUse[] = [];
  const known = new Set(PHASE_WORDS.map(([phase]) => phase));
  for (const seat of seats) {
    const chains = seat.raw.llm;
    if (!chains) continue;
    const phases = [
      ...PHASE_WORDS.filter(([phase]) => phase in chains),
      ...Object.keys(chains)
        .filter((phase) => !known.has(phase))
        .sort()
        .map((phase) => [phase, phase] as const),
    ];
    const runs: string[] = [];
    const fallback: string[] = [];
    for (const [phase, word] of phases) {
      const at = (chains[phase] ?? []).indexOf(key);
      if (at < 0) continue;
      (at === 0 ? runs : fallback).push(word);
    }
    if (runs.length || fallback.length) {
      out.push({ seat, runs, fallback, every: runs.length === phases.length });
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// The edit form
// ---------------------------------------------------------------------------

/**
 * What the form edits: the parts of an entry a person changes by hand. The
 * CLI and how it signs in are the entry's identity rather than its tuning —
 * changing one changes what the model id means or which account is billed —
 * and are changed in Configuration, where the whole entry is in view.
 */
export interface ModelForm {
  model: string;
  /**
   * The credential the entry's `auth.mode` reads ([credentialField]): a
   * `${NAME}`, [REDACTED] for one written into the document, or "" for none.
   * Unused where the mode reads none.
   */
  credential: string;
  /** Seconds, as typed; empty is the engine's default. */
  timeout: string;
  /** Processes, as typed; empty is the engine's default. */
  concurrency: string;
}

export type ModelField = "model" | "credential" | "timeout" | "concurrency";

/**
 * Which `auth` field the entity's sign-in reads — `token` under subscription,
 * `api_key` under api-key — or null for a mode that reads neither from the
 * document. The engine refuses a credential written under a mode that does
 * not read it, so the form offers only this one.
 */
export function credentialField(entity: Record<string, unknown>): "token" | "api_key" | null {
  const auth = isRecord(entity.auth) ? entity.auth : {};
  const mode = nonEmpty(auth.mode) ?? DEFAULT_CLI_AUTH_MODE;
  if (mode === "subscription") return "token";
  if (mode === "api-key") return "api_key";
  return null;
}

/** The entity's resolved auth mode. */
export function authModeOf(entity: Record<string, unknown>): string {
  const auth = isRecord(entity.auth) ? entity.auth : {};
  return nonEmpty(auth.mode) ?? DEFAULT_CLI_AUTH_MODE;
}

/** The form over an entity as the engine served it (redacted). */
export function formOf(entity: Record<string, unknown>): ModelForm {
  const auth = isRecord(entity.auth) ? entity.auth : {};
  const field = credentialField(entity);
  const credential = field ? auth[field] : undefined;
  return {
    model: typeof entity.model === "string" ? entity.model : "",
    credential: typeof credential === "string" ? credential : "",
    timeout: numberText(entity.timeout_seconds),
    concurrency: numberText(entity.max_concurrent),
  };
}

/**
 * What stops a save before the engine is asked. The engine validates the
 * whole company again; this is only what a person can see is wrong while
 * typing.
 */
export function formErrors(form: ModelForm): Partial<Record<ModelField, string>> {
  const out: Partial<Record<ModelField, string>> = {};
  if (!form.model.trim()) out.model = "Name the model this entry runs.";
  const timeout = form.timeout.trim();
  if (timeout) {
    const n = Number(timeout);
    if (!Number.isFinite(n) || n <= 0) {
      out.timeout = "A number of seconds above 0, or empty for the default.";
    }
  }
  const concurrency = form.concurrency.trim();
  if (concurrency) {
    const n = Number(concurrency);
    if (!Number.isInteger(n) || n < 1) {
      out.concurrency = "A whole number from 1, or empty for the default.";
    }
  }
  return out;
}

/**
 * The entity to send: the one the engine served, with only the edited fields
 * replaced. Everything the form does not show — the CLI, the mode, where it
 * runs, its state directory, its environment, its overrides, the credential
 * bundle — goes back exactly as it came, and a credential the document holds
 * inline goes back as the mask, which the engine restores from the revision
 * this was read from.
 */
export function modelEntity(
  entity: Record<string, unknown>,
  form: ModelForm,
): Record<string, unknown> {
  const out: Record<string, unknown> = { ...entity, model: form.model.trim() };
  const field = credentialField(entity);
  if (field) {
    const auth: Record<string, unknown> = isRecord(entity.auth) ? { ...entity.auth } : {};
    const credential = form.credential.trim();
    if (credential) auth[field] = credential;
    else delete auth[field];
    out.auth = auth;
  }
  const timeout = form.timeout.trim();
  if (timeout) out.timeout_seconds = Number(timeout);
  else delete out.timeout_seconds;
  const concurrency = form.concurrency.trim();
  if (concurrency) out.max_concurrent = Number(concurrency);
  else delete out.max_concurrent;
  return out;
}

/**
 * The revision's audit summary: which model, and what moved. A credential is
 * named by the variable it reads, never by a value — one written inline is
 * "inline", and a literal typed into the field is "a value".
 */
export function editSummary(
  key: string,
  entity: Record<string, unknown>,
  before: ModelForm,
  after: ModelForm,
): string {
  const changes: string[] = [];
  if (before.model.trim() !== after.model.trim()) {
    changes.push(`model ${before.model.trim() || "unset"} → ${after.model.trim()}`);
  }
  const field = credentialField(entity);
  if (field && before.credential.trim() !== after.credential.trim()) {
    const word = field === "token" ? "token" : "API key";
    changes.push(
      `${word} ${credentialSummary(before.credential)} → ${credentialSummary(after.credential)}`,
    );
  }
  for (const [name, word, unit] of [
    ["timeout", "timeout", " s"],
    ["concurrency", "max concurrent", ""],
  ] as const) {
    const was = before[name].trim();
    const now = after[name].trim();
    if (was !== now) {
      changes.push(
        `${word} ${was ? `${was}${unit}` : "default"} → ${now ? `${now}${unit}` : "default"}`,
      );
    }
  }
  return changes.length ? `Edit the model ${key}: ${changes.join("; ")}` : `Edit the model ${key}`;
}

/** Whether the form differs from what was read. */
export function formChanged(before: ModelForm, after: ModelForm): boolean {
  const flat = (f: ModelForm) =>
    JSON.stringify([f.model.trim(), f.credential.trim(), f.timeout.trim(), f.concurrency.trim()]);
  return flat(before) !== flat(after);
}

/**
 * The engine's problems placed beside the field each is about, by the
 * problem's own segments — `["providers", "llm", "smart", "auth", "api_key"]`
 * is the credential on an api-key entry — never its message. The rest is said
 * above the form.
 */
export function problemsByField(
  problems: readonly ConfigProblem[],
  key: string,
  sent: Record<string, unknown>,
): { fields: Partial<Record<ModelField, string>>; rest: string[] } {
  const fields: Partial<Record<ModelField, string>> = {};
  const rest: string[] = [];
  const credential = credentialField(sent);
  for (const p of problems) {
    const seg = p.segments ?? [];
    const field =
      seg[0] === "providers" && seg[1] === "llm" && seg[2] === key
        ? fieldOf(seg.slice(3), credential)
        : null;
    if (field && !fields[field]) fields[field] = p.message;
    else rest.push(p.message);
  }
  return { fields, rest };
}

function fieldOf(
  seg: readonly (string | number)[],
  credential: "token" | "api_key" | null,
): ModelField | null {
  switch (seg[0]) {
    case "model":
      return "model";
    case "timeout_seconds":
      return "timeout";
    case "max_concurrent":
      return "concurrency";
    case "auth":
      // ONLY THE FIELD THE FORM SHOWS: a fault in another auth field is
      // about something this form cannot change, and is said above it.
      return credential !== null && seg[1] === credential ? "credential" : null;
    default:
      return null;
  }
}

function credentialSummary(value: string): string {
  const text = value.trim();
  if (!text) return "none";
  if (text === REDACTED) return "inline";
  return wholeReferenceName(text) ?? "a value";
}

function numberText(value: unknown): string {
  return typeof value === "number" && value > 0 ? String(value) : "";
}

function positive(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) && value > 0 ? value : undefined;
}

function nonEmpty(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() !== "" ? value : undefined;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
