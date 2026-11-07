/**
 * The edit session's controls, in the page bar where Edit Home was: editing
 * adds nothing to the page itself, and the hatch under the board says what
 * mode this is. A live dot says how much is unsaved; Undo, Redo, Add widgets,
 * Cancel and Done follow it.
 */

import { Button, IconButton, StatusDot } from "@crewlethq/ui";
import { LayoutGridGlyph, RedoGlyph, UndoGlyph } from "@crewlethq/icons/glyphs";
import type { PageMenuEntry } from "~/app/header/PageHeader.tsx";

export interface EditBarProps {
  /** Whether the draft differs from the saved Home. */
  changed: boolean;
  /** Steps Undo can take back. */
  steps: number;
  canRedo: boolean;
  onUndo: () => void;
  onRedo: () => void;
  onAdd: () => void;
  onCancel: () => void;
  onDone: () => void;
}

const HINT = "Drag to move. Drag a corner or edge to resize. Links open in a new tab.";
const NOTHING_TO_UNDO = "Nothing to undo.";
const NOTHING_TO_REDO = "Nothing to redo.";

/** How much is unsaved, in words. */
function editState(changed: boolean, steps: number): string {
  if (!changed) return "Editing layout";
  return steps === 1 ? "1 unsaved change" : `${steps} unsaved changes`;
}

/**
 * The tools a phone's bar folds into its More menu while Home is edited
 * (`usePageMenu`): the same actions, labels and reasons the inline ones carry.
 */
export function editMenu({
  steps,
  canRedo,
  onUndo,
  onRedo,
  onAdd,
}: Pick<EditBarProps, "steps" | "canRedo" | "onUndo" | "onRedo" | "onAdd">): PageMenuEntry[] {
  return [
    {
      key: "undo",
      label: "Undo",
      icon: <UndoGlyph size="sm" />,
      onSelect: onUndo,
      disabled: steps === 0,
      ...(steps === 0 ? { description: NOTHING_TO_UNDO } : {}),
    },
    {
      key: "redo",
      label: "Redo",
      icon: <RedoGlyph size="sm" />,
      onSelect: onRedo,
      disabled: !canRedo,
      ...(canRedo ? {} : { description: NOTHING_TO_REDO }),
    },
    {
      key: "add",
      label: "Add widgets",
      icon: <LayoutGridGlyph size="sm" />,
      onSelect: onAdd,
    },
  ];
}

export function EditBar({
  changed,
  steps,
  canRedo,
  onUndo,
  onRedo,
  onAdd,
  onCancel,
  onDone,
}: EditBarProps) {
  return (
    <div className="home-editbar" role="group" aria-label="Editing Home">
      <span className="home-editbar-state" title={HINT}>
        <StatusDot tone="info" pulse />
        {/* ON A PHONE THE DOT AND THE COUNT, so Cancel and Done stay in view. */}
        <span className="page-wide-words">{editState(changed, steps)}</span>
        {changed && (
          <span className="page-phone-only" aria-hidden="true">
            {steps}
          </span>
        )}
      </span>
      {/* ON A PHONE THESE FOLD INTO THE BAR'S MORE (`editMenu`), never into a
          run that scrolls Done past the edge. */}
      <span className="page-action-folds">
        <span className="home-editbar-tools">
          <span className="home-editbar-sep" aria-hidden="true" />
          <IconButton
            size="sm"
            variant="ghost"
            label="Undo"
            icon={<UndoGlyph size="sm" />}
            {...(steps === 0 ? { disabledReason: NOTHING_TO_UNDO } : {})}
            onClick={onUndo}
          />
          <IconButton
            size="sm"
            variant="ghost"
            label="Redo"
            icon={<RedoGlyph size="sm" />}
            {...(canRedo ? {} : { disabledReason: NOTHING_TO_REDO })}
            onClick={onRedo}
          />
          <span className="home-editbar-sep" aria-hidden="true" />
          <Button
            variant="ghost"
            size="small"
            leadingIcon={<LayoutGridGlyph size="sm" />}
            onClick={onAdd}
          >
            Add widgets
          </Button>
        </span>
      </span>
      <Button variant="ghost" size="small" onClick={onCancel}>
        Cancel
      </Button>
      <Button variant="primary" size="small" className="home-editbar-done" onClick={onDone}>
        Done
      </Button>
    </div>
  );
}
