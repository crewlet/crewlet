/**
 * The marketing and growth integrations (Google Analytics, Search Console,
 * Google Ads, AdSense, AdMob, Google Play, App Store Connect, Apple Ads and
 * Meta Ads): the classes their tools are filed in, the vendor accounts and
 * connections a product reaches, the roles a vendor offers, a spending
 * product's caps, and the card a held spend, public or admin call carries
 * for the person who answers it (`internal/growth`, `internal/config`).
 *
 * MONEY IS INTEGER MICROS, as a decimal-integer string on the wire (one unit
 * of the account's currency is 1,000,000 micros; a string keeps an int64
 * exact where a JSON number would not), and is only ever drawn through the
 * kit's `Money`, never summed across currencies and never through a float.
 *
 * Every word a vendor or an agent wrote (a review, a reply, a campaign or
 * account name, the agent's reason, a policy finding) is drawn as plain text,
 * never as Markdown.
 */

/**
 * The class every growth tool is filed in (`growth.Class`): `read` and
 * `operate` run on the seat's level, `public`, `spend` and `admin` are held
 * for a person holding admin at a fresh step-up, and `never` is not offered.
 */
export type GrowthClass = "read" | "operate" | "public" | "spend" | "admin" | "never";

/**
 * What a held growth call asks a person to allow (`growth.Class.Act()`):
 * starting or raising spend, publishing to customers, or managing the vendor
 * account itself. None is ever answered by a delegate.
 */
export type GrowthAct = "ad_spend" | "public_post" | "account_admin";

/** A seat's level on a growth lane (`growth.Level`): absent reads `read`. */
export type GrowthLevel = "read" | "operate" | "administer";

/** Whether a vendor serves an account (`growth.AccountState`); spend tools refuse one that does not. */
export type GrowthAccountState =
  "serving" | "paused" | "suspended" | "closed" | "unapproved" | "unknown";

/** One vendor account a connection reaches (`growth.Account`). */
export interface GrowthAccount {
  /** The integration's kind: `google_ads`, `meta_ads`. */
  product: string;
  /** The block's connection that reaches it, by its id; absent for a product with one connection. */
  connection?: string;
  /** The vendor's own account ID, never cut on a screen. */
  account: string;
  /** The account's name at the vendor: somebody's words. */
  account_name: string;
  /** The account's ISO 4217 currency, where it has one. */
  currency?: string;
  /** The account's own IANA zone, in which its "today" is cut. */
  time_zone?: string;
  state: GrowthAccountState;
  /** The vendor roles the connection holds on it. */
  roles?: string[];
}

/** One company connection of a product (`growth.Connection`): a team key set, an owner of ad accounts, an identity. */
export interface GrowthConnection {
  /** The connection's id in the document. */
  id: string;
  /** What the connection is, as the vendor names it: somebody's words. */
  name: string;
  /** Who Crewlet is at the vendor through it, public only: an email, a key's ID, a system user's name. */
  identity: string;
  /** Where the connection stands, in the engine's words. */
  state: string;
  /** The roles Crewlet holds through it, read from the vendor, by id. */
  roles_held: string[];
  accounts: GrowthAccount[];
}

/**
 * A vendor role or permission level (`growth.RoleOffer`), in the vendor's own
 * name, with what it allows there and what Crewlet offers a seat through it.
 */
export interface GrowthRoleOffer {
  /** The vendor's own identifier for the role. */
  id: string;
  /** The role as the vendor's console names it: "Marketer", "Standard". */
  name: string;
  /** What the role allows at the vendor, in a sentence. */
  note: string;
  /** The classes of tool the role lets Crewlet call. */
  classes: GrowthClass[];
  /** The seat levels the role serves: none for a role Crewlet does not use. */
  levels: GrowthLevel[];
}

/**
 * One field an act changes (`growth.CardChange`), as it stands and as the act
 * leaves it. A money value is integer micros of the account's currency, as a
 * decimal-integer string; any other value is plain text.
 */
export interface GrowthCardChange {
  /** The vendor's field name. */
  field: string;
  /** The field in product words: "Daily budget". */
  label: string;
  /** Its value now; empty where the act sets it for the first time. */
  before: string;
  /** Its value once the act is applied; empty where the act clears it. */
  after: string;
  /** Whether both values are money. */
  money?: boolean;
}

/** One act of a held call (`growth.CardAct`): a batch of public acts holds up to ten. */
export interface GrowthCardAct {
  /** What the act targets, as the vendor names it: a campaign, a review, a listing. Somebody's words. */
  target: string;
  /** What the act does, in the engine's words: "Raise the daily budget". */
  summary: string;
  /** The fields it changes, in the order the vendor lists them. */
  changes: GrowthCardChange[];
  /** Whether the act cannot be undone at the vendor (a deletion, a revoked key). */
  danger?: boolean;
  /** Text it publishes, whole: shown in full and never cut. */
  text?: string;
  /** The vendor's limit on that text, in characters. */
  text_limit?: number;
  /** An excerpt of what it answers (a review's words), somebody's words. */
  target_excerpt?: string;
  /** The stars of the review it answers, where it answers one. */
  target_rating?: number;
  /** Who wrote what it answers, as the vendor shows them: somebody's words. */
  target_author?: string;
}

/**
 * A spend act's effect on the account's daily capacity, against its caps
 * (`growth.CardMoney`): every figure integer micros of `currency`, as a
 * decimal-integer string.
 */
export interface GrowthCardMoney {
  currency: string;
  /** What the act adds to the daily capacity: 0 for an act within budgets, negative for one that lowers it. */
  delta_micros: string;
  /** The account's daily capacity now. */
  daily_micros: string;
  /** The daily capacity once the act is applied. */
  daily_after_micros: string;
  /** The most one change may add. */
  per_change_cap_micros: string;
  /** The ceiling on the daily capacity after any change. */
  daily_cap_micros: string;
  /** The vendor's own rule on spending past a daily budget, in one sentence. */
  overdelivery?: string;
}

/**
 * What a person reads before allowing or denying a held growth call
 * (`growth.Card`), read live for whoever may answer it.
 */
export interface GrowthCard {
  act: GrowthAct;
  /** The integration's kind: `google_ads`, `meta_ads`. */
  product: string;
  /** The tool the agent called. */
  tool: string;
  account: GrowthAccount;
  /** The acts, each shown whole: one for spend and admin, one to ten for public. */
  acts: GrowthCardAct[];
  /** The spend act's money against the caps; null for a public or admin act. */
  money: GrowthCardMoney | null;
  /** The vendor's own findings from validating the act (policy topics), most serious first. */
  findings: string[];
  /** Why the agent asks, in its own words. */
  reason: string;
  /** When the ask lapses unanswered, and nothing runs. */
  lapses_at: string;
  /** Whether what the act changes moved since it was asked: allowing it now runs nothing. */
  moved: boolean;
  /** How many other open asks change the same thing: allowing one voids the others. */
  other_open_asks: number;
}

/**
 * A spending product's caps in the company document
 * (`integrations.<kind>.spend`, `config.GrowthSpend`): per vendor account, in
 * the account's own currency, as decimal strings the engine parses once. An
 * account without both caps refuses every spend call.
 */
export interface GrowthSpend {
  accounts?: GrowthSpendAccount[];
}

/** One account's caps (`config.GrowthCap`): the most one change may add, and the ceiling on the daily budgets after any change. */
export interface GrowthSpendAccount {
  /** The vendor's account ID. */
  account: string;
  /** Must equal the account's own currency, read live; a mismatch refuses spending. */
  currency?: string;
  per_change?: string;
  daily?: string;
}

/** A step of a growth act in the access log (`growth.Step`). */
export type GrowthStep =
  "made" | "asked" | "allowed" | "denied" | "lapsed" | "applied" | "void" | "failed";

/** Why a growth act did not run (`growth.Failure`); empty for one that did. */
export type GrowthFailure =
  | ""
  | "vendor_refused"
  | "unauthorized"
  | "rate_limited"
  | "vendor_unavailable"
  | "timed_out"
  | "unknown_outcome"
  | "cap_exceeded"
  | "access_lowered"
  | "engine_error";

/**
 * One field a recorded act changed. A value is sent as written, a money
 * value as decimal-integer micros; published text is never sent, only its
 * digest and length.
 */
export interface GrowthChangeField {
  field: string;
  label: string;
  money: boolean;
  /** Whether the field is published text, sent as its digest and length alone. */
  text: boolean;
  before?: string;
  after?: string;
  before_sha256?: string;
  before_bytes?: number;
  after_sha256?: string;
  after_bytes?: number;
}

/** A recorded spend act's figures in micros of `currency`, as decimal-integer strings. */
interface GrowthChangeMoney {
  currency: string;
  delta_micros: string;
  daily_micros: string;
  daily_after_micros: string;
  per_change_cap_micros: string;
  daily_cap_micros: string;
}

/** One step of an agent's growth act, as the access log keeps it (`growth_changes`). */
export interface GrowthChangeRow {
  /** The seat that acted, by handle. */
  seat: string;
  turn: string;
  /** The held call or the call the step belongs to. */
  act: string;
  /** Whether the act was held for a person's decision. */
  held: boolean;
  product: string;
  connection: string;
  account: string;
  /** The vendor's ID of what the act targeted (a campaign, a review), up to 128 characters with no space. */
  target: string;
  tool: string;
  class: GrowthClass;
  step: GrowthStep;
  /** Who answered a held act: a person, or the engine's lapse. */
  answered_by: string;
  /** The human seat that allowed or denied a held act, by handle; empty for any other step. */
  answerer: string;
  operation: string;
  before: string;
  changes: GrowthChangeField[];
  money: GrowthChangeMoney | null;
  failure: GrowthFailure;
  at: string;
}

/** `growth_changes`: the agents' growth acts, newest first, an administrator's read, paged by `next`. */
export interface GrowthChangesAnswer {
  rows: GrowthChangeRow[];
  /** The cursor of the next older page; empty at the end of the list. */
  next: string;
  /** Whether this node could read every record the page covers: false where some may be missing, never the list's end. */
  complete: boolean;
}
