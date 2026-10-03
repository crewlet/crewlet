/**
 * The engine's own conditions — the attention queue — as the screens that
 * draw it read it.
 *
 * `lib/attention.ts` is the pure derivation; this is the one place its inputs
 * are gathered, because Home and the Inbox both draw the queue and two copies
 * of "which reads feed it" is how the landing screen and the place you act
 * come to disagree about what needs a decision.
 *
 * # The clock is the reading, compared by value
 *
 * THE QUEUE READS THE CLOCK — a live call goes stale at two minutes and
 * stalled at ten, and a parked run counts down its pause window in minutes —
 * and a hook that took the second for it drew both screens whole once a
 * second: every row of the Inbox's list and Home's attention, to change
 * nothing on all but the handful of ticks a threshold is crossed on.
 *
 * SO THE QUEUE IS THE READING. It is worked out on every tick against the
 * shared instant and compared BY VALUE: its serialisation is the primitive
 * `useClockReading` compares, so a tick that changes no item renders nothing
 * and the tick that crosses a threshold renders exactly once. Every field of
 * an item is plain data — a string, a list of path segments, a record of
 * query parameters — so the serialisation is the queue, and parsing it back
 * is the queue the tick produced. Comparing anything narrower (the staleness
 * words, the countdown) would be a second copy of which fields the clock
 * reaches, and the copy would drift the day a condition starts reading it
 * somewhere else.
 */

import { useMemo } from "react";
import { attentionQueue, type Attention } from "./attention.ts";
import { useQuery } from "./useQuery.ts";
import { useClockReading } from "./clock.ts";
import { useAgents, useConnection, useEngineHealth, useOrg, useOrgBudget } from "./store-hooks.ts";
import { indexOrg, nameOfIn } from "./seats.ts";
import { RUNS_POLL_MS } from "./runs.ts";

export function useAttention(): Attention[] {
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
  const reading = useClockReading((now) =>
    JSON.stringify(
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
    ),
  );
  return useMemo(() => JSON.parse(reading) as Attention[], [reading]);
}
