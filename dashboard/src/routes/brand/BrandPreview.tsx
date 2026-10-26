/**
 * The preview beside a brand's cards: a sample doc or slide, in light or
 * dark, drawn from the brand as it stands (an administrator's edit while a
 * card is being changed, or a draft nobody published yet), and said to be a
 * sample rather than a real artifact. A draft's preview is tagged as one.
 *
 * Drawn here, in the page, with the brand's values scoped to the sample's own
 * box: nothing of the brand reaches the dashboard's chrome.
 */

import { useState } from "react";
import {
  BrandSample,
  Card,
  IconButton,
  SegmentedControl,
  Tag,
  Text,
  type BrandSampleKind,
  type BrandSampleMode,
} from "@crewlethq/ui";
import { MoonGlyph, SunGlyph } from "@crewlethq/icons/glyphs";
import type { BrandDocument } from "~/protocol/index.ts";
import { faceDrawing } from "~/lib/brand/brand.ts";
// THE BRAND'S OWN SHEET, imported here because every surface that draws a
// brand draws this preview or its sample: Company › Brand, a kit's view and a
// kit's gallery cover. It arrives with them rather than in the first paint.
import "~/styles/brand.css";
import type { BrandAssets } from "~/lib/brand/useBrand.ts";

/**
 * What the preview draws: the brand as published (or a kit's version as it
 * is), an edit not yet saved, or a saved draft nobody published yet.
 */
export type BrandPreviewDrawn = "brand" | "changes" | "draft";

/** The note under the sample, by what it draws. */
const NOTES: Readonly<Record<BrandPreviewDrawn, string>> = {
  brand: "A sample drawn with this brand, not a real artifact.",
  changes: "A sample drawn with your changes, not a real artifact.",
  draft: "A sample drawn with this draft, not a real artifact.",
};

const KINDS = [
  { value: "doc", label: "Doc" },
  { value: "slide", label: "Slide" },
] as const;

/** The sample for one brand, in one kind and mode: what the preview and a kit's cover both draw. */
export function BrandSampleOf({
  document,
  assets,
  kind,
  mode,
}: {
  document: BrandDocument;
  assets: BrandAssets;
  kind: BrandSampleKind;
  mode: BrandSampleMode;
}) {
  const heading = faceDrawing(document.type.heading, assets.face("heading")).stack;
  const body = faceDrawing(document.type.body, assets.face("body")).stack;
  return (
    <BrandSample
      kind={kind}
      mode={mode}
      name={document.name}
      colors={document.colors}
      logo={assets.logo("primary")}
      logoOnDark={assets.logo("on_dark")}
      mark={assets.logo("mark")}
      headingFamily={heading || undefined}
      bodyFamily={body || undefined}
    />
  );
}

export function BrandPreview({
  document,
  assets,
  drawn,
  title = "Preview",
  placement = "sticky",
}: {
  document: BrandDocument;
  assets: BrandAssets;
  /** What the sample draws, which its note says and a draft's tag marks. */
  drawn: BrandPreviewDrawn;
  /** The card's title, which also names its region. */
  title?: string;
  /**
   * `sticky` holds the card in view beside a long column (an aside); `inline`
   * leaves it where it is laid out, in a column of its own on Review.
   */
  placement?: "sticky" | "inline";
}) {
  const held = placement !== "inline";
  const [kind, setKind] = useState<BrandSampleKind>("doc");
  const [mode, setMode] = useState<BrandSampleMode>("light");
  return (
    <Card as="section" className={held ? "brand-preview" : undefined} aria-label={title}>
      <Card.Header
        actions={
          <span className="brand-preview-controls">
            {drawn === "draft" ? (
              <Tag size="sm" variant="info">
                Draft
              </Tag>
            ) : null}
            <SegmentedControl
              label="Preview kind"
              semantics="radio"
              size="sm"
              options={KINDS}
              value={kind}
              onValueChange={setKind}
            />
            <IconButton
              label={mode === "light" ? "Preview on a dark ground" : "Preview on a light ground"}
              icon={mode === "light" ? <MoonGlyph size="sm" /> : <SunGlyph size="sm" />}
              size="sm"
              variant="ghost"
              onClick={() => setMode(mode === "light" ? "dark" : "light")}
            />
          </span>
        }
      >
        <Card.Title>{title}</Card.Title>
      </Card.Header>
      <Card.Body>
        <BrandSampleOf document={document} assets={assets} kind={kind} mode={mode} />
        <Text as="p" variant="caption" tone="secondary" className="brand-preview-note">
          {NOTES[drawn]}
        </Text>
      </Card.Body>
    </Card>
  );
}
