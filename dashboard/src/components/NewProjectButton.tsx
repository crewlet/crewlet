/**
 * "New project", the Projects lens's own action, in its page bar and in its
 * empty state.
 *
 * It opens the New project dialog (`routes/worklet/NewProject.tsx`), which hands
 * the reader to Edit org: the org is what makes a project. Setting a project
 * key is Admin's alone, never a lead's: the key outlives
 * the team that declares it, so the engine refuses a lead every write that
 * sets one (`config.DiffOrg`'s keyed fields). So unlike New task, whose sheet
 * is where a reader who cannot file is told why, this door is HELD for a
 * reader Admin's gate refuses (`useConfigWriteAccess`), with the
 * gate's own sentence, exactly as the switcher's footer holds it: the dialog
 * behind it would lead somewhere they could not finish.
 */

import { Button } from "@crewlethq/ui";
import { PlusGlyph } from "@crewlethq/icons/glyphs";
import { useOpenNewProject } from "~/app/header/ProjectSwitcher.tsx";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";

export function NewProjectButton() {
  const open = useOpenNewProject();
  const access = useConfigWriteAccess();
  return (
    <Button
      size="small"
      variant="primary"
      leadingIcon={<PlusGlyph size="sm" />}
      disabledReason={access.can ? undefined : access.reason}
      onClick={open}
    >
      New project
    </Button>
  );
}
