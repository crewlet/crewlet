/**
 * The company's atlas (ADR-0126): the map and catalogue of everything the
 * company runs on, as the atlas questions answer it (`queries/atlas.go`).
 *
 * Every closed set here is the engine's own, held both ways by
 * `internal/api/queries.TestTheDashboardNamesEveryAtlasWord`, and every
 * answer and row shape by `TestTheAtlasAnswersAreTheirDeclaredShapes`. A
 * value is text the engine already canonicalised; a label is somebody's
 * words and is drawn as plain text, never as Markdown.
 */

/** What one entry is (`atlas.Kind`). */
export type AtlasKind =
  | "domain"
  | "hostname"
  | "endpoint"
  | "certificate"
  | "service"
  | "workload"
  | "host"
  | "datastore"
  | "queue"
  | "bucket"
  | "vendor_account"
  | "repository"
  | "pipeline"
  | "monitor"
  | "tool_server"
  | "model_provider";

/** The column of the map a kind is drawn in (`atlas.Lane`): the five a request
 *  travels, then the band beneath them. */
export type AtlasLane =
  "entry" | "services" | "runtime" | "data" | "platforms" | "code" | "observability" | "tools";

/** How one entry relies on another, pointing from what relies to what is
 *  relied on (`atlas.Relation`). */
export type AtlasRelation =
  | "runs_on"
  | "depends_on"
  | "exposes"
  | "resolves_to"
  | "in_zone"
  | "fronted_by"
  | "served_by"
  | "built_from"
  | "deployed_by"
  | "monitored_by"
  | "secured_by"
  | "belongs_to"
  | "documented_in"
  | "owned_by";

/** What a relation says of the two entries it joins, as the map draws it
 *  (`atlas.Family`): a request's path, what an entry stands on, and
 *  everything around it. */
export type AtlasFamily = "calls" | "runs_on" | "around";

/** Who provides an account, a model or a resource (`atlas.Vendor`). */
export type AtlasVendor =
  | "aws"
  | "anthropic"
  | "atlassian"
  | "cloudflare"
  | "cursor"
  | "datadog"
  | "digitalocean"
  | "e2b"
  | "expo"
  | "github"
  | "gitlab"
  | "gitlet"
  | "google"
  | "google_cloud"
  | "mattermost"
  | "openai"
  | "scaleway"
  | "slack"
  | "xai"
  | "other";

/** One thing the atlas says about an entry (`atlas.Fact`). */
export type AtlasFact =
  | "name"
  | "purpose"
  | "environment"
  | "importance"
  | "vendor"
  | "capability"
  | "account_ref"
  | "account_name"
  | "site"
  | "region"
  | "integration"
  | "inbound_url"
  | "url"
  | "address"
  | "port"
  | "containers"
  | "open_port"
  | "image"
  | "engine"
  | "engine_version"
  | "default_branch"
  | "visibility"
  | "status"
  | "pull_mirror_of"
  | "push_mirror_to"
  | "imported_from"
  | "last_result"
  | "runner"
  | "required"
  | "pipeline_state"
  | "harness"
  | "model"
  | "sign_in"
  | "chain_position"
  | "transport"
  | "command"
  | "shared"
  | "server_name"
  | "server_version"
  | "tool_count"
  | "oauth_issuer"
  | "sign_in_expires"
  | "covers"
  | "expires"
  | "issuer"
  | "validity"
  | "certificate_type"
  | "monitor_state"
  | "state_since";

/** The grammar a fact's value is written in (`atlas.ValueType`). */
type AtlasValueType =
  | "word"
  | "int"
  | "port"
  | "hostname"
  | "ip"
  | "address"
  | "url"
  | "version"
  | "region"
  | "account_ref"
  | "branch"
  | "label"
  | "instant"
  | "image";

/** Where a claim came from (`atlas.SourceKind`). */
export type AtlasSourceKind = "settings" | "records" | "integration" | "code" | "agent" | "person";

/** What backs an agent's claim, as the engine checked it (`atlas.Evidence`). */
export type AtlasEvidence = "call" | "file" | "none";

/** How far a claim can be trusted, derived from its source (`atlas.Confidence`),
 *  the most trusted first. */
type AtlasConfidence = "declared" | "asserted" | "verified" | "observed" | "inferred" | "reported";

/** How a confidence reads on screen (`atlas.Word`). */
export type AtlasWord = "confirmed" | "likely" | "unconfirmed";

/** Where an entry stands, read off its claims (`atlas.State`). */
export type AtlasState = "confirmed" | "discovered" | "stale" | "retired";

/** Whether the sources agree on one fact or relation (`atlas.Standing`). */
export type AtlasStanding = "settled" | "contested" | "disputed";

/** An identity outside the atlas a relation names (`atlas.External`). */
export type AtlasExternal = "seat" | "unit" | "page";

/** Something about an entry that needs attention now (`atlas.SignalKind`). */
export type AtlasSignalKind =
  | "integration_blocked"
  | "integration_failing"
  | "tool_server_down"
  | "tool_server_waiting"
  | "sign_in_expired"
  | "sign_in_expiring"
  | "sign_in_failed"
  | "model_sitting_out"
  | "model_failing"
  | "server_test_failed"
  | "standing_alarm"
  | "monitor_alerting"
  | "monitor_warning"
  | "monitor_no_data"
  | "pipeline_failing";

/** How urgent a signal is (`atlas.Severity`). */
type AtlasSeverity = "danger" | "warning";

/** Who may read a signal (`atlas.Audience`); an answer carries only the
 *  signals its reader may read. */
type AtlasAudience = "holders" | "admins";

/** The one word an entry's open signals add up to (`atlas.Health`). */
export type AtlasHealth = "alerting" | "needs_look" | "quiet";

/** One kind of use the engine records (`atlas.UseChannel`). */
export type AtlasUseChannel = "command" | "tool_call" | "model_call" | "push";

/** Which way a trace walks (`atlas.Direction`): `up` is what depends on the
 *  entry, `down` what it depends on. */
type AtlasDirection = "up" | "down";

/** What one line of the atlas's log did (`atlas.Change`). `withdrawn` is a
 *  claim its source stopped making: what an integration's read no longer
 *  holds, or a "the same" a person took back. */
type AtlasChange =
  | "recorded"
  | "retired"
  | "fact_set"
  | "fact_denied"
  | "related"
  | "unrelated"
  | "joined"
  | "kept_apart"
  | "withdrawn";

/** Why an entry waits for a person (`atlas.ReviewKind`): its sources disagree
 *  on a fact or a relation, or only an agent reported it, with nothing the
 *  engine could check (`unconfirmed`) or backed by a call or a file (`likely`).
 *  What only a repository's files declare is likely and waits for nobody. */
export type AtlasReviewKind = "disagreement" | "unconfirmed" | "likely";

/** A place an atlas read reads (`gather.SourceName`). */
export type AtlasCoverageSource =
  | "settings"
  | "log"
  | "gitlet"
  | "integrations"
  | "seat_access"
  | "tool_servers"
  | "nodes"
  | "server_tests"
  | "access_log"
  | "usage"
  | "alarms";

/** One source's part in a read: whether it was read in full, and why not. */
export interface AtlasCoverage {
  source: AtlasCoverageSource;
  complete: boolean;
  reason?: string;
}

/** What every atlas answer carries: the settings revision it read ("" where
 *  the serving node cannot tell), each source's coverage, and how many values
 *  were left out for being shaped like a credential, counted and never shown. */
export interface AtlasRead {
  revision: string;
  coverage: AtlasCoverage[];
  withheld: number;
}

/** Who answers for an entry (`atlas.Owner`): a seat by its handle or a unit
 *  by its key, never a page and never free text. */
interface AtlasOwner {
  kind: AtlasExternal;
  name: string;
}

/** How many current entries an entry relies on and how many rely on it,
 *  through the relations the map draws at rest (`atlas.RelationCount`). */
interface AtlasRelationCount {
  depends_on: number;
  used_by: number;
}

/** One entry as a list row or a map card shows it (`atlas.Summary`). */
export interface AtlasSummary {
  id: string;
  kind: AtlasKind;
  lane: AtlasLane;
  name: string;
  /** The entry's canonical key, the card's second line. */
  key: string;
  vendor?: AtlasVendor;
  /** The owner a person or the engine's records name, never a suggestion. */
  owner?: AtlasOwner;
  /** The one line a person gave it: somebody's words, drawn as plain text. */
  purpose?: string;
  relations: AtlasRelationCount;
  state: AtlasState;
  word: AtlasWord;
  health: AtlasHealth;
  /** Why the entry is not quiet, in one sentence of the engine's own words;
   *  absent for a quiet one. */
  reason?: string;
  /** How many open signals the reader may read. */
  signals: number;
  review: boolean;
  not_used_recently: boolean;
  last_seen: string;
}

/** `atlas_entries`: one page of the atlas as a list. */
export interface AtlasEntriesAnswer extends AtlasRead {
  rows: AtlasSummary[];
  /** Absent on the last page. */
  next_cursor?: string;
  total: number;
}

/** One lane of the map, drawn whether or not it holds anything. */
interface AtlasMapLane {
  lane: AtlasLane;
  /** In the band beneath the request path. */
  band: boolean;
  count: number;
}

/** One vendor's entries in one lane. */
interface AtlasMapGroup {
  /** `<lane>/<vendor>`, or the lane alone for entries no vendor names. */
  key: string;
  lane: AtlasLane;
  vendor?: AtlasVendor;
  count: number;
  closed: boolean;
}

/** One relation between two entries a map or a trace draws, and the family
 *  it is drawn in. */
export interface AtlasMapEdge {
  from: string;
  to: string;
  relation: AtlasRelation;
  family: AtlasFamily;
}

/** `atlas_map`: the atlas drawn as its lanes. Its edges are the relations
 *  drawn at rest or, asked with `relations: "all"`, every relation between
 *  two drawn entries; `collapsed` is the groups the engine closed past its
 *  bound. */
export interface AtlasMapAnswer extends AtlasRead {
  lanes: AtlasMapLane[];
  groups: AtlasMapGroup[];
  nodes: AtlasSummary[];
  edges: AtlasMapEdge[];
  collapsed: string[];
}

/** An entry by its identity: kind and canonical key. */
interface AtlasRef {
  kind: AtlasKind;
  key: string;
}

/** One fact's value in its canonical text. `private` is an address no public
 *  network routes. */
export interface AtlasValue {
  type: AtlasValueType;
  text: string;
  private?: boolean;
}

/** Who or what made a claim. `name` is a record family, an integration's
 *  kind, an agent's handle or a person's handle; the settings are named by
 *  their `revision`. */
export interface AtlasSource {
  kind: AtlasSourceKind;
  name?: string;
  revision?: string;
  turn?: string;
  evidence?: AtlasEvidence;
  /** An agent's alone: the tool whose answer the claim cited, by name. */
  tool?: string;
}

/** One agent's word on an entry (`atlas.Report`): who reported it, in which
 *  turn, citing which call, and when. */
export interface AtlasReport {
  source: AtlasSource;
  observed_at: string;
}

/** Another source's live claim beside the value the atlas holds. */
export interface AtlasClaimed {
  value?: AtlasValue;
  denied?: boolean;
  source: AtlasSource;
  confidence: AtlasConfidence;
  observed_at: string;
}

/** One fact the atlas holds, and every other value a source holds for it. */
export interface AtlasFactView {
  fact: AtlasFact;
  value: AtlasValue;
  source: AtlasSource;
  confidence: AtlasConfidence;
  word: AtlasWord;
  observed_at: string;
  standing: AtlasStanding;
  /** A fact of meaning only an integration or an agent has given. */
  suggested?: boolean;
  others?: AtlasClaimed[];
  /** The member a fact of each host is of: which host's visibility this is. */
  of?: AtlasRef;
}

/** One relation from an entry: to another entry by `to`, or to a seat, a
 *  unit or a page by `external` and `name`. */
export interface AtlasEdge {
  relation: AtlasRelation;
  to?: string;
  external?: AtlasExternal;
  name?: string;
  source: AtlasSource;
  confidence: AtlasConfidence;
  word: AtlasWord;
  observed_at: string;
  standing: AtlasStanding;
  suggested?: boolean;
  others?: AtlasClaimed[];
}

/** How much the company relies on an entry through one channel. A channel
 *  that keeps only the last use (pushes) is not `counted`. */
interface AtlasUse {
  channel: AtlasUseChannel;
  last?: string;
  counted: boolean;
  week: number;
  month: number;
  seats?: string[];
}

/** One agent's account at the vendor an entry is. */
interface AtlasHolder {
  seat: string;
  lane: string;
  level?: string;
  enabled: boolean;
  observed_at?: string;
}

/** One open condition on an entry. `alarm` names a standing alarm's kind. */
interface AtlasSignal {
  kind: AtlasSignalKind;
  severity: AtlasSeverity;
  audience: AtlasAudience;
  since?: string;
  alarm?: string;
}

/** One entry in full (`atlas.Entry`). `members` is every entry an exact key
 *  merged into it, the canonical one first. */
export interface AtlasEntry {
  id: string;
  ref: AtlasRef;
  lane: AtlasLane;
  name: string;
  members: AtlasRef[];
  state: AtlasState;
  confidence: AtlasConfidence;
  word: AtlasWord;
  facts: AtlasFactView[];
  relations: AtlasEdge[];
  sources: AtlasSource[];
  last_seen: string;
  review?: boolean;
  use?: AtlasUse[];
  holders?: AtlasHolder[];
  signals?: AtlasSignal[];
  not_used_recently?: boolean;
}

/** One relation into an entry, with the entry it comes from. */
interface AtlasLink {
  relation: AtlasRelation;
  entry: AtlasSummary;
}

/** `atlas_entry`: one entry in full, what its open signals add up to as the
 *  reader may read them, and what relies on it. */
export interface AtlasEntryAnswer extends AtlasRead {
  entry: AtlasEntry;
  health: AtlasHealth;
  vendor?: AtlasVendor;
  used_by: AtlasLink[];
}

/** The entries a trace reached at one distance. */
interface AtlasTraceHop {
  distance: number;
  entries: AtlasSummary[];
}

/** `atlas_trace`: what depends on an entry (`up`) or what it depends on
 *  (`down`), hop by hop; `truncated` where it reached its bound first. */
export interface AtlasTraceAnswer extends AtlasRead {
  from: AtlasSummary;
  direction: AtlasDirection;
  hops: AtlasTraceHop[];
  edges: AtlasMapEdge[];
  truncated: boolean;
}

/** `atlas_facets`: how many entries each filter value leaves, each counted
 *  over every other filter. */
export interface AtlasFacetsAnswer extends AtlasRead {
  kinds: Partial<Record<AtlasKind, number>>;
  lanes: Partial<Record<AtlasLane, number>>;
  vendors: Partial<Record<AtlasVendor, number>>;
  words: Partial<Record<AtlasWord, number>>;
  health: Partial<Record<AtlasHealth, number>>;
  total: number;
}

/** What a log line's relation, or its "the same", names (`atlas.LineTarget`):
 *  another entry by `id`, or a seat, a unit or a page by `external`. `name`
 *  is the entry's name or the identity's own. */
interface AtlasLineTarget {
  id?: string;
  external?: AtlasExternal;
  name: string;
}

/** One line of the atlas's log (`atlas.Line`): what changed on which entry,
 *  when, and in whose voice. `text` is one sentence of the engine's own
 *  words; `value`, `name` and `target` are somebody's, drawn as plain text. */
interface AtlasLine {
  id: string;
  at: string;
  /** The entry the line is about, with its name and kind. */
  entry: string;
  name: string;
  kind: AtlasKind;
  change: AtlasChange;
  text: string;
  fact?: AtlasFact;
  relation?: AtlasRelation;
  value?: AtlasValue;
  target?: AtlasLineTarget;
  source: AtlasSource;
}

/** `atlas_log`: one page of the atlas's log, newest first, the company's or
 *  one entry's. A page may hold fewer lines than its limit where lines are
 *  not the reader's to see; `next_cursor` is absent on the last page. */
export interface AtlasLogAnswer extends AtlasRead {
  lines: AtlasLine[];
  next_cursor?: string;
}

/** One entry's part of the log: its lines a day over the window, oldest
 *  first (absent where it took none), and its newest line however old. */
interface AtlasEntryActivity {
  id: string;
  days?: number[];
  last?: AtlasLine;
}

/** `atlas_activity`: the log counted over its last fourteen UTC days, oldest
 *  first and today last, from `from`: the company's lines a day, and each
 *  entry the log holds a line of. */
export interface AtlasActivityAnswer extends AtlasRead {
  from: string;
  days: number[];
  entries: AtlasEntryActivity[];
}

/** How a source's last read went (`collect.State`): never read, read whole,
 *  read with some part refused, or not finished. */
export type AtlasSourceState = "never" | "read" | "partial" | "failed";

/** One part of a vendor a source was refused (`collect.Finding`), and what
 *  to grant. `scope` names the server the part belongs to, where it has one. */
interface AtlasSourceFinding {
  part: string;
  scope?: string;
  detail?: string;
}

/** One part of a source with a standing of its own (`collect.Scope`): a
 *  server of the SSH source. `last_read_at` is its last whole read, absent
 *  where it has none, and `unread` how many of its parts the last read was
 *  refused. */
export interface AtlasSourceScope {
  id: string;
  last_read_at?: string;
  unread?: number;
}

/** One source as a reader is shown it (`collect.Standing`). The counts are
 *  the last complete read's; `withheld` counts the values left out for their
 *  shape, which are never shown. `last_error` is sent to an administrator
 *  alone. `next_read_at` is absent where the source is due now,
 *  `refresh_requested_at` where no ask waits on it, and `refreshable_at`
 *  where an ask is honoured at once. */
export interface AtlasSourceStanding {
  kind: string;
  state: AtlasSourceState;
  last_attempt_at?: string;
  last_complete_at?: string;
  entries: number;
  calls: number;
  withheld: number;
  recorded: number;
  retired: number;
  purged: number;
  missing: number;
  last_error?: string;
  unread: AtlasSourceFinding[];
  unread_overflow: number;
  scopes: AtlasSourceScope[];
  next_read_at?: string;
  refresh_requested_at?: string;
  refresh_requested_by?: string;
  refreshable_at?: string;
}

/** `atlas_sources`: each source the atlas reads for the company, in the order
 *  of their names, and the cadence they are read on. */
export interface AtlasSourcesAnswer {
  sources: AtlasSourceStanding[];
  refresh_interval_seconds: number;
  refresh_floor_seconds: number;
}

/** One thing that waits for a person (`atlas.ReviewItem`), computed on every
 *  read and stored nowhere. `fact` or `relation` is what the sources disagree
 *  on, with every other value beside the one shown; both are absent for an
 *  entry that waits as a whole. `target` names what a relation points at. */
export interface AtlasReviewItem {
  id: string;
  kind: AtlasReviewKind;
  entry: AtlasSummary;
  fact?: AtlasFactView;
  relation?: AtlasEdge;
  target?: AtlasLineTarget;
  /** For an entry that waits as a whole: each agent's newest word on it,
   *  newest first, at most five. */
  reports?: AtlasReport[];
  /** The newest claim the item rests on. */
  since: string;
}

/** `atlas_review`: one page of what waits for a person, disagreements first.
 *  `total` counts every item of the asked kind and `counts` each kind's,
 *  whatever the page holds; `next_cursor` is absent on the last page. */
export interface AtlasReviewAnswer extends AtlasRead {
  items: AtlasReviewItem[];
  next_cursor?: string;
  total: number;
  counts: Partial<Record<AtlasReviewKind, number>>;
}

/** A count of tasks the engine may have stopped at its ceiling: `capped`
 *  says `total` is a floor. */
interface AtlasWorkTotal {
  total: number;
  capped?: boolean;
}

/** `atlas_work`: how many open tasks name each entry, by the entry's id.
 *  `unread` is the engine's sentence for why no work was read (the asker does
 *  not use Worklet, or the company's work is not kept there), and `open` is
 *  empty with it. `capped` says more entries have open work than one read
 *  counts, and which were left out is not known. */
export interface AtlasWorkAnswer {
  unread?: string;
  open: Record<string, number>;
  capped?: boolean;
}

/** `atlas_entry_work`: the tasks naming one entry, the open ones most
 *  recently changed first and the ones finished in the last 30 days, each
 *  list with its whole count. `Row` is the tracker's own list row, bound
 *  where the wire's questions are declared. */
export interface AtlasEntryWorkAnswer<Row> {
  unread?: string;
  open: Row[];
  recent: Row[];
  open_total: AtlasWorkTotal;
  recent_total: AtlasWorkTotal;
}

/** One asked id and the entry it names: `id` exactly as asked, and the
 *  entry it merged into, so two ids of one merged entry carry one `entry.id`. */
export interface AtlasNamed {
  id: string;
  entry: AtlasSummary;
}

/** `atlas_named`: the entries a list of ids names, as the asker reads them:
 *  one item per distinct asked id that names an entry, in the order asked,
 *  and the asked ids that name no entry the asker's atlas holds. */
export interface AtlasNamedAnswer {
  named: AtlasNamed[];
  missing: string[];
}

/** What each fact admits (`atlas.Fact.Spec`): the grammar its value is written
 *  in, the closed words of a word fact, the kinds that carry it (none listed:
 *  every kind), and its traits. `many` is a fact an entry holds several values
 *  of, `meaning` one only a person decides, `per_host` one each host of a
 *  merged entry keeps its own value of. Held to the engine's, fact by fact, by
 *  `TestTheDashboardKnowsWhatEachFactAndRelationAdmits`. */
export const ATLAS_FACT_SPECS = {
  name: { type: "label", traits: [], words: [], kinds: [] },
  purpose: { type: "label", traits: ["meaning"], words: [], kinds: [] },
  environment: {
    type: "word",
    traits: ["meaning"],
    words: ["production", "staging", "development", "test"],
    kinds: [],
  },
  importance: {
    type: "word",
    traits: ["meaning"],
    words: ["critical", "standard", "experimental"],
    kinds: [],
  },
  vendor: {
    type: "word",
    traits: [],
    words: [
      "aws",
      "anthropic",
      "atlassian",
      "cloudflare",
      "cursor",
      "datadog",
      "digitalocean",
      "e2b",
      "expo",
      "github",
      "gitlab",
      "gitlet",
      "google",
      "google_cloud",
      "mattermost",
      "openai",
      "scaleway",
      "slack",
      "xai",
      "other",
    ],
    kinds: ["vendor_account", "model_provider", "tool_server"],
  },
  capability: {
    type: "word",
    traits: ["many"],
    words: [
      "chat",
      "tracker",
      "knowledge",
      "code_hosting",
      "ci",
      "monitoring",
      "alerting",
      "dns",
      "cdn",
      "compute",
      "object_storage",
      "database",
      "email",
      "model_api",
      "app_delivery",
      "identity",
    ],
    kinds: ["vendor_account"],
  },
  account_ref: { type: "account_ref", traits: [], words: [], kinds: ["vendor_account"] },
  account_name: { type: "label", traits: [], words: [], kinds: ["vendor_account"] },
  site: { type: "url", traits: [], words: [], kinds: ["vendor_account"] },
  region: {
    type: "region",
    traits: ["many"],
    words: [],
    kinds: ["vendor_account", "host", "datastore", "queue", "bucket"],
  },
  integration: { type: "word", traits: [], words: ["on", "off"], kinds: ["vendor_account"] },
  inbound_url: { type: "url", traits: ["many"], words: [], kinds: ["vendor_account"] },
  url: {
    type: "url",
    traits: ["many"],
    words: [],
    kinds: ["hostname", "model_provider", "tool_server"],
  },
  address: { type: "address", traits: ["many"], words: [], kinds: ["host"] },
  port: { type: "port", traits: [], words: [], kinds: ["host", "datastore", "queue"] },
  containers: { type: "word", traits: [], words: ["yes", "no"], kinds: ["host"] },
  open_port: { type: "port", traits: ["many"], words: [], kinds: ["host"] },
  image: { type: "image", traits: [], words: [], kinds: ["workload"] },
  engine: {
    type: "word",
    traits: [],
    words: [
      "postgres",
      "mysql",
      "mariadb",
      "sqlite",
      "redis",
      "valkey",
      "mongodb",
      "elasticsearch",
      "opensearch",
      "clickhouse",
      "nats",
      "kafka",
      "rabbitmq",
      "other",
    ],
    kinds: ["datastore", "queue"],
  },
  engine_version: { type: "version", traits: [], words: [], kinds: ["datastore", "queue"] },
  default_branch: { type: "branch", traits: [], words: [], kinds: ["repository"] },
  visibility: {
    type: "word",
    traits: ["per_host"],
    words: ["company", "unit", "public", "private", "internal"],
    kinds: ["repository"],
  },
  status: {
    type: "word",
    traits: [],
    words: ["importing", "active", "archived"],
    kinds: ["repository"],
  },
  pull_mirror_of: { type: "url", traits: [], words: [], kinds: ["repository"] },
  push_mirror_to: { type: "url", traits: [], words: [], kinds: ["repository"] },
  imported_from: { type: "url", traits: [], words: [], kinds: ["repository"] },
  last_result: {
    type: "word",
    traits: [],
    words: ["queued", "running", "passed", "failed", "errored", "cancelled", "skipped"],
    kinds: ["pipeline"],
  },
  runner: { type: "word", traits: [], words: ["engine", "external"], kinds: ["pipeline"] },
  required: { type: "word", traits: [], words: ["yes", "no"], kinds: ["pipeline"] },
  pipeline_state: { type: "word", traits: [], words: ["active", "disabled"], kinds: ["pipeline"] },
  harness: { type: "label", traits: [], words: [], kinds: ["model_provider"] },
  model: { type: "label", traits: [], words: [], kinds: ["model_provider"] },
  sign_in: {
    type: "word",
    traits: [],
    words: ["subscription", "api_key", "inherit_env", "env"],
    kinds: ["model_provider"],
  },
  chain_position: { type: "int", traits: [], words: [], kinds: ["model_provider"] },
  transport: { type: "word", traits: [], words: ["stdio", "http"], kinds: ["tool_server"] },
  command: { type: "label", traits: [], words: [], kinds: ["tool_server"] },
  shared: { type: "word", traits: [], words: ["yes", "no"], kinds: ["tool_server"] },
  server_name: { type: "label", traits: [], words: [], kinds: ["tool_server"] },
  server_version: { type: "version", traits: [], words: [], kinds: ["tool_server"] },
  tool_count: { type: "int", traits: [], words: [], kinds: ["tool_server"] },
  oauth_issuer: { type: "url", traits: [], words: [], kinds: ["tool_server"] },
  sign_in_expires: { type: "instant", traits: [], words: [], kinds: ["tool_server"] },
  covers: { type: "hostname", traits: ["many"], words: [], kinds: ["certificate"] },
  expires: { type: "instant", traits: [], words: [], kinds: ["certificate"] },
  issuer: { type: "label", traits: [], words: [], kinds: ["certificate"] },
  validity: {
    type: "word",
    traits: [],
    words: ["active", "pending", "failed", "expired", "inactive"],
    kinds: ["certificate"],
  },
  certificate_type: {
    type: "word",
    traits: [],
    words: ["universal", "advanced", "custom"],
    kinds: ["certificate"],
  },
  monitor_state: {
    type: "word",
    traits: [],
    words: ["ok", "warn", "alert", "no_data", "unknown"],
    kinds: ["monitor"],
  },
  state_since: { type: "instant", traits: [], words: [], kinds: ["monitor"] },
} as const;

/** What each relation may join (`atlas.Relation.Joins` and `Names`): the kinds
 *  it runs from, the kinds it runs to, and the identities outside the atlas it
 *  names instead. Held to the engine's by the same gate. */
export const ATLAS_RELATION_PAIRS = {
  runs_on: {
    from: ["service", "workload", "datastore", "queue"],
    to: ["host", "vendor_account"],
    externals: [],
  },
  depends_on: {
    from: ["service"],
    to: [
      "service",
      "datastore",
      "queue",
      "bucket",
      "endpoint",
      "vendor_account",
      "model_provider",
      "tool_server",
    ],
    externals: [],
  },
  exposes: { from: ["service"], to: ["endpoint"], externals: [] },
  resolves_to: { from: ["hostname"], to: ["host", "endpoint", "hostname"], externals: [] },
  in_zone: { from: ["hostname", "certificate"], to: ["domain"], externals: [] },
  fronted_by: { from: ["hostname", "endpoint", "service"], to: ["vendor_account"], externals: [] },
  served_by: { from: ["domain"], to: ["vendor_account"], externals: [] },
  built_from: { from: ["service", "pipeline"], to: ["repository"], externals: [] },
  deployed_by: { from: ["service", "endpoint", "hostname"], to: ["pipeline"], externals: [] },
  monitored_by: {
    from: ["service", "endpoint", "hostname", "host", "datastore", "queue"],
    to: ["monitor", "vendor_account"],
    externals: [],
  },
  secured_by: { from: ["hostname"], to: ["certificate"], externals: [] },
  belongs_to: {
    from: [
      "domain",
      "hostname",
      "endpoint",
      "service",
      "host",
      "datastore",
      "queue",
      "bucket",
      "repository",
      "pipeline",
      "monitor",
      "model_provider",
      "tool_server",
      "certificate",
    ],
    to: ["vendor_account"],
    externals: [],
  },
  documented_in: {
    from: [
      "domain",
      "hostname",
      "endpoint",
      "certificate",
      "service",
      "workload",
      "host",
      "datastore",
      "queue",
      "bucket",
      "vendor_account",
      "repository",
      "pipeline",
      "monitor",
      "tool_server",
      "model_provider",
    ],
    to: [],
    externals: ["page"],
  },
  owned_by: {
    from: [
      "domain",
      "hostname",
      "endpoint",
      "certificate",
      "service",
      "workload",
      "host",
      "datastore",
      "queue",
      "bucket",
      "vendor_account",
      "repository",
      "pipeline",
      "monitor",
      "tool_server",
      "model_provider",
    ],
    to: [],
    externals: ["seat", "unit"],
  },
} as const;
