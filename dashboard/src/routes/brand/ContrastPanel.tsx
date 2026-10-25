/**
 * Every colour pair an adoption measures, while the colours are chosen: how
 * each reads, which one blocks an adoption, which are only low, why a low one
 * matters, and the one-click change that fixes it.
 *
 * ONE BLOCKS, THE REST ADVISE. Text on the light ground below its minimum
 * refuses an adoption, said in danger tone ("Blocks adoption"); every other
 * pair below its minimum is allowed and said in warning tone ("Low contrast,
 * allowed") with the reason, so a person can keep a colour on purpose. The
 * badge of an advisory pair reads "Low" rather than "Fails"
 * (`ContrastBadge advisory`).
 */

import { Button, ContrastBadge, Tag, Text } from "@crewlethq/ui";
import { CircleAlertGlyph, TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import type { BrandDocument } from "~/protocol/index.ts";
import {
  brandContrast,
  refusalWords,
  type BrandColorRole,
  type BrandContrast,
} from "~/lib/brand/brand.ts";
import { contrastSuggestions, pairReason } from "~/lib/brand/suggest.ts";

/** Whether a pair falls short of its minimum, measured. */
function short(pair: BrandContrast): boolean {
  return pair.ratio !== null && !pair.passes;
}

export function ContrastPanel({
  colors,
  onApply,
}: {
  colors: BrandDocument["colors"];
  /** Apply a suggestion's colours; left out, the panel offers none (a reader's view). */
  onApply?: ((set: Partial<Record<BrandColorRole, string>>) => void) | undefined;
}) {
  const results = brandContrast(colors);
  return (
    <ul className="brand-contrast-pairs" aria-label="Contrast">
      {results.map((pair) => {
        const low = short(pair);
        const offers = onApply && low ? contrastSuggestions(colors, pair) : [];
        return (
          <li key={`${pair.foreground}/${pair.ground}`} className="brand-contrast-pair">
            <span className="brand-contrast-pair-head">
              <ContrastBadge
                foreground={pair.foregroundColor}
                background={pair.groundColor}
                label={pair.label}
                scale={pair.scale}
                advisory={!pair.refuses}
              />
              {low ? (
                pair.refuses ? (
                  <Tag size="sm" variant="danger" leadingIcon={<CircleAlertGlyph size="xs" />}>
                    Blocks adoption
                  </Tag>
                ) : (
                  <Tag size="sm" variant="warning" leadingIcon={<TriangleAlertGlyph size="xs" />}>
                    Low contrast, allowed
                  </Tag>
                )
              ) : null}
            </span>
            {low && pair.refuses ? (
              // THE ENGINE'S OWN REASON for the pair that refuses, said the
              // moment it falls short while the colours are being chosen.
              <Text
                as="p"
                variant="body"
                className="brand-refusal"
                {...(onApply ? { role: "alert" } : {})}
              >
                {refusalWords(pair)}
              </Text>
            ) : low ? (
              <Text as="p" variant="caption" tone="secondary" className="brand-contrast-reason">
                {pairReason(pair)}
              </Text>
            ) : null}
            {offers.length > 0 ? (
              <span className="brand-contrast-offers">
                {offers.map((offer) => (
                  <Button
                    key={offer.key}
                    size="small"
                    variant="secondary"
                    onClick={() => onApply?.(offer.set)}
                  >
                    {offer.label}
                  </Button>
                ))}
              </span>
            ) : null}
          </li>
        );
      })}
    </ul>
  );
}
