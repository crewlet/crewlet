/**
 * What Settings › MCP & tools reads, and the one view of it every part of the
 * page draws: each server's condition, the alarms standing on them, the calls
 * held for approval, each agent's tools and the built-in tools
 * (`mcp_servers_status`), and on a server's page that server whole
 * (`GET /mcp-servers/{name}`).
 *
 * NOTHING HERE DECIDES A STATE, A RISK OR A REACH. A server's state, its
 * alarms and their fixes, a tool's risk and level and a failure's class are
 * the engine's answer, and which agents a server reaches is the engine's
 * grant, answered for an administrator as each agent's servers (`agents`,
 * each with its reason). The page names them.
 *
 * TWO READINGS. Whoever the engine answers the administrator's reading
 * (`reading: "admin"`) is the page's admin: every gesture and every launch
 * line is theirs. Everybody else reads a member's, and the page draws what it
 * carries.
 *
 * KEPT CURRENT three ways, because each misses what the others catch: every
 * 15 seconds while the page is open (the heartbeat's cadence), when the fleet
 * applies a new configuration (a server added, changed or removed by anyone),
 * and when the engine's alarms move (a server that just went down).
 */

import { useCallback, useEffect, useMemo, useRef } from "react";
import type {
  BuiltinGroup,
  McpAlarm,
  McpMemberServerStatus,
  McpServerStatus,
  McpServersStatusAnswer,
  McpMemberServersStatusAnswer,
} from "~/contract/mcp.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";
import { MCP_STATUS_POLL_MS } from "~/lib/mcpStatusPoll.ts";
import { indexOrg, type Seat } from "~/lib/seats.ts";
import { useEngineHealth, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useRest, type RestResult } from "~/lib/useRest.ts";
import { getEntity } from "~/protocol/configWrite.ts";
import type { LogRefusal, QueryRefusal } from "~/protocol/index.ts";
import { readServer, type ServerDetail } from "./api.ts";

/** The page's whole reading of the servers. */
export interface McpData {
  /** Whether the engine answered the administrator's reading: every gesture is theirs. */
  admin: boolean;
  answer: McpServersStatusAnswer | McpMemberServersStatusAnswer | null;
  /** In name order, as the engine sends them. */
  servers: readonly McpMemberServerStatus[];
  server: (name: string) => McpMemberServerStatus | null;
  /** One server's row as an administrator reads it; null for a member. */
  adminRow: (name: string) => McpServerStatus | null;
  /** The MCP alarms standing now; none for a member. */
  alarms: readonly McpAlarm[];
  builtin: readonly BuiltinGroup[];
  /** Every agent seat, for names. */
  agents: readonly Seat[];
  /** A seat's name by its handle; the handle where the chart holds no such seat. */
  seatName: (handle: string) => string;
  /**
   * The agent seats a server's grant reaches, in the chart's order, read off
   * the answer's `agents`; null where the engine did not say: a member's
   * reading carries no reach, and an administrator's says it is still
   * counting (`agents: null`), which is not "no agent".
   */
  reaches: (name: string) => readonly Seat[] | null;
  /** Whether the answer says who each server reaches: an administrator's, once counted. */
  reachKnown: boolean;
  loading: boolean;
  /** Why the list was not answered, as `QueryState` draws it; null while it was. */
  failure: {
    error: QueryErrorCode;
    refusal: QueryRefusal | LogRefusal | null;
    detail?: string;
  } | null;
  /** Ask again now, after a write this page made. */
  reload: () => void;
}

/** A key that moves whenever the engine's alarms do: its count and the one standing longest. */
function alarmsKey(health: ReturnType<typeof useEngineHealth>): string {
  const a = health?.alarms;
  return a ? `${a.count}:${a.worst ?? ""}` : "";
}

/**
 * Calls `reload` when the fleet applies a configuration or the alarms move,
 * never on the first draw (the read is already in flight then).
 */
function useReloadOnChange(reload: () => void): void {
  const health = useEngineHealth();
  const key = `${health?.applied_epoch ?? 0}|${alarmsKey(health)}`;
  const seen = useRef(key);
  useEffect(() => {
    if (seen.current === key) return;
    seen.current = key;
    reload();
  }, [key, reload]);
}

export function useMcpData(): McpData {
  const status = useQuery("mcp_servers_status", undefined, {
    pollMs: MCP_STATUS_POLL_MS,
    refetchOnFocus: true,
  });
  const org = useOrg();
  const { refetch } = status;
  const reload = useCallback(() => refetch(), [refetch]);
  useReloadOnChange(reload);

  const answer = status.data;
  const seats = useMemo(() => indexOrg(org).seats.filter((s) => s.kind === "agent"), [org]);
  const view = useMemo(() => mcpViewOf(answer, seats), [answer, seats]);
  return useMemo(
    () => ({
      ...view,
      loading: status.loading && !answer,
      failure: status.error
        ? {
            error: status.error,
            refusal: status.refusal,
            ...(status.detail ? { detail: status.detail } : {}),
          }
        : null,
      reload,
    }),
    [view, answer, status.loading, status.error, status.refusal, status.detail, reload],
  );
}

/** What the page draws of an answer, joined to the agent seats: the hook's reading without its read. */
export type McpView = Omit<McpData, "loading" | "failure" | "reload">;

/**
 * The page's join of the engine's answer and the agent seats, PURE, so a
 * suite draws exactly what a reader would from the same answer.
 */
export function mcpViewOf(
  answer: McpServersStatusAnswer | McpMemberServersStatusAnswer | null,
  seats: readonly Seat[],
): McpView {
  const servers: readonly McpMemberServerStatus[] = answer?.servers ?? [];
  const byName = new Map(servers.map((s) => [s.name, s]));
  const names = new Map(seats.map((s) => [s.handle, s.name]));
  const agents = answer?.reading === "admin" ? answer.agents : null;
  const reachKnown = agents !== null;
  const reached = new Map<string, Set<string>>();
  for (const a of agents ?? []) {
    for (const s of a.servers) {
      const set = reached.get(s.server) ?? new Set<string>();
      set.add(a.seat);
      reached.set(s.server, set);
    }
  }
  return {
    admin: answer?.reading === "admin",
    answer,
    servers,
    server: (name) => byName.get(name) ?? null,
    adminRow: (name) =>
      answer?.reading === "admin" ? (answer.servers.find((s) => s.name === name) ?? null) : null,
    alarms: answer?.reading === "admin" ? answer.alarms : [],
    builtin: answer?.builtin ?? [],
    agents: seats,
    seatName: (handle) => names.get(handle) ?? handle,
    reaches: (name) =>
      reachKnown ? seats.filter((s) => reached.get(name)?.has(s.handle) ?? false) : null,
    reachKnown,
  };
}

/**
 * One server whole, on its page: re-read on the list's cadence, and when the
 * fleet applies a configuration or the alarms move, so the page and the list
 * beside it never disagree for longer than a beat.
 */
export function useServerDetail(name: string): RestResult<ServerDetail> {
  const read = useRest<ServerDetail>(`mcp-server:${name}`, (signal) => readServer(name, signal), {
    pollMs: MCP_STATUS_POLL_MS,
    refetchOnFocus: true,
  });
  const { reload } = read;
  const quiet = useCallback(() => void reload({ quiet: true }), [reload]);
  useReloadOnChange(quiet);
  return read;
}

/**
 * A server's entry as the configuration writes it (`GET /config/mcp-servers/{name}`,
 * values masked), for an administrator: the form fields no answer carries
 * (its `access` as written, its `tool_prefix`). `enabled` false asks nothing.
 */
export function useServerEntry(
  name: string,
  enabled: boolean,
): RestResult<Record<string, unknown> | null> {
  return useRest(
    enabled ? `mcp-entity:${name}` : null,
    async (signal) => {
      const read = await getEntity("mcp-servers", name, signal);
      return read.kind === "entity" ? read.entity : null;
    },
    { refetchOnFocus: true },
  );
}
