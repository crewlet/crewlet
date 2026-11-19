/**
 * The Advisor's routes as its page reads them: stored findings with their
 * evidence and lifecycle, the re-check that answers whether a finding still
 * holds before its fix is sent, reports and runs.
 */

import type { AdvisorFix } from "~/protocol/advisor.ts";

export type { AdvisorFix };

/** What ignoring a finding costs. */
export type AdvisorSeverity = "blocks" | "costs" | "note";

/** Where a finding stands: open, applied and waiting on its effect check, or closed. */
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

/** The unit a fact is measured in. */
export type AdvisorFactUnit =
  "hours" | "items" | "turns" | "tokens" | "percent" | "count" | "minutes";

/** One number a finding's verdict rests on. */
export interface AdvisorFact {
  key: string;
  label: string;
  value: number;
  unit: AdvisorFactUnit;
}

/** One record a finding cites. */
export interface AdvisorEvidenceRecord {
  kind: string;
  id: string;
  name: string | null;
}

/** One value that moved, as a re-check or a revision says it. */
export interface AdvisorMoved {
  key: string;
  label: string;
  was: number | string;
  now: number | string;
}

/** One stored finding: an episode of a condition about one subject. */
export interface AdvisorFinding {
  id: string;
  key: string;
  class: string;
  source: "rule" | "model";
  actor: "owner" | "engine";
  area: string;
  severity: AdvisorSeverity;
  subject: { kind: string; id: string; name: string };
  title: string;
  why: string;
  explanation: string | null;
  state: AdvisorState;
  close_reason: AdvisorCloseReason | null;
  ineffective: { fix_digest: string; applied_at: string } | null;
  snoozed_until: string | null;
  opened_at: string;
  last_confirmed_at: string;
  closed_at: string | null;
  effect_due_at: string | null;
  confirmed: "fresh" | "stale";
  revision: number;
  seen_before: number;
  impact: { hours_per_week: number | null; tokens_per_week: number | null } | null;
  evidence: {
    window: { from: string; to: string };
    facts: AdvisorFact[];
    records: AdvisorEvidenceRecord[];
  };
  fix: AdvisorFix;
}

/** One run of the Advisor's checks, and its model pass. */
export interface AdvisorRunSummary {
  run_id: string;
  started_at: string;
  ended_at: string | null;
  status: "running" | "done" | "failed" | "ceiling";
  classes_run: number;
  opened: number;
  resolved: number;
  model: {
    status: "off" | "done" | "declined" | "ceiling" | "failed";
    tokens: number;
    listed: number;
    dropped: Record<string, number>;
  } | null;
}

/** `GET /advisor/findings`: a page of findings in one state, and the open ones counted. */
export interface AdvisorFindingList {
  findings: AdvisorFinding[];
  next_cursor: string | null;
  counts: Record<AdvisorSeverity, number>;
  last_run: AdvisorRunSummary | null;
}

/** One thing that happened to a finding. */
export interface AdvisorHistoryEntry {
  at: string;
  event:
    | "opened"
    | "revised"
    | "confirmed"
    | "applied"
    | "ineffective"
    | "snoozed"
    | "dismissed"
    | "reopened"
    | "closed"
    | "explained";
  by: { kind: "engine" } | { kind: "person"; handle: string; name: string };
  moved: AdvisorMoved[];
}

/** `GET /advisor/findings/{id}`: a finding with its history and its earlier episodes. */
export interface AdvisorFindingDetail extends AdvisorFinding {
  history: AdvisorHistoryEntry[];
  episodes: {
    id: string;
    opened_at: string;
    closed_at: string;
    close_reason: AdvisorCloseReason;
  }[];
}

/** One field the change sets, as it is and as it would be. */
export interface AdvisorPreviewRow {
  field: string;
  label: string;
  before: string | null;
  after: string | null;
}

/** The request the person's session sends, under the revision the re-check read. */
export interface AdvisorSend {
  method: "PUT" | "PATCH" | "POST" | "DELETE";
  path: string;
  body: unknown;
  headers: { "If-Match"?: string; "If-None-Match"?: "*" };
  dry_run_path: string | null;
}

/** `POST /advisor/findings/{id}/recheck`: whether the finding still holds, now. */
export interface AdvisorRecheck {
  outcome: "holds" | "changed" | "resolved" | "not_applicable";
  finding: AdvisorFinding;
  moved: AdvisorMoved[];
  previous_fix: AdvisorFix | null;
  preview: { rows: AdvisorPreviewRow[]; notes: string[] } | null;
  send: AdvisorSend | null;
  digest: string;
  checked_at: string;
  expires_at: string;
  refusal: { reason: string; who_can: string } | null;
}

/** A person's answer to a finding. */
export type AdvisorDecision = "snooze" | "dismiss" | "reopen" | "useful";

/** Why a finding is dismissed. */
export type AdvisorDismissReason = "wont_fix" | "wrong" | "known";

/** One week's report, as the list names it. */
export interface AdvisorReportSummary {
  id: string;
  period: { from: string; to: string };
  written_at: string;
}

/** `GET /advisor/reports/{id}`: a week's numbers against the week before, and what it moved. */
export interface AdvisorReport extends AdvisorReportSummary {
  metrics: {
    key: string;
    label: string;
    value: number;
    unit: AdvisorFactUnit;
    was: number | null;
  }[];
  opened: number;
  resolved: number;
  applied: number;
  dismissed: number;
  top: AdvisorFinding[];
  summary: string | null;
}
