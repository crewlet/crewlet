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
 * HELD AGAINST `config.CLIAgentAuthModes`, `config.CLIAgentModes` and an unset
 * `config.LLMProvider`'s `AgentName`, `AgentMode`, `ResolvedAuthMode`,
 * `Timeout` and `Concurrency` by
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
 * How the engine uses the CLI. `text`: a model behind the engine's own tool
 * loop. `agent`: the CLI runs the seat's executor itself, its tools bridged in
 * over MCP, in the sandbox cell `run_in` names.
 */
export type CLIMode = "text" | "agent";

/** The CLI an entry that names no `agent` drives. */
export const DEFAULT_CLI_AGENT = "claude-code";

/** An entry's `mode` when it names none. */
export const DEFAULT_CLI_MODE: CLIMode = "text";

/** An entry's `auth.mode` when it names none. */
export const DEFAULT_CLI_AUTH_MODE: CLIAuthMode = "subscription";

/** The wall-clock cap on one CLI invocation when `timeout_seconds` is unset. */
export const DEFAULT_CLI_TIMEOUT_SECONDS = 600;

/** How many CLI processes an entry runs at once when `max_concurrent` is unset. */
export const DEFAULT_CLI_MAX_CONCURRENT = 4;
