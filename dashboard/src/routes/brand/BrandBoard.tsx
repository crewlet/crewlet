/**
 * A brand as a page draws it: the four cards in the main column and the
 * preview beside them, stacked under the cards on a narrow page.
 *
 * ONE LAYOUT FOR EVERY BRAND A PAGE SHOWS: the published brand on Company ›
 * Brand (each card changed in place by an administrator), a draft nobody
 * published yet on the same page, and one version of a kit on its own page.
 * The board draws; whoever mounts it decides which cards take a change
 * (`edit`), and a board handed none is read-only.
 */

import { AsideLayout, HeadingLevelProvider } from "@crewlethq/ui";
import type { BrandDocument } from "~/protocol/index.ts";
import type { BrandAssets } from "~/lib/brand/useBrand.ts";
import {
  BrandCard,
  ColoursBody,
  LogosBody,
  TypographyBody,
  VoiceBody,
  type BrandCardEdit,
  type BrandCardKey,
} from "./BrandCards.tsx";
import { BrandPreview, type BrandPreviewDrawn } from "./BrandPreview.tsx";

export function BrandBoard({
  document,
  assets,
  drawn,
  edit,
}: {
  document: BrandDocument;
  assets: BrandAssets;
  /** What the preview draws, which its note says. */
  drawn: BrandPreviewDrawn;
  /** How each card is changed, where the reader may change it. */
  edit?: (card: BrandCardKey) => BrandCardEdit | undefined;
}) {
  return (
    <AsideLayout
      asideLabel="Preview"
      aside={<BrandPreview document={document} assets={assets} drawn={drawn} />}
    >
      {/* ONE LEVEL UNDER THE PAGE'S TITLE: every card's title heads a part of
          the page the title names. */}
      <HeadingLevelProvider level={3}>
        <BrandCard card="logos" edit={edit?.("logos")}>
          <LogosBody document={document} assets={assets} />
        </BrandCard>
        <BrandCard card="colours" edit={edit?.("colours")}>
          <ColoursBody colors={document.colors} />
        </BrandCard>
        <BrandCard card="type" edit={edit?.("type")}>
          <TypographyBody type={document.type} assets={assets} />
        </BrandCard>
        <BrandCard card="voice" edit={edit?.("voice")}>
          <VoiceBody voice={document.voice} />
        </BrandCard>
      </HeadingLevelProvider>
    </AsideLayout>
  );
}
