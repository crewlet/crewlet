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
 * One claim on a human seat a `409 seat_held` names (`configapi.SeatHolder`):
 * the refusal's `held` maps each seat a write would take out of the company to
 * the ONE thing holding it — a person, a service account, or an open
 * invitation, never the invitation's address — and a person wins where both
 * claim one seat.
 *
 * HELD BY `internal/api/configapi.TestTheDashboardReadsWhatASeatHeldRefusalSends`.
 * The refusal was read as a LIST of holders per seat while the engine sent one
 * object, so every seat it named was "held by somebody" with no remedy at all.
 * Which remedy is right turns on the kind: a person is moved to another human
 * seat or removed, a service account unbound, an invitation cancelled.
 */
export interface SeatHeldHolder {
  /** The holder's id, when a person or a service account holds the seat. */
  person?: string;
  /** `person` or `machine` (`iam.Kind`). */
  kind?: string;
  login?: string;
  stage?: string;
  /** The open invitation's id, when one holds the seat and nobody else does. */
  invitation?: string;
  /** When that invitation lapses and frees the seat on its own. */
  expires_at?: string;
}

/**
 * The company's charter as `GET /config/charter` answers it and
 * `PUT /config/charter` takes it (`config.Charter`): what Company › Principles
 * shows and edits in place. Every field is always sent, `""` and `[]` when
 * nothing is written, and a write names all three.
 *
 * HELD BY `internal/api/configapi.TestTheDashboardReadsTheCharterTheEngineSends`,
 * both ways: a field the engine adds is one an editor would drop on its next
 * save, since the write replaces the charter whole.
 */
export interface Charter {
  mission: string;
  vision: string;
  /** In the order every seat reads them. */
  policies: string[];
}

/**
 * What a `412 version_moved` from `PUT /config/charter` carries beside the
 * engine's envelope: the charter as it stands now, and its tag. The editor
 * shows the person their draft beside `current`, and sends the draft again
 * under `etag` to keep it.
 *
 * HELD BY `internal/api/configapi.TestTheDashboardReadsWhatACharterConflictSends`.
 */
export interface CharterMoved {
  current: Charter;
  etag: string;
}

/*
 * THE CHARTER'S BOUNDS, in CHARACTERS (code points, as the kit's
 * `countCharacters` and the engine's `utf8.RuneCountInString` both count them):
 * the engine's `config.MaxMissionChars`, `MaxVisionChars`, `MaxPolicyChars`
 * and `MaxPolicies`. Every character of the charter is in every seat's prompt
 * on every round, so the engine refuses a charter past them rather than cut
 * it. The editor marks a draft past one before Save, which only works at the
 * engine's own figures: a copy above them sends drafts the engine refuses,
 * and one below refuses drafts it would take. The mission and each policy
 * are also ONE LINE (the vision may hold paragraphs), which the engine
 * refuses at `mission` or `policies[i]` with kind `shape`. Held by
 * `internal/api/configapi.TestTheDashboardBoundsTheCharterAtTheEnginesCaps`.
 */

/** The most characters the mission holds. */
export const MISSION_MAX_CHARS = 1024;
/** The most characters the vision holds. */
export const VISION_MAX_CHARS = 2048;
/** The most characters one policy holds. */
export const POLICY_MAX_CHARS = 1024;
/** The most policies a company keeps. */
export const POLICIES_MAX = 25;
