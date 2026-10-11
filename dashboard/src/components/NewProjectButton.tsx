/**
 * "New project", the Projects lens's own action, in its page bar and in its
 * empty state.
 *
 * It opens the New project dialog (`routes/work/NewProject.tsx`), which hands
 * the reader to Edit org: the org is what makes a project, and saving there
 * needs an operator credential. So unlike New task, whose sheet is where a
 * reader who cannot file is told why, this door is HELD for a viewer without
 * that credential, saying so, exactly as the switcher's footer holds it: the
 * dialog behind it would lead somewhere they could not finish.
 */

import { Button } from "@crewlethq/ui";
import { PlusGlyph } from "@crewlethq/icons/glyphs";
import { NEW_PROJECT_HELD, useOpenNewProject } from "~/app/header/ProjectSwitcher.tsx";
import { useViewer } from "~/lib/viewer.ts";

export function NewProjectButton() {
  const open = useOpenNewProject();
  const viewer = useViewer();
  return (
    <Button
      size="small"
      variant="primary"
      leadingIcon={<PlusGlyph size="sm" />}
      disabledReason={viewer.operator ? undefined : NEW_PROJECT_HELD}
      onClick={open}
    >
      New project
    </Button>
  );
}
