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
 * n log n of them, where one built here orders a whole list.
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
 * A ZONE `Intl` DOES NOT KNOW throws, as `new Intl.DateTimeFormat` does, and
 * nothing is kept for it: a caller holding a zone it did not choose (the
 * company's, off the wire) catches and falls back as it always has.
 */

/** A date formatter's options, the zone apart: it is [dateTimeFormat]'s own argument. */
export type DateShape = Omit<Intl.DateTimeFormatOptions, "timeZone">;

const dates = new Map<string, Intl.DateTimeFormat>();
const numbers = new Map<string, Intl.NumberFormat>();
const collators = new Map<string, Intl.Collator>();

/** The formatter writing `shape` in `zone` for `locale` (the browser's language when undefined). */
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

/** Drop every kept formatter and collator, so the next ask builds it afresh. */
export function forgetFormatters(): void {
  dates.clear();
  numbers.clear();
  collators.clear();
}

/** How many formatters and collators are kept — for a suite holding the cache to its bound. */
export function keptFormattersForTest(): number {
  return dates.size + numbers.size + collators.size;
}

// A suite in the node environment has no window.
if (typeof window !== "undefined") window.addEventListener("languagechange", forgetFormatters);
