/**
 * `GET /advisor`: the `advisor` question, Settings › Advisor. The engine's own
 * shape is `advisor.Answer` (`internal/advisor`), and its lists are always
 * lists. A fix is never applied by the engine: a change is a write of one
 * entity this screen previews through the entity route's dry run and sends,
 * when the administrator presses Apply, under the entity's own tag.
 */

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

/** What fixes a finding. */
export interface AdvisorFix {
  kind: "config_change" | "open_setting" | "external_step" | "none" | (string & {});
  sentence: string;
  unchanged?: string;
  options: AdvisorOption[];
  /** The document path an `open_setting` fix opens, or an external step's integration. */
  setting?: string;
  /** Where an `external_step` is taken, where one was named. */
  url?: string;
}

/** The one thing a finding is about. */
export interface AdvisorSubject {
  kind: "company" | "seat" | "schedule" | "integration" | (string & {});
  seat?: string;
  unit?: string;
  schedule?: string;
  name?: string;
}

/** One thing about the company's setup worth acting on. */
export interface AdvisorFinding {
  class: string;
  severity: AdvisorSeverity;
  actor: AdvisorActor;
  source: "static" | "evidence" | "reconcile";
  area: string;
  subject: AdvisorSubject;
  title: string;
  why: string;
  path: string;
  evidence: { key: string; value: number }[];
  fix: AdvisorFix;
}

/** Every finding at one revision, the most costly first. */
export interface AdvisorAnswer {
  revision: string;
  findings: AdvisorFinding[];
  /** What the answer could not read (`integrations`), so a short list never reads as all clear. */
  unread: string[];
}

/** `GET /advisor/access`: the change that grants an agent one access it asked for. */
export interface AccessFixAnswer {
  seat: string;
  access: string;
  revision: string;
  fix: AdvisorFix;
}
