/**
 * How the engine reads one `providers.llm` entry: the closed sets of its
 * `auth.mode` and `mode`, and what each field the entry leaves out comes to.
 *
 * Settings › Models & keys draws every entry off the company document, which
 * holds what an operator WROTE — and an entry that names no `agent`, no `mode`
 * or no `auth.mode` still runs as something. What it runs as is the engine's
 * default, so the screen says it from here rather than guessing, and the words
 * for each mode are keyed on these unions so a mode the engine adds is a
 * typecheck failure rather than a row drawn with nothing said.
 *
 * HELD AGAINST `config.CLIAgentAuthModes`, `config.CLIModes` and an unset
 * `config.LLMProvider`'s `AgentName`, `Box`, `ResolvedAuthMode`, `Timeout` and
 * `Concurrency` by
 * `internal/config.TestTheModelsScreenReadsAnEntryAsTheEngineDoes`: a default
 * that drifted would name a CLI, a sign-in or a cap the engine does not apply
 * to the entry on screen.
 */

/**
 * How an entry's CLI signs in. `subscription`: the operator's own CLI login —
 * a headless token, or the files `crewlet llm login` wrote. `api-key`: one key,
 * put in the CLI's own key variable, billed to the metered account.
 * `inherit-env`: whichever of the CLI's two variables the engine's own
 * environment holds.
 */
export type CLIAuthMode = "subscription" | "api-key" | "inherit-env";

/**
 * WHERE the engine runs an entry's CLI — in either mode the CLI drives its own
 * loop, calling the seat's tools over MCP. `host`: on this engine's host, turn
 * by turn, a subprocess of the turn. `box`: the seat's executor runs detached
 * in a box of `providers.sandbox` — the cell `run_in` names — with the seat's
 * tools bridged in.
 */
export type CLIMode = "host" | "box";

/**
 * What each built-in CLI is called where a person reads it: "Claude Code
 * subscription limit reached", never "claude-code". EXACTLY the engine's
 * built-in profiles (`cliprofile.BuiltinNames`) bar `custom`, held by
 * `internal/config.TestEveryBuiltInCLIHasAProductName`, so a profile the
 * engine ships is named the day it ships. An entry on `custom` describes a
 * CLI of its own and is called by its key.
 */
export const CLI_PRODUCTS: Readonly<Record<string, string>> = {
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

/** The CLI an entry that names no `agent` drives. */
export const DEFAULT_CLI_AGENT = "claude-code";

/** An entry's `mode` when it names none. */
export const DEFAULT_CLI_MODE: CLIMode = "host";

/** An entry's `auth.mode` when it names none. */
export const DEFAULT_CLI_AUTH_MODE: CLIAuthMode = "subscription";

/** The wall-clock cap on one stretch of a CLI's own work — no tool call in flight — when `timeout_seconds` is unset. */
export const DEFAULT_CLI_TIMEOUT_SECONDS = 600;

/** How many CLI processes an entry runs at once when `max_concurrent` is unset. */
export const DEFAULT_CLI_MAX_CONCURRENT = 4;
