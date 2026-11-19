/**
 * The Advisor's re-check before a fix is sent (`POST
 * /advisor/findings/{id}/recheck`), as the Apply dialog reads it. Every other
 * shape is the engine's, in `protocol/advisorFindings.ts`.
 */

import type { AdvisorFix } from "~/protocol/advisor.ts";
import type { AdvisorKeptFinding, AdvisorMoved } from "~/protocol/advisorFindings.ts";

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

/** Whether the finding still holds, now. */
export interface AdvisorRecheck {
  outcome: "holds" | "changed" | "resolved" | "not_applicable";
  finding: AdvisorKeptFinding;
  moved: AdvisorMoved[];
  previous_fix: AdvisorFix | null;
  preview: { rows: AdvisorPreviewRow[]; notes: string[] } | null;
  send: AdvisorSend | null;
  digest: string;
  checked_at: string;
  expires_at: string;
  refusal: { reason: string; who_can: string } | null;
}
