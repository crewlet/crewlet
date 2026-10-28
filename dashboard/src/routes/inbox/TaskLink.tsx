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
 * ITS KEY IS ITS WORDS, in mono and with no glyph, as a key in the row's
 * words is (`components/InlineToken.tsx`): a mark before a key says nothing
 * its letters do not, and the marks in a line are kept for what lives
 * outside the company.
 */

import { Link, peekClickHandler } from "@crewlethq/ui";
import { peekHref, usePeekControls } from "~/app/frame/peekAddress.ts";
import type { ObjectRef } from "~/app/frame/objects.ts";
import { itemAddress, type ItemRef } from "~/lib/work.ts";

export function TaskLink({
  item,
  label,
  title,
}: {
  /** What it opens ([itemAddress]). */
  item: ItemRef;
  /** The key it is named by, or "": a turn's task is addressed by id alone. */
  label: string;
  title?: string | undefined;
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
      <span className="mono">{label || "Task"}</span>
      {title ? ` ${title}` : ""}
    </Link>
  );
}
