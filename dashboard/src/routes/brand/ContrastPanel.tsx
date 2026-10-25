/**
 * Every colour pair an adoption measures: how each reads, which one blocks an
 * adoption, which are only low, why a low one matters, and the one-click
 * changes that fix it.
 *
 * ONE COMPACT ROW A PAIR. The badge says the pair, its measure and its level
 * ("Low" for a pair that only advises, `ContrastBadge advisory`); a low pair
 * adds what it needs, one muted line on why it matters, and small offers, each
 * with a chip of the colour it applies. Text on the light ground below its
 * minimum refuses an adoption: it says "Blocks adoption" and the engine's own
 * reason instead.
 */

import { useId } from "react";
import { Button, ColorChip, ContrastBadge, Tag, Text } from "@crewlethq/ui";
import { CircleAlertGlyph } from "@crewlethq/icons/glyphs";
import type { BrandDocument } from "~/protocol/index.ts";
import {
  brandContrast,
  refusalWords,
  type BrandColorRole,
  type BrandContrast,
} from "~/lib/brand/brand.ts";
import { contrastSuggestions, pairNeeds, pairReason } from "~/lib/brand/suggest.ts";

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
  const heading = useId();
  const results = brandContrast(colors);
  return (
    <div className="brand-contrast">
      <div className="brand-contrast-head">
        <Text as="span" variant="label" id={heading}>
          Contrast
        </Text>
      </div>
      <ul className="brand-contrast-pairs" aria-labelledby={heading}>
        {results.map((pair) => {
          const low = short(pair);
          const offers = onApply && low ? contrastSuggestions(colors, pair) : [];
          return (
            <li key={`${pair.foreground}/${pair.ground}`} className="brand-contrast-pair">
              <div className="brand-contrast-pair-head">
                <ContrastBadge
                  foreground={pair.foregroundColor}
                  background={pair.groundColor}
                  label={pair.label}
                  scale={pair.scale}
                  advisory={!pair.refuses}
                />
                {low ? (
                  <Text as="span" variant="caption" tone="secondary">
                    {pairNeeds(pair)}
                  </Text>
                ) : null}
                {low && pair.refuses ? (
                  <Tag size="sm" variant="danger" leadingIcon={<CircleAlertGlyph size="xs" />}>
                    Blocks adoption
                  </Tag>
                ) : null}
              </div>
              {low && pair.refuses ? (
                // THE ENGINE'S OWN REASON for the pair that refuses, said the
                // moment it falls short while the colours are being chosen.
                <Text
                  as="p"
                  variant="caption"
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
                <div className="brand-contrast-offers">
                  {offers.map((offer) => (
                    <Button
                      key={offer.key}
                      size="small"
                      variant="secondary"
                      leadingIcon={<ColorChip value={offer.color} size="xs" />}
                      onClick={() => onApply?.(offer.set)}
                    >
                      {offer.label}
                    </Button>
                  ))}
                </div>
              ) : null}
            </li>
          );
        })}
      </ul>
    </div>
  );
}
