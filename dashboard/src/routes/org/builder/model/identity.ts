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
 * every key the draft or the saved company holds.
 */

/** The longest handle or key: a seat handle's width (`iam.MaxLogin`) and the key grammar's own bound. */
export const MAX_IDENTITY = 64;

/** A seat handle: one segment of lowercase letters, digits and hyphens (`org.ValidHandle`). */
export const HANDLE_PATTERN = /^[a-z0-9][a-z0-9-]*$/;

/** A unit key: a lowercase letter, then letters, digits, `-` and `_` (`config.entityID`). */
export const UNIT_KEY_PATTERN = /^[a-z][a-z0-9_-]{0,63}$/;

/** A name reduced to the slug a handle is minted from (`org.Slugify`). */
export function slugify(name: string): string {
  return name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

/** The identity `base` with the first suffix that is free: `base`, `base-2`, `base-3`. */
function free(base: string, taken: ReadonlySet<string>, fits: (value: string) => string): string {
  if (!taken.has(base)) return base;
  for (let n = 2; ; n++) {
    const suffix = `-${n}`;
    const candidate = fits(base.slice(0, MAX_IDENTITY - suffix.length)) + suffix;
    if (!taken.has(candidate)) return candidate;
  }
}

/** Trims what a cut leaves dangling at the end of a slug. */
const trimEnd = (value: string): string => value.replace(/[-_]+$/, "");

/** The handle a new seat called `name` is offered: its slug, made free of `taken`. */
export function mintHandle(name: string, taken: ReadonlySet<string>): string {
  const slug = trimEnd(slugify(name).slice(0, MAX_IDENTITY)) || "seat";
  return free(slug, taken, trimEnd);
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
  slug = trimEnd(slug.slice(0, MAX_IDENTITY));
  return free(slug, taken, trimEnd);
}

/**
 * Why `value` cannot be a new seat's handle, or `null` when it can. `taken`
 * is every identity the draft and the saved company hold.
 */
export function handleProblem(value: string, taken: ReadonlySet<string>): string | null {
  if (value === "") return "A seat needs a handle.";
  if (value.length > MAX_IDENTITY) return `A handle is at most ${MAX_IDENTITY} characters.`;
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
    return `A key is lowercase letters, digits, hyphens and underscores, starting with a letter, at most ${MAX_IDENTITY} characters.`;
  }
  if (taken.has(value)) return `${value} already names a seat or a unit.`;
  return null;
}
