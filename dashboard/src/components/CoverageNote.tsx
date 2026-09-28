/**
 * What a fleet answer is MISSING, said where the answer is drawn.
 *
 * The fleet's turn-level history is not replicated: every node keeps the
 * events it published, and a read asks every live node at query time
 * (ADR-0021). An answer can therefore be short a node — one that did not
 * answer inside the read budget, or that runs a build speaking another
 * version of the scatter — and the engine says so, by name, in the answer's
 * `coverage`. A screen that drew the rows and dropped that sentence would
 * show a short list exactly like a quiet company.
 *
 * ONE NOTE FOR EVERY ANSWER A SCREEN DREW FROM. A list and its axis are two
 * scatters, and a node silent on one is usually silent on both; the note
 * names each missing node once, with the first reason it was given.
 */

import { Callout } from "@crewlethq/ui";
import type { Coverage } from "~/contract/coverage.ts";

/** One node an answer was assembled without, and why. */
export interface MissingNode {
  id: string;
  error: string;
}

/**
 * The nodes absent from any of these answers, each once, in id order — and
 * whether the roster itself could not be read, which is incompleteness with
 * no node to name.
 */
export function missingFrom(coverages: readonly (Coverage | null | undefined)[]): {
  nodes: MissingNode[];
  incomplete: boolean;
} {
  const byId = new Map<string, MissingNode>();
  let incomplete = false;
  for (const c of coverages) {
    if (!c) continue;
    if (!c.complete) incomplete = true;
    for (const n of c.nodes ?? []) {
      if (!n.answered && !byId.has(n.id)) byId.set(n.id, { id: n.id, error: n.error });
    }
  }
  return {
    nodes: [...byId.values()].sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0)),
    incomplete,
  };
}

export function CoverageNote({
  coverage,
  what,
}: {
  /** Every answer the screen drew from. */
  coverage: readonly (Coverage | null | undefined)[];
  /** What the answer is, as the note's subject: "this list", "these turns". */
  what: string;
}) {
  const { nodes, incomplete } = missingFrom(coverage);
  if (!incomplete && nodes.length === 0) return null;
  return (
    <Callout variant="warning" className="coverage-note">
      {nodes.length === 0 ? (
        <>The fleet&rsquo;s node roster could not be read, so {what} may not include every node.</>
      ) : (
        <>
          {what.charAt(0).toUpperCase() + what.slice(1)}{" "}
          {nodes.length === 1 ? "is missing one node" : `is missing ${nodes.length} nodes`}, and
          what each of them published is not here:
          <ul className="coverage-nodes">
            {nodes.map((n) => (
              <li key={n.id}>
                <code className="inline">{n.id}</code>
                {n.error ? ` — ${n.error}` : ""}
              </li>
            ))}
          </ul>
        </>
      )}
    </Callout>
  );
}
