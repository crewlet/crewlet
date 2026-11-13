/**
 * The engine's own conditions — the attention queue — as the screens that
 * draw it read it.
 *
 * `lib/attention.ts` is the pure derivation; this is the one place its inputs
 * are gathered, because Home and the Inbox both draw the queue and two copies
 * of "which reads feed it" is how the landing screen and the place you act
 * come to disagree about what needs a decision.
 */

import { useMemo } from "react";
import type { McpAlarm } from "~/contract/mcp.ts";
import type { AdvisorFinding, AdvisorNotice } from "~/protocol/advisor.ts";
import { attentionQueue, type Attention, type Subject } from "./attention.ts";
import { useQuery } from "./useQuery.ts";
import { useNow } from "./clock.ts";
import { useAgents, useConnection, useEngineHealth, useOrg, useOrgBudget } from "./store-hooks.ts";
import { indexOrg, nameOfIn } from "./seats.ts";
import { RUNS_POLL_MS } from "./runs.ts";
import { MCP_STATUS_POLL_MS } from "./mcpStatusPoll.ts";
import { useViewer } from "./viewer.ts";
import { ATLAS, holds } from "./access.ts";
import type { AtlasReviewItem } from "~/contract/atlas.ts";

/** No MCP alarm: a reader who is not asked them, or an answer not in yet. */
const NO_MCP_ALARMS: readonly McpAlarm[] = [];

/** No notice or finding: a reader the advisor does not answer, or an answer not in yet. */
const NO_NOTICES: readonly AdvisorNotice[] = [];
const NO_FINDINGS: readonly AdvisorFinding[] = [];

/** Nothing unread: a reader the advisor does not answer, or an answer not in yet. */
const NO_SETUP_UNREAD: readonly string[] = [];

const NO_DRIFT: readonly AtlasReviewItem[] = [];

/** What contradicts the reader's own word in the atlas, and nothing else of its review. */
const MY_DRIFT = { mine: true, kind: "disagreement" } as const;

/** How often the reader's own drift is asked: the atlas moves on reads that are minutes apart. */
export const ATLAS_DRIFT_POLL_MS = 60_000;

/**
 * Whether this reader's word can be contradicted in the atlas: a holder of
 * Atlas who acts as a seat. A credential acting as no seat sets nothing in a
 * person's name, and the engine answers it none.
 */
function useReadsAtlasDrift(): boolean {
  const viewer = useViewer();
  return !viewer.loading && viewer.handle !== "" && holds(viewer.grants, ATLAS);
}

/** How often the queue asks the advisor again: a setup changes by revisions. */
export const ADVISOR_POLL_MS = 60_000;

/**
 * Whether this reader is asked the MCP alarms: an administrator, the reader
 * `mcp_servers_status` answers in the reading that carries them. The MCP
 * section opens to every member, whose reading carries no alarm, so asking a
 * member would be a poll whose answer is never read. False while the frame
 * has not said who the reader is.
 */
function useReadsMcpAlarms(): boolean {
  const viewer = useViewer();
  return !viewer.loading && viewer.admin;
}

/**
 * The subjects the attention queue does not watch for this reader, for the
 * quiet band that says what it watched (`watchedIn`): the MCP servers, for a
 * reader who is not asked their alarms.
 */
export function useUnwatched(): readonly Subject[] {
  const admin = useReadsMcpAlarms();
  const atlas = useReadsAtlasDrift();
  return [
    ...(admin ? [] : (["tools", "setup", "alarms", "holds"] as const)),
    ...(atlas ? [] : (["atlas"] as const)),
  ];
}

export function useAttention(): Attention[] {
  const now = useNow();
  const agents = useAgents();
  const budget = useOrgBudget();
  const { connected, authRejected } = useConnection();
  const engine = useEngineHealth();
  const org = useOrg();
  const nameOf = useMemo(() => nameOfIn(indexOrg(org)), [org]);
  // THE EXECUTORS' DURABLE ROWS, because a parked one is the longest-lived item
  // this queue has by construction — it is waiting for a person — and what the
  // row says about it is how long its box is still held, which is the pause
  // window only the durable record carries. On the Executors board's own poll, so
  // the queue and the board cannot disagree about a run — see RUNS_POLL_MS.
  const { data: runs } = useQuery("executors", undefined, { pollMs: RUNS_POLL_MS });
  // THE MCP ALARMS ON THE MCP PAGE'S OWN POLL, asked only of a reader that
  // page opens for, so the Inbox and the page cannot disagree about a server.
  const readsMcp = useReadsMcpAlarms();
  const { data: mcp } = useQuery("mcp_servers_status", undefined, {
    pollMs: MCP_STATUS_POLL_MS,
    enabled: readsMcp,
  });
  const mcpAlarms = readsMcp && mcp?.reading === "admin" ? mcp.alarms : NO_MCP_ALARMS;
  // THE PLATFORM NOTICES, asked of the same administrator on the advisor's own
  // poll: what blocks the setup and the alarms that have stood, nothing else.
  const { data: advisor } = useQuery("advisor", undefined, {
    pollMs: ADVISOR_POLL_MS,
    enabled: readsMcp,
  });
  // READ, NEVER TRUSTED AS TYPED: an engine that answers no advisor, or one
  // older than the notices, raises nothing here.
  const notices = readsMcp && Array.isArray(advisor?.notices) ? advisor.notices : NO_NOTICES;
  const findings = readsMcp && Array.isArray(advisor?.findings) ? advisor.findings : NO_FINDINGS;
  const setupUnread = readsMcp && Array.isArray(advisor?.unread) ? advisor.unread : NO_SETUP_UNREAD;
  const readsDrift = useReadsAtlasDrift();
  const { data: drift } = useQuery("atlas_review", MY_DRIFT, {
    pollMs: ATLAS_DRIFT_POLL_MS,
    enabled: readsDrift,
  });
  const atlasDrift = readsDrift && Array.isArray(drift?.items) ? drift.items : NO_DRIFT;
  return useMemo(
    () =>
      attentionQueue({
        agents,
        runs: runs?.runs ?? [],
        budget,
        engine: engine ?? null,
        connected,
        authRejected,
        now,
        nameOf,
        mcpAlarms,
        notices,
        findings,
        setupUnread,
        atlasDrift,
      }),
    [
      agents,
      runs,
      budget,
      engine,
      connected,
      authRejected,
      now,
      nameOf,
      mcpAlarms,
      notices,
      findings,
      setupUnread,
      atlasDrift,
    ],
  );
}
