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
  MODEL_KEY_PATTERN,
  type HarnessAuthMode,
  type RunnerCellName,
} from "~/contract/models.ts";
import { HARNESS_FACTS } from "~/contract/harnesses.ts";
import { isReferenceName } from "~/lib/references.ts";
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

/** A built-in harness a model can be added on (`HARNESS_FACTS`, held to the engine's profiles). */
export type Harness = string;
/**
 * How a model signs in, as the wizard offers it. `unchanged` is a model on a
 * sign-in the wizard does not write (another harness, or the engine's own
 * environment): an edit keeps it exactly as Configuration wrote it.
 */
export type Method = "api-key" | "cloud" | "host-login" | "token" | "device" | "unchanged";
export type Cloud = "bedrock" | "vertex" | "foundry" | "gateway" | "gemini-vertex";
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
  /** The provider a harness that fronts many reads its key for (`providersFor`); OpenCode's and Pi's model names it first. */
  provider: string;
  model: string;
  /** The reasoning effort, in the harness's own levels; empty for its default. */
  effort: string;
  /** Qwen Code's OpenAI-compatible endpoint; empty for the harness's own. */
  endpoint: string;
  runIn: RunIn;
  timeout: string;
  concurrent: string;
  webDenied: boolean;
  /** Which seats run each job on the model, and whether the Dashboard Assistant does. */
  uses: Uses;
  /** What the model is for, read from its uses; `custom` where no one choice describes them. */
  intent: Intent;
  /**
   * The harnesses the engine runs a seat's executor on (`ModelsData.executors`),
   * which decide whether the draft's harness can run coding. Not written.
   */
  executors: readonly string[];
}

/** The harnesses a model can be added on, in the order offered: the three that run coding or most work, then by name. */
export const HARNESSES: readonly Harness[] = [
  "claude-code",
  "codex",
  "opencode",
  "copilot",
  "cursor-agent",
  "gemini-cli",
  "grok",
  "hermes",
  "kimi-code",
  "muse-code",
  "pi",
  "qwen-code",
];

const isHarness = (harness: string): boolean => Object.hasOwn(HARNESS_FACTS, harness);

/** What the engine's profile says about adding a model on one harness. */
export type HarnessFacts = (typeof HARNESS_FACTS)[string];

const NO_FACTS: HarnessFacts = { binary: "", signIns: [], efforts: [] };

/** What the engine's profile says about adding a model on the harness; empty for one it does not ship. */
export const factsOf = (harness: string): HarnessFacts => HARNESS_FACTS[harness] ?? NO_FACTS;

/** Whether the draft's harness can run a seat's coding: one the engine runs an executor on. */
export const canExecute = (d: Pick<Draft, "harness" | "executors">) =>
  d.executors.includes(d.harness);

/** The sign-in methods a harness offers, in its facts' order: the one allowed on any engine first. */
export const methodsOf = (harness: string): readonly Method[] => factsOf(harness).signIns;

/** The methods the draft's harness offers; a harness the wizard does not add keeps its sign-in. */
export function methodsFor(d: Pick<Draft, "harness" | "method">): readonly Method[] {
  return isHarness(d.harness) && d.method !== "unchanged" ? methodsOf(d.harness) : ["unchanged"];
}

/** A model provider a harness that fronts many reads a key for, and the variable it reads. */
export interface ProviderKey {
  value: string;
  label: string;
  variable: string;
}

/** The providers OpenCode is offered with, and the variable each one's key is read from. */
export const OPENCODE_PROVIDERS: readonly ProviderKey[] = [
  { value: "anthropic", label: "Anthropic", variable: "ANTHROPIC_API_KEY" },
  { value: "openai", label: "OpenAI", variable: "OPENAI_API_KEY" },
  { value: "openrouter", label: "OpenRouter", variable: "OPENROUTER_API_KEY" },
  { value: "groq", label: "Groq", variable: "GROQ_API_KEY" },
];

/** Hermes's providers by the variables its registry reads (hermes_cli/auth.py); it picks the one keyed. */
const HERMES_PROVIDERS: readonly ProviderKey[] = [
  { value: "openrouter", label: "OpenRouter", variable: "OPENROUTER_API_KEY" },
  { value: "anthropic", label: "Anthropic", variable: "ANTHROPIC_API_KEY" },
  { value: "openai", label: "OpenAI", variable: "OPENAI_API_KEY" },
  { value: "gemini", label: "Google AI Studio", variable: "GEMINI_API_KEY" },
  { value: "xai", label: "xAI", variable: "XAI_API_KEY" },
  { value: "deepseek", label: "DeepSeek", variable: "DEEPSEEK_API_KEY" },
];

/** Pi's providers by the variables its docs name (docs/providers.md). */
const PI_PROVIDERS: readonly ProviderKey[] = [
  { value: "anthropic", label: "Anthropic", variable: "ANTHROPIC_API_KEY" },
  { value: "openai", label: "OpenAI", variable: "OPENAI_API_KEY" },
  { value: "openrouter", label: "OpenRouter", variable: "OPENROUTER_API_KEY" },
  { value: "google", label: "Google Gemini", variable: "GEMINI_API_KEY" },
  { value: "groq", label: "Groq", variable: "GROQ_API_KEY" },
  { value: "xai", label: "xAI", variable: "XAI_API_KEY" },
];

const PROVIDER_KEYS: Readonly<Record<string, readonly ProviderKey[]>> = {
  opencode: OPENCODE_PROVIDERS,
  hermes: HERMES_PROVIDERS,
  pi: PI_PROVIDERS,
};

/** The providers a harness reads a key from `env` for; empty for one with a key of its own. */
export const providersFor = (harness: string): readonly ProviderKey[] =>
  factsOf(harness).providerKeys ? (PROVIDER_KEYS[harness] ?? []) : [];

/** The harnesses whose model names its provider first, `<provider>/<model>`. */
const PROVIDER_IN_MODEL: readonly string[] = ["opencode", "pi"];

/** Whether the harness's model names its provider first. */
export const namesProvider = (harness: string) => PROVIDER_IN_MODEL.includes(harness);

/** Qwen Code's endpoint variable, which the wizard writes from the endpoint field. */
export const QWEN_ENDPOINT = "OPENAI_BASE_URL";

export interface CloudField {
  name: string;
  label: string;
  secret?: boolean;
  placeholder: string;
}

/** Each cloud provider's fields, by the variable its harness reads. */
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
  "gemini-vertex": {
    label: "Google Vertex AI",
    fields: [
      { name: "GOOGLE_CLOUD_PROJECT", label: "Project ID", placeholder: "my-project" },
      { name: "GOOGLE_CLOUD_LOCATION", label: "Region", placeholder: "us-central1" },
      {
        name: "GOOGLE_API_KEY",
        label: "Vertex AI API key",
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

/**
 * The switch that sends a harness to a cloud provider, and the value it is set
 * to (each harness's profile `sign_in_env`); a gateway has none.
 */
const CLOUD_FLAG: Readonly<Partial<Record<Cloud, readonly [string, string]>>> = {
  bedrock: ["CLAUDE_CODE_USE_BEDROCK", "1"],
  vertex: ["CLAUDE_CODE_USE_VERTEX", "1"],
  foundry: ["CLAUDE_CODE_USE_FOUNDRY", "1"],
  "gemini-vertex": ["GOOGLE_GENAI_USE_VERTEXAI", "true"],
};

/** The clouds each harness that signs in from its env (`cloud`) is offered, in order. */
const HARNESS_CLOUDS: Readonly<Record<string, readonly Cloud[]>> = {
  "claude-code": ["bedrock", "vertex", "foundry", "gateway"],
  "gemini-cli": ["gemini-vertex"],
};

/** The clouds harness is offered, none for a harness with no `cloud` sign-in. */
export const cloudsOf = (harness: string): readonly Cloud[] => HARNESS_CLOUDS[harness] ?? [];

/** Every env variable the wizard writes on the harness, which an edit replaces rather than keeps. */
function managedEnv(harness: string): Set<string> {
  return new Set<string>([
    ...(methodsOf(harness).includes("cloud")
      ? cloudsOf(harness).flatMap((c) => [
          ...(CLOUD_FLAG[c] ? [CLOUD_FLAG[c][0]] : []),
          ...CLOUD_FIELDS[c].fields.map((f) => f.name),
        ])
      : []),
    ...providersFor(harness).map((p) => p.variable),
    ...(harness === "qwen-code" ? [QWEN_ENDPOINT] : []),
  ]);
}

/** The fields of an entry the wizard writes; every other field an edit keeps as written. */
const MANAGED_FIELDS = new Set([
  "harness",
  "model",
  "effort",
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
/** The one rule a new name keeps, said by its field and by its problem alike. */
export const KEY_RULE = `Lowercase letters, digits, dots, hyphens and underscores, up to ${KEY_MAX} characters.`;
/** The engine's grammar for a model's name (`config.ProviderKeyPattern`, held as `MODEL_KEY_PATTERN`). */
const KEY_PATTERN = new RegExp(MODEL_KEY_PATTERN);

/** The steps a draft walks, ending on Review: Where it runs only for a harness that runs coding. */
export function stepsFor(d: Pick<Draft, "harness" | "executors">): WizardStep[] {
  return WIZARD_STEPS.filter((s) => s !== "runs" || canExecute(d));
}

export const STEP_LABELS: Readonly<Record<WizardStep, string>> = {
  harness: "Name and harness",
  signin: "Method",
  model: "Model",
  runs: "Where it runs",
  limits: "Limits",
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
export function emptyDraft(executors: readonly string[]): Draft {
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
    effort: "",
    endpoint: "",
    runIn: "",
    timeout: "",
    concurrent: "",
    webDenied: false,
    uses: noUses(),
    intent: "none",
    executors,
  };
}

/** Whether the draft signs in with a credential pasted into the form. */
export const pastesCredential = (d: Pick<Draft, "method">) =>
  d.method === "api-key" || d.method === "token";

/** Whether the credential pasted is a token (a subscription's) rather than a key: the word every sentence uses. */
export const credentialWord = (d: Pick<Draft, "method">) =>
  d.method === "token" ? "token" : "key";

/** The convention a credential's secret starts from: the variable the harness itself reads. */
function baseVariable(d: Pick<Draft, "harness" | "method" | "provider">): string {
  if (d.method === "token") return "CLAUDE_CODE_OAUTH_TOKEN";
  const facts = factsOf(d.harness);
  if (facts.providerKeys)
    return providersFor(d.harness).find((p) => p.value === d.provider)?.variable ?? "API_KEY";
  // A KEY OTHER VENDORS SHARE (Qwen Code's OPENAI_API_KEY) is stored under a name of its own.
  if (facts.keyShared) return "QWEN_API_KEY";
  return facts.keyEnv ?? "API_KEY";
}

/** A model's name as a secret's suffix: SONNET_KEY for sonnet-key. */
const suffixOf = (key: string) =>
  key
    .toUpperCase()
    .replace(/[^A-Z0-9]+/g, "_")
    .replace(/^_|_$/g, "");

/**
 * The secret a new credential is stored under: the harness's own variable while
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

/**
 * The model's id as its harness takes it: OpenCode's and Pi's name the chosen
 * provider before the provider's own id, always, since that id may itself hold
 * a slash or the provider's name (OpenRouter's `qwen/qwen3-coder`, `openrouter/auto`).
 */
export function modelId(d: Pick<Draft, "harness" | "provider" | "model">): string {
  const model = d.model.trim();
  return namesProvider(d.harness) && model ? `${d.provider}/${model}` : model;
}

/** A catalogue id offered for the draft's harness as the draft holds it: the provider's own id. */
export function catalogueModel(d: Pick<Draft, "harness" | "provider">, id: string): string {
  return namesProvider(d.harness) && id.startsWith(`${d.provider}/`)
    ? id.slice(d.provider.length + 1)
    : id;
}

/** The choices a model id is named under: the harness, and the provider or cloud it is sent to. */
export const modelScope = (d: Pick<Draft, "harness" | "method" | "provider" | "cloud">) =>
  [
    d.harness,
    providersFor(d.harness).length > 0 ? d.provider : "",
    d.method === "cloud" ? d.cloud : "",
  ].join("|");

/** The choices a credential is for: the harness, the method, and the provider or cloud it signs in to. */
export const credentialScope = (d: Pick<Draft, "harness" | "method" | "provider" | "cloud">) =>
  `${d.method}|${modelScope(d)}`;

/** What a change to the draft is weighed against. */
export interface Choices {
  /** The model as it stood when an edit opened; null while adding. */
  original: Draft | null;
  /** The secrets other models read, which a suggested name avoids. */
  taken: readonly string[];
  /** Whether the reader named the secret for the credential scope the draft is in. */
  named: boolean;
  /** The helpers Everything gives the draft on its harness (`everythingHelpers`); none where left out. */
  everything?: (d: Draft) => { assistant: boolean; advisor: boolean };
}

/**
 * The draft after `patch`, with every value that belongs to a choice the patch
 * moved cleared, or put back as the edited model had it when the choice is
 * back to its own: the model and its effort are their vendor's, the pasted
 * credential and its secret's name are the provider's they sign in to, and a
 * use or a place to run is the harness's. Returns whether the secret stays named.
 */
export function withChoices(
  d: Draft,
  patch: Partial<Draft>,
  { original, taken, named, everything }: Choices,
): { draft: Draft; named: boolean } {
  const next: Draft = { ...d, ...patch };
  const keep = <K extends keyof Draft>(field: K, scope: (x: Draft) => string, blank: Draft[K]) => {
    if (field in patch || scope(next) === scope(d)) return;
    next[field] = original && scope(next) === scope(original) ? original[field] : blank;
  };
  keep("model", modelScope, "");
  keep("effort", modelScope, "");
  keep("credential", credentialScope, "");
  keep("endpoint", (x) => x.harness, "");
  const moved = credentialScope(next) !== credentialScope(d);
  let stays = "variable" in patch || (named && !moved);
  if (moved && original && credentialScope(next) === credentialScope(original))
    stays = !!original.variable;
  if (!("variable" in patch) && pastesCredential(next)) {
    if (moved && stays && original) next.variable = original.variable;
    else if (!stays) next.variable = suggestedVariable(next, taken);
  }
  // A CLOUD IS THE HARNESS'S: Claude Code's Bedrock is no Gemini CLI provider.
  const clouds = cloudsOf(next.harness);
  if (!("cloud" in patch) && clouds.length > 0 && !clouds.includes(next.cloud)) {
    next.cloud = clouds[0] ?? next.cloud;
    next.cloudFields = {};
  }
  if (next.harness !== d.harness) {
    const coding = canExecute(next);
    const helpers = !!factsOf(next.harness).assistant;
    if (!coding) next.runIn = "";
    const dropsCoding = !coding && (next.intent === "all" || next.intent === "coding");
    const dropsHelper = !helpers && (next.intent === "assistant" || next.intent === "advisor");
    // EVERYTHING IS THE HARNESS'S TOO: kept, it takes again the helpers this
    // harness runs; dropped, it leaves nothing it gave behind.
    const takes =
      next.intent === "all" && !dropsCoding && everything
        ? everything(next)
        : { assistant: false, advisor: false };
    const gone = dropsCoding && next.intent === "all";
    next.uses = {
      ...(dropsCoding ? noUses() : next.uses),
      assistant: helpers && !gone && (next.uses.assistant || takes.assistant),
      advisor: helpers && !gone && (next.uses.advisor || takes.advisor),
    };
    if (dropsCoding || dropsHelper) next.intent = intentOf(next.uses);
    if (original && next.harness === original.harness) {
      next.uses = original.uses;
      next.intent = original.intent;
      next.runIn = original.runIn;
    }
  }
  return { draft: next, named: stays };
}

/** The effort levels the draft's harness takes on its model, lowest first (its profile may narrow
 * them per model); empty where it has no setting the engine passes. */
export function effortLevels(d: Pick<Draft, "harness" | "provider" | "model">): readonly string[] {
  const facts = factsOf(d.harness);
  const model = modelId(d);
  for (const rule of facts.effortModels ?? []) {
    if (model && new RegExp(`^(?:${rule.match})$`).test(model)) return rule.levels;
  }
  return facts.efforts;
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
      if (factsOf(d.harness).providerKeys) {
        // A HARNESS THAT FRONTS MANY READS ITS PROVIDER'S OWN VARIABLE from env: its profile has no key slot.
        const variable = baseVariable({ ...d, method: "api-key" });
        if (d.variable) env[variable] = ref(d.variable);
        return { auth: null, env };
      }
      if (d.harness === "qwen-code" && d.endpoint.trim()) env[QWEN_ENDPOINT] = d.endpoint.trim();
      return { auth: { mode: "api-key", api_key: d.variable ? ref(d.variable) : "" }, env };
    case "token":
      return { auth: { mode: "subscription", token: d.variable ? ref(d.variable) : "" }, env };
    case "host-login":
    case "device":
      return { auth: { mode: "subscription" }, env };
    case "cloud": {
      const flag = CLOUD_FLAG[d.cloud];
      if (flag) env[flag[0]] = flag[1];
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
  if (d.effort) entity.effort = d.effort;
  // WHERE AN EXECUTOR RUNS is a choice only a harness with a runner has; any other leaves it unset.
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
    const managed = managedEnv(d.harness);
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
      ...Object.fromEntries(Object.entries(rawEnv).filter(([k]) => !managed.has(k))),
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

/** Which of harness's clouds an entry's env sends it to: the one whose switch it sets, else the switchless one. */
function cloudOf(harness: string, env: Record<string, unknown>): Cloud {
  const clouds = cloudsOf(harness);
  const flagged = clouds.find((c) => {
    const flag = CLOUD_FLAG[c];
    return flag !== undefined && env[flag[0]] !== undefined;
  });
  return flagged ?? clouds.find((c) => !CLOUD_FLAG[c]) ?? clouds[0] ?? "gateway";
}

/** The method a model signs in with, from its entry and the engine's reading of it. */
export function methodOf(model: ModelView): Method {
  const raw = model.raw ?? {};
  const auth = isRecord(raw.auth) ? raw.auth : {};
  const env = isRecord(raw.env) ? raw.env : {};
  // AN ENTRY THAT NAMES NO MODE RUNS AS THE ENGINE'S DEFAULT, held to the engine by a Go gate.
  const mode: string = typeof auth.mode === "string" ? auth.mode : DEFAULT_HARNESS_AUTH_MODE;
  if (!isHarness(model.harness)) return "unchanged";
  const facts = factsOf(model.harness);
  const offers = (m: Method): Method =>
    (facts.signIns as readonly Method[]).includes(m) ? m : "unchanged";
  if (mode === "api-key") return facts.providerKeys ? "unchanged" : offers("api-key");
  if (mode === "env") return offers("cloud");
  if (mode !== "subscription") return "unchanged";
  if (facts.providerKeys)
    return providersFor(model.harness).some((p) => referenced(env[p.variable]))
      ? "api-key"
      : offers("host-login");
  if (model.harness === "codex") return "device";
  if (auth.token || model.entry.sign_in.route === "token") return offers("token");
  return offers("host-login");
}

/** The secret a model's credential is read from now, or "" where none is. */
function variableOf(model: ModelView, method: Method): string {
  const raw = model.raw ?? {};
  const auth = isRecord(raw.auth) ? raw.auth : {};
  const env = isRecord(raw.env) ? raw.env : {};
  const written =
    method === "token"
      ? referenced(auth.token)
      : factsOf(model.harness).providerKeys
        ? (providersFor(model.harness)
            .map((p) => referenced(env[p.variable]))
            .find(Boolean) ?? null)
        : referenced(auth.api_key);
  return written ?? model.admin?.sign_in.variable ?? "";
}

/**
 * A model as the wizard holds it for an edit: every field it writes, read
 * back from the entry, and the uses the engine reports for it. The credential
 * is never read back; a stored one is kept unless replaced.
 */
export function draftOf(model: ModelView, executors: readonly string[]): Draft {
  const d = emptyDraft(executors);
  const raw = model.raw ?? {};
  const env = isRecord(raw.env) ? raw.env : {};
  const method = methodOf(model);
  const cloud =
    method === "cloud" ? cloudOf(model.harness, env) : (cloudsOf(model.harness)[0] ?? d.cloud);
  // THE PROVIDER a provider-key entry is keyed for, else the one its model names first.
  const keyed = providersFor(model.harness).find((p) => typeof env[p.variable] === "string");
  const [provider, rest] =
    namesProvider(model.harness) && model.model.includes("/")
      ? (model.model.split(/\/(.*)/s, 2) as [string, string])
      : [keyed?.value ?? d.provider, model.model];
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
    effort: typeof raw.effort === "string" ? raw.effort : "",
    endpoint:
      model.harness === "qwen-code" && typeof env[QWEN_ENDPOINT] === "string"
        ? (env[QWEN_ENDPOINT] as string)
        : "",
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
  // A COMPANY WITH NO PLACE TO RUN CODING gets this host, as the engine's own
  // remedy says: every agent seat's executor runs in a box `providers.sandbox`
  // configures, so a first coding model was refused, with nothing on the page
  // to set one up. `local: {}` with `default_run_in: direct` runs it on this
  // host as the engine's user; Where it runs and Review say so before a save.
  if (canExecute(d) && !hasSandbox(doc)) providers.sandbox = { ...HOST_SANDBOX };
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

/** The place to run coding a save sets up where the company has none: this engine's host. */
const HOST_SANDBOX = { local: {}, default_run_in: "direct" } as const;

/** The document with this engine's host set up to run coding, as the engine's own remedy says. */
export function withHostSandbox(doc: CompanyDocument): CompanyDocument {
  const next = structuredClone(doc) as Record<string, unknown>;
  const providers = (isRecord(next.providers) ? next.providers : (next.providers = {})) as Record<
    string,
    unknown
  >;
  providers.sandbox = { ...HOST_SANDBOX };
  return next as CompanyDocument;
}

/** Whether the company document configures a place to run a seat's executor (`providers.sandbox`). */
export function hasSandbox(doc: CompanyDocument | null): boolean {
  return !!doc && isRecord(doc.providers) && isRecord(doc.providers.sandbox);
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
 * The harness program a problem says is missing from the engine host, or "":
 * the doctor's "not on this host's PATH". No step of the wizard installs a
 * program, so such a problem is said plainly and offers no step to fix it in.
 */
export function notInstalled(p: Pick<ConfigProblem, "message">): string {
  return /"([^"]+)" is not on this host's PATH/.exec(p.message)?.[1] ?? "";
}

/** Whether a step of the wizard can fix a problem: not a program missing from the host. */
export function fixable(p: Pick<ConfigProblem, "message">): boolean {
  return !notInstalled(p);
}

/**
 * The step a problem belongs to, by its field: a value the credential route
 * tested is the Method's, a field of the entry is the step that sets it, and
 * anything in the seats or the helpers is Review's, which lists what moves.
 */
export function stepOf(p: Pick<ConfigProblem, "segments">, key: string): WizardStep {
  const s = p.segments ?? [];
  if (s[0] === "key") return "harness";
  // THE TEST CALL IS RUN ON REVIEW, where its result is said.
  if (s[0] === "test") return "review";
  if (s[0] === "values" || s[0] === "variable") return "signin";
  // WHERE CODING RUNS is the company's sandbox block: Where it runs says it.
  if (s[0] === "providers" && s[1] === "sandbox") return "runs";
  if (s[0] === "providers" && s[1] === "llm" && s[2] === key) {
    switch (s[3]) {
      case "model":
      case "effort":
        return "model";
      case "run_in":
        return "runs";
      case "timeout_seconds":
      case "max_concurrent":
        return "limits";
      case "auth":
      case "env":
        return "signin";
      // WEB TOOLS are switched on Limits.
      case "overrides":
        return "limits";
      default:
        return "harness";
    }
  }
  return "review";
}

/** The field a problem is about, in the words its step labels it: never an engine path (design QA M9). */
export function fieldOf(p: Pick<ConfigProblem, "segments">, key: string): string {
  const s = p.segments ?? [];
  if (s[0] === "key") return "Name";
  if (s[0] === "values") return "Credential";
  if (s[0] === "variable") return "Stored as";
  if (s[0] === "providers" && s[1] === "llm" && s[2] === key) {
    const words: Record<string, string> = {
      harness: "Harness",
      model: "Model",
      effort: "Reasoning effort",
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
  if (s[0] === "advisor") return "Advisor";
  return STEP_LABELS[stepOf(p, key)];
}

/**
 * A problem's message without the engine's path in front of it
 * ("providers.llm.sonnet.model: name the model" reads "Name the model"), so a
 * person reads the sentence and the field it belongs to is said beside it.
 */
export function messageOf(p: Pick<ConfigProblem, "path" | "message">): string {
  const missing = notInstalled(p);
  if (missing) return `Install ${missing} on the engine host: the harness runs there.`;
  let message = p.message.trim();
  if (p.path && message.startsWith(`${p.path}: `)) message = message.slice(p.path.length + 2);
  else message = message.replace(/^(providers|roles|units|assistant|advisor)\.[^\s:]*: /, "");
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

/**
 * What an edit keeps of the model as it opened: its stored key or token only
 * while the draft signs in to the same provider the same way under the same
 * secret, and its cloud secrets only for the same cloud.
 */
export function keptOf(
  d: Draft,
  original: Draft | null,
  base: Base | undefined,
  replacing: boolean,
): Kept {
  return {
    credential:
      !!original &&
      !replacing &&
      pastesCredential(d) &&
      credentialScope(d) === credentialScope(original) &&
      d.variable === original.variable &&
      !!original.variable,
    cloud:
      d.method === "cloud" && original?.method === "cloud" && d.cloud === original.cloud
        ? storedCloudSecrets(base)
        : {},
  };
}

/** The problems the form can see before asking the engine. */
export function draftProblems(
  d: Draft,
  existing: readonly string[],
  kept: Kept = { credential: false, cloud: {} },
  /**
   * Whether the draft edits a model that exists. ITS NAME IS NOT ASKED AGAIN:
   * the engine accepted it (up to 63 characters), the edit shows it read-only,
   * and the wizard's own cap of 40 refused every edit of a longer one.
   */
  editing = false,
): ConfigProblem[] {
  const out: ConfigProblem[] = [];
  const at = (field: string, message: string, kind?: string) =>
    problem(["providers", "llm", d.key, ...field.split(".")], message, kind);
  if (editing) {
    // The name is the model's identity, read-only here.
  } else if (!d.key) {
    out.push(problem(["key"], "Name the model. Seats and Ask Crewlet name it.", "missing"));
  } else if (d.key.length > KEY_MAX) {
    out.push(problem(["key"], `Keep the name to ${KEY_MAX} characters or fewer.`, "out_of_range"));
  } else if (!KEY_PATTERN.test(d.key)) {
    out.push(problem(["key"], `${KEY_RULE} It starts with a letter or a digit.`));
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
    // THE SECRET STORE'S OWN GRAMMAR (`secrets.CheckName`, which is the
    // `${VAR}` grammar), and no narrower: a name the store takes is one a
    // person may already keep their key under.
    if (!kept.credential && !isReferenceName(d.variable)) {
      out.push(
        problem(
          ["variable"],
          "Name the secret with letters, digits and underscores, starting with a letter or an underscore.",
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
  if (!d.model.trim()) out.push(at("model", "Choose the model the harness runs.", "missing"));
  else if (d.effort && !effortLevels(d).includes(d.effort)) {
    const levels = effortLevels(d);
    out.push(
      at(
        "effort",
        levels.length === 0
          ? `${modelId(d)} runs with no reasoning effort setting. Choose the harness default.`
          : `${modelId(d)} takes ${levels.join(", ")}. Choose one of those or the harness default.`,
      ),
    );
  }
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
