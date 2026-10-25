/**
 * The brand setup's Review step: the brand as it will be adopted, part by
 * part, beside its preview, then what to confirm before adopting.
 *
 * # Two columns, never overlapping
 *
 * On the left a grid of summary cards, one a part, each under its glyph with
 * a status (Complete, Uses defaults, Missing required) and an Edit that jumps
 * to its step. Each card SHOWS the values: the logos on their grounds, the
 * colours as swatches with their values, the faces as specimens, the voice's
 * summary and how many lines it holds. On the right the preview, a card of a
 * fixed width held in its own column (the wizard draws no aside on Review). On
 * a narrower page the preview follows the cards; on a phone it is one column.
 *
 * # Before you adopt
 *
 * One card under both: each low-contrast pair as a checkbox to confirm, and
 * what adopting does (`WhenYouAdopt`).
 */

import type { ReactNode } from "react";
import {
  Button,
  Card,
  Checkbox,
  ColorSwatch,
  List,
  ListItem,
  LogoSlot,
  Tag,
  Text,
  TypeSpecimen,
} from "@crewlethq/ui";
import {
  BlendGlyph,
  BuildingComplexGlyph,
  ImageGlyph,
  PencilGlyph,
  TextQuoteGlyph,
  TypeGlyph,
} from "@crewlethq/icons/glyphs";
import type { BrandDocument } from "~/protocol/index.ts";
import {
  BRAND_COLOR_ROLES,
  BRAND_FACE_ROLES,
  BRAND_LOGO_SLOTS,
  DEFAULT_FACE,
  brandContrast,
  faceDrawing,
  warningPairs,
  warningWords,
  type BrandContrast,
} from "~/lib/brand/brand.ts";
import { pairReason } from "~/lib/brand/suggest.ts";
import { stepSpec, type SetupStepId } from "~/lib/brand/setup.ts";
import type { BrandAssets } from "~/lib/brand/useBrand.ts";
import type { BrandEdit } from "~/lib/brand/draft.ts";
import { BrandPreview } from "../BrandPreview.tsx";
import { WhenYouAdopt } from "../WhenYouAdopt.tsx";

/** A part's state on Review, as its pill says it. */
export type PartStatus = "complete" | "defaults" | "missing";

const STATUS: Readonly<
  Record<PartStatus, { label: string; variant: "success" | "neutral" | "danger" }>
> = {
  complete: { label: "Complete", variant: "success" },
  defaults: { label: "Uses defaults", variant: "neutral" },
  missing: { label: "Missing required", variant: "danger" },
};

type Part = Exclude<SetupStepId, "review">;

const GLYPHS: Readonly<Record<Part, ReactNode>> = {
  identity: <BuildingComplexGlyph size="sm" />,
  logos: <ImageGlyph size="sm" />,
  colours: <BlendGlyph size="sm" />,
  type: <TypeGlyph size="sm" />,
  voice: <TextQuoteGlyph size="sm" />,
};

const sameFace = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

/**
 * A part's status: missing what an adoption requires, complete, or leaning on
 * Crewlet's defaults for what it leaves unset (a colour role, a face, the
 * voice agents fall back on).
 */
export function partStatus(part: Part, doc: BrandDocument): PartStatus {
  switch (part) {
    case "identity":
      return doc.name.trim() ? "complete" : "missing";
    case "logos":
      return doc.logos.primary ? "complete" : "missing";
    case "colours":
      if (!doc.colors.primary) return "missing";
      return BRAND_COLOR_ROLES.every((r) => doc.colors[r.role]) ? "complete" : "defaults";
    case "type":
      return BRAND_FACE_ROLES.some(({ role }) => !sameFace(doc.type[role], DEFAULT_FACE))
        ? "complete"
        : "defaults";
    case "voice":
      return doc.voice.summary?.trim() || doc.voice.write_like?.length || doc.voice.avoid?.length
        ? "complete"
        : "defaults";
  }
}

/** The key a contrast warning is acknowledged by. */
export function warningKey(pair: BrandContrast): string {
  return `${pair.foreground}/${pair.ground}`;
}

/** The contrast warnings a finish asks to acknowledge, for these colours. */
export function setupWarnings(doc: BrandDocument): BrandContrast[] {
  return warningPairs(brandContrast(doc.colors));
}

/** How many lines a voice list holds, in words: "3 lines", "1 line". */
const lines = (n: number) => (n === 1 ? "1 line" : `${n} lines`);

function SummaryCard({
  part,
  doc,
  onEdit,
  children,
}: {
  part: Part;
  doc: BrandDocument;
  onEdit: (step: SetupStepId) => void;
  children: ReactNode;
}) {
  const label = stepSpec(part).label;
  const status = STATUS[partStatus(part, doc)];
  return (
    <Card as="section" aria-label={label} className="brand-review-card">
      <Card.Header
        actions={
          <span className="brand-review-card-actions">
            <Tag size="sm" variant={status.variant}>
              {status.label}
            </Tag>
            <Button
              size="small"
              variant="ghost"
              leadingIcon={<PencilGlyph size="sm" />}
              aria-label={`Edit ${label}`}
              onClick={() => onEdit(part)}
            >
              Edit
            </Button>
          </span>
        }
      >
        <span className="brand-review-card-title">
          <span className="brand-review-card-glyph" aria-hidden="true">
            {GLYPHS[part]}
          </span>
          <Card.Title>{label}</Card.Title>
        </span>
      </Card.Header>
      <Card.Body>{children}</Card.Body>
    </Card>
  );
}

export function ReviewStep({
  edit,
  assets,
  onEditStep,
  acknowledged,
  onAcknowledge,
}: {
  edit: BrandEdit;
  assets: BrandAssets;
  onEditStep: (step: SetupStepId) => void;
  acknowledged: ReadonlySet<string>;
  onAcknowledge: (key: string, on: boolean) => void;
}) {
  const doc = edit.document;
  const warnings = setupWarnings(doc);
  const setColours = BRAND_COLOR_ROLES.filter((r) => doc.colors[r.role]);
  const defaultColours = BRAND_COLOR_ROLES.length - setColours.length;
  const like = doc.voice.write_like?.length ?? 0;
  const avoid = doc.voice.avoid?.length ?? 0;
  return (
    <div className="brand-review">
      <div className="brand-review-columns">
        <div className="brand-review-cards">
          <SummaryCard part="identity" doc={doc} onEdit={onEditStep}>
            <Text as="p" variant="heading" className="brand-review-name">
              {doc.name.trim() || "No name yet"}
            </Text>
            <Text as="p" variant="body" tone="secondary" className="brand-review-line">
              {doc.tagline?.trim() || "No tagline"}
            </Text>
          </SummaryCard>
          <SummaryCard part="logos" doc={doc} onEdit={onEditStep}>
            <div className="brand-review-logos">
              {BRAND_LOGO_SLOTS.map((spec) => (
                <LogoSlot
                  key={spec.slot}
                  label={spec.label}
                  ground={spec.ground}
                  groundColor={
                    spec.ground === "dark" ? doc.colors.dark_ground : doc.colors.light_ground
                  }
                  shape={spec.shape}
                  src={assets.logo(spec.slot)}
                  alt={`${doc.name} ${spec.label.toLowerCase()} logo`}
                />
              ))}
            </div>
          </SummaryCard>
          <SummaryCard part="colours" doc={doc} onEdit={onEditStep}>
            <div className="brand-review-swatches">
              {setColours.map((r) => (
                <ColorSwatch
                  key={r.role}
                  name={r.label}
                  value={doc.colors[r.role]}
                  size="sm"
                  copyable={false}
                />
              ))}
            </div>
            {defaultColours > 0 ? (
              <Text as="p" variant="caption" tone="secondary" className="brand-review-line">
                {defaultColours === 1
                  ? "1 more uses Crewlet's default."
                  : `${defaultColours} more use Crewlet's defaults.`}
              </Text>
            ) : null}
          </SummaryCard>
          <SummaryCard part="type" doc={doc} onEdit={onEditStep}>
            <div className="brand-review-faces">
              {BRAND_FACE_ROLES.map(({ role, label }) => {
                const face = faceDrawing(doc.type[role], assets.face(role));
                return (
                  <TypeSpecimen
                    key={role}
                    label={label}
                    family={face.name}
                    fontFamily={face.stack || undefined}
                    sample=""
                    weight={role === "heading" ? "semibold" : "regular"}
                  />
                );
              })}
            </div>
          </SummaryCard>
          <SummaryCard part="voice" doc={doc} onEdit={onEditStep}>
            <Text as="p" variant="passage" className="brand-review-voice">
              {doc.voice.summary?.trim() || "No summary"}
            </Text>
            <Text as="p" variant="caption" tone="secondary" className="brand-review-line">
              {`Write like this: ${lines(like)}. Avoid: ${lines(avoid)}.`}
            </Text>
          </SummaryCard>
        </div>
        <div className="brand-review-preview">
          <BrandPreview
            document={doc}
            assets={assets}
            draft
            title="Preview of your draft"
            placement="inline"
          />
        </div>
      </div>
      <Card as="section" aria-label="Before you adopt" className="brand-review-before">
        <Card.Header>
          <Card.Title>Before you adopt</Card.Title>
        </Card.Header>
        <Card.Body>
          <div className="brand-review-before-body">
            {warnings.length > 0 ? (
              <section className="brand-setup-warnings" aria-label="Contrast warnings">
                <Text as="p" variant="body">
                  These pairs are allowed but may be hard to read. Confirm each one.
                </Text>
                <List variant="plain">
                  {warnings.map((pair) => (
                    <ListItem key={warningKey(pair)}>
                      <Checkbox
                        label={warningWords(pair)}
                        description={pairReason(pair)}
                        checked={acknowledged.has(warningKey(pair))}
                        onCheckedChange={(on) => onAcknowledge(warningKey(pair), on)}
                      />
                    </ListItem>
                  ))}
                </List>
              </section>
            ) : null}
            <WhenYouAdopt />
          </div>
        </Card.Body>
      </Card>
    </div>
  );
}
