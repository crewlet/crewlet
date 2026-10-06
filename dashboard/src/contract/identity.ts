/**
 * The identity directory's closed sets that People & access offers back to the
 * engine: the grants a person, an invitation or a token may carry.
 *
 * EXACTLY THE ENGINE'S — held by
 * `internal/api/iamapi.TestTheDashboardOffersExactlyTheEnginesGrants` in both
 * directions and in order. A grant the engine grew that this lacks is one no
 * administrator can confer from the dashboard; one this keeps that the engine
 * dropped is a checkbox every write refuses.
 */

/** Every grant, in the engine's own order (`iam.AllGrants`). */
export const GRANTS = [
  "state:read",
  "audit:read",
  "config:read",
  "secrets:read",
  "work:write",
  "knowledge:write",
  "config:write",
  "secrets:write",
  "fleet:operate",
  "people:manage",
  "sandbox:run",
] as const;

/**
 * The grants a machine token never carries, whatever its owner holds
 * (`iam.PersonPresentGrants`): revealing a credential's value, and deciding who
 * may do anything at all, need a person present. A token mint naming one is
 * refused, so the dialog says so before it is sent.
 */
export const TOKEN_WITHHELD_GRANTS = ["secrets:read", "people:manage"] as const;
