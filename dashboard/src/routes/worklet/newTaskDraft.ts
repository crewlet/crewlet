/**
 * The New task sheet's unsent draft, kept for the tab.
 *
 * A RELOAD KEEPS WHAT WAS TYPED, as a change's drafted findings are kept
 * (`lib/reviewDrafts.ts`): the sheet held its fields in memory alone, so a
 * reload mid-sentence lost the title and the description, and the sheet
 * reopened empty. Kept in `sessionStorage`, the tab's own: closing the tab
 * lets it go, and the hand-over that empties a tab's storage when somebody
 * else signs in (`lib/session.ts`) takes it with everything else, so a shared
 * browser never offers one person's half-written task to the next.
 *
 * ONE DRAFT, FOR THE DOOR IT WAS OPENED FROM. A draft begun from a board lane
 * (its project, status or assignee preset) or a seat's Message (`ask`) comes
 * back when the same door opens the sheet again; another door starts from
 * its own preset, since a question to one seat reopened as a plain task, or
 * in another project, is not what the reader opened.
 *
 * READ AND WRITTEN UNDER A TRY: a private window refuses storage, and the
 * sheet then keeps the draft for as long as it is open, as it always did.
 */

import type { NewTaskPreset } from "~/app/newTask.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import type { NewTaskDraft } from "./NewTask.tsx";

/** What identifies a door: every field of its preset, in one order. */
function doorOf(preset: NewTaskPreset): string {
  return JSON.stringify([
    preset.project ?? "",
    preset.status ?? "",
    preset.type ?? "",
    preset.priority ?? "",
    preset.assignee ?? "",
    [...(preset.labels ?? [])].sort(),
    preset.ask ?? "",
    preset.title ?? "",
    preset.description ?? "",
  ]);
}

/** The draft kept for this door, or null where none is kept (or storage refuses). */
export function keptDraft(preset: NewTaskPreset): NewTaskDraft | null {
  try {
    const raw = sessionStorage.getItem(STORAGE_KEYS.newTaskDraft);
    if (!raw) return null;
    const kept = JSON.parse(raw) as { door?: unknown; draft?: unknown };
    if (kept.door !== doorOf(preset) || !kept.draft || typeof kept.draft !== "object") {
      return null;
    }
    return kept.draft as NewTaskDraft;
  } catch {
    return null;
  }
}

/** Keep the draft for the door it was opened from, in place of any other. */
export function keepDraft(preset: NewTaskPreset, draft: NewTaskDraft): void {
  try {
    sessionStorage.setItem(
      STORAGE_KEYS.newTaskDraft,
      JSON.stringify({ door: doorOf(preset), draft }),
    );
  } catch {
    // Refused storage keeps nothing; the open sheet still holds the draft.
  }
}

/** Let the kept draft go: it was filed, discarded, or emptied. */
export function forgetDraft(): void {
  try {
    sessionStorage.removeItem(STORAGE_KEYS.newTaskDraft);
  } catch {
    // Nothing was kept where storage is refused.
  }
}
