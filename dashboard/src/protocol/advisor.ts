/**
 * `GET /advisor`: the `advisor` question, Settings › Advisor. The engine's own
 * shape is `advisor.Answer` (`internal/advisor`), and its lists are always
 * lists. A fix is never applied by the engine: a change is a write of one
 * entity this screen previews through the entity route's dry run and sends,
 * when the administrator presses Apply, under the entity's own tag.
 */

import type { ADVISOR_PAGES, OAUTH_CLIENT_VENDORS } from "../contract/advisor.ts";
import type { AtlasKind } from "../contract/atlas.ts";

/** What ignoring a finding costs. */
export type AdvisorSeverity = "blocks" | "costs" | "note";

/** Who acts on a finding. An `engine` finding is never the owner's to-do. */
export type AdvisorActor = "owner" | "engine";

/**
 * One entity write: read the entity, merge `merge` into it key by key, set
 * these top-level fields, write it back.
 */
export interface AdvisorChange {
  entity: "roles" | "units";
  id: string;
  set: Record<string, unknown>;
  /** Fields merged into the entity as read, nested objects key by key. */
  merge?: Record<string, unknown>;
  summary: string;
}

/** One choice of a config change. */
export interface AdvisorOption {
  label: string;
  change: AdvisorChange;
}

/** One page an `open_page` fix is taken on. */
export type AdvisorPage = (typeof ADVISOR_PAGES)[number];

/** What fixes a finding. */
export interface AdvisorFix {
  kind:
    | "config_change"
    | "open_setting"
    | "open_page"
    | "external_step"
    | "ask_crewlet"
    | "atlas_act"
    | "none"
    | (string & {});
  sentence: string;
  unchanged?: string;
  options: AdvisorOption[];
  /** The document path an `open_setting` fix opens, or an external step's integration. */
  setting?: string;
  /** Where an `external_step` is taken, where one was named. */
  url?: string;
  /** The dashboard page an `open_page` fix is taken on (`ADVISOR_PAGES`). */
  page?: AdvisorPage | (string & {});
  /** What that page opens on: an item's key, a seat's handle. */
  target?: string;
  /** The question an `ask_crewlet` fix asks Ask Crewlet. */
  prompt?: string;
  /** The atlas tool call an `atlas_act` fix sends, once per entry the finding names. */
  act?: AdvisorAtlasAct;
}

/**
 * The atlas tool that fixes an atlas finding (`advisor.AtlasAct`): `args` is
 * what every call carries, and `asks` what the reader adds before sending it.
 */
export interface AdvisorAtlasAct {
  tool: "atlas_set_owner" | "atlas_relate" | "atlas_confirm" | "atlas_refresh" | (string & {});
  args?: Record<string, string>;
  asks?: "owner" | "monitor" | "value" | (string & {});
}

/** One atlas entry a finding names, sent only to a reader who holds the atlas app. */
export interface AdvisorAtlasEntry {
  id: string;
  kind: AtlasKind;
  key: string;
  name: string;
  /** Who the atlas names for it, where the finding is about that. */
  owner?: { kind: "seat" | "unit" | (string & {}); name: string };
}

/** The one thing a finding is about. */
export interface AdvisorSubject {
  kind:
    | "company"
    | "seat"
    | "schedule"
    | "integration"
    | "repository"
    | "alarm"
    | "atlas"
    | (string & {});
  seat?: string;
  unit?: string;
  schedule?: string;
  name?: string;
}

/** One record behind a finding: a work item by key, a turn by id, a change by its address or
 *  Gitlet reference (`web!42`), or a comment by id. */
export interface AdvisorRecord {
  kind: "item" | "turn" | "change" | "comment" | (string & {});
  id: string;
}

/** One thing about the company's setup worth acting on. */
export interface AdvisorFinding {
  class: string;
  severity: AdvisorSeverity;
  actor: AdvisorActor;
  source: "static" | "evidence" | "reconcile" | "atlas" | "analyst";
  area: string;
  subject: AdvisorSubject;
  title: string;
  why: string;
  path: string;
  evidence: { key: string; value: number }[];
  /** The one thing within the subject it is about, as a list reads it: `health-check (24 a day)`. */
  item?: string;
  /** The seats it is about, by name, where one finding speaks for several. */
  names?: string[];
  /** At most five records behind its evidence. */
  records?: AdvisorRecord[];
  /** At most five atlas entries the finding is about; absent for a reader without the atlas app. */
  entries?: AdvisorAtlasEntry[];
  fix: AdvisorFix;
}

/**
 * One condition that reaches the administrators' Inbox: a finding that blocks
 * the company and is the owner's to fix, or an alarm that has stood past its
 * threshold. Open exactly while its condition holds, with nothing to answer.
 */
export interface AdvisorNotice {
  /** The finding's class, or `alarm` for an alarm no finding carries. */
  class: string;
  /** The finding's subject, or `{ kind: "alarm", name }` naming the alarm's kind. */
  subject: AdvisorSubject;
  /** When the condition began, where the engine can say. */
  since?: string;
  /** The nodes an alarm stands on. */
  nodes?: string[];
  /** How a held conversation failed: `context_exhausted`, `out_of_memory` or `request_refused`. */
  cause?: string;
  /** The alarm kind that raised a finding's notice, for a reader without the finding. */
  alarm?: string;
  /** The raising alarm's words for a company owner; absent where no alarm raised it. */
  title?: string;
  person?: string;
  /** An `oauth_redirect_moved` notice's new URI and the clients still registered without it. */
  redirect?: AdvisorRedirectMoved;
}

/** The redirect URI to add, and every OAuth client a person registered that lacks it. */
export interface AdvisorRedirectMoved {
  uri: string;
  clients: AdvisorRegisteredClient[];
}

/** An OAuth client a person registered at its vendor, as its last grant left it. */
export interface AdvisorRegisteredClient {
  vendor: (typeof OAUTH_CLIENT_VENDORS)[number] | (string & {});
  /** Its client ID, and what a person finds it by: a Google client's ID shortened, an MCP server's name. */
  id: string;
  label: string;
  /** What signs in as it: each Google product (its integration kind) or the MCP server. */
  uses: { name: string; title: string }[];
  /** The redirect URI its last sign-in came back through, and when. */
  redirect: string;
  at: string;
}

/** Every finding at one revision, the most costly first. */
export interface AdvisorAnswer {
  revision: string;
  findings: AdvisorFinding[];
  /** What reaches the administrators' Inbox; absent from an engine older than the notices. */
  notices?: AdvisorNotice[];
  /**
   * What the answer could not read (`integrations`, `work`, `ledger`, `pauses`, `runs`, `alarms`,
   * `budget_stops`, `merges`, `atlas`, `atlas_sources`),
   * so a short list never reads as all clear.
   */
  unread: string[];
}

/** How an analyst run ended, or that it is running. */
export type AnalysisStatus = "running" | "done" | "ceiling" | "refused" | "failed" | (string & {});

/** How a person answered an analyst finding. */
export type AnalystVerdict = "useful" | "wrong" | "known";

/** One analyst run, as the fleet keeps the last one (ADR-0157). */
export interface AdvisorAnalysis {
  id: string;
  status: AnalysisStatus;
  started: string;
  ended?: string;
  /** `schedule` or `person`, and the person who pressed Run now. */
  trigger: string;
  by?: string;
  /** The providers.llm entry it ran on, and its token ceiling. */
  model: string;
  ceiling: number;
  input_tokens: number;
  output_tokens: number;
  /** How many submitted findings were not listed, by why. */
  dropped: Record<string, number>;
  unread: string[];
  reason?: string;
}

/** One listed analyst finding, with the fingerprint an answer names. */
export interface AnalystFinding extends AdvisorFinding {
  fingerprint: string;
  answer?: AnalystVerdict;
}

/** `GET /advisor/analysis`: whether the analyst is on, its last run and that run's findings. */
export interface AdvisorAnalysisAnswer {
  on: boolean;
  model?: string;
  cadence: "daily" | "weekly" | "off" | (string & {});
  hour: number;
  ceiling: number;
  /** The entry to turn it on with, where it is off. */
  suggest?: string;
  last?: AdvisorAnalysis;
  stale: boolean;
  findings: AnalystFinding[];
  /** The last run's findings people answered wrong or already known. */
  set_aside: number;
}

/** `GET /advisor/access`: the change that grants an agent one access it asked for. */
export interface AccessFixAnswer {
  seat: string;
  access: string;
  revision: string;
  fix: AdvisorFix;
}
