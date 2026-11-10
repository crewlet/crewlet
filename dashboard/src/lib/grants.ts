/**
 * Telling a grant option from any other (ADR-0118): the engine names every
 * grant an agent may ask for with the contract's prefix.
 */

import { ACCESS_OPTION_PREFIX } from "~/contract/advisor.ts";

/** Whether a decision's option id names a grant. */
export function isAccessOption(id: string): boolean {
  return id.startsWith(ACCESS_OPTION_PREFIX);
}
