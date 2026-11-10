/**
 * What the setup advisor and an access request share with the engine
 * (`internal/advisor`, ADR-0118).
 */

/**
 * Every option that grants an agent access begins with this, and the rest is a
 * key of the engine's closed catalogue: the engine builds the change from it
 * for the asking agent alone. Held to the engine's own `advisor.AccessPrefix`.
 */
export const ACCESS_OPTION_PREFIX = "grant-";
