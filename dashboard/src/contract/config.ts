/**
 * The addressable collections of the active revision, as `configapi` names
 * them — Settings › Configuration's Entities lens.
 *
 * FOUR NAMES THE ENGINE OWNS, held against `configapi.EntityKinds()` by
 * `internal/api/configapi/entities_client_test.go` — a kind this list spells
 * differently asks for a collection that does not exist, and the answer is a
 * bad-params refusal rather than anything a reader could act on.
 */
export const ENTITY_KINDS = [
  { kind: "roles", label: "Seats" },
  { kind: "units", label: "Units" },
  { kind: "llm-providers", label: "LLM providers" },
  { kind: "mcp-servers", label: "MCP servers" },
] as const;

/**
 * The calendar windows a `token_budget:` mapping caps, in the engine's order
 * — the keys the org builder offers a ceiling for, and the words it uses.
 *
 * THREE NAMES THE ENGINE OWNS, held against `period.Periods` and
 * `config.TokenBudget`'s keys by `internal/config/budget_test.go`. A window
 * spelled differently here is a field whose every save is refused as an
 * unknown key; one the engine grows that this list lacks is a ceiling an
 * operator can only write by hand. `none` is the engine's own phrase for an
 * absent key ("remove `token_budget.day` for no daily ceiling"), so a field
 * left empty says what the refusal of a 0 says.
 */
export const BUDGET_WINDOWS = [
  { period: "day", label: "Daily token ceiling", none: "No daily ceiling" },
  { period: "week", label: "Weekly token ceiling", none: "No weekly ceiling" },
  { period: "month", label: "Monthly token ceiling", none: "No monthly ceiling" },
] as const;

/**
 * The seat fields a human seat may not carry, by authored name (a seat's own
 * integrations dotted: `integrations.slack`) and in the order the engine's
 * refusal lists them — what the org builder strips from a seat it turns
 * human.
 *
 * THE ENGINE'S SET, held against its refusals by
 * `internal/config/problems_test.go`'s `TestTheBuilderStripsWhatEachKindRefuses`.
 * A field missing here is kept by the change and refused by the very next
 * check, over a field the change itself made wrong; one spelled differently
 * is a path no seat has, so it strips nothing — a seat's tracker `project`
 * and knowledge `space` are written at the top of the seat, never under
 * `integrations:`.
 *
 * `integrations.github` comes last because a different rule refuses it: the
 * org model carries no code-host identity for a seat, so the config layer's
 * admission rule (`config.Company.validateHumanSeatApps`) is what refuses a
 * seat's own GitHub App on a human seat.
 */
export const HUMAN_FORBIDDEN = [
  "llm",
  "llm_review",
  "llm_subagent",
  "llm_auxiliary",
  "llm_judge",
  "llm_sandbox",
  "sandbox",
  "token_budget",
  "workers",
  "learning_enabled",
  "schedules",
  "placement",
  "integrations.slack",
  "integrations.mattermost",
  "project",
  "space",
  "mcp_env",
  "behavioral_guidelines",
  "integrations.github",
] as const;

/**
 * The seat fields an agent seat may not carry, in the order the engine's
 * refusal lists them — what the org builder strips from a seat it turns into
 * an agent. Held by the same gate as {@link HUMAN_FORBIDDEN}.
 */
export const AGENT_FORBIDDEN = ["contact", "availability"] as const;

/**
 * The seat fields whose value holds a credential — every one a config read
 * masks something inside — in the order `config.Role` declares them.
 *
 * A credential a kind change strips was masked in the document the builder
 * holds, so it cannot be re-entered there and is gone for good once the change
 * is saved; the builder calls each one out before it lets that happen. Held
 * against the engine's own redaction predicate by the same gate as
 * {@link HUMAN_FORBIDDEN}, so a seat field that starts carrying a credential
 * is never stripped without the warning.
 */
export const SEAT_CREDENTIALS = [
  "mcp_env",
  "sandbox",
  "integrations.github",
  "integrations.slack",
  "integrations.mattermost",
] as const;
