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
import { settingsSectionOpen } from "~/app/nav.ts";
import type { McpAlarm } from "~/contract/mcp.ts";
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

/**
 * Whether this reader is asked the MCP alarms: a reader the MCP section opens
 * for, by the rule its lock is drawn by, since `mcp_servers_status` is that
 * section's answer and a refusal on a timer is a refusal nobody reads. False
 * while the frame has not said who the reader is.
 */
function useReadsMcpAlarms(): boolean {
  const viewer = useViewer();
  return !viewer.loading && settingsSectionOpen("mcp-tools", viewer.grants);
}

/**
 * The subjects the attention queue does not watch for this reader, for the
 * quiet band that says what it watched (`watchedIn`): the MCP servers, for a
 * reader who is not asked their alarms.
 */
export function useUnwatched(): readonly Subject[] {
  return useReadsMcpAlarms() ? [] : ["tools"];
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
  const mcpAlarms = readsMcp ? (mcp?.alarms ?? NO_MCP_ALARMS) : NO_MCP_ALARMS;
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
      }),
    [agents, runs, budget, engine, connected, authRejected, now, nameOf, mcpAlarms],
  );
}
