/**
 * What a fleet answer is MISSING, said where the answer is drawn.
 *
 * The fleet's turn-level history is not replicated: each data node keeps the
 * events it published and the events it keeps in custody for a node without
 * `data`, and a read asks every live data node at query time (ADR-0021). An
 * answer can therefore be short a node — one that did not answer inside the
 * read budget, or that runs a build speaking another version of the scatter —
 * and the engine says so, by name, in the answer's `coverage`. A screen that
 * drew the rows and dropped that sentence would show a short list exactly
 * like a quiet company.
 *
 * ONE NOTE FOR EVERY ANSWER A SCREEN DREW FROM. A list and its axis are two
 * scatters, and a node silent on one is usually silent on both; the note
 * names each missing node once, with the first reason it was given.
 */

import { Callout, InlineCode } from "@crewlethq/ui";
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

/**
 * What the missing nodes cost an answer, as the sentence after the count. It
 * points back at them ("Its agents…", "Their agents…"), so it is written once
 * for a single node and once for several.
 */
export interface LostSentence {
  one: string;
  many: string;
}

/** What a history answer loses: whatever the missing nodes published. */
const PUBLISHED: LostSentence = {
  one: "What it published is not here",
  many: "What they published is not here",
};

export function CoverageNote({
  coverage,
  what,
  lost = PUBLISHED,
}: {
  /** Every answer the screen drew from. */
  coverage: readonly (Coverage | null | undefined)[];
  /** What the answer is, as the note's singular subject: "this list", "this trace". */
  what: string;
  /**
   * What a missing node COSTS this answer. A history answer loses what the
   * node published; an answer each node gives about the seats it HOLDS (the
   * diaries) loses those seats' counts, and saying "published" there would
   * send a reader looking for the wrong gap.
   */
  lost?: LostSentence;
}) {
  const { nodes, incomplete } = missingFrom(coverage);
  if (!incomplete && nodes.length === 0) return null;
  const one = nodes.length === 1;
  return (
    <Callout variant="warning" className="coverage-note">
      {nodes.length === 0 ? (
        <>The fleet&rsquo;s node roster could not be read, so {what} may not include every node.</>
      ) : (
        <>
          {what.charAt(0).toUpperCase() + what.slice(1)} is missing {nodes.length}{" "}
          {one ? "node" : "nodes"}. {one ? lost.one : lost.many}:
          <ul className="coverage-nodes">
            {nodes.map((n) => (
              <li key={n.id}>
                <InlineCode>{n.id}</InlineCode>
                {n.error ? `: ${n.error}` : ""}
              </li>
            ))}
          </ul>
        </>
      )}
    </Callout>
  );
}
