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

/** The lengths a grant of entry is offered for: for one change, an hour up to its bound. */
export function grantHours(entry: AccessGrantEntry | undefined): number[] {
  const max = entry?.max_hours;
  if (!max) return [...GRANT_HOURS];
  const hours = new Set([1, ...GRANT_HOURS.filter((h) => h > 0 && h <= max), max]);
  return [...hours].sort((a, b) => a - b);
}

/** What a person reads before granting entry, beyond its name: what else it allows and its bound. */
export function grantCaution(entry: AccessGrantEntry | undefined): string {
  const parts: string[] = [];
  if (entry?.note) parts.push(entry.note);
  if (entry?.max_hours) {
    parts.push(
      `It is granted for one change, for at most ${hoursWords(entry.max_hours)}, never permanently.`,
    );
  }
  return parts.join(" ");
}

/** A lane's catalogue entry for a permission, or undefined where it lists none. */
export function grantEntry(lane: AccessOptions, permission: string): AccessGrantEntry | undefined {
  return lane.grants?.find((e) => e.id === permission);
}

/** A phrase as a label starts it: its first letter a capital. */
function capital(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/**
 * Where a grant naming no scope entry applies, in the lane's own words: the
 * whole connection for an account permission ("the account", "the
 * Organization"), else the lane's whole reach ("every zone the company's
 * agents reach", "the agent's own projects").
 */
export function grantWhole(lane: AccessOptions, entry: AccessGrantEntry | undefined): string {
  if (entry?.scope_kind === "account") return lane.grant_account ?? "the account";
  return lane.grant_whole ?? `every ${entry?.scope_kind ?? "entry"} the company's agents reach`;
}

/** What a lane's scope entries are, as a picker labels them: "Zones", "Projects". */
export function scopeKindLabel(kind: string): string {
  return `${capital(kind)}s`;
}

/** One scope entry as a person reads it: the vendor's name for it, else its identifier. */
export function grantScopeLabel(lane: AccessOptions, id: string): string {
  return lane.scope_names?.[id] ?? id;
}

/** Where a grant applies, in words: its entries by name, or its whole reach in the lane's words. */
export function grantReach(lane: AccessOptions, g: AccessGrant): string {
  if (g.scope && g.scope.length > 0) return andList(g.scope.map((id) => grantScopeLabel(lane, id)));
  return capital(grantWhole(lane, grantEntry(lane, g.permission)));
}
