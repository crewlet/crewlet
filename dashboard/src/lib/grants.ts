/**
 * Telling a grant option from any other (ADR-0118): the engine names every
 * grant an agent may ask for with the contract's prefix, and an agent's
 * request for one permission at an integration with a prefix of its own,
 * followed by the request's id and whether it grants for the request's hours
 * or for good.
 */

import { ACCESS_OPTION_PREFIX } from "~/contract/advisor.ts";
import {
  GRANT_REQUEST_OPTION_PREFIX,
  type AccessGrant,
  type AccessGrantEntry,
  type AccessOptions,
} from "~/contract/access.ts";
import { plural } from "./format.ts";
import { andList } from "./words.ts";

/** A request's grant option: the request's id, and whether it grants for good. */
export interface RequestOption {
  id: string;
  permanent: boolean;
}

const REQUEST_OPTION = new RegExp(`^${GRANT_REQUEST_OPTION_PREFIX}([a-z0-9]{10})-([tp])$`);

/** The request a decision's option grants, or null for any other option. */
export function requestOption(id: string): RequestOption | null {
  const m = REQUEST_OPTION.exec(id);
  return m ? { id: m[1]!, permanent: m[2] === "p" } : null;
}

/** Whether a decision's option id names a grant of the access catalogue. */
export function isAccessOption(id: string): boolean {
  return id.startsWith(ACCESS_OPTION_PREFIX) && requestOption(id) === null;
}

/** The lengths a grant is given for, in hours; 0 is permanent. */
export const GRANT_HOURS = [4, 24, 168, 0] as const;

/** A grant's length in words. */
export function hoursWords(hours: number): string {
  if (hours === 0) return "Permanent";
  if (hours % 24 === 0) return plural(hours / 24, "day");
  return plural(hours, "hour");
}

/**
 * A grant as a draft adds it: no id, author or time, which the engine writes
 * when it is saved, and an expiry `hours` from now unless it is permanent.
 */
export function newGrant(
  permission: string,
  scope: string[],
  hours: number,
  reason: string,
  now: number,
): AccessGrant {
  return {
    permission,
    ...(scope.length > 0 ? { scope: [...scope].sort() } : {}),
    ...(hours > 0 ? { expires_at: new Date(now + hours * 3_600_000).toISOString() } : {}),
    reason: reason.trim(),
  };
}

/** A lane's catalogue entry for a permission, or undefined where it lists none. */
export function grantEntry(lane: AccessOptions, permission: string): AccessGrantEntry | undefined {
  return lane.grants?.find((e) => e.id === permission);
}

/** Where a grant applies, in words: its zones, or its whole reach by its kind. */
export function grantReach(lane: AccessOptions, g: AccessGrant): string {
  if (g.scope && g.scope.length > 0) return andList(g.scope);
  return grantEntry(lane, g.permission)?.scope_kind === "account" ? "The account" : "Every zone";
}
