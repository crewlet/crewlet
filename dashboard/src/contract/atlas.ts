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
type AtlasKind =
  | "domain"
  | "hostname"
  | "endpoint"
  | "service"
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
type AtlasLane =
  "entry" | "services" | "runtime" | "data" | "platforms" | "code" | "observability" | "tools";

/** How one entry relies on another, pointing from what relies to what is
 *  relied on (`atlas.Relation`). */
type AtlasRelation =
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
  | "belongs_to"
  | "documented_in"
  | "owned_by";

/** Who provides an account, a model or a resource (`atlas.Vendor`). */
type AtlasVendor =
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
type AtlasFact =
  | "name"
  | "purpose"
  | "environment"
  | "importance"
  | "vendor"
  | "capability"
  | "account_ref"
  | "site"
  | "region"
  | "integration"
  | "inbound_url"
  | "url"
  | "address"
  | "port"
  | "containers"
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
  | "sign_in_expires";

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
  | "instant";

/** Where a claim came from (`atlas.SourceKind`). */
type AtlasSourceKind = "settings" | "records" | "integration" | "agent" | "person";

/** What backs an agent's claim, as the engine checked it (`atlas.Evidence`). */
type AtlasEvidence = "call" | "file" | "none";

/** How far a claim can be trusted, derived from its source (`atlas.Confidence`),
 *  the most trusted first. */
type AtlasConfidence = "declared" | "asserted" | "verified" | "observed" | "inferred" | "reported";

/** How a confidence reads on screen (`atlas.Word`). */
type AtlasWord = "confirmed" | "likely" | "unconfirmed";

/** Where an entry stands, read off its claims (`atlas.State`). */
type AtlasState = "confirmed" | "discovered" | "stale" | "retired";

/** Whether the sources agree on one fact or relation (`atlas.Standing`). */
type AtlasStanding = "settled" | "contested" | "disputed";

/** An identity outside the atlas a relation names (`atlas.External`). */
type AtlasExternal = "seat" | "unit" | "page";

/** Something about an entry that needs attention now (`atlas.SignalKind`). */
type AtlasSignalKind =
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
  | "standing_alarm";

/** How urgent a signal is (`atlas.Severity`). */
type AtlasSeverity = "danger" | "warning";

/** Who may read a signal (`atlas.Audience`); an answer carries only the
 *  signals its reader may read. */
type AtlasAudience = "holders" | "admins";

/** The one word an entry's open signals add up to (`atlas.Health`). */
type AtlasHealth = "alerting" | "needs_look" | "quiet";

/** One kind of use the engine records (`atlas.UseChannel`). */
type AtlasUseChannel = "command" | "tool_call" | "model_call" | "push";

/** Which way a trace walks (`atlas.Direction`): `up` is what depends on the
 *  entry, `down` what it depends on. */
type AtlasDirection = "up" | "down";

/** A place an atlas read reads (`gather.SourceName`). */
type AtlasCoverageSource =
  | "settings"
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
interface AtlasCoverage {
  source: AtlasCoverageSource;
  complete: boolean;
  reason?: string;
}

/** What every atlas answer carries: the settings revision it read ("" where
 *  the serving node cannot tell), each source's coverage, and how many values
 *  were left out for being shaped like a credential, counted and never shown. */
interface AtlasRead {
  revision: string;
  coverage: AtlasCoverage[];
  withheld: number;
}

/** One entry as a list row or a map card shows it (`atlas.Summary`). */
interface AtlasSummary {
  id: string;
  kind: AtlasKind;
  lane: AtlasLane;
  name: string;
  /** The entry's canonical key, the card's second line. */
  key: string;
  vendor?: AtlasVendor;
  state: AtlasState;
  word: AtlasWord;
  health: AtlasHealth;
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

/** One relation between two entries a map or a trace draws. */
interface AtlasMapEdge {
  from: string;
  to: string;
  relation: AtlasRelation;
}

/** `atlas_map`: the atlas drawn as its lanes. Only structural relations are
 *  edges; `collapsed` is the groups the engine closed past its bound. */
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
interface AtlasValue {
  type: AtlasValueType;
  text: string;
  private?: boolean;
}

/** Who or what made a claim. `name` is a record family, an integration's
 *  kind, an agent's handle or a person's handle; the settings are named by
 *  their `revision`. */
interface AtlasSource {
  kind: AtlasSourceKind;
  name?: string;
  revision?: string;
  turn?: string;
  evidence?: AtlasEvidence;
}

/** Another source's live claim beside the value the atlas holds. */
interface AtlasClaimed {
  value?: AtlasValue;
  denied?: boolean;
  source: AtlasSource;
  confidence: AtlasConfidence;
  observed_at: string;
}

/** One fact the atlas holds, and every other value a source holds for it. */
interface AtlasFactView {
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
}

/** One relation from an entry: to another entry by `to`, or to a seat, a
 *  unit or a page by `external` and `name`. */
interface AtlasEdge {
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
interface AtlasEntry {
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
