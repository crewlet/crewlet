/** What each built-in harness's profile says about adding a model on it: a module of its own,
 * so only Settings › Models & Fleets loads it. */

/** How a model on a harness can sign in: a key (or a provider's key in `env`), a host login, a token, a device code or a cloud. */
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

/** Every built-in harness bar `custom`, as its profile reads, so the wizard offers only what the
 * engine runs (held by `internal/config.TestTheModelsScreenKnowsEveryHarnessAsItsProfileReads`). */
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
    efforts: ["none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "persistent"],
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
    signIns: ["api-key", "host-login", "cloud"],
    keyEnv: "GEMINI_API_KEY",
    login: "adopt",
    efforts: [],
    effortNote:
      "Gemini CLI reads a thinking level only from its own settings file, per model family, which the engine writes for each call",
  },
  "qwen-code": {
    binary: "qwen",
    signIns: ["api-key", "host-login"],
    keyEnv: "OPENAI_API_KEY",
    keyShared: true,
    login: "adopt",
    efforts: [],
    effortNote:
      "Qwen Code reads its reasoning effort only from its own settings file, which the engine writes for each call",
  },
  "cursor-agent": {
    binary: "cursor-agent",
    signIns: ["api-key", "host-login"],
    keyEnv: "CURSOR_API_KEY",
    login: "command",
    efforts: [],
    effortNote:
      "Cursor Agent takes effort as part of the model id, such as gpt-5.5-high; cursor-agent --list-models lists them",
  },
  copilot: {
    binary: "copilot",
    webDeniable: true,
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
    webDeniable: true,
    signIns: ["api-key", "host-login"],
    keyEnv: "KIMI_MODEL_API_KEY",
    login: "command",
    efforts: ["low", "medium", "high", "xhigh", "max"],
  },
  hermes: {
    binary: "hermes",
    signIns: ["api-key", "host-login"],
    providerKeys: true,
    login: "command",
    efforts: ["none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"],
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
