/**
 * The demo company: everything the demo engine answers from, as plain data.
 *
 * A fixture writes one of these (`fixtures/fairsplit.ts`), a scenario returns
 * a changed copy of one (`fixtures/scenarios.ts`), and the engine holds one in
 * memory and changes it as a person makes the writes it models. It is plain
 * JSON from top to bottom, so it can be copied, compared, kept in session
 * storage across a variant switch and handed to a scenario without anything
 * on it being shared.
 *
 * WIRE SHAPES WHEREVER THE ENGINE HAS ONE. A field here is the answer the
 * real engine sends for that question, typed by the dashboard's own protocol
 * module, so the compiler holds a fixture to what a screen will read. What the
 * real engine DERIVES (the org projection, the roster, the inbox's asks, a
 * project's counts) is derived here too, from the same inputs, by the engine
 * modules beside this file, so a write that changes an input changes every
 * answer that reads it.
 */

import type { AgentMemory, AgentEpisode, MemoryOverviewSeat } from "~/contract/memory.ts";
import type { BackupsAnswer } from "~/contract/backups.ts";
import type { CredentialPoolAnswer } from "~/contract/credentials.ts";
import type { EngineHealth } from "~/contract/health.ts";
import type { McpServersStatusAnswer } from "~/contract/mcp.ts";
import type { PageReadRow, SkillLoad } from "~/contract/pages.ts";
import type { DemoSurfaceState, PassAnswer } from "./loop.ts";
import type { DemoArtifacts } from "./artifacts.ts";
import type { DemoDirectory } from "./iam.ts";
import type { DemoPrincipal } from "./principal.ts";
import type { DemoSpendDay } from "./spend.ts";
import type { FleetBrokerAnswer } from "~/protocol/broker.ts";
import type {
  A2AChannel,
  CompanyDocument,
  ConversationEntry,
  ConversationRow,
  EventRecord,
  FleetAnswer,
  KnowledgeHit,
  Overlay,
  Page,
  PageChange,
  PageComment,
  PageContainer,
  PageRevision,
  RetentionReport,
  RevisionMeta,
  SandboxEntry,
  SandboxRun,
  ScheduleRunRow,
  SeatActivityRow,
  SecretRow,
  SetupListing,
  SetupRun,
  ToolRow,
  TurnRow,
  WorkChange,
  WorkComment,
  WorkFieldDef,
  WorkFile,
  WorkInboxNotice,
  WorkItem,
  WorkLink,
  WorkItemTurn,
  WorkProjectTag,
  WorkStatusDef,
  WorkTypeDef,
  WorkView,
} from "~/protocol/types.ts";

/** One revision of the company document, with the document it holds. */
export interface DemoRevision {
  meta: RevisionMeta;
  /** The document as the guarded surface serves it: credentials are `${VAR}` references. */
  payload: CompanyDocument;
}

/** The configuration surface: every revision and which epoch the fleet is on. */
export interface DemoConfig {
  /** Oldest first; the last one is ACTIVE, and its payload is the company. */
  revisions: DemoRevision[];
  /** The activation epoch, which a save advances by one. */
  epoch: number;
}

/** One agent seat's live state and record, keyed by handle. */
export interface DemoSeat {
  /**
   * The live half of the seat's `agents` row, exactly as the engine pushes
   * it: `activity`, `stopped_reason`, `paused`, `turn`, `last_turn` and the
   * rest. The static half (id, role, handle) is derived from the document.
   */
  overlay: Overlay;
  /** The seat's row in `seat_activity`, per company day. */
  activity: SeatActivityRow;
  /** What the seat remembers, as `agent_memory` answers it. */
  memory: AgentMemory;
  /** The seat's newest episode, as `agent_episode` answers it. */
  episode: AgentEpisode["episode"];
  /** The seat's phase records, newest first, as `agent` answers its history. */
  history: EventRecord[];
  /** The threads the seat's conversation ledger holds, and their entries. */
  conversations: { rows: ConversationRow[]; entries: ConversationEntry[] };
  /** The overview row Knowledge › Diaries draws for the seat. */
  overview: Omit<MemoryOverviewSeat, "handle">;
}

/** One task, its thread, its history and what it links to. */
export interface DemoItem {
  task: WorkItem;
  comments: WorkComment[];
  history: WorkChange[];
  links: WorkLink[];
  /** The turns charged to the task, newest first. */
  turns: WorkItemTurn[];
}

/** One project, as the project screen reads its settings. */
export interface DemoProject {
  key: string;
  name: string;
  purpose?: string;
  /** The unit that owns it, by its KEY. */
  unit: string;
  lead: string;
  default_assignee?: string;
  target_date?: string;
  statuses: WorkStatusDef[];
  types: WorkTypeDef[];
  fields: WorkFieldDef[];
  tags: WorkProjectTag[];
  archived?: boolean;
  version: number;
  created_at: string;
}

/** The native tracker. */
export interface DemoWork {
  /** The log position every tracker write advances; an answer's `log_seq`. */
  logSeq: number;
  projects: DemoProject[];
  items: DemoItem[];
  /** The workspace's own type and field declarations. */
  types: WorkTypeDef[];
  fields: WorkFieldDef[];
  /** Saved views, beside the builtin ones every container has. */
  views: WorkView[];
  /** Every person's inbox, newest first, keyed by handle. */
  inbox: Record<string, WorkInboxNotice[]>;
  /** Every person's own record: priorities, pins, snoozes. */
  people: Record<string, DemoPerson>;
  files: WorkFile[];
}

/** One human's own bookkeeping, as `work_person` answers it. */
export interface DemoPerson {
  /** The person's own inbox split; absent or empty means the shipped default. */
  primary_reasons?: string[];
  priorities: string[];
  pinned_views: string[];
  favorites: { kind: string; id: string }[];
  version: number;
}

/** One knowledge-base page, its thread and its revisions. */
export interface DemoPage {
  page: Page;
  comments: PageComment[];
  revisions: (PageRevision & { body: string; title: string })[];
  changes: PageChange[];
  /** Who read it over thirty company days. */
  reads: PageReadRow[];
  /** For a tool-skill page: who it reached as a skill. */
  skillLoads?: SkillLoad[];
}

/** The native knowledge base. */
export interface DemoPages {
  logSeq: number;
  containers: Omit<PageContainer, "pages">[];
  pages: DemoPage[];
  /** What `knowledge` answers for any search, best match first. */
  hits: KnowledgeHit[];
}

/**
 * The integrations room, as the engine's two answers about it stand. Both
 * are derived on every read: the setup surface's (`GET /setup/integrations`)
 * from `catalogue` and the document (`engine/setup.ts`), and the
 * `integrations` answer the room's cards read from the document, the event
 * log and the loop's records (`engine/integrations.ts`).
 */
export interface DemoIntegrations {
  /**
   * The engine's setup forms, captured for the demo company with nothing
   * connected (`fixtures/engine`): the DEFINITIONS every listing is derived
   * from. Never written: what a form holds is read from the document.
   */
  catalogue: SetupListing;
  /** The reconcile loop's record of each surface it has run on, by surface key (`loop.ts`). */
  states: Record<string, DemoSurfaceState>;
  /**
   * What a scenario scripted each surface's tool to answer every pass with
   * (`integrationStatus`), by surface key, until a person writes the
   * surface's inputs or disconnects it.
   */
  scripted: Record<string, PassAnswer>;
  /** The setup passes this node remembers, newest first, by kind. */
  runs: Record<string, SetupRun[]>;
  /**
   * How many deliveries each surface's notifications skipped and coalesced
   * over the traffic window, which the engine counts from outcome records the
   * demo does not keep (`integrationTraffic`).
   */
  outcomes: Record<string, { skipped: number; coalesced: number }>;
}

/** The engine's own surfaces: health, the fleet, retention and the rest of Settings. */
export interface DemoEngineState {
  health: EngineHealth;
  fleet: FleetAnswer;
  broker: FleetBrokerAnswer;
  retention: RetentionReport;
  backups: BackupsAnswer;
  mcp: McpServersStatusAnswer;
  credentials: CredentialPoolAnswer;
  /**
   * Where a third-party app reaches this deployment: the node's Tier A
   * `api.external_url` (`config.API.WebhookBase`), which a serving node is
   * never without. "" only models a node built with no address, which the
   * engine's own suites construct and no configuration a node boots reaches.
   */
  external_url: string;
  /**
   * The Tier A token ids that alone may change the company document
   * (`api.auth.company_writers`, ADR-0030); empty while it is unmanaged.
   */
  company_writers: string[];
  /** This node's Tier A token ids (`api.auth.tokens`), by label. */
  tokens: string[];
}

/**
 * Spend, as the engine keeps it: one row per seat, phase, model and company
 * day. The live window, every named window and series, and every budget
 * meter are derived from these (`engine/spend.ts`).
 */
export interface DemoSpend {
  daily: DemoSpendDay[];
}

/** Live coding runs, and the durable records behind them. */
export interface DemoRuns {
  live: SandboxEntry[];
  records: SandboxRun[];
}

/** Everything a demo company is. */
export interface DemoCompany {
  /**
   * The instant every relative time in the company was written against, as
   * RFC 3339. A fixture computes its times from it, so the demo always reads
   * as recent.
   */
  now: string;
  /**
   * Who the browser is signed in as, and through what credential: the one
   * value `GET /auth/session`, the `viewer` question and every write's
   * attribution are read from (`engine/principal.ts`).
   */
  principal: DemoPrincipal;
  /** The identity directory the principal is resolved through (`engine/iam.ts`). */
  directory: DemoDirectory;
  config: DemoConfig;
  /** Every agent seat's live state, by handle. */
  seats: Record<string, DemoSeat>;
  /** The company's events, newest first: the feed, the event log and every trace. */
  events: EventRecord[];
  /** The company's turns, newest first. */
  turns: TurnRow[];
  tools: ToolRow[];
  spend: DemoSpend;
  /** The schedules' dispatch ledger, newest first. */
  scheduleRuns: ScheduleRunRow[];
  runs: DemoRuns;
  a2a: A2AChannel[];
  work: DemoWork;
  pages: DemoPages;
  integrations: DemoIntegrations;
  secrets: SecretRow[];
  /**
   * What a provisioning pass recorded in the secret store that the engine
   * reads back to show, by variable: an Atlassian account's address. Apart
   * from `secrets`, which the secret store's listing serves as it stands,
   * and never a credential.
   */
  recorded: Record<string, string>;
  /** Every artifact, its versions, its store and its room (`engine/artifacts.ts`). */
  artifacts: DemoArtifacts;
  engine: DemoEngineState;
}

/** The active revision's document. */
export function documentOf(company: DemoCompany): CompanyDocument {
  const active = company.config.revisions[company.config.revisions.length - 1];
  if (!active) throw new Error("The demo company has no configuration revision.");
  return active.payload;
}

/** A deep copy, so a scenario or a write never shares state with what it was given. */
export function copyCompany(company: DemoCompany): DemoCompany {
  return structuredClone(company);
}
