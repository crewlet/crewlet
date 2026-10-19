/**
 * A deck's gallery cover: its cover slide drawn natively at card size from the
 * version's outline, never a screenshot (D64), and how many slides it holds.
 * Pictures are left as their sized placeholders, so a gallery of fifty decks
 * fetches fifty small outlines and nothing else.
 */

import { useId } from "react";
import { SlideStage } from "@crewlethq/ui";
import type { ArtifactListItem, ArtifactOutline } from "~/protocol/index.ts";
import { useRest } from "~/lib/useRest.ts";
import { readOutline } from "~/lib/artifacts/api.ts";
import { renderSlide } from "~/lib/slides.tsx";

const noImages = () => null;

export function SlidesCover({
  item,
}: {
  item: Pick<ArtifactListItem, "id" | "title" | "head_number">;
}) {
  const outline = useRest<ArtifactOutline>(
    item.head_number > 0 ? `outline:${item.id}:${item.head_number}` : null,
    (signal) => readOutline(item.id, item.head_number, signal),
  );
  const prefix = useId().replace(/:/g, "");
  const slides = outline.data?.slides;
  if (!slides) return null;
  return (
    <span className="slides-cover">
      <SlideStage label={`${item.title}, cover`}>
        {renderSlide(slides.cover_slide, { prefix, image: noImages })}
      </SlideStage>
      <span className="slides-cover-count">
        {slides.count} {slides.count === 1 ? "slide" : "slides"}
      </span>
    </span>
  );
}
