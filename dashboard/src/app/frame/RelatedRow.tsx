/**
 * The one related place a page points at, as a quiet linked row under its
 * content: Company › Policies to Settings › Limits, Settings › Nodes to
 * Backups & retention.
 *
 * A ROW, NOT A FOOTNOTE. A trailing caption ("The caps ... are in Settings ›
 * Limits.") was a sentence a reader had to find the link inside, set apart
 * from everything above it. The row stands on a card's own inset, so its
 * glyph stands under the card heads' glyphs and its words under their titles,
 * and the whole row is the link.
 *
 * FOR A READER WHO MAY OPEN IT. A row to a page the reader would be refused
 * draws nothing (the rule `ServiceNotice` keeps for Integrations): a link to
 * a lock is a dead end offered as a way on.
 *
 * Composed of the kit's Card, Inline, Stack and Text only, so it carries no
 * visual of its own.
 */

import type { ReactNode } from "react";
import { Card, Inline, Spacer, Stack, Text } from "@crewlethq/ui";
import { ChevronRightGlyph } from "@crewlethq/icons/glyphs";
import { closedBy } from "~/app/nav.ts";
import { href } from "~/app/router.tsx";
import { useHeldReading } from "~/lib/viewer.ts";

export function RelatedRow({
  path,
  icon,
  title,
  description,
}: {
  /** Where the row goes, as an address. */
  path: readonly string[];
  /** The destination's own glyph, at the small step. */
  icon: ReactNode;
  /** The place, by its name. */
  title: string;
  /** One sentence: what is there, and where. */
  description: ReactNode;
}) {
  const held = useHeldReading();
  if (held === null || closedBy(path, held.grants) !== null) return null;
  return (
    <nav aria-label="Related">
      <Card href={href(path)} interactive padding="sm">
        <Inline gap={3}>
          <span className="muted">{icon}</span>
          <Stack gap={1}>
            <Text variant="cell">{title}</Text>
            <Text variant="caption">{description}</Text>
          </Stack>
          <Spacer />
          <span className="muted">
            <ChevronRightGlyph size="sm" />
          </span>
        </Inline>
      </Card>
    </nav>
  );
}
