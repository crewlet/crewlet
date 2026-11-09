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

/** One entity write: read the entity, set these top-level fields, write it back. */
export interface AdvisorChange {
  entity: "roles" | "units";
  id: string;
  set: Record<string, unknown>;
  summary: string;
}

/** One choice of a config change. */
export interface AdvisorOption {
  label: string;
  change: AdvisorChange;
}

/** What fixes a finding. */
export interface AdvisorFix {
  kind: "config_change" | "open_setting" | (string & {});
  sentence: string;
  unchanged?: string;
  options: AdvisorOption[];
  /** The document path an `open_setting` fix opens. */
  setting?: string;
}

/** The one thing a finding is about. */
export interface AdvisorSubject {
  kind: "company" | "seat" | "schedule" | (string & {});
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
  source: "static" | "evidence";
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
}
