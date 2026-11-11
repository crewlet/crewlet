/**
 * A seat's handle and a unit's key: the identities every reference in the
 * company document names, and the ones a new node is given here.
 *
 * AN IDENTITY IS NOT A NAME. A seat's `lead:`, every `manages:` entry and a
 * root seat's `unit:` name a seat by its HANDLE and a unit by its KEY (its
 * `id`). A name is prose two seats may share and anybody may change; a handle
 * derives the seat's agent id, its mailbox and its memory, and a unit's key is
 * what its schedules, its work and every reference to it are filed under, so
 * neither ever changes. The engine writes both into every revision it stores
 * (`config.MintIdentities`), and a document that changes one has removed a node
 * and created another.
 *
 * THE BUILDER MINTS AND SENDS. A node this draft adds gets its identity when
 * it is added — offered from its name, editable until the add — and the
 * document carries it from then on, so a lead or a `manages` entry can name a
 * node before any check has answered, and the engine never derives one from a
 * name the person may still change. The rule here is the engine's own
 * (`org.Slugify`, `config.MintUnitID`) only so that the offered identity reads
 * as the one the engine would have chosen: nothing depends on the two agreeing,
 * because the engine takes the handle it is sent and holds it to the grammar
 * below, which is what this module checks before an add is recorded.
 *
 * ONE NAMESPACE FOR BOTH. A `manages:` entry may name a seat or a unit, and
 * the engine reads it as the seat when both answer to it, so a unit keyed like
 * a seat is a unit no entry can name. A minted identity avoids every handle and
 * every key the draft or the saved company holds, and the running company's
 * from the org push (`nodeFacts.takenIdentities`), which is the rest of the
 * company a lead's draft of one unit does not hold.
 */

import { MAX_UNIT_ID, UNIT_ID_PATTERN } from "~/contract/config.ts";
import { MAX_LOGIN, NON_SLUG_PATTERN, SEAT_HANDLE } from "~/contract/identity.ts";

/**
 * A seat handle: one segment of lowercase letters, digits and hyphens, at most
 * [MAX_LOGIN] characters (`org.ValidHandle`, held as `SEAT_HANDLE`).
 */
export const HANDLE_PATTERN = new RegExp(SEAT_HANDLE);

/**
 * A unit key: a lowercase letter, then letters, digits, `-` and `_`, at most
 * [MAX_UNIT_ID] characters (`config.UnitIDPattern`, held as `UNIT_ID_PATTERN`).
 */
export const UNIT_KEY_PATTERN = new RegExp(UNIT_ID_PATTERN);

/** The runs of a name a slug replaces with one hyphen (`org.NonSlugPattern`, held as `NON_SLUG_PATTERN`). */
const NON_SLUG = new RegExp(NON_SLUG_PATTERN, "g");

/** A name reduced to the slug a handle is minted from (`org.Slugify`). */
export function slugify(name: string): string {
  return name
    .trim()
    .toLowerCase()
    .replace(NON_SLUG, "-")
    .replace(/^-+|-+$/g, "");
}

/**
 * The identity `base` with the first suffix that is free: `base`, `base-2`,
 * `base-3`, cut so the numbered one stays within `width`.
 */
function free(
  base: string,
  taken: ReadonlySet<string>,
  fits: (value: string) => string,
  width: number,
): string {
  if (!taken.has(base)) return base;
  for (let n = 2; ; n++) {
    const suffix = `-${n}`;
    const candidate = fits(base.slice(0, width - suffix.length)) + suffix;
    if (!taken.has(candidate)) return candidate;
  }
}

/** Trims what a cut leaves dangling at the end of a slug. */
const trimEnd = (value: string): string => value.replace(/[-_]+$/, "");

/** The handle a new seat called `name` is offered: its slug, made free of `taken`. */
export function mintHandle(name: string, taken: ReadonlySet<string>): string {
  const slug = trimEnd(slugify(name).slice(0, MAX_LOGIN)) || "seat";
  return free(slug, taken, trimEnd, MAX_LOGIN);
}

/**
 * The key a new unit called `name` is offered (`config.MintUnitID`): its slug,
 * `unit` for a name with nothing to slug, and a `unit-` prefix where the slug
 * does not start with a letter, made free of `taken`.
 */
export function mintUnitKey(name: string, taken: ReadonlySet<string>): string {
  let slug = slugify(name);
  if (slug === "") slug = "unit";
  else if (!/^[a-z]/.test(slug)) slug = `unit-${slug}`;
  slug = trimEnd(slug.slice(0, MAX_UNIT_ID));
  return free(slug, taken, trimEnd, MAX_UNIT_ID);
}

/**
 * Why `value` cannot be a new seat's handle, or `null` when it can. `taken`
 * is every identity the draft and the saved company hold.
 */
export function handleProblem(value: string, taken: ReadonlySet<string>): string | null {
  if (value === "") return "A seat needs a handle.";
  if (value.length > MAX_LOGIN) return `A handle is at most ${MAX_LOGIN} characters.`;
  if (!HANDLE_PATTERN.test(value)) {
    return "A handle is lowercase letters, digits and hyphens, starting with a letter or a digit.";
  }
  if (taken.has(value)) return `${value} already names a seat or a unit.`;
  return null;
}

/** Why `value` cannot be a new unit's key, or `null` when it can. */
export function unitKeyProblem(value: string, taken: ReadonlySet<string>): string | null {
  if (value === "") return "A unit needs a key.";
  if (!UNIT_KEY_PATTERN.test(value)) {
    return `A key is lowercase letters, digits, hyphens and underscores, starting with a letter, at most ${MAX_UNIT_ID} characters.`;
  }
  if (taken.has(value)) return `${value} already names a seat or a unit.`;
  return null;
}
