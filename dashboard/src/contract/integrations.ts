/**
 * The `integrations` answer: one row per third-party surface, and what the
 * reconcile loop last found for each.
 *
 * Held against what the engine sends by
 * `internal/api/queries.TestTheIntegrationsRoomReadsWhatThisAnswerSends`, which
 * runs the real answer over a company with every surface configured and a
 * reconcile report with findings, and reads these interfaces by name.
 */

/** One observation a reconcile pass made that is not "fine". */
export interface ReconcileFinding {
  kind: string;
  subject?: string;
  /**
   * What is wrong, in one sentence — and only that.
   *
   * What to do about it is `remedy` and which things it is about is
   * `subjects`. Three fields because a reader wants them in three different
   * moments and one string cannot be laid out: glued, they arrived as one
   * unbroken paragraph carrying a problem, a name and an instruction, with
   * the disclosure summary running straight into its last word.
   */
  detail?: string;
  /** What to do about it, rendered as its own line at a quieter weight. */
  remedy?: string;
  action_url?: string;
  /**
   * What this finding MEANS, from the engine's own per-kind verdict table
   * (integration.FindingKind.Verdict) rather than from a second copy of the
   * closed set kept here.
   *
   * Two kinds are advisory — their phase is `ready`: something the engine did
   * not do and cannot undo, on an integration that is working. A reader that
   * treats every finding as a fault reports those as broken; one that keeps
   * its own list of which kinds are advisory reports a kind it has not heard
   * of as fine, which is worse. Optional because a node older than the field
   * sends neither — treat an absent phase as "cannot say", never as ready.
   */
  phase?: string;
  actor?: string;
  /**
   * Everything this finding is about, when there are many and `subject`
   * cannot name them all.
   *
   * `detail` is the card's status line and the engine caps it, so a finding
   * that listed its subjects inline arrived as a wall cut off mid-item —
   * thirty-six Datadog service accounts ending `…@agents.cr…`. The engine
   * now puts the COUNT in `detail` and the whole list here, so a reader with
   * room lays them out and one without is still correct.
   *
   * It named three of them in the sentence too, which put every short list
   * on screen twice: once as prose and once as the list, a centimetre apart.
   *
   * Absent on the ordinary finding about one thing.
   */
  subjects?: string[];
}

/**
 * What the reconcile loop last found for one surface.
 *
 * The row's `reconcile` is THREE-VALUED like every count beside it: an object
 * is a real finding, and `null` is either a node that could not read the
 * fleet's rows or a surface the loop has not reached yet. Neither is a claim
 * that the surface is healthy.
 */
export interface ReconcileStatus {
  /** unconfigured | awaiting_admin | provisioning | activating | degraded | ready */
  phase: string;
  /**
   * The phase in a reader's words, from [integration.Phase.Label] in Go.
   *
   * Optional because a node older than the field sends none, not because a
   * screen may skip it: derive nothing from `phase` that this can answer.
   */
  phase_label?: string;
  /**
   * Whether a disconnect has been ASKED FOR, which is a fact the phase
   * cannot carry on its own: between the request and the first teardown pass
   * the stored phase is still whatever the last reconcile concluded.
   */
  disconnecting?: boolean;
  /** "" | engine | provider | admin | operator — who has to act. */
  actor?: string;
  detail?: string;
  action_url?: string;
  /** settled | waiting | blocked */
  outcome?: string;
  attempts?: number;
  last_error?: string;
  last_attempt_at?: string | null;
  settled_at?: string | null;
  next_attempt_at?: string | null;
  /** Everything the pass saw, not only what the phase was derived from. */
  findings?: ReconcileFinding[];
}

/**
 * One third-party surface, as the `integrations` answer rows it.
 *
 * THE DECLARED MEMBERS ARE THE ONES A SCREEN READS BY NAME, and every one of
 * them is something the engine sends: `internal/api/queries`' integrations
 * gate fails on a declared member no row carries, as well as on a required one
 * some row omits. `label` and `detail` were declared here for as long as the
 * room existed and never sent, so the card's summary line read a field that
 * was always undefined and its fallback was the only branch that ever ran. The
 * rest of a row — the per-surface detail (`url`, `seats`, `org_id`, …) and the
 * flags the room reads loosely — rides the index signature, because which of
 * them a row carries depends on the surface.
 */
export interface IntegrationRow {
  key: string;
  configured: boolean;
  /** Deliveries the edge accepted. */
  inbound?: number | null;
  /**
   * The two OUTCOME counts, three-valued: a number, or null when this process
   * could not read its event log. Reporting that as 0 would claim every
   * delivery woke a seat on a node that cannot tell.
   *
   * They are read together with `inbound` and are misleading apart: "128
   * arrived" alone cannot tell a working integration from one whose every
   * delivery reaches nobody, and a seat draining a thread's backlog as one
   * turn looks like a seat that ignored twelve messages.
   */
  skipped?: number | null;
  coalesced?: number | null;
  /** Null means "nothing here can say", never "this surface is fine". */
  reconcile?: ReconcileStatus | null;
  /**
   * The public base URL this surface's registration at the third-party app
   * points at, and whether it is still the one in force.
   *
   * `endpoint_current` is three-valued: null is "nothing has recorded an
   * address for this surface", which is not the same claim as "the address
   * moved". False is a delivery route pointing somewhere that no longer
   * answers, which for a surface no pass converges only a person can fix.
   */
  endpoint?: string | null;
  endpoint_current?: boolean | null;
  [key: string]: unknown;
}

export interface IntegrationsAnswer {
  integrations: IntegrationRow[];
  traffic_known: boolean;
  /** The oldest delivery counted, or null when nothing was: the page is
   *  capped rather than time-bounded, so there is no fixed window to name. */
  traffic_since: string | null;
}
