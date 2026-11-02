/**
 * The sidebar's shape, as each reader left it: `full`, the rows under their
 * group headings, or `compact`, the kit's narrow rail of glyphs with each
 * named group folded into one tile (`Sidebar.tsx`).
 *
 * A CHOICE ABOUT ONE DESK, kept per reader in this browser the way a reader's
 * Home board is (`lib/readerStore.ts`): it is nobody else's, so it is never the
 * engine's, and one browser shared by two people keeps each their own. A tab
 * that has not learned its reader keeps the choice for the visit.
 *
 * THE KIT DRAWS WHAT THIS SAYS ONLY ABOVE THE SHELL BREAKPOINT. Below it the
 * sidebar is the kit's drawer, which always has the room for the full list, so
 * the stored choice is left as it is and comes back when the window widens.
 *
 * ONE STORE FOR THE TAB, read by the frame (which hands the shape to the
 * kit's `AppShell`) and written by the fold control and `Mod+\`.
 */

import { readerStore } from "~/lib/readerStore.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";

/** The two shapes the sidebar takes above the shell breakpoint. */
export type RailShape = "full" | "compact";

/**
 * STORAGE IS UNTRUSTED INPUT: anything but the one word that folds the rail
 * is the full sidebar, which is the shape every reader starts from.
 */
function railShapeOf(raw: unknown): RailShape {
  return raw === "compact" ? "compact" : "full";
}

const store = readerStore<RailShape>(STORAGE_KEYS.railShape, "full", railShapeOf);

/** The shape this reader chose, re-rendering when it changes. */
export function useRailShape(): RailShape {
  return store.use();
}

/** Keep `shape` as this reader's choice. */
export function setRailShape(shape: RailShape): void {
  store.write(shape);
}

/** Fold the sidebar, or unfold it. */
export function toggleRailShape(): void {
  store.update((shape) => (shape === "compact" ? "full" : "compact"));
}
