/**
 * The ONE place an `Intl` date or number formatter, or a collator, is built:
 * once per locale, zone and options, and kept until something its key cannot
 * see changes.
 *
 * # Why a cache, and why here
 *
 * BUILDING A FORMATTER IS THE EXPENSIVE HALF OF FORMATTING. A `Date`'s
 * `toLocaleString` with options, or a number's, builds one on every call: the
 * engine resolves the locale, the calendar and the zone's rules again for each
 * value. A list draws a time per row and often a tooltip beside it, so a
 * screen of rows built a formatter or two a row on every render, and reading
 * the zone they wrote in built one more each time. Kept here, a page of rows
 * builds each shape once whatever its length, and a re-render builds none.
 *
 * A COLLATOR IS THE SAME COST IN A SORT. `localeCompare` handed a locale or
 * options builds an `Intl.Collator` on every comparison, and a sort makes
 * n log n of them, where one built here orders a whole list. A sort takes it
 * once, before it starts: asked for inside the comparator, even a kept one
 * costs its key on every comparison.
 *
 * ONE CACHE, NOT ONE PER MODULE. `format.ts`'s wall clock and `range.ts`'s
 * clocks each kept a map of their own beside a dozen sites that kept nothing;
 * one map is one rule for what a key holds, and one place a change that
 * invalidates them all is handled.
 *
 * # What the key holds
 *
 * EVERYTHING THAT SHAPES THE OUTPUT: the locale, the zone and every option.
 * The map holds one entry per shape a call site asks for in each zone it asks
 * in — a few dozen, bounded by the shapes the source spells and the zones a
 * page meets (the reader's, the company's, UTC for a civil day), never by how
 * many values are formatted.
 *
 * THE ZONE IS REQUIRED, and is its own argument. A formatter built without
 * one takes the runtime's zone AT CONSTRUCTION and keeps it: kept for the
 * page's life, it would go on writing dates in the zone the page opened in
 * after the system's had moved, while its key said nothing about a zone at
 * all. With the zone named — the reader's (`prefs.ts`'s `zone`), the
 * browser's (`browserZone`), the company's, UTC for a civil day — every kept
 * formatter is exactly what its key says.
 *
 * THE ONE INPUT NO KEY CAN SEE is the browser's own language, which an
 * `undefined` locale resolves from when the formatter is built: a
 * `languagechange` drops every formatter, so the next one is built in the
 * language in force. `prefs.ts` drops them too when the zone moves or a suite
 * pins one, so formatters for a zone nobody asks for any more do not stay.
 *
 * # A zone the caller did not choose
 *
 * A ZONE OFF THE WIRE CAN BE ONE THIS RUNTIME DOES NOT KNOW. The engine
 * admits a company's or a schedule's zone by Go's time-zone database, and a
 * browser's own data can predate the name, where building a formatter in it
 * throws. [dateTimeFormat] throws then, as `new Intl.DateTimeFormat` does,
 * and keeps nothing. Such a zone reaches a formatter only through
 * [knownZone], which answers null for it, so its caller says what it is read
 * as instead, with no catch of its own to forget.
 *
 * ASKED ONCE A ZONE. Whether `Intl` reads a zone is a fact about this
 * runtime's time-zone data, not about a call, so the answer is kept beside
 * the formatters: a zone it refuses costs a lookup on every ask after the
 * first. Each caller that caught the throw instead built a formatter and
 * threw it away on every call, which a list paid once a row.
 */

/** A date formatter's options, the zone apart: it is [dateTimeFormat]'s own argument. */
export type DateShape = Omit<Intl.DateTimeFormatOptions, "timeZone">;

const dates = new Map<string, Intl.DateTimeFormat>();
const numbers = new Map<string, Intl.NumberFormat>();
const collators = new Map<string, Intl.Collator>();
/** Every zone [knownZone] was asked about, and whether this runtime reads it. */
const zones = new Map<string, boolean>();

/**
 * The formatter writing `shape` in `zone` for `locale` (the browser's
 * language when undefined). `zone` is one the caller chose — the reader's,
 * the browser's, UTC — or one [knownZone] answered: any other can throw.
 */
export function dateTimeFormat(
  locale: string | undefined,
  zone: string,
  shape: DateShape,
): Intl.DateTimeFormat {
  const key = `${locale ?? ""}\u0000${zone}\u0000${JSON.stringify(shape)}`;
  let built = dates.get(key);
  if (!built) {
    built = new Intl.DateTimeFormat(locale, { ...shape, timeZone: zone });
    dates.set(key, built);
  }
  return built;
}

/**
 * `zone` where this runtime reads it, and null where its `Intl` does not know
 * the name: the one way a zone the caller did not choose reaches a formatter.
 *
 * DECIDED BY BUILDING the zone's default formatter, kept like any other:
 * nothing else in `new Intl.DateTimeFormat(undefined, { timeZone })` can be
 * refused, so a throw there is the zone's. Decided once, for the reason the
 * module doc gives.
 */
export function knownZone(zone: string): string | null {
  let reads = zones.get(zone);
  if (reads === undefined) {
    try {
      dateTimeFormat(undefined, zone, ANY);
      reads = true;
    } catch {
      reads = false;
    }
    zones.set(zone, reads);
  }
  return reads ? zone : null;
}

/** The default shape, which every zone `Intl` knows can be written in. */
const ANY: DateShape = {};

/** The formatter writing a number in `options` for `locale` (the browser's language when undefined). */
export function numberFormat(
  locale: string | undefined,
  options: Intl.NumberFormatOptions,
): Intl.NumberFormat {
  const key = `${locale ?? ""}\u0000${JSON.stringify(options)}`;
  let built = numbers.get(key);
  if (!built) {
    built = new Intl.NumberFormat(locale, options);
    numbers.set(key, built);
  }
  return built;
}

/** The collator ordering strings by `options` for `locale` (the browser's language when undefined). */
export function collator(locale: string | undefined, options: Intl.CollatorOptions): Intl.Collator {
  const key = `${locale ?? ""}\u0000${JSON.stringify(options)}`;
  let built = collators.get(key);
  if (!built) {
    built = new Intl.Collator(locale, options);
    collators.set(key, built);
  }
  return built;
}

/**
 * Drop every kept formatter and collator, so the next ask builds it afresh,
 * and every answer [knownZone] kept: one call puts the module back as it
 * loaded, which a suite's `afterEach` relies on to count what a case builds.
 */
export function forgetFormatters(): void {
  dates.clear();
  numbers.clear();
  collators.clear();
  zones.clear();
}

/** How many formatters and collators are kept — for a suite holding the cache to its bound. */
export function keptFormattersForTest(): number {
  return dates.size + numbers.size + collators.size;
}

// A suite in the node environment has no window.
if (typeof window !== "undefined") window.addEventListener("languagechange", forgetFormatters);
