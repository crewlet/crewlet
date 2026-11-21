/** Re-queue: a change a merge queue took out, added again as it was to land. */

import { RotateCcwGlyph } from "@crewlethq/icons/glyphs";
import { WriteButton } from "~/components/WriteButton.tsx";
import { changeRef } from "~/lib/code.ts";
import type { QueuePlace } from "~/lib/checks.ts";
import { useAct } from "~/lib/useAct.ts";
import type { CodeEjectedRow } from "~/protocol/index.ts";

/** Held, never hidden: where the fleet does not say how it was to land, or it is queued again. */
export function Requeue({
  ejected,
  queued,
}: {
  ejected: CodeEjectedRow;
  /** Every live entry loaded, to tell one queued again. */
  queued: readonly QueuePlace[];
}) {
  const write = useAct("code_merge_change");
  const ref = changeRef(ejected.repo, ejected.number);
  const again = queued.find((p) => p.row.change_id === ejected.change_id);
  const blocked = !ejected.method
    ? "This fleet does not say how it was to land: add it to the queue from its page."
    : again
      ? `It is in the queue again, at #${again.row.place}.`
      : undefined;
  return (
    <WriteButton
      write={write}
      size="small"
      variant="secondary"
      leadingIcon={<RotateCcwGlyph size="sm" />}
      aria-label={`Add ${ref} to the queue again`}
      blocked={blocked}
      onPress={() =>
        void write.run(
          {
            change: ref,
            expected_head: ejected.head_oid,
            method: ejected.method!,
            delete_branch: ejected.delete_source === true,
            close_task: ejected.close_task === true,
          },
          { done: `Added ${ref} to the queue again` },
        )
      }
    >
      Re-queue
    </WriteButton>
  );
}
