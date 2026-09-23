/**
 * The addressable collections of the active revision, as `configapi` names
 * them — the Configuration screen's Entities lens.
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
