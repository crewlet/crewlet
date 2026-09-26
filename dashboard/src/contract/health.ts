import type { Coverage } from "./coverage.ts";

/**
 * The alarm table as a public health body carries it: how many are firing and
 * which has stood longest. `api.HealthAlarms` in Go, held member for member by
 * the same gate as `EngineHealth`.
 *
 * A COUNT AND ONE NAME, never the rows: the envelope is public, and what each
 * alarm measured is the operator-only `work_retention` answer.
 */
interface HealthAlarms {
  /** How many alarms are firing. Zero is a real zero: the table was evaluated. */
  count: number;
  /**
   * The alarm that has been firing LONGEST — the condition that has gone
   * unanswered longest, since the engine's table asserts no severity of its
   * own. Absent when nothing is firing.
   */
  worst?: string;
}

/**
 * What a node says about itself: the `health` push, which is `api.Health` in
 * Go, WHOLE — in the snapshot and on every five-second tick. There is no query
 * for it any more; read it with `useEngineHealth()`.
 *
 * EVERY JSON TAG ON THAT STRUCT IS A MEMBER HERE AND NOTHING ELSE IS, held by
 * `internal/api.TestTheDashboardDeclaresExactlyTheHealthTheEngineReports` —
 * and a field the engine omits when empty is optional here, because a required
 * member the wire can leave out is a claim the type cannot keep. The two this
 * interface was missing, `stall_lag_seconds` and `unproven_seconds`, are the
 * two an operator most needs before a node restarts itself, and neither could
 * reach a screen while nothing declared them.
 *
 * Optional beyond `status` even where the struct always sends a member: a
 * fleet mid-upgrade answers from older nodes too, each lacking whatever it
 * predates, and a screen has to say "unknown" for that rather than read
 * `undefined` as a value.
 */
export interface EngineHealth {
  status: string;
  node?: string;
  configured?: boolean;
  version?: string;
  /**
   * When this node's ENGINE started, which is when the node started: the API
   * is served inside the engine's process, and the fleet view reports the same
   * instant for this node.
   */
  started_at?: string;
  queue?: string;
  clients?: number;
  /**
   * How far back the event log can be read, in SECONDS: the hard bottom of
   * paging, past which every page is empty for ever.
   *
   * `store.EventHistory` is the only thing that decides it, and three screens
   * restated it as literal copy ("the store keeps 30 days") while nothing on
   * the wire carried it — so a change to the retention would have left all
   * three lying with nothing to catch it, and a reader told the wrong floor
   * stops paging early. Seconds rather than days, because a client that
   * re-derives the unit is a second place it can be wrong; the sentence is
   * `eventHistoryLabel` in `lib/format.ts`.
   */
  event_history_seconds?: number;
  /**
   * How far back a NAMED spend window can reach, in SECONDS: the replicated
   * usage domain's own history, which is not the event log's. A spend chart
   * states this floor, never `event_history_seconds` — the two answer "can I
   * still chart that month" and "can I still open that turn".
   */
  spend_history_seconds?: number;
  in_flight?: number;
  shutting_down?: boolean;
  posture?: string;
  applied_epoch?: number;
  /** The handles THIS node currently holds. */
  seats?: string[];
  /**
   * How far behind this node's watched duty is, in seconds — present only
   * while it is behind at all. It climbs towards the seat lease TTL, at which
   * the watchdog ends the process, so a node degrading shows here before the
   * restart rather than only afterwards in its exit code.
   */
  stall_lag_seconds?: number;
  /**
   * Each seat stranded by a teardown that could not be proven, mapped to how
   * long it has been stranded in seconds — present only while one is.
   */
  unproven_seconds?: Record<string, number>;
  /**
   * How many nodes hold a presence lease — the fleet this node's fan-outs
   * divide their work by. ABSENT WHEN THE PRESENCE READ FAILED, never 0: say
   * "node count unavailable" (`nodeCountLabel` in `lib/format.ts`).
   */
  nodes?: number;
  /**
   * This node's standing alarms, from the one evaluation the engine's gauge and
   * alarm log lines come from. Absent before that evaluation first runs and on
   * a node running no state log — neither has looked, which is not healthy.
   */
  alarms?: HealthAlarms;
  /**
   * Which nodes this node's live projection was seeded from at boot — the
   * feed, the spend window and each seat's last turn every screen starts from.
   * Absent until the seed has run.
   */
  seeded_from?: Coverage;
}
