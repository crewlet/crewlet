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
  "sandbox.billing_refused",
  "sandbox.credential_refused",
  "sandbox.image_unstartable",
] as const;

/**
 * The dashboard pages an `open_page` fix is taken on, each one gesture there:
 * `backups` is Backups & retention's Take a backup. Held to the engine's own
 * `advisor.Pages`, in both directions: a page the engine names that this list
 * lacks is a finding whose row offers no way to take its step.
 */
export const ADVISOR_PAGES = ["backups"] as const;
