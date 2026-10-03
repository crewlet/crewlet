/**
 * Which nodes a fleet answer was assembled from — the `coverage` every
 * history answer carries (`events`, `event`, `event_series`, `trace`, `turn`,
 * `turns`, `phases`, a seat's `llm_history`, and the integrations' traffic).
 *
 * The fleet's turn-level detail is NOT replicated: each node keeps the events
 * it published, and a read asks every live node at query time (ADR-0021). So
 * an answer can be missing a node — one that left, or one that did not answer
 * inside the read budget — and this is where it says so, by name. A short
 * answer that did not would read exactly like a quiet company.
 *
 * EXACTLY WHAT `internal/eventfan` SENDS, held by
 * `TestTheDashboardDeclaresExactlyTheCoverageTheEngineSends` in both
 * directions. The node row is composed into [Coverage] and not exported,
 * because nothing reads it on its own.
 */

/** One node's part in an answer. */
interface NodeCoverage {
  id: string;
  answered: boolean;
  /** Why it did not answer, in words an operator can act on; empty when it
   *  answered. */
  error: string;
}

export interface Coverage {
  /** Every node asked or heard from, sorted by id — the answering node
   *  always among them. */
  nodes: NodeCoverage[];
  /** True only when the roster could be read and every node on it answered. */
  complete: boolean;
}
