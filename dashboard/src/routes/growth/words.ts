/**
 * The growth screens' shared words: a product's name, and what its vendor
 * does with a daily budget that the spending caps cannot promise.
 */

import { CATALOG } from "~/routes/settings/integrations/catalogue.tsx";

/** A growth product's name as its catalogue tile says it, or its kind as written for one the catalogue does not list. */
export function growthProductName(kind: string): string {
  return CATALOG.find((entry) => entry.key === kind)?.name ?? kind;
}

/**
 * What caps cannot promise about a vendor's spending, said wherever a cap is:
 * they bound the budgets agents may set, and a vendor may spend past a daily
 * budget on a single day.
 */
const OVERDELIVERY: Readonly<Record<string, string>> = {
  google_ads:
    "Google may spend up to twice a daily budget on a single day, and bills no more than 30.4 times it in a month.",
  meta_ads: "Meta may spend more than a daily budget on a given day.",
  apple_ads: "Apple bills by the UTC day.",
};

/** The caps' sentence for one product: what they limit, and what its vendor may still spend. */
export function spendCapsNote(kind: string): string {
  const vendor = OVERDELIVERY[kind];
  return vendor
    ? `Caps limit the budgets agents may set. ${vendor}`
    : "Caps limit the budgets agents may set.";
}

/** The products whose agents may ask to spend, each held to its own caps. */
export const SPEND_PRODUCTS: ReadonlySet<string> = new Set(["google_ads", "apple_ads", "meta_ads"]);

/** The marketing, growth and app store products, each drawn with its connections, roles and figures. */
export const GROWTH_PRODUCTS: ReadonlySet<string> = new Set([
  "google_analytics",
  "search_console",
  "google_ads",
  "apple_ads",
  "meta_ads",
  "adsense",
  "admob",
  "google_play",
  "app_store_connect",
]);
