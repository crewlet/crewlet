/**
 * What a node says about itself: the `stream` query's answer, which is
 * `api.Health` in Go, whole.
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
}
