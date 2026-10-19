/**
 * Two versions of a deck, compared slide by slide: the History tab's By slide
 * lens. Both versions' canonical trees are read, paired by slide id
 * (`lib/artifacts/deckCompare.ts`) and drawn side by side by uilet's
 * `SlideCompare`, each version's pictures from its own files.
 */

import { useId } from "react";
import { Skeleton, SlideCompare } from "@crewlethq/ui";
import type { ArtifactDeckAnswer, ArtifactDetail } from "~/protocol/index.ts";
import { QueryState } from "~/components/common.tsx";
import { useRest } from "~/lib/useRest.ts";
import { readDeck } from "~/lib/artifacts/api.ts";
import { compareDecks } from "~/lib/artifacts/deckCompare.ts";
import { useDeckAssets } from "~/lib/artifacts/deckAssets.ts";
import { renderSlide } from "~/lib/slides.tsx";

export function SlidesCompare({
  detail,
  older,
  newer,
}: {
  detail: ArtifactDetail;
  older: number;
  newer: number;
}) {
  const from = useRest<ArtifactDeckAnswer>(`deck:${detail.id}:${older}`, (signal) =>
    readDeck(detail.id, older, signal),
  );
  const to = useRest<ArtifactDeckAnswer>(`deck:${detail.id}:${newer}`, (signal) =>
    readDeck(detail.id, newer, signal),
  );
  const fromImage = useDeckAssets(detail.id, older, from.data?.deck ?? null);
  const toImage = useDeckAssets(detail.id, newer, to.data?.deck ?? null);
  const prefix = useId().replace(/:/g, "");
  if (!from.data || !to.data) {
    const failed = from.data ? to : from;
    return failed.loading ? (
      <Skeleton variant="text" rows={4} label="Loading both versions" />
    ) : (
      <QueryState error={failed.code} refusal={failed.refusal} loading={false} />
    );
  }
  const rows = compareDecks(from.data.deck, to.data.deck).map((r) => ({
    key: r.id,
    title: r.title,
    change: r.change,
    moved: r.moved,
    ...(r.fromPosition !== undefined ? { fromPosition: r.fromPosition } : {}),
    ...(r.toPosition !== undefined ? { toPosition: r.toPosition } : {}),
    ...(r.from
      ? { from: renderSlide(r.from, { prefix: `${prefix}-f-${r.id}-`, image: fromImage }) }
      : {}),
    ...(r.to ? { to: renderSlide(r.to, { prefix: `${prefix}-t-${r.id}-`, image: toImage }) } : {}),
  }));
  return <SlideCompare rows={rows} fromLabel={`Version ${older}`} toLabel={`Version ${newer}`} />;
}
