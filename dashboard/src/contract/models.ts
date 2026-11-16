/**
 * How the engine reads one `providers.llm` entry: the closed set of its
 * `auth.mode`, and what each field the entry leaves out comes to.
 *
 * Settings › Models edits an entry as the company document WRITES it, and
 * an entry that names no `harness` or no `auth.mode` still runs as something.
 * What it runs as is the engine's default, so the wizard says it from here
 * rather than guessing, and the words for each sign-in are keyed on this union
 * so a mode the engine adds is a typecheck failure rather than a choice drawn
 * with nothing said.
 *
 * HELD AGAINST `config.HarnessAuthModes`, `config.ExecutorHarnesses` and an unset
 * `config.LLMProvider`'s `HarnessName`, `ResolvedAuthMode`, `Timeout` and
 * `Concurrency` by
 * `internal/config.TestTheModelsScreenReadsAnEntryAsTheEngineDoes`: a default
 * that drifted would name a harness, a sign-in or a cap the engine does not apply
 * to the entry on screen, and an executor set that drifted would say a model
 * runs an executor that the engine skips.
 */

/**
 * How an entry's harness signs in. `subscription`: the operator's own harness login:
 * a headless token, or the files `crewlet llm login` wrote. `api-key`: one key,
 * put in the harness's own key variable, billed to the metered account.
 * `inherit-env`: whichever of the harness's two variables the engine's own
 * environment holds. `env`: the entry's own env signs the harness in (a cloud
 * provider's switch and credentials, or a gateway's token), and neither the
 * token nor the key reaches the harness.
 */
export type HarnessAuthMode = "subscription" | "api-key" | "inherit-env" | "env";

/**
 * What each built-in harness is called where a person reads it: "Claude Code
 * subscription limit reached", never "claude-code". EXACTLY the engine's
 * built-in profiles (`harnessprofile.BuiltinNames`) bar `custom`, held by
 * `internal/config.TestEveryBuiltInHarnessHasAProductName`, so a profile the
 * engine ships is named the day it ships. An entry on `custom` describes a
 * harness of its own and is called by its key.
 */
export const HARNESS_PRODUCTS: Readonly<Record<string, string>> = {
  "claude-code": "Claude Code",
  codex: "Codex",
  copilot: "GitHub Copilot",
  "cursor-agent": "Cursor",
  "gemini-cli": "Gemini CLI",
  grok: "Grok",
  hermes: "Hermes",
  "kimi-code": "Kimi Code",
  "muse-code": "Muse Code",
  opencode: "OpenCode",
  pi: "Pi",
  "qwen-code": "Qwen Code",
};

/** The harness an entry that names no `harness` drives. */
export const DEFAULT_HARNESS = "claude-code";

/**
 * The harnesses that can run a seat's executor: the ones with an in-box
 * runner. Every agent seat's executor is its harness's own run, placed by the
 * launching entry's `run_in`, and a member of an executor chain on any other
 * harness is skipped at launch, and that entry serves the text phases alone (the
 * reviewer, workers, onboarding and side passes).
 */
export const EXECUTOR_HARNESSES = ["claude-code", "opencode"] as const;

/**
 * How a model on a harness can sign in, as Settings › Models offers it:
 * `api-key` (a key in the harness's own variable, or, where `providerKeys`, a
 * provider's key in the entry's `env`), `host-login` (`crewlet llm login` on the
 * engine host), `token` (a headless subscription token), `device` (a device
 * code approved on the vendor's site) and `cloud` (a cloud provider or gateway).
 */
type HarnessSignIn = "api-key" | "host-login" | "token" | "device" | "cloud";

/** What the engine's built-in profile of one harness says about adding a model on it. */
interface HarnessFacts {
  /** The executable it runs as. */
  binary: string;
  /** The sign-ins the engine runs for the harness, in the order offered. */
  signIns: readonly HarnessSignIn[];
  /** The variable its key goes in under `auth.mode: api-key`. */
  keyEnv?: string;
  /** The key variable is one other vendors' keys go in too, so a model names its own secret. */
  keyShared?: boolean;
  /** It reads a provider's key from the entry's `env` (`env_sign_in`). */
  providerKeys?: boolean;
  /** `crewlet llm login` runs its own login (`command`), or adopts one made with the harness (`adopt`). */
  login?: "command" | "adopt";
  /** The engine can deny its web tools (`overrides.web_tools: denied`). */
  webDeniable?: boolean;
  /** Ask Crewlet can run on it: web denied, it keeps no tool of its own. */
  assistant?: boolean;
  /** Its reasoning effort levels, lowest first; empty where it has no setting the engine passes. */
  efforts: readonly string[];
  /** The models it restricts: the levels each takes, by a regular expression over the model, matched whole. */
  effortModels?: readonly { match: string; levels: readonly string[] }[];
  /** Why it has no effort setting the engine passes. */
  effortNote?: string;
}

/**
 * Every built-in harness bar `custom`, as its profile reads. Held against
 * `harnessprofile` by `internal/config.TestTheModelsScreenKnowsEveryHarnessAsItsProfileReads`,
 * so a sign-in or a level the wizard offers is one the engine runs.
 */
export const HARNESS_FACTS: Readonly<Record<string, HarnessFacts>> = {
  "claude-code": {
    binary: "claude",
    webDeniable: true,
    assistant: true,
    signIns: ["api-key", "host-login", "token", "cloud"],
    keyEnv: "ANTHROPIC_API_KEY",
    login: "command",
    efforts: ["low", "medium", "high", "xhigh", "max"],
    effortModels: [
      {
        match:
          "(.*[./])?(claude-3-.*|claude-(opus|sonnet)-4(-0)?|claude-opus-4-1|claude-sonnet-4-5|claude-haiku-4-5)(-20[0-9]{6})?(-v[0-9].*|@.*|\\[.*)?",
        levels: [],
      },
      {
        match: "(.*[./])?claude-opus-4-5(-20[0-9]{6})?(-v[0-9].*|@.*|\\[.*)?",
        levels: ["low", "medium", "high"],
      },
      {
        match: "(.*[./])?claude-(opus|sonnet)-4-6(-20[0-9]{6})?(-v[0-9].*|@.*|\\[.*)?",
        levels: ["low", "medium", "high", "max"],
      },
    ],
  },
  codex: {
    binary: "codex",
    webDeniable: true,
    signIns: ["api-key", "device", "host-login"],
    keyEnv: "CODEX_API_KEY",
    login: "command",
    efforts: ["none", "minimal", "low", "medium", "high", "xhigh", "max"],
  },
  opencode: {
    binary: "opencode",
    webDeniable: true,
    signIns: ["api-key", "host-login"],
    providerKeys: true,
    login: "command",
    efforts: ["none", "minimal", "low", "medium", "high", "xhigh", "max"],
  },
  "gemini-cli": {
    binary: "gemini",
    signIns: ["api-key", "host-login"],
    keyEnv: "GEMINI_API_KEY",
    login: "adopt",
    efforts: [],
    effortNote:
      "Gemini CLI 0.65 reads a thinking level only from its settings.json, per model family (thinkingLevel or thinkingBudget), and the engine writes that file for each call",
  },
  "qwen-code": {
    binary: "qwen",
    signIns: ["api-key"],
    keyEnv: "OPENAI_API_KEY",
    keyShared: true,
    efforts: [],
    effortNote:
      "Qwen Code 0.26 reads model.reasoningEffort only from its settings.json, which the engine writes for each call",
  },
  "cursor-agent": {
    binary: "cursor-agent",
    signIns: ["api-key", "host-login"],
    keyEnv: "CURSOR_API_KEY",
    login: "command",
    efforts: [],
    effortNote:
      "Cursor Agent takes effort only as part of the model id: name a variant `cursor-agent --list-models` prints, such as gpt-5.5-high",
  },
  copilot: {
    binary: "copilot",
    signIns: ["api-key", "host-login"],
    keyEnv: "COPILOT_GITHUB_TOKEN",
    login: "command",
    efforts: ["none", "minimal", "low", "medium", "high", "xhigh", "max"],
  },
  grok: {
    binary: "grok",
    signIns: ["api-key", "host-login"],
    keyEnv: "XAI_API_KEY",
    login: "adopt",
    efforts: ["none", "minimal", "low", "medium", "high", "xhigh", "max"],
  },
  "muse-code": {
    binary: "muse",
    signIns: ["api-key", "host-login"],
    keyEnv: "META_API_KEY",
    login: "command",
    efforts: ["minimal", "low", "medium", "high", "xhigh", "max"],
  },
  "kimi-code": {
    binary: "kimi",
    signIns: ["host-login"],
    login: "command",
    efforts: ["low", "medium", "high", "xhigh", "max"],
  },
  hermes: {
    binary: "hermes",
    signIns: ["api-key", "host-login"],
    providerKeys: true,
    login: "command",
    efforts: ["none", "minimal", "low", "medium", "high", "xhigh", "max"],
  },
  pi: {
    binary: "pi",
    webDeniable: true,
    assistant: true,
    signIns: ["api-key", "host-login"],
    providerKeys: true,
    login: "command",
    efforts: ["off", "minimal", "low", "medium", "high", "xhigh", "max"],
  },
};

/** An entry's `auth.mode` when it names none. */
export const DEFAULT_HARNESS_AUTH_MODE: HarnessAuthMode = "subscription";

/** The wall-clock cap on one stretch of a harness's own work (no tool call in flight) when `timeout_seconds` is unset. */
export const DEFAULT_HARNESS_TIMEOUT_SECONDS = 600;

/** How many harness processes an entry runs at once when `max_concurrent` is unset. */
export const DEFAULT_HARNESS_MAX_CONCURRENT = 4;

/**
 * What a model's name, its `providers.llm` key, may look like
 * (`config.ProviderKeyPattern`): a lowercase letter or digit, then up to 62
 * more of those, `.`, `_` and `-`. The wizard holds the name a person types to
 * it before the entry is written. A STRING, compiled where it is used.
 */
export const MODEL_KEY_PATTERN = "^[a-z0-9][a-z0-9._-]{0,62}$";

/**
 * ONE reading of a model's condition, decided by the engine
 * (`queries.ModelHealth`) so the list, the model's page and the By job view
 * cannot each decide it differently. Held against `queries.ModelHealths` by
 * `internal/api/queries.TestTheModelsScreenReadsTheEnginesHealthAndAlarms`.
 */
export type ModelHealth =
  "sitting_out" | "no_sign_in" | "rejected" | "expiring" | "untested" | "ready";

/**
 * The five model alarms (statelog) `llm_entries` carries. Held against the
 * engine's own five by the same gate, so an alarm the engine raises is never
 * one the page has no words for.
 */
export type ModelAlarmKind =
  | "llm_credential_expiring"
  | "llm_credential_expires_soon"
  | "llm_sign_in_refused"
  | "llm_limit"
  | "llm_limit_waiting";

/**
 * Where a device sign-in is (`coord.DeviceSignInStatus`): the dialog's status
 * line and its tone are keyed on it. Held against `coord.DeviceSignInStatuses`
 * by `internal/coord.TestTheModelsScreenNamesEveryDeviceSignInStatus`, so a
 * status the engine adds is never one the dialog has no words for.
 */
export type ModelDeviceStatus =
  "starting" | "pending" | "cancel_requested" | "approved" | "failed" | "expired" | "cancelled";

/**
 * The device sign-in statuses a session ends in (`DeviceSignInStatus.Ended`):
 * nothing changes the record again, so the dialog stops polling and offers to
 * start again. Held to the engine's by the same gate.
 */
export const DEVICE_SIGN_IN_ENDED = [
  "approved",
  "failed",
  "expired",
  "cancelled",
] as const satisfies readonly ModelDeviceStatus[];

/**
 * How a model's harness signs in as one node resolves it (`harness.SignInRoute`).
 * Held against `harness.SignInRoutes` by
 * `internal/providers/llm/harness.TestTheModelsScreenNamesTheEnginesSignIns`.
 */
export type ModelSignInRoute =
  "token" | "credentials" | "api-key" | "env" | "inherit-env" | "environment" | "none";

/**
 * Which window of a vendor's limit a model sits out (`llm.LimitWindow`): a
 * session or a weekly window holds every model on the plan, a model window
 * holds the one model. Held against `llm.LimitWindows` by the sign-in gate.
 */
export type ModelLimitWindow = "session" | "weekly" | "model" | "unknown";

/**
 * One line of a model's test (`harness.CheckName`), in the doctor's order.
 * Held against the doctor's own order by the sign-in gate.
 */
export type ModelCheckName =
  "binary" | "version" | "sign-in" | "usage" | "smoke" | "shell" | "web" | "executor";

/**
 * The work a written reference points at a model (`config.ProviderJob`): a
 * seat's executor or one of its satellites, or the assistant's chat or deep
 * work. Held against `config.ProviderJobs` by
 * `internal/config.TestTheModelsScreenReadsAnEntryAsTheEngineDoes`.
 */
export type ModelJob =
  "executor" | "review" | "subagent" | "auxiliary" | "judge" | "assistant" | "deep_work";

/**
 * The seat field each seat job is written in, as the page writes it: the
 * executor in `llm`, each satellite flat beside it. Held by the same gate to
 * the fields `config.Company.ProviderReferences` reports for each job, so a
 * write from the page lands in the field the engine resolves.
 */
export const SEAT_JOB_FIELDS = {
  executor: "llm",
  review: "llm_review",
  subagent: "llm_subagent",
  auxiliary: "llm_auxiliary",
  judge: "llm_judge",
} as const;

/**
 * Where a model's test is (`queries.ModelCheckState`): running on the node that
 * took it, ended with what it found or why it found nothing, or lost with a
 * node that stopped. The page asks again while it is running and stops on the
 * others. Held against `queries.ModelCheckStates` by
 * `internal/api/queries.TestTheModelsScreenNamesEveryCheckState`.
 */
export type ModelCheckState = "running" | "ended" | "lost";

/**
 * Where a model's executors run (`config.Placement`). Held against
 * `config.Placements` by the entry gate.
 */
export type RunnerCellName = "direct" | "container" | "e2b";

/**
 * The longest the engine lets each model route work before it answers, in
 * seconds: a credential's test one isolated run, a retry one probe. The
 * dashboard waits a little past each rather than abandoning a request the
 * engine is still answering. A model's test is no such route: it runs on its
 * node's own time and is polled ({@link ModelCheckState}). Held against
 * `llmapi.CredentialBudget` and the engine's own bound on a retry's probe,
 * `engine.ModelRetryProbeBudget`, by
 * `internal/api/llmapi.TestTheModelsScreenWaitsAsLongAsTheEngineWorks`.
 */
export const MODEL_CREDENTIAL_BUDGET_SECONDS = 120;
export const MODEL_RETRY_BUDGET_SECONDS = 60;
