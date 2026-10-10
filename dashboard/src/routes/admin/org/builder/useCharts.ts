/**
 * The React half the canvas and the outline share: the charts of the current
 * draft, and where "Open seat" goes.
 *
 * ONE COPY OF EACH. Both views draw the same structure and open the same seat
 * screen, so that half lives here once. The other half, the expansion and
 * roving-focus rules over a shared visible-order model, is the design
 * system's: `useTreeState` and `treeStep` are what both views take from
 * `@crewlethq/ui`, and only their ARIA patterns differ (a tree of cards, a
 * treegrid of rows).
 */

import { useCallback, useMemo } from "react";
import { useNavigator } from "~/app/router.tsx";
import { chartInputs, reporting, structure, type Reporting, type Structure } from "./chartModel.ts";
import type { BuilderState } from "./model/reducer.ts";
import type { OpenScreen } from "./nodeActions.tsx";

/**
 * The structure chart of the current draft. A reading of the draft, the saved
 * base and the last check and of nothing else, so a live push, a refusal or a
 * pending update leaves it (and every layout built on it) as it was.
 */
export function useStructure(state: BuilderState): Structure {
  const { draft, baseDraft, check, generation } = state;
  return useMemo(
    () => structure(chartInputs({ draft, baseDraft, check, generation })),
    [draft, baseDraft, check, generation],
  );
}

/** The reporting chart of the current draft, on the same terms as [useStructure]. */
export function useReporting(state: BuilderState): Reporting {
  const { draft, baseDraft, check, generation } = state;
  return useMemo(
    () => reporting(chartInputs({ draft, baseDraft, check, generation })),
    [draft, baseDraft, check, generation],
  );
}

/** Opens a seat's own screen: a new screen, so it pushes. */
export function useOpenScreen(): OpenScreen {
  const nav = useNavigator();
  return useCallback((path) => nav.to(path), [nav]);
}
