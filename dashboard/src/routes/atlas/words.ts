/**
 * The atlas in the dashboard's words: what each engine name reads as on
 * screen. Every table is keyed by the contract's own closed set, so a value
 * the engine adds and the dashboard has not named fails the typecheck.
 */

import type { GlyphName } from "@crewlethq/icons/glyphs";
import type { Vendor as MarkVendor } from "@crewlethq/icons";
import type { Tone } from "@crewlethq/ui";
import type {
  AtlasCoverageSource,
  AtlasFact,
  AtlasHealth,
  AtlasKind,
  AtlasLane,
  AtlasRelation,
  AtlasSignalKind,
  AtlasSourceKind,
  AtlasState,
  AtlasUseChannel,
  AtlasVendor,
  AtlasWord,
} from "~/contract/atlas.ts";
import type { Grant } from "~/lib/access.ts";
import { plural } from "~/lib/format.ts";

/** One entry, many: the word on screen for one item of the atlas. */
export function entries(count: number): string {
  return plural(count, "entry", "entries");
}

/** What each kind of entry is called, and the glyph drawn on its tile. */
export const KINDS: Readonly<Record<AtlasKind, { word: string; glyph: GlyphName }>> = {
  domain: { word: "Domain", glyph: "globe" },
  hostname: { word: "Hostname", glyph: "globe" },
  endpoint: { word: "Endpoint", glyph: "waypoints" },
  service: { word: "Service", glyph: "layers" },
  host: { word: "Server", glyph: "server" },
  datastore: { word: "Database", glyph: "database" },
  queue: { word: "Queue", glyph: "list-ordered" },
  bucket: { word: "Bucket", glyph: "hard-drive" },
  vendor_account: { word: "Account", glyph: "cloud" },
  repository: { word: "Repository", glyph: "git-branch" },
  pipeline: { word: "Pipeline", glyph: "workflow" },
  monitor: { word: "Monitor", glyph: "activity" },
  tool_server: { word: "Tool server", glyph: "plug" },
  model_provider: { word: "Model", glyph: "brain" },
};

/** The kinds in the order a menu offers them: the map's, lane by lane. */
export const KIND_ORDER = Object.keys(KINDS) as AtlasKind[];

/**
 * Each lane's name, and what an empty one says would fill it in this build.
 * `link` is where the sentence points, offered only to a reader who holds
 * `grant` there.
 */
export const LANES: Readonly<
  Record<
    AtlasLane,
    { label: string; empty: string; link?: { label: string; path: string[]; grant: Grant } }
  >
> = {
  entry: {
    label: "Entry",
    empty:
      "Domains appear from the zones the Cloudflare integration declares, and host names from the addresses this engine answers at.",
    link: {
      label: "Open Cloudflare",
      path: ["company", "integrations", "cloudflare"],
      grant: "admin",
    },
  },
  services: {
    label: "Services",
    empty: "No source this version of Crewlet reads names a service yet.",
  },
  runtime: {
    label: "Runtime",
    empty: "Servers appear from the SSH servers the SSH integration declares.",
    link: { label: "Open SSH", path: ["company", "integrations", "ssh"], grant: "admin" },
  },
  data: {
    label: "Data",
    empty: "No source this version of Crewlet reads names a database, a queue or a bucket yet.",
  },
  platforms: {
    label: "Platforms",
    empty: "Each integration the company connects appears here as its account.",
    link: { label: "Open Integrations", path: ["company", "integrations"], grant: "admin" },
  },
  code: {
    label: "Code and delivery",
    empty:
      "Repositories appear from Gitlet and from the repositories the GitHub and GitLab integrations name, with the checks Gitlet runs.",
  },
  observability: {
    label: "Observability",
    empty: "No source this version of Crewlet reads names a monitor yet.",
  },
  tools: {
    label: "Tools and models",
    empty: "Tool servers and models appear from Settings › MCP & tools and Settings › Models.",
    link: { label: "Open Models", path: ["settings", "models"], grant: "admin" },
  },
};

/** The title of the band beneath the lanes a request travels. */
export const BAND_LABEL = "Around it";

/** Each vendor's name, and its mark where the kit draws one. */
export const VENDORS: Readonly<Record<AtlasVendor, { name: string; mark?: MarkVendor }>> = {
  aws: { name: "AWS", mark: "aws" },
  anthropic: { name: "Anthropic" },
  atlassian: { name: "Atlassian", mark: "atlassian" },
  cloudflare: { name: "Cloudflare", mark: "cloudflare" },
  cursor: { name: "Cursor" },
  datadog: { name: "Datadog", mark: "datadog" },
  digitalocean: { name: "DigitalOcean", mark: "digitalocean" },
  e2b: { name: "E2B" },
  expo: { name: "Expo", mark: "expo" },
  github: { name: "GitHub", mark: "github" },
  gitlab: { name: "GitLab", mark: "gitlab" },
  gitlet: { name: "Gitlet", mark: "crewlet" },
  google: { name: "Google" },
  google_cloud: { name: "Google Cloud", mark: "google-cloud" },
  mattermost: { name: "Mattermost", mark: "mattermost" },
  openai: { name: "OpenAI" },
  scaleway: { name: "Scaleway", mark: "scaleway" },
  slack: { name: "Slack", mark: "slack" },
  xai: { name: "xAI" },
  other: { name: "Other" },
};

/** The three words a confidence reads as. */
export const WORDS: Readonly<Record<AtlasWord, string>> = {
  confirmed: "Confirmed",
  likely: "Likely",
  unconfirmed: "Unconfirmed",
};

/** What an entry's health is called, and the tone its dot and tag take. */
export const HEALTH: Readonly<
  Record<AtlasHealth, { label: string; tone: Tone; dot?: "danger" | "warning" }>
> = {
  alerting: { label: "Alerting", tone: "danger", dot: "danger" },
  needs_look: { label: "Needs a look", tone: "warning", dot: "warning" },
  quiet: { label: "Quiet", tone: "neutral" },
};

/** Where an entry stands, where that is worth a word beside its name. */
export const STATES: Readonly<Record<AtlasState, string>> = {
  confirmed: "Confirmed",
  discovered: "Discovered",
  stale: "Not seen lately",
  retired: "Retired",
};

/** What each fact is called. */
export const FACTS: Readonly<Record<AtlasFact, string>> = {
  name: "Name",
  purpose: "Purpose",
  environment: "Environment",
  importance: "Importance",
  vendor: "Vendor",
  capability: "Capabilities",
  account_ref: "Account id",
  site: "Site",
  region: "Region",
  integration: "Integration",
  inbound_url: "Inbound address",
  url: "Address",
  address: "Address",
  port: "Port",
  containers: "Runs containers",
  engine: "Engine",
  engine_version: "Engine version",
  default_branch: "Default branch",
  visibility: "Visibility",
  status: "Status",
  pull_mirror_of: "Mirrors from",
  push_mirror_to: "Mirrors to",
  imported_from: "Imported from",
  last_result: "Last result",
  runner: "Runner",
  required: "Required",
  harness: "Harness",
  model: "Model",
  sign_in: "Sign-in",
  chain_position: "Place in the fallback chain",
  transport: "Transport",
  command: "Command",
  shared: "Shared",
  server_name: "Server name",
  server_version: "Server version",
  tool_count: "Tools",
  oauth_issuer: "Sign-in issuer",
  sign_in_expires: "Sign-in lapses",
};

/** The facts of meaning, which only people and the settings decide. */
export const MEANING: ReadonlySet<AtlasFact> = new Set(["purpose", "environment", "importance"]);

/** Each relation, read from the entry that relies on the other: "Runs on". */
export const RELATIONS: Readonly<Record<AtlasRelation, string>> = {
  runs_on: "Runs on",
  depends_on: "Depends on",
  exposes: "Exposes",
  resolves_to: "Resolves to",
  in_zone: "In zone",
  fronted_by: "Fronted by",
  served_by: "Served by",
  built_from: "Built from",
  deployed_by: "Deployed by",
  monitored_by: "Monitored by",
  belongs_to: "Belongs to",
  documented_in: "Documented in",
  owned_by: "Owned by",
};

/** What each open signal says. */
export const SIGNALS: Readonly<Record<AtlasSignalKind, string>> = {
  integration_blocked: "The integration is waiting on a person",
  integration_failing: "The integration's last pass failed",
  tool_server_down: "An instance of this tool server failed",
  tool_server_waiting: "An instance of this tool server is still waiting to start",
  sign_in_expired: "The tool server's sign-in has lapsed",
  sign_in_expiring: "The tool server's sign-in lapses within a week",
  sign_in_failed: "The tool server's sign-in failed to refresh",
  model_sitting_out: "The model's plan is spent, so it sits out until it resets",
  model_failing: "The model's last call failed, with no answer since",
  server_test_failed: "The server's last connection test failed",
  standing_alarm: "A standing alarm is raised",
};

/** What each kind of use the engine records is called. */
export const USES: Readonly<Record<AtlasUseChannel, { label: string; unit: [string, string] }>> = {
  command: { label: "Commands", unit: ["command", "commands"] },
  tool_call: { label: "Tool calls", unit: ["call", "calls"] },
  model_call: { label: "Model calls", unit: ["call", "calls"] },
  push: { label: "Pushes", unit: ["push", "pushes"] },
};

/** Where a claim came from, as a source's kind reads in a sentence. */
export const SOURCE_KINDS: Readonly<Record<AtlasSourceKind, string>> = {
  settings: "The company settings",
  records: "The engine's records",
  integration: "An integration",
  agent: "An agent",
  person: "A person",
};

/** Each record family the engine reads, named as a reader knows it. */
export const RECORDS: Readonly<Record<string, string>> = {
  gitlet: "Gitlet",
  integrations: "integration status",
  seat_access: "the agents' account ledger",
  tool_servers: "tool server listings",
  ssh_tests: "connection tests",
  access_log: "the access log",
  usage: "the usage history",
};

/** Each place an atlas read reads, named as a reader knows it. */
export const COVERAGE: Readonly<Record<AtlasCoverageSource, string>> = {
  settings: "The company settings",
  gitlet: "Gitlet",
  integrations: "Integration status",
  seat_access: "The agents' account ledger",
  tool_servers: "Tool server listings",
  nodes: "The nodes' health",
  server_tests: "Connection tests",
  access_log: "The access log",
  usage: "The usage history",
  alarms: "Standing alarms",
};

/** A word the engine sends as a value (`private`, `cdn`), as it reads in a sentence. */
export function valueWord(text: string): string {
  return text.replaceAll("_", " ");
}
