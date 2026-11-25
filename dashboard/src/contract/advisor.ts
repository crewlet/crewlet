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

/**
 * Every class of finding the advisor raises for an agent the engine paused
 * after the same failure twice (ADR-0117), one per failure kind, each naming
 * the agents it holds and the fix. The Inbox says such a pause by that finding
 * and draws no seat row beside it. Held to the engine's own
 * `advisor.PausedClasses`.
 */
export const PAUSED_SEAT_CLASSES = [
  "models.credential_refused",
  "sandbox.image_unstartable",
] as const;

/** Pages an `open_page` fix opens on its `target`: `backups` (Take a backup), `item`, `seat`.
 *  Held both ways to the engine's `advisor.Pages`, so no finding's row lacks its step. */
export const ADVISOR_PAGES = ["backups", "item", "seat"] as const;

/**
 * The class of the notice naming the OAuth clients a person registered whose
 * last sign-in came back to an earlier engine address. Held to the engine's
 * `notice.ClassOAuthRedirectMoved`.
 */
export const OAUTH_REDIRECT_MOVED = "oauth_redirect_moved";

/** Where such a client is registered: `google` (Google Cloud) or `mcp` (an MCP
 *  server's vendor). Held both ways to the engine's `notice.Vendors`. */
export const OAUTH_CLIENT_VENDORS = ["google", "mcp"] as const;
