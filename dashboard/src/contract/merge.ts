/**
 * How a change merges, and why a merge is refused, in the engine's words —
 * what a change page's merge controls offer and answer in.
 *
 * Each is a COPY of a closed set `internal/code` owns, HELD BOTH WAYS, in
 * order, by `internal/code`'s dashboard tests: a method the engine grew that
 * this lacks is one nobody here can choose, a refusal it grew is answered only
 * in its class's general words, and a value kept here that the engine dropped
 * is a choice refused on every press or a sentence nobody will ever read.
 */

/**
 * How a change merges into its target (`code.MergeMethods`), the engine's
 * default first: a merge commit, one squashed commit, or a fast-forward of the
 * target to the change's head. Held by
 * `internal/code.TestTheDashboardOffersEveryMergeMethod`.
 */
export const MERGE_METHODS = ["merge", "squash", "fast_forward"] as const;

/**
 * Why a merge, an enqueue or an override decided nothing (`code.MergeRefusals`),
 * in the order the engine's decides reach them. The act route names the word
 * beside the refusal's class (`refusal`), because several share one class — a
 * head that moved and a train whose base moved are both `stale_version` — and
 * the merge controls answer each in its own words. Held by
 * `internal/code.TestTheDashboardKnowsEveryMergeRefusal`.
 */
export const MERGE_REFUSALS = [
  "repo_deleted",
  "repo_archived",
  "repo_importing",
  "repo_mirrored",
  "expunging",
  "change_not_found",
  "change_not_open",
  "change_draft",
  "stale",
  "head_moved",
  "target_moved",
  "target_moving",
  "not_person",
  "queue_target",
  "no_queue",
  "already_queued",
  "queue_full",
  "gate_unmet",
  "source_refused",
  "upload_expired",
  "repo_size",
  "compaction_behind",
  "not_queued",
  "not_permitted",
  "not_green",
  "not_red",
  "no_landing",
  "record_full",
  "busy",
  "not_applied",
  "unavailable",
] as const;

/**
 * The word a merge's BUILD answers when the change does not merge cleanly
 * onto its target's tip (`code.WriteConflict`): not a decide's refusal but
 * the git node's, naming the conflicting paths. Held with [MERGE_REFUSALS].
 */
export const MERGE_CONFLICT = "conflict";
