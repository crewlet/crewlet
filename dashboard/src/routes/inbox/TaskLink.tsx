/**
 * The task a row or a byline is about, as a link that opens it beside the
 * Inbox.
 *
 * A REAL LINK EITHER WAY: its `href` is the task's own page, so a middle click
 * or a modified one opens a tab, and a plain click opens the task in the side
 * panel instead (`rowPeekHandler`), because the inbox is where the reader is
 * and sending them away to read one task is the navigation every tracker
 * learned not to make.
 *
 * QUIET, because it sits under every row: a column of full accent links reads
 * as a column of links rather than of things to read.
 */

import { Link } from "@crewlethq/ui";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import type { ObjectRef } from "~/app/frame/objects.ts";

export function TaskLink({ item, title }: { item: string; title?: string | undefined }) {
  const peek = usePeekControls();
  const ref: ObjectRef = { kind: "item", id: item };
  return (
    <Link
      href={peekHref(ref)}
      variant="quiet"
      size="inherit"
      onClick={rowPeekHandler(() => peek.open(ref))}
    >
      <span className="mono">{item}</span>
      {title ? ` ${title}` : ""}
    </Link>
  );
}
