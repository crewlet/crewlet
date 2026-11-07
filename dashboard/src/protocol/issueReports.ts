/**
 * `GET /diagnostics/issue-reports`: the `issue_reports` question, Settings ›
 * Diagnostics. The engine's own shape is `queries.IssueReportsAnswer`, and its
 * lists are always lists.
 */

/** Why this build sends nothing whatever the setting says, or nothing for one that may. */
export type IssueReportInert = "development" | "modified" | "no_commit";

/** How far a report got. */
export type IssueReportStatus = "sending" | "filed" | "duplicate" | "failed";

/** One report the company sent. */
export interface IssueReportRow {
  class: string;
  title: string;
  fingerprint: string;
  first_sent: string;
  last_sent: string;
  status: IssueReportStatus;
  /** The issue the relay named, "" where it named none. */
  issue: string;
  /** The exact report, as it went out. */
  payload: unknown;
}

/** What a report of one class would send. */
export interface IssueReportPreview {
  class: string;
  title: string;
  summary: string;
  payload: unknown;
}

export interface IssueReportsAnswer {
  /** The setting, as the node that answered applied it. */
  enabled: boolean;
  inert: IssueReportInert | "";
  relay: string;
  reports: IssueReportRow[];
  previews: IssueReportPreview[];
}
