/**
 * A screen's "New task", in its page bar — the primary action every work
 * screen ends its bar with, as the approved designs draw it.
 *
 * It OPENS THE SHEET and is not itself a write, so it is never disabled: the
 * sheet's Create is the write control, and it is where a reader who cannot
 * file is told why (`components/WriteButton.tsx`). A door that went dead for
 * an anonymous reader would hide the one place that says what would let them.
 *
 * What the screen knows about the task goes with it — a project's page knows
 * its project — and nothing else: see `app/newTask.ts`.
 */

import { Button } from "@crewlethq/ui";
import { PlusGlyph } from "@crewlethq/icons/glyphs";
import { useOpenNewTask, type NewTaskPreset } from "~/app/newTask.ts";

export function NewTaskButton({ preset }: { preset?: NewTaskPreset }) {
  const open = useOpenNewTask();
  return (
    <Button
      size="small"
      variant="primary"
      leadingIcon={<PlusGlyph size="sm" />}
      onClick={() => open(preset)}
    >
      New task
    </Button>
  );
}
