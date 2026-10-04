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
import { attentionQueue, type Attention } from "./attention.ts";
import { useQuery } from "./useQuery.ts";
import { useNow } from "./clock.ts";
import { useAgents, useConnection, useEngineHealth, useOrg, useOrgBudget } from "./store-hooks.ts";
import { indexOrg, nameOfIn } from "./seats.ts";
import { RUNS_POLL_MS } from "./runs.ts";

export function useAttention(): Attention[] {
  const now = useNow();
  const agents = useAgents();
  const budget = useOrgBudget();
  const { connected, authRejected } = useConnection();
  const engine = useEngineHealth();
  const org = useOrg();
  const nameOf = useMemo(() => nameOfIn(indexOrg(org)), [org]);
  // THE DURABLE CODING RUNS, because a parked one is the longest-lived item
  // this queue has by construction — it is waiting for a person — and what the
  // row says about it is how long its box is still held, which is the pause
  // window only the durable record carries. On the runs board's own poll, so
  // the queue and the board cannot disagree about a run — see RUNS_POLL_MS.
  const { data: runs } = useQuery("sandbox_runs", undefined, { pollMs: RUNS_POLL_MS });
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
      }),
    [agents, runs, budget, engine, connected, authRejected, now, nameOf],
  );
}
