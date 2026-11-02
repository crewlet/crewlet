/**
 * Every colour pair an adoption measures: how each reads, which one blocks an
 * adoption, which are only low, why a low one matters, and the one-click
 * changes that fix it.
 *
 * ONE COMPACT ROW A PAIR. The badge says the pair, its measure and its level
 * ("Low" for a pair that only advises, `ContrastBadge advisory`); a low pair
 * adds what it needs, one muted line on why it matters, and its offers as
 * small answer options (`ChoiceButtons size="sm"`), each with a chip of the
 * colour it applies. Text on the light ground below its minimum refuses an
 * adoption: it says "Blocks publishing" and the engine's own reason instead.
 *
 * AN APPLIED OFFER STAYS CHOSEN. Applying one fixes the pair, which would
 * take its offers away with it; they stay drawn, the applied one marked, for
 * as long as the colours are still what it set, so the person sees what they
 * chose and can pick another.
 */

import { useId, useState } from "react";
import { ChoiceButtons, ColorChip, ContrastBadge, ContrastHelp, Tag, Text } from "@crewlethq/ui";
import { CircleAlertGlyph } from "@crewlethq/icons/glyphs";
import type { BrandDocument } from "~/protocol/index.ts";
import {
  brandContrast,
  refusalWords,
  type BrandColorRole,
  type BrandContrast,
} from "~/lib/brand/brand.ts";
import {
  contrastSuggestions,
  pairNeeds,
  pairReason,
  type ContrastSuggestion,
} from "~/lib/brand/suggest.ts";

/** An applied offer: the pair's offers when it was applied, and which one. */
interface Applied {
  offers: ContrastSuggestion[];
  key: string;
}

/** Whether the colours still hold everything an offer set. */
function holds(colors: BrandDocument["colors"], offer: ContrastSuggestion | undefined): boolean {
  if (!offer) return false;
  return Object.entries(offer.set).every(
    ([role, value]) => colors[role as BrandColorRole] === value,
  );
}

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
  const [applied, setApplied] = useState<Readonly<Record<string, Applied>>>({});
  return (
    <div className="brand-contrast">
      <div className="brand-contrast-head">
        <Text as="span" variant="label" id={heading}>
          Contrast
        </Text>
        {/* BESIDE THE HEADING, never in it, so the list stays named "Contrast". */}
        <ContrastHelp />
      </div>
      <ul className="brand-contrast-pairs" aria-labelledby={heading}>
        {results.map((pair) => {
          const id = `${pair.foreground}/${pair.ground}`;
          const low = short(pair);
          const kept = applied[id];
          const chosen =
            kept &&
            holds(
              colors,
              kept.offers.find((o) => o.key === kept.key),
            );
          const offers = !onApply
            ? []
            : chosen
              ? kept.offers
              : low
                ? contrastSuggestions(colors, pair)
                : [];
          return (
            <li key={id} className="brand-contrast-pair">
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
                    Blocks publishing
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
                <ChoiceButtons
                  label={`Suggestions for ${pair.label}`}
                  size="sm"
                  chosen={chosen ? kept.key : null}
                  onChoose={(key) => {
                    const offer = offers.find((o) => o.key === key);
                    if (!offer) return;
                    setApplied((was) => ({ ...was, [id]: { offers, key } }));
                    onApply?.(offer.set);
                  }}
                  options={offers.map((offer) => ({
                    value: offer.key,
                    label: offer.label,
                    detail: offer.detail,
                    ...(offer.color ? { media: <ColorChip value={offer.color} /> } : {}),
                  }))}
                />
              ) : null}
            </li>
          );
        })}
      </ul>
    </div>
  );
}
