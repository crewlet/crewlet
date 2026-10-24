/**
 * The one related place a page points at: the kit's `RelatedRow`, for a
 * reader who may open it.
 *
 * The row is uilet's (a quiet linked card under the page's content, its glyph
 * under the card heads' glyphs and its words under their titles). What stays
 * here is the access gate: a row to a page the reader would be refused draws
 * nothing (the rule `ServiceNotice` keeps for Integrations), because a link to
 * a lock is a dead end offered as a way on.
 */

import type { ReactNode } from "react";
import { RelatedRow as KitRelatedRow } from "@crewlethq/ui";
import { closedBy } from "~/app/nav.ts";
import { href } from "~/app/router.tsx";
import { useHeldReading } from "~/lib/viewer.ts";

export function RelatedRow({
  path,
  icon,
  title,
  description,
  className,
}: {
  /** Where the row goes, as an address. */
  path: readonly string[];
  /** The destination's own glyph, at the small step. */
  icon: ReactNode;
  /** The place, by its name. */
  title: string;
  /** One sentence: what is there, and where. */
  description: ReactNode;
  /** Where the page places the row, for a page whose blocks keep no gap. */
  className?: string;
}) {
  const held = useHeldReading();
  if (held === null || closedBy(path, held.grants) !== null) return null;
  return (
    <KitRelatedRow
      href={href(path)}
      icon={icon}
      title={title}
      description={description}
      className={className}
    />
  );
}
