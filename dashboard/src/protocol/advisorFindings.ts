/**
 * The Advisor's findings as its duty keeps them (ADR-0164): `GET
 * /advisor/findings`, `GET /advisor/findings/{id}`, `GET /advisor/runs/latest`
 * and `GET /advisor/notices`, and the writes `POST /advisor/runs` and `POST
 * /advisor/findings/{id}/decision`. The engine's shapes are in
 * `internal/api/queries` (advisorlog.go, advisorrun.go). A nullable field is
 * always sent, as `null` where it has no value.
 */

import type {
  AdvisorActor,
  AdvisorAtlasEntry,
  AdvisorFix,
  AdvisorNotice,
  AdvisorSeverity,
} from "./advisor.ts";

/** Where a finding is: open and applied are live, the rest closed. */
export type AdvisorState = "open" | "applied" | "resolved" | "retired" | "expired" | "dismissed";

/** Why a finding closed. */
export type AdvisorCloseReason =
  | "condition_cleared"
  | "fixed_by_this"
  | "fixed_elsewhere"
  | "subject_gone"
  | "fix_unbuildable"
  | "not_confirmed"
  | "dismissed_wont_fix"
  | "dismissed_wrong"
  | "dismissed_known";

/** What a fact's number counts. */
export type AdvisorFactUnit =
  | "hours"
  | "items"
  | "turns"
  | "tokens"
  | "percent"
  | "count"
  | "minutes"
  | "bytes"
  | (string & {});

/** One number a finding rests on. */
export interface AdvisorFact {
  key: string;
  label: string;
  value: number;
  unit: AdvisorFactUnit;
}

/** One record behind a finding, named where the read could name it. */
export interface AdvisorRecordRef {
  kind: "item" | "turn" | "change" | "comment" | (string & {});
  id: string;
  name: string | null;
}

/** What a finding is about. */
export interface AdvisorFindingSubject {
  kind: "company" | "seat" | "schedule" | "integration" | "repository" | "atlas" | (string & {});
  /** Its id within its kind: a handle, `<seat or unit/id>/<schedule>`, a name. */
  id: string;
  name: string;
  seat: string;
  unit: string;
  schedule: string;
}

/** One finding as a person reads it. */
export interface AdvisorKeptFinding {
  /** The episode: one stretch of time the condition held. */
  id: string;
  /** The class and subject, stable across episodes. */
  key: string;
  /** An open set: a class this build does not know is drawn by its words. */
  class: string;
  source: "rule" | "model";
  actor: AdvisorActor;
  area: string;
  severity: AdvisorSeverity;
  subject: AdvisorFindingSubject;
  title: string;
  why: string;
  item: string;
  names: string[];
  path: string;
  /** What an atlas fix acts on, for a reader holding the atlas app; null for any other fix or reader. */
  entries: AdvisorAtlasEntry[] | null;
  /** The model's words for this revision, null until written. */
  explanation: string | null;
  state: AdvisorState;
  close_reason: AdvisorCloseReason | null;
  /** A fix that was applied and did not clear it, never offered again. */
  ineffective: { fix_digest: string; applied_at: string } | null;
  /** Set while a person's snooze holds it off the list. */
  snoozed_until: string | null;
  opened_at: string;
  /** When a run last confirmed it, or its close. */
  last_confirmed_at: string;
  closed_at: string | null;
  /** When an applied fix's effect is checked. */
  effect_due_at: string | null;
  /** `stale` once its class has not run for three of its intervals. */
  confirmed: "fresh" | "stale";
  revision: number;
  /** How many earlier episodes of the key closed. */
  seen_before: number;
  impact: { hours_per_week: number | null; tokens_per_week: number | null } | null;
  evidence: {
    window: { from: string; to: string };
    facts: AdvisorFact[];
    records: AdvisorRecordRef[];
  };
  fix: AdvisorFix;
}

/** What the latest run's model pass did. */
export interface AdvisorModelSummary {
  status: "off" | "done" | "declined" | "ceiling" | "failed" | (string & {});
  tokens: number;
  listed: number;
  dropped: Record<string, number>;
  reason: string;
}

/** One run of the Advisor; `running` is a Run now the duty has not taken yet. */
export interface AdvisorRunSummary {
  run_id: string;
  trigger: "schedule" | "event" | "person" | (string & {});
  by: string;
  started_at: string;
  ended_at: string | null;
  status: "running" | "done" | "failed" | "ceiling" | (string & {});
  classes_run: number;
  /** The sources it could not read, by their words. */
  unread: string[];
  opened: number;
  resolved: number;
  model: AdvisorModelSummary | null;
}

/** `GET /advisor/findings`: a page of findings, the live ones counted, and the latest run. */
export interface AdvisorFindingList {
  findings: AdvisorKeptFinding[];
  next_cursor: string | null;
  /** Open findings neither snoozed nor applied, by severity. */
  counts: Record<AdvisorSeverity, number>;
  last_run: AdvisorRunSummary | null;
}

/** One move a finding's history records. */
export interface AdvisorMoved {
  key: string;
  label: string;
  was: number | string | null;
  now: number | string | null;
}

/** One line of a finding's history. */
export interface AdvisorHistoryEntry {
  at: string;
  event:
    | "opened"
    | "revised"
    | "applied"
    | "ineffective"
    | "snoozed"
    | "unsnoozed"
    | "useful"
    | "closed"
    | "explained"
    | "released"
    | (string & {});
  by: { kind: "engine" | "person" | (string & {}); handle: string };
  moved: AdvisorMoved[];
}

/** `GET /advisor/findings/{id}`: one finding, its history and its key's earlier episodes. */
export interface AdvisorFindingDetail extends AdvisorKeptFinding {
  history: AdvisorHistoryEntry[];
  episodes: {
    id: string;
    opened_at: string;
    closed_at: string | null;
    close_reason: AdvisorCloseReason;
  }[];
}

/** A finding's notice names the finding whose fix ends it. */
export interface AdvisorNoticeFinding {
  id: string;
  title: string;
  fix: string;
}

/** `GET /advisor/notices`: the platform notices, and what the latest run could not read. */
export interface AdvisorNoticesAnswer {
  notices: (AdvisorNotice & { finding: AdvisorNoticeFinding | null })[];
  /** `findings` where this node could not read the Advisor's own list. */
  unread: string[];
}

/** `POST /advisor/findings/{id}/decision`'s body; a dismissal names its reason. */
export type AdvisorDecision =
  | { decision: "snooze" | "reopen" | "useful" }
  | { decision: "dismiss"; reason: "wont_fix" | "wrong" | "known" };
