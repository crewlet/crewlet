/**
 * Home's board, while the person arranges it, as the assistant reads it
 * (`Home.tsx`, `useAssistantDraft`).
 *
 * READ, NEVER SET. The board is a layout kept per reader in this browser,
 * and every change to it is a gesture on the board itself (a drag, the tray,
 * a widget's sheet) with Undo, Redo and Cancel around it. A field-by-field
 * change cannot say where a widget goes, so the assistant is told what the
 * board holds and that arranging it is the person's to do.
 */

import type { AssistantDraftSpec } from "~/app/assistant/draft.ts";
import type { Board } from "./model.ts";
import { widgetName } from "./settings.tsx";

/** What the assistant is told about the board, beside its fields. */
const HELP =
  "The person is arranging their Home board, kept in this browser. You can read which " +
  "widgets it holds and in what order; moving, adding, removing and configuring widgets " +
  "is done on the board itself, and Done keeps the arrangement.";

/** The board's widgets in order, a group with what it holds. */
export function boardLines(board: Board): string[] {
  return board.widgets.map((w) =>
    w.kind === "group"
      ? `${widgetName(w)} (${w.children.map(widgetName).join(", ") || "empty"})`
      : widgetName(w),
  );
}

/** The board being arranged, as the page declares it. */
export function homeBoardDraft(args: {
  board: Board;
  /** The widget whose settings sheet is open, by name, or null. */
  settings: string | null;
  changed: boolean;
}): AssistantDraftSpec<Board> {
  const { board, settings, changed } = args;
  return {
    kind: "client",
    step: settings === null ? "arrange" : "widget_settings",
    scope: "Your Home board (not saved)",
    help: HELP,
    value: board,
    fields: [
      { path: "widgets", label: "Widgets, in order", read: boardLines },
      { path: "settings", label: "Settings open for", read: () => settings ?? "None" },
      { path: "unsaved", label: "Unsaved changes", read: () => (changed ? "Yes" : "No") },
    ],
    // NOTHING HERE IS SET (see the file's doc): no apply, so the page takes
    // no draft.set at all.
  };
}
