/**
 * What the Integrations home says, built here and nowhere else so every case
 * it can say is a row of one table test.
 *
 * # A description, not a status
 *
 * The heading's sentence says what the page is for and never changes. It was
 * a count of the third-party apps, which read "No apps connected yet." over a
 * page whose cards showed Worklet and Doclet in use: a count of one kind of
 * thing, in the place a reader takes for the whole page.
 *
 * # One count, of everything in use
 *
 * The count stands beside the In use heading and counts what is under it:
 * every Crewlet service in use and every connected app, Jira and Confluence
 * each as its own product, since each is its own card. What is wrong with an
 * app is said under Needs a look, never folded into this number.
 */

/** The heading's sentence: what the page is for, the same for every reader. */
export const DESCRIPTION =
  "The tools your agents work with: Crewlet's own services and the apps your company connects for them.";

/** The hint an Admin reads under the cards while no app is connected. */
export const CONNECT_HINT = "Apps you connect from the list will show here too.";

/** One item in use, as the home counts it. */
export type InUse = "service" | "app" | "product";

export interface HomeFacts {
  /** How many items are in use: the count beside the heading, and the cards under it. */
  inUse: number;
  /** The line under the cards, or null where none is said. */
  hint: string | null;
  /** True when nothing is in use, and the home says so instead of drawing cards. */
  empty: boolean;
}

/**
 * The home's facts over what is in use. `admin` is whether the reader may
 * connect an app: a reader without Admin is told who does by their own note,
 * so they are offered no hint to do it themselves.
 */
export function homeFacts(items: readonly InUse[], admin: boolean): HomeFacts {
  const inUse = items.length;
  const anyApp = items.some((i) => i !== "service");
  return {
    inUse,
    hint: admin && inUse > 0 && !anyApp ? CONNECT_HINT : null,
    empty: inUse === 0,
  };
}
