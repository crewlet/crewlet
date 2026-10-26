/**
 * Where a node stands on a revision a write from this tab activated — one
 * rule, read by every screen that follows its own configuration write: the
 * org builder's after-save strip (by this node's health push for a reader
 * without Admin, by every node's row in the fleet view for Admin) and the org
 * chart's note, a token ceiling's mark beside its figure, and a charter part
 * shown as saved.
 *
 * # One rule, whichever wire the report came over
 *
 * A node says two things alike on its own health push and on its row in the
 * fleet view — the epoch it last applied, and its last concluded attempt (the
 * epoch, the revision, how it went) — and on the row a third: the revision it
 * runs. [nodeStanding] reads the push and [fleetNodeStanding] the row into one
 * report, and ONE predicate judges it ([standingOf]), so an Admin and a lead
 * reading the same node about the same save are told the same. The fleet path
 * judged by a rule of its own once, and could not see the applied epoch: a
 * node that applied a later epoch and then refused a still later one was one
 * the lead's push called applied and the Admin's strip called passed, and a
 * reconnect that read the fleet again took an Admin's "Applied." back.
 *
 * The row's one fact more is the revision the node runs, and a node running
 * the written revision has applied it.
 *
 * WHAT A RESTART FORGETS, it forgets on both wires, and a verdict can go with
 * it. The applied epoch starts again at 0 with the process — the push's
 * `applied_epoch` and the row's `config_applied_epoch` alike — and moves only
 * on the node's next apply that goes ok, while a node with the `data` role
 * boots on the revision it applied before (one without it boots on nothing and
 * runs no company, so for it "passed" is simply true); so until such a node
 * applies again, a refusal of a later activation reads as having PASSED the
 * write. Booted on the written revision,
 * the node reads applied on its row, which says what it runs, and passed on
 * its push. Booted on a revision activated AFTER the write, it reads passed on
 * both, since nothing either wire carries says it ever applied the write. A
 * verdict said before the restart then turns: the strip's "Applied." into
 * "activated since" (a lead's on the next push, an Admin's on the fleet read
 * a reconnect makes), and the org chart's note, gone once the node applied
 * the write, comes back as "did not apply". Neither wire carries the epoch of
 * the activation a restarted node booted on, so no follower can tell this
 * from a node that never ran the write; carrying that epoch across a restart
 * belongs with issue #322 (an activation's identity across pointer
 * lifetimes), in the engine.
 *
 * # Not by the applied epoch alone
 *
 * The applied epoch ALONE CANNOT SAY A REFUSAL: a failed attempt leaves it
 * where it was, so a node that refused the revision read as one still applying
 * it, for as long as the tab stayed open — the strip, the ceiling's
 * "applying…" and the charter card's saved words alike. The node's last
 * concluded attempt says it, matched on the revision as well as the epoch,
 * since one epoch number names two revisions once a rebuilt coordination store
 * has restarted the count.
 *
 * # The attempt naming the write first
 *
 * For the same restart, that attempt is read BEFORE the applied epoch is
 * compared. A running node keeps the epoch it applied under the previous
 * pointer in memory (the reconciler moves it only on an apply that went ok),
 * so after a rebuild an applied epoch of 41 can stand beside a refusal of this
 * write at epoch 2, and compared first it called that refusal applied. And
 * before the revision the node runs: a refused re-activation of the revision
 * a node already runs — the credential-rotation gesture — names one revision
 * in both, and is a refusal all the same. Only when the attempt names
 * something else does the rest decide: an applied epoch at or past the
 * write's, or the written revision running, is applied, and an attempt at a
 * LATER epoch with this one never applied has passed it — the node will not
 * come back to it.
 *
 * What this cannot tell is a node that still holds an applied epoch from the
 * previous pointer above the write's and has not attempted the write yet: it
 * reads as applied. Both wires carry the applied activation's epoch alone,
 * and an epoch orders activations only within one pointer; telling the two
 * apart takes the applied activation whole, which issue #322 (a node skipping
 * a revision at an epoch it applied before the pointer was recreated) makes
 * the engine keep and say.
 *
 * # An absent push is no news
 *
 * [nodeStanding] takes a push, never its absence. A dropped socket CLEARS the
 * health slice (`Store.setConnected`), rightly for a reader asking whether the
 * node is up, and read as a standing that absence said "applying": a ceiling
 * marked refused by this node drew the saved figure marked applying again,
 * and a charter card the saved words, for as long as the socket was down. A
 * follower reads this node's LAST push instead (`useLastHealth`), and keeps
 * its write's first standing only until the tab has had one. The TAB keeps
 * that push (`StoreState.lastHealth`), not the follower: the org chart's note
 * and the Builder's strip follow a save that outlives their mount, and one
 * mounted while the socket was down had nothing to read, and said the save
 * was still applying.
 *
 * Pure over the wire types and holding no words: each follower says a
 * standing in its own screen's words, and keeps them with that screen — the
 * rule `configStanding.ts` keeps, so a shared rule never carries one screen's
 * copy into the bundle of every other screen that imports it.
 */

import type { ConfigAttempt, EngineHealth } from "~/contract/health.ts";
import type { FleetNode } from "~/protocol/types.ts";

/** A revision a write stored, and the epoch the write activated it at. */
export interface ActivatedRevision {
  readonly revisionId: string;
  readonly epoch: number;
}

/** Where a node stands on one activated revision. */
export type Standing =
  /** It runs the revision, or an epoch at or past it. */
  | { readonly kind: "applied" }
  /** Its last attempt was this revision at this epoch, and failed: it runs the one before. */
  | { readonly kind: "refused" }
  /** It attempted a later epoch (`by`, the revision there) and never applied this one. */
  | { readonly kind: "passed"; readonly by: string }
  /** Nothing it has reported concludes this revision yet. */
  | { readonly kind: "applying" };

/** What one node has said about the configuration it applies, from either wire. */
interface NodeReport {
  /** The epoch it last applied; 0 before its first apply since it started. */
  readonly appliedEpoch: number;
  /**
   * Its last concluded apply attempt; null before it has concluded one. The
   * push's own shape, which a fleet row spells as three fields of its own.
   */
  readonly attempt: ConfigAttempt | null;
  /** The revision it runs, where the report names it: the fleet row does, the push does not. */
  readonly serving: string | null;
}

/** THE rule ([nodeStanding], [fleetNodeStanding]): see the module doc for its order. */
function standingOf(report: NodeReport, write: ActivatedRevision): Standing {
  const { attempt } = report;
  if (attempt && attempt.epoch === write.epoch && attempt.revision_id === write.revisionId) {
    return attempt.status === "ok" ? { kind: "applied" } : { kind: "refused" };
  }
  if (report.appliedEpoch >= write.epoch || report.serving === write.revisionId) {
    return { kind: "applied" };
  }
  if (attempt && attempt.epoch > write.epoch) return { kind: "passed", by: attempt.revision_id };
  return { kind: "applying" };
}

/** This node's standing on `write`, from a health push it sent. */
export function nodeStanding(
  health: Pick<EngineHealth, "applied_epoch" | "config_attempt">,
  write: ActivatedRevision,
): Standing {
  return standingOf(
    { appliedEpoch: health.applied_epoch, attempt: health.config_attempt ?? null, serving: null },
    write,
  );
}

/**
 * A fleet node's standing on `write`, from its row in the fleet view — judged
 * as its own push would be ([nodeStanding]).
 *
 * A row with no epoch has reported no attempt (the view writes 0 for a node
 * with no record yet), and one with no applied epoch has applied nothing since
 * it started: the view leaves that out rather than writing a 0.
 */
export function fleetNodeStanding(node: FleetNode, write: ActivatedRevision): Standing {
  const epoch = node.config_epoch ?? 0;
  return standingOf(
    {
      appliedEpoch: node.config_applied_epoch ?? 0,
      attempt:
        epoch > 0
          ? {
              epoch,
              revision_id: node.config_revision_id ?? "",
              status: node.config_status ?? "",
            }
          : null,
      serving: node.config_serving_revision_id || null,
    },
    write,
  );
}
