/**
 * A design's gallery cover: a schematic of its boards where the canvas places
 * them, the cover board titled, drawn from the version's outline and never a
 * screenshot (D64). No board is framed, so a gallery of fifty designs runs no
 * page at all.
 */

import { ArtifactPreviewGlyph, CanvasSchematic } from "@crewlethq/ui";
import type { ArtifactListItem, ArtifactOutline } from "~/protocol/index.ts";
import { useRest } from "~/lib/useRest.ts";
import { readOutline } from "~/lib/artifacts/api.ts";

export function DesignCover({
  item,
}: {
  item: Pick<ArtifactListItem, "id" | "title" | "head_number">;
}) {
  const outline = useRest<ArtifactOutline>(
    item.head_number > 0 ? `outline:${item.id}:${item.head_number}` : null,
    (signal) => readOutline(item.id, item.head_number, signal),
  );
  const design = outline.data?.design;
  if (!design) return <ArtifactPreviewGlyph kind="design" />;
  return (
    <span className="design-cover">
      <CanvasSchematic
        label={`${item.title}, ${design.count} ${design.count === 1 ? "board" : "boards"}`}
        boards={design.boards.map((b) => ({ ...b, id: b.path }))}
        cover={design.cover}
      />
    </span>
  );
}
