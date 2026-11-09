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
import type { AdvisorFinding } from "~/protocol/advisor.ts";
import { attentionQueue, type Attention, type Subject } from "./attention.ts";
import { useQuery } from "./useQuery.ts";
import { useNow } from "./clock.ts";
import { useAgents, useConnection, useEngineHealth, useOrg, useOrgBudget } from "./store-hooks.ts";
import { indexOrg, nameOfIn } from "./seats.ts";
import { RUNS_POLL_MS } from "./runs.ts";
import { MCP_STATUS_POLL_MS } from "./mcpStatusPoll.ts";
import { useViewer } from "./viewer.ts";

/** No MCP alarm: a reader who is not asked them, or an answer not in yet. */
const NO_MCP_ALARMS: readonly McpAlarm[] = [];

/** No setup blocker: a reader the advisor does not answer, or an answer not in yet. */
const NO_SETUP_BLOCKERS: readonly AdvisorFinding[] = [];

/** How often the queue asks the advisor again: a setup changes by revisions. */
const ADVISOR_POLL_MS = 60_000;

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
  return useReadsMcpAlarms() ? [] : ["tools", "setup"];
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
  // THE SETUP'S BLOCKERS, asked of the same administrator, on the advisor's own
  // poll: only the owner's and only what blocks, so a note never reaches here.
  const { data: advisor } = useQuery("advisor", undefined, {
    pollMs: ADVISOR_POLL_MS,
    enabled: readsMcp,
  });
  const setupBlockers = useMemo(
    () =>
      // READ, NEVER TRUSTED AS TYPED: an engine that answers no advisor, or
      // answers in another shape, raises nothing here.
      readsMcp && Array.isArray(advisor?.findings)
        ? advisor.findings.filter((f) => f.actor === "owner" && f.severity === "blocks")
        : NO_SETUP_BLOCKERS,
    [readsMcp, advisor],
  );
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
        setupBlockers,
      }),
    [agents, runs, budget, engine, connected, authRejected, now, nameOf, mcpAlarms, setupBlockers],
  );
}
