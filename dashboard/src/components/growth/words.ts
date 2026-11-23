/**
 * The growth products' shared words: a product's name, and what its vendor
 * does with a daily budget that the spending caps cannot promise. They live
 * outside `routes/` so the decision card and the caps reach them without a
 * screen's module, and the integrations catalogue names each product here.
 */

/** Each growth product's name, which its catalogue tile shows too. */
const NAMES: Readonly<Record<string, string>> = {
  app_store_connect: "App Store Connect",
};

/** A growth product's name, or its kind as written for one not named here. */
export function growthProductName(kind: string): string {
  return NAMES[kind] ?? kind;
}

/** What a connection reaches, in a product's own words: its accounts, or an app store's apps. */
export interface ReachWords {
  /** The list's noun: "Accounts". */
  noun: string;
  /** A connection that reaches none of them yet. */
  none: string;
  /** A connection the company narrows to none, whose list Crewlet has not read. */
  every: string;
  /** The settings above already name a lone connection (App Store Connect's issuer). */
  settingsSay?: boolean;
  /**
   * Every call is signed by the agent's own credential, so a connection holds
   * no role of its own and none is said for it.
   */
  agentsOwn?: boolean;
}

const REACH: Readonly<Record<string, ReachWords>> = {
  // A TEAM'S APPS ARE LISTED WITH AN AGENT'S OWN KEY: none shows until one is checked.
  app_store_connect: {
    noun: "Apps",
    none: "No app listed yet.",
    every: "No app listed yet.",
    settingsSay: true,
    agentsOwn: true,
  },
};

const ACCOUNTS: ReachWords = {
  noun: "Accounts",
  none: "Reaches no account yet.",
  every: "Reaches every account its sign-in reaches.",
};

/** A growth product's words for what its connections reach. */
export function reachWords(kind: string): ReachWords {
  return REACH[kind] ?? ACCOUNTS;
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
