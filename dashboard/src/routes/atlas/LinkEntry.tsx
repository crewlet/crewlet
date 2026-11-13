/**
 * Link an atlas entry to a task, from the task's own page. The entry is
 * picked from the reader's own read of the atlas, and the write is Worklet's
 * (`update_work_item`): the engine adds only an entry the reader can read,
 * and says so in its own words where it refuses.
 */

import { useState } from "react";
import { MapGlyph } from "@crewlethq/icons/glyphs";
import { useAct } from "~/lib/useAct.ts";
import { WriteDialog } from "./AddEntry.tsx";
import { EntryPicker } from "./pickers.tsx";

export function LinkEntryDialog({
  item,
  named,
  linked,
  onClose,
}: {
  /** The task's address, which the entry is linked to. */
  item: string;
  /** What the dialog's words call the task. */
  named: string;
  /** The entries the task already names, left out of the list. */
  linked: readonly string[];
  onClose: () => void;
}) {
  const write = useAct("update_work_item");
  const [picked, setPicked] = useState({ id: "", name: "" });
  return (
    <WriteDialog
      title={`Link an atlas entry to ${named}`}
      subtitle="An entry the task touches: what it changes, runs on or depends on. The entry's page then lists the task under Work."
      icon={<MapGlyph />}
      write={write}
      blocked={picked.id === "" ? "Pick an entry first" : undefined}
      submitLabel="Link entry"
      onClose={onClose}
      onSubmit={() =>
        write.run(
          { item, linked_entries: { add: [picked.id] } },
          { done: `Linked ${picked.name} to ${named}` },
        )
      }
    >
      <EntryPicker
        label="Entry"
        exclude={linked}
        value={picked.id}
        onChange={(id, summary) => setPicked({ id, name: summary?.name ?? "" })}
      />
    </WriteDialog>
  );
}
