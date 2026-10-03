/**
 * Every key this dashboard keeps in the browser's storage, in one table.
 *
 * # Why a registry rather than a constant beside each reader
 *
 * Browser storage outlives every build. A key a module stops reading is not
 * deleted by deleting the module: it sits in every reader's profile forever,
 * and the next module to pick a similar name reads somebody else's stale value
 * as its own. The rail's `crewlet.rail.collapsed` is the case that made this a
 * table — the rail it collapsed is gone, and nothing but a list of every key
 * would have said so.
 *
 * So a key is declared HERE, with who owns it and what it holds, and a reader
 * imports it by name. `storage.test.ts` holds the other half: no source file
 * outside this one spells a `crewlet_…` or `crewlet.…` key literal, so a new
 * key cannot be added without being written down.
 *
 * AND A KEY NOTHING READS ANY MORE IS DELETED, not left behind. It moves to
 * [RETIRED_STORAGE_KEYS], and `main.tsx` removes each at boot — so a stale
 * value leaves a reader's profile the first time they open a build that no
 * longer reads it, and a later module that picks the same name starts from
 * nothing rather than from somebody else's value.
 *
 * # What storage is for in this product
 *
 * Per-browser conveniences and nothing else: a token, a theme, the stars and
 * recents a reader keeps, the width they dragged a panel to, an org draft not
 * yet saved. Nothing the COMPANY agrees on lives here — that is the engine's —
 * and every reader wraps each access in a try/catch, because a private window
 * throws on the accessor and a cleared profile answers nothing.
 */

export const STORAGE_KEYS = {
  /** The API token this browser presents (`protocol/authToken.ts`). */
  apiToken: "crewlet_api_token",
  /** Light, dark or the system's (`lib/prefs.ts`). */
  theme: "crewlet_theme",
  /** Compact, normal or comfortable (`lib/prefs.ts`). */
  density: "crewlet_density",
  /** The zone timestamps are drawn in; empty is the browser's (`lib/prefs.ts`). */
  timezone: "crewlet_timezone",
  /** How a date is written (`lib/prefs.ts`). */
  dateFormat: "crewlet_date_format",
  /** Objects this reader opened, for the palette (`lib/recents.ts`). */
  recents: "crewlet_recents",
  /** Pages this reader starred, for the sidebar (`lib/starred.ts`). */
  starred: "crewlet_starred",
  /** The width the reader dragged the detail rail to (`app/frame/DetailRail.tsx`). */
  peekWidth: "crewlet.peek.width",
  /** An org draft not yet saved (`routes/org/builder/model/persistence.ts`). */
  orgDraft: "crewlet_org_draft",
  /** The folders open in the Knowledge tree, for this tab (`routes/knowledge/KnowledgeTree.tsx`). */
  knowledgeOpen: "crewlet.knowledge.open",
} as const;

export type StorageKey = (typeof STORAGE_KEYS)[keyof typeof STORAGE_KEYS];

/**
 * Keys an earlier build wrote and this one never reads, removed at boot.
 *
 * `crewlet.rail.collapsed` was whether the app rail was folded to its icons;
 * the rail is gone — the sidebar is the kit's `AppShell`, which is a drawer
 * below its breakpoint and never folds.
 */
export const RETIRED_STORAGE_KEYS: readonly string[] = ["crewlet.rail.collapsed"];

/**
 * Remove every retired key. NEVER THROWS: a private window, blocked site data
 * or a sandboxed frame makes the accessor itself throw, and a stale key is not
 * a reason for the dashboard not to boot.
 */
export function forgetRetiredKeys(storage: () => Pick<Storage, "removeItem">): void {
  try {
    const store = storage();
    for (const key of RETIRED_STORAGE_KEYS) store.removeItem(key);
  } catch {
    // Storage is refused; there is nothing to clean and nothing to stale.
  }
}
