/**
 * The edit session's controls, in the page bar where Edit Home was: editing
 * adds nothing to the page itself, and the hatch under the board says what
 * mode this is. A live dot says how much is unsaved; Undo, Redo, Add widgets,
 * Cancel and Done follow it.
 */

import { Button, IconButton, StatusDot } from "@crewlethq/ui";
import { LayoutGridGlyph, RedoGlyph, UndoGlyph } from "@crewlethq/icons/glyphs";

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
        {!changed
          ? "Editing layout"
          : steps === 1
            ? "1 unsaved change"
            : `${steps} unsaved changes`}
      </span>
      <span className="home-editbar-sep" aria-hidden="true" />
      <IconButton
        size="sm"
        variant="ghost"
        label="Undo"
        icon={<UndoGlyph size="sm" />}
        {...(steps === 0 ? { disabledReason: "Nothing to undo." } : {})}
        onClick={onUndo}
      />
      <IconButton
        size="sm"
        variant="ghost"
        label="Redo"
        icon={<RedoGlyph size="sm" />}
        {...(canRedo ? {} : { disabledReason: "Nothing to redo." })}
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
      <Button variant="ghost" size="small" onClick={onCancel}>
        Cancel
      </Button>
      <Button variant="primary" size="small" className="home-editbar-done" onClick={onDone}>
        Done
      </Button>
    </div>
  );
}
