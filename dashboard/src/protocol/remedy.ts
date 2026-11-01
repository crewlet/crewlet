/**
 * A refusal's remedy, read off the wire: where a person changes the limit a
 * refusal was caused by (ADR-0060). The engine sends it beside a budget
 * refusal's words, on a question the assistant refused (`429
 * budget_exhausted`), a turn's error frame, and an act's refusal.
 *
 * READ, NEVER TRUSTED AS TYPED: a setting this build does not know, or a seat
 * remedy naming no seat, reads as no remedy at all, so a refusal from a newer
 * engine draws its words and nothing to open rather than a link to nowhere.
 */

import { LIMIT_SETTINGS, type Remedy } from "../contract/remedies.ts";

export type { Remedy };

/** The remedy `raw` carries, or null where it carries none this build reads. */
export function remedyOf(raw: unknown): Remedy | null {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) return null;
  const { setting, seat } = raw as Record<string, unknown>;
  const known = LIMIT_SETTINGS.find((s) => s === setting);
  if (known === undefined) return null;
  const handle = typeof seat === "string" ? seat.trim() : "";
  if ((known === "seat_token_budget") !== (handle !== "")) return null;
  return handle === "" ? { setting: known } : { setting: known, seat: handle };
}
