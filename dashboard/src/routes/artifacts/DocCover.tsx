/**
 * A doc's gallery cover: the opening of its first tab, drawn natively at card
 * size from the version's outline by the same renderer the doc is read with,
 * never a screenshot (D64). Pictures are left as their alt text, so a gallery
 * of fifty docs fetches fifty small outlines and nothing else, and links as
 * their words: the whole card is one link, which may hold no second one.
 */

import { DocCover as Cover } from "@crewlethq/ui";
import type { ArtifactListItem, ArtifactOutline } from "~/protocol/index.ts";
import { useRest } from "~/lib/useRest.ts";
import { readOutline } from "~/lib/artifacts/api.ts";
import { renderMarkdown } from "~/lib/markdown.ts";

const noImages = () => null;

export function DocCover({ item }: { item: Pick<ArtifactListItem, "id" | "head_number"> }) {
  const outline = useRest<ArtifactOutline>(
    item.head_number > 0 ? `outline:${item.id}:${item.head_number}` : null,
    (signal) => readOutline(item.id, item.head_number, signal),
  );
  const cover = outline.data?.doc?.cover;
  if (!cover?.markdown) return null;
  return <Cover>{renderMarkdown(cover.markdown, { image: noImages, links: "text" })}</Cover>;
}
