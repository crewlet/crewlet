/**
 * Whether a node runs the revision the fleet activated — one rule, read by
 * every screen that counts "behind on config".
 *
 * # Not by the epoch alone
 *
 * A node's `config_epoch` is the epoch it ATTEMPTED (`engine.Reconciler.record`
 * writes the pointer's epoch beside every outcome), not the one it runs. Every
 * figure used to call a node current once that number reached the target, so
 * the commonest per-node failure — a revision one node refuses, for an env var
 * or an MCP binary it alone lacks — read as current everywhere: left out of
 * the "Behind on config" tile, the sidebar figure and both Settings home
 * widgets, "Lag: current" in its header, and "running the active revision" in
 * its Running panel while it ran the one before. And the epoch is the pointer
 * key's revision in the coordination store, which restarts at 1 when that store
 * is rebuilt, so one number can name two revisions.
 *
 * So a node is CURRENT only when it reports the target epoch, on the target's
 * revision, with status `ok` — and runs it. `degraded` is not current: the
 * apply failed after a restart-required subsystem was already mutated, which
 * the engine never counts as converged (`configplane.ApplyStatus.Converged`).
 *
 * # Six answers, so a screen never prints "0 epochs behind"
 *
 * Every node that is not current is not on the active revision, and the screen
 * says which way: an older epoch, a refusal of the target, a partial apply of
 * it, or another revision at an epoch the target does not explain. A node that
 * has reported nothing is a different fact again — not a failure, just silent —
 * and an answer with no activation has no target to be behind.
 *
 * Pure over the wire types, so the rule is tested without a screen.
 *
 * # The rule here, its words with the screens
 *
 * The sidebar's Nodes figure counts by this rule, and the sidebar is in the
 * first paint, so whatever this module holds every reader downloads before the
 * first screen draws. A standing's words are Settings' alone
 * (`routes/settings/standingWords.ts`), and travel with those screens.
 */

import type { FleetAnswer, FleetNode } from "~/protocol/types.ts";

/** Where one node stands against the fleet's activation. */
export type ConfigStanding =
  /** The answer carries no activation: nothing to be current on or behind. */
  | { kind: "no-target" }
  /** The node has reported no apply at all. */
  | { kind: "unreported"; serving?: string }
  /** It runs the activated revision at the activated epoch. */
  | { kind: "current"; serving?: string }
  /** It last attempted an older epoch than the target. */
  | { kind: "behind"; epochs: number; serving?: string }
  /** It refused the target and rolled back: it runs `serving`, the one before. */
  | { kind: "refused"; serving?: string }
  /** Its apply of the target failed after a restart-required change. */
  | { kind: "degraded"; serving?: string }
  /**
   * Any other reading: the target epoch on another revision, or an epoch the
   * pointer does not explain (one a rebuilt coordination store has not reached
   * again). `servesActive` is a node whose engine runs the activated revision
   * all the same, from an activation the fleet has since replaced.
   */
  | { kind: "elsewhere"; attempted?: string; serving?: string; servesActive: boolean };

/** The kinds a node counts as not running the fleet's revision under. */
const OFF = new Set<ConfigStanding["kind"]>([
  "unreported",
  "behind",
  "refused",
  "degraded",
  "elsewhere",
]);

/** Where `node` stands against the activation `answer` carries. */
export function configStanding(
  answer: Pick<FleetAnswer, "activation">,
  node: FleetNode,
): ConfigStanding {
  const activation = answer.activation;
  if (!activation) return { kind: "no-target" };
  const serving = node.config_serving_revision_id || undefined;
  const epoch = node.config_epoch ?? 0;
  const attempted = node.config_revision_id ?? "";
  if (epoch === 0 && attempted === "") return { kind: "unreported", serving };
  // THE POINTER'S OWN EPOCH, read in the same read as its revision — never
  // `target_epoch` and `activation` from two reads that could disagree.
  const target = activation.epoch;
  if (epoch < target) return { kind: "behind", epochs: target - epoch, serving };
  if (epoch === target && attempted === activation.revision_id) {
    if (node.config_status === "error") return { kind: "refused", serving };
    if (node.config_status === "degraded") return { kind: "degraded", serving };
    if (node.config_status === "ok" && serving === activation.revision_id) {
      return { kind: "current", serving };
    }
  }
  return {
    kind: "elsewhere",
    attempted: attempted || undefined,
    serving,
    servesActive: serving === activation.revision_id,
  };
}

/**
 * Whether a node does not run the fleet's revision — what every "behind on
 * config" figure counts. A silent node is counted: nothing says it runs the
 * fleet's revision. An answer with no activation counts none.
 */
export function offRevision(answer: Pick<FleetAnswer, "activation">, node: FleetNode): boolean {
  return OFF.has(configStanding(answer, node).kind);
}
