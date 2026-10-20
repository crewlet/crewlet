/**
 * `${NAME}` references in the company document, read as the engine reads
 * them (`envref`), and the secret store they resolve through. A node resolves
 * a reference from its environment or from the company's sealed credentials;
 * the demo has no environment and holds the sealed credentials' names only,
 * so a reference resolves exactly when the secret store holds its name. The
 * one value it keeps is what a pass recorded for display, an account's
 * address (`DemoCompany.recorded`), and never a credential.
 */

import type { SecretRow } from "~/protocol/types.ts";
import type { DemoCompany } from "./company.ts";
import type { DemoActor } from "./principal.ts";

/** The variable a whole `${NAME}` reference names, or null (`envref.Whole`). */
export function wholeRef(value: string): string | null {
  return /^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$/.exec(value.trim())?.[1] ?? null;
}

/** Whether a variable resolves: the secret store holds it. */
export function resolves(c: DemoCompany, name: string): boolean {
  return c.secrets.some((s) => s.name === name);
}

/**
 * Whether a stored value is there, and whether it resolves (`setup.Resolution`):
 * a literal resolves to itself, a whole reference through the secret store.
 */
export function resolution(
  c: DemoCompany,
  stored: string,
): { present: boolean; resolved: boolean | null } {
  const value = stored.trim();
  if (!value) return { present: false, resolved: null };
  const name = wholeRef(value);
  return { present: true, resolved: name ? resolves(c, name) : true };
}

/** Whether a document value is a credential a node can use: there, and resolving. */
export function usable(c: DemoCompany, value: unknown): boolean {
  return typeof value === "string" && resolution(c, value).resolved === true;
}

/**
 * A value as the engine reads it for display (`setup.Deref`): a literal as
 * written, a whole reference as the value recorded for it while the secret
 * store holds it, and nothing for a reference whose value the demo does not
 * hold.
 */
export function deref(c: DemoCompany, value: string): string {
  const name = wholeRef(value);
  if (!name) return value.trim();
  return resolves(c, name) ? (c.recorded[name] ?? "").trim() : "";
}

/** The part of a variable's name an agent's handle contributes: `sre-lead` is `SRE_LEAD`. */
export function varSuffix(handle: string): string {
  return handle.toUpperCase().replace(/[^A-Z0-9_]/g, "_");
}

/**
 * Seal `name` in the secret store, as written by `by` through `source`, and
 * answer its row: who stored it in the three halves a revision records too
 * (the author, its kind and the credential).
 */
export function seal(
  c: DemoCompany,
  name: string,
  at: string,
  by: DemoActor,
  source: string,
): SecretRow {
  const row: SecretRow = {
    name,
    key_id: "key-1",
    updated_at: at,
    updated_by: by.name,
    updated_by_kind: by.kind,
    operator_id: by.operator_id,
    source,
  };
  const existing = c.secrets.find((s) => s.name === name);
  if (existing) Object.assign(existing, row);
  else c.secrets.push(row);
  return existing ?? row;
}
