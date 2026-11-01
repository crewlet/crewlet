/**
 * The task a row or a byline is about, as a link that opens it beside the
 * Inbox.
 *
 * A REAL LINK EITHER WAY: its `href` is the task's own page, so a middle click
 * or a modified one opens a tab, and a plain click opens the task in the side
 * panel instead (`peekClickHandler`), because the inbox is where the reader is
 * and sending them away to read one task is the navigation every tracker
 * learned not to make.
 *
 * QUIET, because it sits under every row: a column of full accent links reads
 * as a column of links rather than of things to read.
 *
 * NAMED BY ITS KEY, OPENED AT ITS ADDRESS ([itemAddress]): a key another task
 * claimed first opens that task, so a row about the task that did not claim it
 * would open the claimant's page and thread from a duplicate's notice.
 *
 * A ROW'S TASK CARRIES THE TASK GLYPH (`glyph`), as a key in the row's words
 * does (`components/InlineToken.tsx`), so the two read as the same thing; the
 * reading pane's byline is the frame's navigation and stays plain.
 */

import { InlineRef, Link, peekClickHandler } from "@crewlethq/ui";
import { SquareKanbanGlyph } from "@crewlethq/icons/glyphs";
import { peekHref, usePeekControls } from "~/app/frame/peekAddress.ts";
import type { ObjectRef } from "~/app/frame/objects.ts";
import { itemAddress, type ItemRef } from "~/lib/work.ts";

export function TaskLink({
  item,
  label,
  title,
  glyph = false,
}: {
  /** What it opens ([itemAddress]). */
  item: ItemRef;
  /** The key it is named by, or "": a turn's task is addressed by id alone. */
  label: string;
  title?: string | undefined;
  /** Draw the task glyph before the key, as a row does. */
  glyph?: boolean;
}) {
  const peek = usePeekControls();
  const ref: ObjectRef = { kind: "item", id: itemAddress(item) };
  return (
    <Link
      href={peekHref(ref)}
      variant="quiet"
      size="inherit"
      onClick={peekClickHandler(() => peek.open(ref))}
    >
      {glyph ? (
        <InlineRef mark={<SquareKanbanGlyph />} code={label !== ""}>
          {label || "Task"}
        </InlineRef>
      ) : (
        <span className="mono">{label || "Task"}</span>
      )}
      {title ? ` ${title}` : ""}
    </Link>
  );
}
