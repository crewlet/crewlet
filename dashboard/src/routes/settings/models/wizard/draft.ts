/**
 * A model as the wizard holds it, and what it becomes: the document entry
 * (`entityOf`), the candidate credential it is tested and stored with, the
 * whole next document with every use it writes, and the step each problem
 * belongs to.
 *
 * THE USES ARE `lib/modelUses.ts`'s. The draft holds them as `Uses` and the
 * next document is `applyUses`' write, so the job-to-field rules live in one
 * place: this module only adds the entry itself and its order.
 *
 * A PASTED CREDENTIAL IS NEVER PART OF THE ENTRY. The entry names the secret
 * (`${VAR}`); the value travels once to the credential route, which tests it
 * and stores it, and is never read back.
 */

import type { ConfigProblem, CompanyDocument } from "~/protocol/types.ts";
import {
  DEFAULT_HARNESS,
  DEFAULT_HARNESS_AUTH_MODE,
  EXECUTOR_HARNESSES,
  type HarnessAuthMode,
  type RunnerCellName,
} from "~/contract/models.ts";
import {
  applyUses,
  noUses,
  intentOf,
  providerOrder,
  type Dropped,
  type Intent,
  type Uses,
} from "~/lib/modelUses.ts";
import { WIZARD_STEPS, type WizardStep } from "../parts.tsx";
import type { ModelView } from "../data.ts";

export type Harness = "claude-code" | "codex" | "opencode";
/**
 * How a model signs in, as the wizard offers it. `unchanged` is a model on a
 * sign-in the wizard does not write (another CLI, or the engine's own
 * environment): an edit keeps it exactly as Configuration wrote it.
 */
export type Method = "api-key" | "cloud" | "host-login" | "token" | "device" | "unchanged";
export type Cloud = "bedrock" | "vertex" | "foundry" | "gateway";
export type RunIn = "" | RunnerCellName;

export interface Draft {
  key: string;
  harness: string;
  method: Method;
  /** The pasted key or token: held in the form, tested, stored on save, never shown again. */
  credential: string;
  /** The secret the credential is stored under. */
  variable: string;
  cloud: Cloud;
  /** The cloud fields as typed; a secret one left empty on an edit keeps what is stored. */
  cloudFields: Record<string, string>;
  /** OpenCode's provider, the first half of its model name. */
  provider: string;
  model: string;
  runIn: RunIn;
  timeout: string;
  concurrent: string;
  webDenied: boolean;
  /** Which seats run each job on the model, and whether the Dashboard Assistant does. */
  uses: Uses;
  /** What the model is for, as Use it for offers it; `custom` once uses are set seat by seat. */
  intent: Intent;
}

/** The CLIs a model can be added on, in the order offered. */
export const HARNESSES: readonly Harness[] = ["claude-code", "codex", "opencode"];

const isHarness = (harness: string): harness is Harness =>
  (HARNESSES as readonly string[]).includes(harness);

/** Whether the draft's CLI can run a seat's coding (`EXECUTOR_HARNESSES`, held to the engine). */
export const canExecute = (d: Pick<Draft, "harness">) =>
  (EXECUTOR_HARNESSES as readonly string[]).includes(d.harness);

/** The sign-in methods each CLI offers, the one allowed on any engine first. */
export const METHODS: Readonly<Record<Harness, readonly Method[]>> = {
  "claude-code": ["api-key", "host-login", "token", "cloud"],
  codex: ["api-key", "device"],
  opencode: ["api-key"],
};

/** The methods the draft's CLI offers; a CLI the wizard does not add keeps its sign-in. */
export function methodsFor(d: Pick<Draft, "harness" | "method">): readonly Method[] {
  return isHarness(d.harness) && d.method !== "unchanged" ? METHODS[d.harness] : ["unchanged"];
}

/** The providers OpenCode is offered with, and the variable each one's key is read from. */
export const OPENCODE_PROVIDERS: readonly { value: string; label: string; variable: string }[] = [
  { value: "anthropic", label: "Anthropic", variable: "ANTHROPIC_API_KEY" },
  { value: "openai", label: "OpenAI", variable: "OPENAI_API_KEY" },
  { value: "openrouter", label: "OpenRouter", variable: "OPENROUTER_API_KEY" },
  { value: "groq", label: "Groq", variable: "GROQ_API_KEY" },
];

export interface CloudField {
  name: string;
  label: string;
  secret?: boolean;
  placeholder: string;
}

/** Each cloud provider's fields, by the variable Claude Code reads. */
export const CLOUD_FIELDS: Readonly<Record<Cloud, { label: string; fields: CloudField[] }>> = {
  bedrock: {
    label: "Amazon Bedrock",
    fields: [
      { name: "AWS_REGION", label: "Region", placeholder: "us-west-2" },
      {
        name: "AWS_BEARER_TOKEN_BEDROCK",
        label: "Bedrock API key",
        secret: true,
        placeholder: "ABSK…",
      },
    ],
  },
  vertex: {
    label: "Google Vertex AI",
    fields: [
      { name: "CLOUD_ML_REGION", label: "Region", placeholder: "us-east5" },
      { name: "ANTHROPIC_VERTEX_PROJECT_ID", label: "Project ID", placeholder: "my-project" },
    ],
  },
  foundry: {
    label: "Microsoft Foundry",
    fields: [
      { name: "ANTHROPIC_FOUNDRY_RESOURCE", label: "Resource", placeholder: "my-foundry" },
      {
        name: "ANTHROPIC_FOUNDRY_API_KEY",
        label: "API key",
        secret: true,
        placeholder: "Paste the key",
      },
    ],
  },
  gateway: {
    label: "LLM gateway",
    fields: [
      {
        name: "ANTHROPIC_BASE_URL",
        label: "Gateway address",
        placeholder: "https://llm.example.com",
      },
      {
        name: "ANTHROPIC_AUTH_TOKEN",
        label: "Gateway token",
        secret: true,
        placeholder: "Paste the token",
      },
    ],
  },
};

/** The switch that sends Claude Code to a cloud provider; a gateway has none. */
const CLOUD_FLAG: Readonly<Partial<Record<Cloud, string>>> = {
  bedrock: "CLAUDE_CODE_USE_BEDROCK",
  vertex: "CLAUDE_CODE_USE_VERTEX",
  foundry: "CLAUDE_CODE_USE_FOUNDRY",
};

/** Every env variable the wizard writes, which an edit replaces rather than keeps. */
const MANAGED_ENV = new Set<string>([
  ...Object.values(CLOUD_FLAG),
  ...Object.values(CLOUD_FIELDS).flatMap((c) => c.fields.map((f) => f.name)),
  ...OPENCODE_PROVIDERS.map((p) => p.variable),
]);

/** The fields of an entry the wizard writes; every other field an edit keeps as written. */
const MANAGED_FIELDS = new Set([
  "harness",
  "model",
  "run_in",
  "timeout_seconds",
  "max_concurrent",
  "auth",
  "env",
  "overrides",
]);

/**
 * The longest name the wizard gives a model (design QA M15). The engine takes
 * 63 characters; a name is shown in tables, chips and sentences, and 40 keeps
 * it to one line wherever it is printed beside a model id.
 */
export const KEY_MAX = 40;
/** The engine's grammar for a model's name (`config.ProviderKeyPattern`). */
const KEY_PATTERN = /^[a-z0-9][a-z0-9._-]*$/;
/** The secret store's grammar for a name (`secrets.CheckName`). */
const VARIABLE_PATTERN = /^[A-Z][A-Z0-9_]*$/;

/** The steps a draft walks; Where it runs only for a CLI that runs coding. */
export function stepsFor(d: Pick<Draft, "harness">): WizardStep[] {
  return WIZARD_STEPS.filter((s) => s !== "runs" || canExecute(d));
}

export const STEP_LABELS: Readonly<Record<WizardStep, string>> = {
  harness: "Name and CLI",
  signin: "Method",
  model: "Model",
  runs: "Where it runs",
  assign: "Use it for",
  limits: "Limits",
  test: "Test call",
  review: "Review",
};

const isRecord = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" && v !== null && !Array.isArray(v);

/** The variable a whole `${VAR}` reference names, or null for anything else. */
function referenced(value: unknown): string | null {
  const match = typeof value === "string" ? /^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$/.exec(value) : null;
  return match ? match[1]! : null;
}

const ref = (variable: string) => `\${${variable}}`;

/** A blank draft for a new model: Claude Code on an API key, used for nothing yet. */
export function emptyDraft(): Draft {
  return {
    key: "",
    harness: DEFAULT_HARNESS,
    method: "api-key",
    credential: "",
    variable: "",
    cloud: "bedrock",
    cloudFields: {},
    provider: "anthropic",
    model: "",
    runIn: "",
    timeout: "",
    concurrent: "",
    webDenied: false,
    uses: noUses(),
    intent: "none",
  };
}

/** Whether the draft signs in with a credential pasted into the form. */
export const pastesCredential = (d: Pick<Draft, "method">) =>
  d.method === "api-key" || d.method === "token";

/** Whether the credential pasted is a token (a subscription's) rather than a key: the word every sentence uses. */
export const credentialWord = (d: Pick<Draft, "method">) =>
  d.method === "token" ? "token" : "key";

/** The convention a credential's secret starts from: the variable the CLI itself reads. */
function baseVariable(d: Pick<Draft, "harness" | "method" | "provider">): string {
  if (d.method === "token") return "CLAUDE_CODE_OAUTH_TOKEN";
  if (d.harness === "codex") return "CODEX_API_KEY";
  if (d.harness === "opencode")
    return OPENCODE_PROVIDERS.find((p) => p.value === d.provider)?.variable ?? "API_KEY";
  return "ANTHROPIC_API_KEY";
}

/** A model's name as a secret's suffix: SONNET_KEY for sonnet-key. */
const suffixOf = (key: string) =>
  key
    .toUpperCase()
    .replace(/[^A-Z0-9]+/g, "_")
    .replace(/^_|_$/g, "");

/**
 * The secret a new credential is stored under: the CLI's own variable while
 * no other model reads it, else that variable suffixed with the model's name.
 * A name that starts with the vendor's word does not say it twice
 * (ANTHROPIC_API_KEY_OPUS for anthropic-opus, design QA M15).
 */
export function suggestedVariable(
  d: Pick<Draft, "harness" | "method" | "provider" | "key">,
  taken: readonly string[],
): string {
  const base = baseVariable(d);
  if (!taken.includes(base)) return base;
  const vendor = `${base.split("_")[0]}_`;
  let suffix = suffixOf(d.key);
  if (suffix.startsWith(vendor) && suffix.length > vendor.length)
    suffix = suffix.slice(vendor.length);
  return suffix ? `${base}_${suffix}` : `${base}_2`;
}

/** The secret a cloud provider's secret field is stored under: its variable, suffixed with the model's name. */
export const cloudVariable = (d: Pick<Draft, "key">, field: string) =>
  `${field}_${suffixOf(d.key)}`;

/**
 * Every whitespace and invisible formatting character, none of which a key or
 * a token holds. A terminal that wraps `claude setup-token` output turns the
 * break into spaces when it is copied, and Claude Code then ignores the whole
 * token; a zero-width character comes along from chat and documents.
 */
const STRAY_IN_CREDENTIAL = /[\s\p{Cc}\p{Cf}]/gu;

/** A pasted key or token without the characters a copy adds to it. */
export const compactCredential = (value: string) => value.replace(STRAY_IN_CREDENTIAL, "");

/**
 * The candidate values the draft's credential is: the pasted key or token
 * under its secret, or the cloud fields that are secrets. Empty when nothing
 * new was typed, which keeps what is stored.
 */
export function credentialValues(d: Draft): Record<string, string> {
  if (pastesCredential(d)) {
    const value = compactCredential(d.credential);
    return value && d.variable ? { [d.variable]: value } : {};
  }
  if (d.method !== "cloud") return {};
  const out: Record<string, string> = {};
  for (const field of CLOUD_FIELDS[d.cloud].fields) {
    const value = compactCredential(d.cloudFields[field.name] ?? "");
    if (field.secret && value) out[cloudVariable(d, field.name)] = value;
  }
  return out;
}

/** The model's id as its CLI takes it: OpenCode's names its provider first. */
export function modelId(d: Pick<Draft, "harness" | "provider" | "model">): string {
  const model = d.model.trim();
  return d.harness === "opencode" && model && !model.includes("/")
    ? `${d.provider}/${model}`
    : model;
}

/** A whole number field as the entry writes it: a number, or the text the engine will refuse. */
function wholeOrText(value: string): number | string {
  const trimmed = value.trim();
  return /^\d+$/.test(trimmed) ? Number(trimmed) : trimmed;
}

/** What the wizard writes for the draft's sign-in: the entry's `auth`, and the env variables it sets. */
function signInOf(
  d: Draft,
  stored: Readonly<Record<string, string>>,
): {
  auth: ({ mode: HarnessAuthMode } & Record<string, unknown>) | null;
  env: Record<string, string>;
} {
  const env: Record<string, string> = {};
  switch (d.method) {
    case "api-key":
      if (d.harness === "opencode") {
        // OPENCODE READS ITS PROVIDER'S OWN VARIABLE from env: its profile has no key slot.
        const variable = baseVariable({ ...d, method: "api-key" });
        if (d.variable) env[variable] = ref(d.variable);
        return { auth: null, env };
      }
      return { auth: { mode: "api-key", api_key: d.variable ? ref(d.variable) : "" }, env };
    case "token":
      return { auth: { mode: "subscription", token: d.variable ? ref(d.variable) : "" }, env };
    case "host-login":
    case "device":
      return { auth: { mode: "subscription" }, env };
    case "cloud": {
      const flag = CLOUD_FLAG[d.cloud];
      if (flag) env[flag] = "1";
      for (const field of CLOUD_FIELDS[d.cloud].fields) {
        const typed = d.cloudFields[field.name]?.trim();
        if (field.secret) {
          const variable = typed ? cloudVariable(d, field.name) : stored[field.name];
          if (variable) env[field.name] = ref(variable);
        } else if (typed) {
          env[field.name] = typed;
        }
      }
      return { auth: { mode: "env" }, env };
    }
    case "unchanged":
      return { auth: null, env };
  }
}

/** The model as Configuration wrote it, which an edit starts from and keeps what it does not hold of. */
export interface Base {
  raw: Record<string, unknown>;
  /** The method the model signed in with when the wizard opened. */
  method: Method;
}

/**
 * The document entry the draft becomes: on an edit, the model as written
 * with the wizard's fields replaced, so a field it does not hold (a state
 * directory, a profile override, a variable of the operator's own) is kept.
 */
export function entityOf(d: Draft, base?: Base): Record<string, unknown> {
  const raw = base?.raw ?? {};
  const entity: Record<string, unknown> = Object.fromEntries(
    Object.entries(raw).filter(([k]) => !MANAGED_FIELDS.has(k)),
  );
  entity.harness = d.harness;
  entity.model = modelId(d);
  // WHERE AN EXECUTOR RUNS is a choice only a CLI with a runner has; any other leaves it unset.
  if (d.runIn && canExecute(d)) entity.run_in = d.runIn;
  if (d.timeout.trim()) entity.timeout_seconds = wholeOrText(d.timeout);
  if (d.concurrent.trim()) entity.max_concurrent = wholeOrText(d.concurrent);

  const rawEnv = isRecord(raw.env) ? (raw.env as Record<string, string>) : {};
  const rawAuth = isRecord(raw.auth) ? raw.auth : null;
  if (d.method === "unchanged") {
    if (rawAuth) entity.auth = rawAuth;
    if (Object.keys(rawEnv).length > 0) entity.env = rawEnv;
  } else {
    const stored = storedCloud(rawEnv);
    const { auth, env } = signInOf(d, stored);
    // AN UNCHANGED METHOD KEEPS ITS OTHER AUTH FIELDS (a credential bundle); a new one starts clean.
    const kept =
      rawAuth && base?.method === d.method
        ? Object.fromEntries(
            Object.entries(rawAuth).filter(([k]) => !["mode", "api_key", "token"].includes(k)),
          )
        : {};
    if (auth) entity.auth = { ...kept, ...auth };
    else if (Object.keys(kept).length > 0) entity.auth = kept;
    const merged = {
      ...Object.fromEntries(Object.entries(rawEnv).filter(([k]) => !MANAGED_ENV.has(k))),
      ...env,
    };
    if (Object.keys(merged).length > 0) entity.env = merged;
  }

  const overrides = Object.fromEntries(
    Object.entries(isRecord(raw.overrides) ? raw.overrides : {}).filter(([k]) => k !== "web_tools"),
  );
  if (d.webDenied) overrides.web_tools = "denied";
  if (Object.keys(overrides).length > 0) entity.overrides = overrides;
  return entity;
}

/** The secret each cloud field reads now, by field, from an entry's env. */
function storedCloud(env: Record<string, unknown>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [name, value] of Object.entries(env)) {
    const variable = referenced(value);
    if (variable) out[name] = variable;
  }
  return out;
}

/** Which cloud provider an entry's env sends Claude Code to. */
function cloudOf(env: Record<string, unknown>): Cloud {
  const flagged = (Object.entries(CLOUD_FLAG) as [Cloud, string][]).find(([, flag]) => env[flag]);
  return flagged ? flagged[0] : "gateway";
}

/** The method a model signs in with, from its entry and the engine's reading of it. */
export function methodOf(model: ModelView): Method {
  const raw = model.raw ?? {};
  const auth = isRecord(raw.auth) ? raw.auth : {};
  // AN ENTRY THAT NAMES NO MODE RUNS AS THE ENGINE'S DEFAULT, held to the engine by a Go gate.
  const mode: string = typeof auth.mode === "string" ? auth.mode : DEFAULT_HARNESS_AUTH_MODE;
  if (!isHarness(model.harness)) return "unchanged";
  if (mode === "api-key") return model.harness === "opencode" ? "unchanged" : "api-key";
  if (mode === "env") return model.harness === "claude-code" ? "cloud" : "unchanged";
  if (mode !== "subscription") return "unchanged";
  if (model.harness === "opencode") return "api-key";
  if (model.harness === "codex") return "device";
  return auth.token || model.entry.sign_in.route === "token" ? "token" : "host-login";
}

/** The secret a model's credential is read from now, or "" where none is. */
function variableOf(model: ModelView, method: Method): string {
  const raw = model.raw ?? {};
  const auth = isRecord(raw.auth) ? raw.auth : {};
  const env = isRecord(raw.env) ? raw.env : {};
  const written =
    method === "token"
      ? referenced(auth.token)
      : model.harness === "opencode"
        ? (OPENCODE_PROVIDERS.map((p) => referenced(env[p.variable])).find(Boolean) ?? null)
        : referenced(auth.api_key);
  return written ?? model.admin?.sign_in.variable ?? "";
}

/**
 * A model as the wizard holds it for an edit: every field it writes, read
 * back from the entry, and the uses the engine reports for it. The credential
 * is never read back; a stored one is kept unless replaced.
 */
export function draftOf(model: ModelView): Draft {
  const d = emptyDraft();
  const raw = model.raw ?? {};
  const env = isRecord(raw.env) ? raw.env : {};
  const method = methodOf(model);
  const cloud = method === "cloud" ? cloudOf(env) : d.cloud;
  const [provider, rest] =
    model.harness === "opencode" && model.model.includes("/")
      ? (model.model.split(/\/(.*)/s, 2) as [string, string])
      : [d.provider, model.model];
  return {
    ...d,
    key: model.key,
    harness: model.harness,
    method,
    variable: pastesCredential({ method }) ? variableOf(model, method) : "",
    cloud,
    cloudFields:
      method === "cloud"
        ? Object.fromEntries(
            CLOUD_FIELDS[cloud].fields
              .filter((f) => !f.secret && typeof env[f.name] === "string")
              .map((f) => [f.name, env[f.name] as string]),
          )
        : {},
    provider,
    model: rest,
    runIn: (model.runIn ?? "") as RunIn,
    timeout: model.timeoutSeconds === null ? "" : String(model.timeoutSeconds),
    concurrent: model.maxConcurrent === null ? "" : String(model.maxConcurrent),
    webDenied: model.webDenied,
    uses: model.uses,
    intent: intentOf(model.uses),
  };
}

/** The base an edit of `model` keeps its other fields from. */
export function baseOf(model: ModelView): Base {
  return { raw: model.raw ?? {}, method: methodOf(model) };
}

/** The cloud secrets an edit keeps, by field: the variable each reads now. */
export function storedCloudSecrets(base: Base | undefined): Record<string, string> {
  return storedCloud(isRecord(base?.raw.env) ? (base.raw.env as Record<string, unknown>) : {});
}

/** What saving the draft writes: the next document, and the fallbacks the write drops. */
export interface NextDocument {
  doc: CompanyDocument;
  dropped: Dropped[];
}

/**
 * The whole next document: the entry written (added last in the company's
 * order, or in place on an edit) and every use it gains or loses, by
 * `applyUses`, in one revision.
 */
export function nextDocument(
  doc: CompanyDocument,
  d: Draft,
  editing?: { base: Base; was: Uses },
): NextDocument {
  const next = structuredClone(doc) as Record<string, unknown>;
  const providers = (isRecord(next.providers) ? next.providers : (next.providers = {})) as Record<
    string,
    unknown
  >;
  const llm = (isRecord(providers.llm) ? providers.llm : (providers.llm = {})) as Record<
    string,
    unknown
  >;
  llm[d.key] = entityOf(d, editing?.base);
  // A NEW MODEL GOES LAST in the company's order, written out whole, rather
  // than wherever its name sorts among the keys `llm_order` leaves out: the
  // first in order is the fallback for seats that name none, and a new model
  // must not become it by its spelling.
  if (!editing)
    providers.llm_order = [
      ...providerOrder(llm, providers.llm_order).filter((k) => k !== d.key),
      d.key,
    ];
  return applyUses(next as CompanyDocument, d.key, editing?.was ?? noUses(), d.uses);
}

/** A problem the form finds itself, in the engine's shape so both are drawn the same way. */
function problem(segments: (string | number)[], message: string, kind = "invalid"): ConfigProblem {
  return {
    path: segments.join("."),
    segments,
    kind: kind as ConfigProblem["kind"],
    message,
  };
}

/**
 * The step a problem belongs to, by its field: a value the credential route
 * tested is the Method's, a field of the entry is the step that sets it, and
 * anything in the seats or the assistant is Use it for.
 */
export function stepOf(p: Pick<ConfigProblem, "segments">, key: string): WizardStep {
  const s = p.segments ?? [];
  if (s[0] === "key") return "harness";
  if (s[0] === "test") return "test";
  if (s[0] === "values" || s[0] === "variable") return "signin";
  if (s[0] === "providers" && s[1] === "llm" && s[2] === key) {
    switch (s[3]) {
      case "model":
        return "model";
      case "run_in":
        return "runs";
      case "timeout_seconds":
      case "max_concurrent":
        return "limits";
      case "auth":
      case "env":
        return "signin";
      case "overrides":
        return "assign";
      default:
        return "harness";
    }
  }
  return "assign";
}

/** The field a problem is about, in the words its step labels it: never an engine path (design QA M9). */
export function fieldOf(p: Pick<ConfigProblem, "segments">, key: string): string {
  const s = p.segments ?? [];
  if (s[0] === "key") return "Name";
  if (s[0] === "values") return "Credential";
  if (s[0] === "variable") return "Stored as";
  if (s[0] === "providers" && s[1] === "llm" && s[2] === key) {
    const words: Record<string, string> = {
      harness: "Coding CLI",
      model: "Model",
      run_in: "Where it runs",
      timeout_seconds: "Timeout",
      max_concurrent: "Processes at once",
      auth: "Method",
      env: "Method",
      overrides: "Web tools",
    };
    return words[String(s[3])] ?? STEP_LABELS[stepOf(p, key)];
  }
  if (s[0] === "assistant") return "Ask Crewlet";
  return STEP_LABELS[stepOf(p, key)];
}

/**
 * A problem's message without the engine's path in front of it
 * ("providers.llm.sonnet.model: name the model" reads "Name the model"), so a
 * person reads the sentence and the field it belongs to is said beside it.
 */
export function messageOf(p: Pick<ConfigProblem, "path" | "message">): string {
  let message = p.message.trim();
  if (p.path && message.startsWith(`${p.path}: `)) message = message.slice(p.path.length + 2);
  else message = message.replace(/^(providers|roles|units|assistant)\.[^\s:]*: /, "");
  return message.charAt(0).toUpperCase() + message.slice(1);
}

/**
 * The problems to list once each: the form's own first, then the engine's
 * that say something the form did not (design QA M9). An engine problem on a
 * field the form already flags is the same problem in other words.
 */
export function distinctProblems(
  local: readonly ConfigProblem[],
  engine: readonly ConfigProblem[],
  key: string,
): ConfigProblem[] {
  const fields = new Set(local.map((p) => `${stepOf(p, key)}|${fieldOf(p, key)}`));
  const seen = new Set<string>();
  const out = [...local];
  for (const p of engine) {
    const field = `${stepOf(p, key)}|${fieldOf(p, key)}`;
    const said = `${p.path}|${messageOf(p)}`;
    if (fields.has(field) || seen.has(said)) continue;
    seen.add(said);
    out.push(p);
  }
  return out;
}

/** What an edit keeps without asking for it again. */
export interface Kept {
  /** The stored key or token stands: the method and its secret are unchanged and no new one was asked for. */
  credential: boolean;
  /** The cloud secrets stored now, by field. */
  cloud: Readonly<Record<string, string>>;
}

/** The problems the form can see before asking the engine. */
export function draftProblems(
  d: Draft,
  existing: readonly string[],
  kept: Kept = { credential: false, cloud: {} },
): ConfigProblem[] {
  const out: ConfigProblem[] = [];
  const at = (field: string, message: string, kind?: string) =>
    problem(["providers", "llm", d.key, ...field.split(".")], message, kind);
  if (!d.key) {
    out.push(problem(["key"], "Name the model. Seats and Ask Crewlet name it.", "missing"));
  } else if (!KEY_PATTERN.test(d.key)) {
    out.push(
      problem(
        ["key"],
        "Use lowercase letters, digits, dots, hyphens and underscores, starting with a letter or a digit, such as sonnet-key.",
      ),
    );
  } else if (d.key.length > KEY_MAX) {
    out.push(problem(["key"], `Keep the name to ${KEY_MAX} characters or fewer.`, "out_of_range"));
  } else if (existing.includes(d.key)) {
    out.push(problem(["key"], `There is already a model called ${d.key}.`, "conflict"));
  }
  if (pastesCredential(d)) {
    if (!kept.credential && !d.credential.trim()) {
      out.push(
        problem(
          ["values", d.variable],
          d.method === "token" ? "Paste the token from claude setup-token." : "Paste the API key.",
          "missing",
        ),
      );
    }
    if (!kept.credential && !VARIABLE_PATTERN.test(d.variable)) {
      out.push(
        problem(
          ["variable"],
          "Name the secret in capital letters, digits and underscores, starting with a letter.",
        ),
      );
    }
  }
  if (d.method === "cloud") {
    const missing = CLOUD_FIELDS[d.cloud].fields.filter(
      (f) => !d.cloudFields[f.name]?.trim() && !(f.secret && kept.cloud[f.name]),
    );
    if (missing.length > 0) {
      out.push(
        at(
          "env",
          `Fill in ${missing.map((f) => f.label.replace(/^\w/, (c) => c.toLowerCase())).join(" and ")}.`,
          "missing",
        ),
      );
    }
  }
  if (!d.model.trim()) out.push(at("model", "Choose the model the CLI runs.", "missing"));
  const whole = (v: string) => v.trim() === "" || /^[1-9]\d*$/.test(v.trim());
  if (!whole(d.concurrent))
    out.push(at("max_concurrent", "Use a whole number of 1 or more, or leave it empty for 4."));
  if (!whole(d.timeout))
    out.push(at("timeout_seconds", "Use a whole number of 1 or more, or leave it empty for 600."));
  return out;
}

/** The problems on one step, for a blocked Continue. */
export const problemsOn = (problems: readonly ConfigProblem[], step: WizardStep, key: string) =>
  problems.filter((p) => stepOf(p, key) === step);
