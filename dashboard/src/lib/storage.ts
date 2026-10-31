/**
 * Every key this dashboard keeps in the browser's storage, in one table.
 *
 * # Why a registry rather than a constant beside each reader
 *
 * Browser storage outlives every build. A key a module stops reading is not
 * deleted by deleting the module: it sits in every reader's profile forever,
 * and the next module to pick a similar name reads somebody else's stale value
 * as its own. Only a list of every key says which ones a build still reads.
 *
 * So a key is declared HERE, with who owns it and what it holds, and a reader
 * imports it by name. `storage.test.ts` holds the other half: no source file
 * outside this one spells a `crewlet_…` or `crewlet.…` key literal, so a new
 * key cannot be added without being written down.
 *
 * AND A KEY NOTHING READS ANY MORE IS DELETED, not left behind. When a build
 * stops reading one of these keys it moves to [RETIRED_STORAGE_KEYS], and
 * `main.tsx` removes each at boot — so a stale value leaves a reader's profile
 * the first time they open a build that no longer reads it, and a later module
 * that picks the same name starts from nothing rather than from somebody
 * else's value.
 *
 * # What storage is for in this product
 *
 * Per-browser conveniences and nothing else: a mode and a theme, the stars and
 * recents a reader keeps, the width they dragged a panel to, an org draft not
 * yet saved, and which person a tab was last read by. Nothing the COMPANY
 * agrees on lives here (that is the engine's), and every reader wraps each
 * access in a try/catch, because a private window throws on the accessor and a
 * cleared profile answers nothing.
 *
 * AND NEVER A CREDENTIAL. The session is a cookie no script can read, and a
 * bearer kept here is one any script injected into the page reads too.
 *
 * A KEY MAY BE A PREFIX, for a list kept PER READER (`lib/recents.ts`,
 * `lib/starred.ts`): the reader's id follows it, so one browser shared by two
 * people holds two lists, and the sign-in that hands the browser on deletes
 * every one but the signing-in person's (`lib/reader.ts`).
 */

export const STORAGE_KEYS = {
  /** The mode: light, dark or the system's (`lib/prefs.ts`). */
  theme: "crewlet_theme",
  /**
   * The theme, one of `themeRegistry`'s ids (`lib/prefs.ts`); anything else
   * reads as no choice, which is the default theme.
   */
  palette: "crewlet_palette",
  /** Compact, normal or comfortable (`lib/prefs.ts`). */
  density: "crewlet_density",
  /**
   * The PREFIX of each reader's list of objects they opened, for the palette
   * (`lib/recents.ts`): the reader's id follows it.
   */
  recents: "crewlet_recents/",
  /**
   * The PREFIX of each reader's starred pages, for the sidebar
   * (`lib/starred.ts`): the reader's id follows it.
   */
  starred: "crewlet_starred/",
  /**
   * The PREFIX of each reader's Home board, the widgets they placed and how
   * they arranged them (`routes/home/board/model.ts`): the reader's id
   * follows it.
   */
  homeBoard: "crewlet_home_board/",
  /**
   * Which principal this TAB was last read by, in `sessionStorage` rather
   * than `localStorage` — it describes one tab, and goes with everything else
   * the tab holds (`lib/reader.ts`).
   */
  reader: "crewlet_reader",
  /** The width the reader dragged the detail rail to (`app/frame/DetailRail.tsx`). */
  peekWidth: "crewlet.peek.width",
  /** An org draft not yet saved (`routes/org/builder/model/persistence.ts`). */
  orgDraft: "crewlet_org_draft",
  /**
   * The PREFIX of a change's drafted findings, in `sessionStorage`: the
   * change's id and the revision follow it (`lib/reviewDrafts.ts`). The tab's
   * until a review sends them, and gone with it.
   */
  reviewDrafts: "crewlet.code.review_drafts/",
  /** How the Artifacts gallery is drawn, `grid` or `list` (`routes/artifacts/Gallery.tsx`). */
  artifactsShape: "crewlet.artifacts.shape",
} as const;

export type StorageKey = (typeof STORAGE_KEYS)[keyof typeof STORAGE_KEYS];

/**
 * Keys a released build wrote and this one never reads, removed at boot.
 * Empty until a build stops reading a key a release declared in
 * [STORAGE_KEYS]; that build moves the key here, with what it held.
 */
export const RETIRED_STORAGE_KEYS: readonly string[] = [];

/**
 * Remove every retired key — `retired` is [RETIRED_STORAGE_KEYS] at boot, and
 * a parameter so the sweep can be held to what it removes while the list is
 * empty. NEVER THROWS: a private window, blocked site data or a sandboxed frame
 * makes the accessor itself throw, and a stale key is not a reason for the
 * dashboard not to boot.
 */
export function forgetRetiredKeys(
  storage: () => Pick<Storage, "removeItem">,
  retired: readonly string[] = RETIRED_STORAGE_KEYS,
): void {
  try {
    const store = storage();
    for (const key of retired) store.removeItem(key);
  } catch {
    // Storage is refused; there is nothing to clean and nothing to stale.
  }
}
