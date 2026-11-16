/**
 * What the engine will run the company with: the check's warnings, for the
 * banner over the chart and a save's review, so the two cannot count apart.
 * Repeats merge where they are drawn (`components/ConfigWarnings.tsx`).
 */

import type { Derived } from "~/protocol/index.ts";
import type { ConfigWarning } from "~/protocol/types.ts";
import type { SeatNamer } from "~/protocol/configAnswer.ts";
import type { PlacedProblem, ProblemLink } from "./problems.ts";
import type { BuilderState } from "./reducer.ts";

/** The check's warnings, each with where it is changed when the builder does not change it. */
export interface DraftWarnings {
  readonly warnings: readonly ConfigWarning[];
  readonly linkOf: (w: ConfigWarning) => ProblemLink | null;
  /** Seats named from the answer that came with the warnings, never the draft as it stands. */
  readonly seat: SeatNamer;
}

export function warningsOf(placed: readonly PlacedProblem[], derived: Derived | null): DraftWarnings {
  const links = new Map<ConfigWarning, ProblemLink | null>();
  for (const p of placed) {
    if (p.severity === "warning") links.set(p.source as ConfigWarning, p.link);
  }
  const seats = new Map((derived?.seats ?? []).map((s) => [s.handle, s]));
  return {
    warnings: [...links.keys()],
    linkOf: (w) => links.get(w) ?? null,
    seat: (handle) => {
      const s = seats.get(handle);
      return s === undefined ? undefined : { name: s.name || handle, agent: s.kind === "agent" };
    },
  };
}

/** The warnings of the check that answered the draft as it stands; none while it answered an older one. */
export function draftWarnings(state: BuilderState): DraftWarnings {
  if (state.check.generation !== state.generation) return warningsOf([], null);
  return warningsOf(state.check.problems.all, state.check.derived);
}
