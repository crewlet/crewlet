/**
 * The preview beside a brand's cards: a sample doc or slide, in light or
 * dark, drawn from the brand as it stands (an administrator's edit while a
 * card is being changed), and said to be a sample rather than a real artifact.
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
  Text,
  type BrandSampleKind,
  type BrandSampleMode,
} from "@crewlethq/ui";
import { MoonGlyph, SunGlyph } from "@crewlethq/icons/glyphs";
import type { BrandDocument } from "~/protocol/index.ts";
import { faceDrawing } from "~/lib/brand/brand.ts";
import type { BrandAssets } from "~/lib/brand/useBrand.ts";

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
  draft,
}: {
  document: BrandDocument;
  assets: BrandAssets;
  /** The sample draws an edit not yet saved, and says so. */
  draft: boolean;
}) {
  const [kind, setKind] = useState<BrandSampleKind>("doc");
  const [mode, setMode] = useState<BrandSampleMode>("light");
  return (
    <Card as="section" className="brand-preview" aria-label="Preview">
      <Card.Header
        actions={
          <span className="brand-preview-controls">
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
        <Card.Title>Preview</Card.Title>
      </Card.Header>
      <Card.Body>
        <BrandSampleOf document={document} assets={assets} kind={kind} mode={mode} />
        <Text as="p" variant="caption" tone="secondary" className="brand-preview-note">
          {draft
            ? "A sample drawn with your changes, not a real artifact."
            : "A sample drawn with this brand, not a real artifact."}
        </Text>
      </Card.Body>
    </Card>
  );
}
