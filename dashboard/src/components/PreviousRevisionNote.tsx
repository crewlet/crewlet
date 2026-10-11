/**
 * What a read screen says while this node is still applying the revision this
 * tab saved: the org projection it draws from is the previous one.
 *
 * On the org chart, the units and the charter rather than in the builder,
 * because that is where somebody goes to look at what they just changed, and
 * a chart that has not moved yet is exactly what would otherwise read as a
 * save that did nothing.
 *
 * A MODULE OF ITS OWN, out of the builder's after-save strip: the strip is the
 * console's — it reads every node's apply status, an admin's answer — and the
 * screens that draw this note are the person shell's, which may not reach a
 * console module (`app/shell.test.ts`). What this note reads is the health
 * push, which every reader is sent, and the tab's own record of its save.
 */

import { Callout, InlineCode } from "@crewlethq/ui";
import { RotateCwGlyph } from "@crewlethq/icons/glyphs";
import { useEngineHealth } from "~/lib/store-hooks.ts";
import { useSavedRevision } from "~/lib/savedRevision.ts";

/** The first characters of a revision id, as the console's Configuration shows one. */
export const shortRevision = (id: string) => id.slice(0, 10);

export function PreviousRevisionNote() {
  const saved = useSavedRevision();
  // THE HEALTH PUSH, which carries this node's applied epoch every five
  // seconds to every tab — there is nothing to poll. This note lives for as
  // long as the node has not applied the revision, which can be for ever when
  // the node refuses it, so a poller of its own would be paid for the life of
  // the tab.
  const health = useEngineHealth();
  if (!saved || saved.epoch === null) return null;
  if ((health?.applied_epoch ?? 0) >= saved.epoch) return null;
  return (
    <Callout variant="neutral" icon={<RotateCwGlyph />}>
      This node is still applying revision{" "}
      <InlineCode>{shortRevision(saved.revisionId)}</InlineCode>, so what is drawn below is the
      revision before it.
    </Callout>
  );
}
