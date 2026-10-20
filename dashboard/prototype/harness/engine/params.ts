/**
 * Reading a question's parameters the way the engine's `MapParams` reads them,
 * for the answers that port one of its grammars.
 *
 * A parameter arrives as whatever JSON the dashboard sent, and the engine
 * renders it to text before parsing it, so a boolean `true` and the string
 * `"true"` are the same value and a number is its decimal spelling. Reading
 * them any other way would accept, or refuse, something the engine does not.
 */

import { DemoRefusal } from "./refusal.ts";

export type Params = Record<string, unknown>;

/** One value as text, rendered as the transport renders it (`MapParams.String`). */
export function text(p: Params, key: string): string {
  const value = p[key];
  if (value === undefined || value === null) return "";
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  if (Array.isArray(value)) return `[${value.join(" ")}]`;
  return String(value);
}

/** Whether the parameter is PRESENT, which some keys are read by rather than by value. */
export function has(p: Params, key: string): boolean {
  return Object.hasOwn(p, key) && p[key] !== undefined;
}

/** A whole number, or `fallback` when the value is absent or is not one (`MapParams.Int`). */
export function int(p: Params, key: string, fallback: number): number {
  const raw = text(p, key);
  return /^[+-]?\d+$/.test(raw) ? Number(raw) : fallback;
}

/** A boolean as Go's `strconv.ParseBool` reads one, or `fallback` (`MapParams.Bool`). */
export function bool(p: Params, key: string, fallback: boolean): boolean {
  const raw = text(p, key);
  if (["1", "t", "T", "true", "TRUE", "True"].includes(raw)) return true;
  if (["0", "f", "F", "false", "FALSE", "False"].includes(raw)) return false;
  return fallback;
}

/** A comma-separated value, trimmed, empties dropped. */
export function csv(value: string): string[] {
  return value
    .split(",")
    .map((v) => v.trim())
    .filter(Boolean);
}

/** A request the engine refuses as malformed, with the sentence saying why. */
export function refuse(detail: string): never {
  throw new DemoRefusal("bad_params", detail);
}
