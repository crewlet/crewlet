/**
 * The brand setup's Review step: the brand as it will be adopted, part by
 * part, and beside it what a person decides on before adopting.
 *
 * # A summary, then the decision
 *
 * On the left the summary (`ReviewList`): one row a part in one box, each
 * under its glyph with its state (Complete, Uses defaults, Needs attention,
 * Missing required) and an Edit that goes back to its step. Each row SHOWS the
 * values: the logos as thumbnails on their grounds, the colours as swatches
 * with the roles left to Crewlet's defaults named, the faces as specimens, the
 * voice's summary and its lines. One box with a row a part, rather than a
 * card a part in a grid, so a tall row of logos never leaves holes beside a
 * one-line name.
 *
 * On the right, the column a person decides in, ending above the finish
 * action: the preview at a fixed proportion, then "Before you publish" with
 * each low-contrast pair to confirm and what adopting does (`WhenYouAdopt`).
 * Where the step is narrow the column follows the summary, so it still ends
 * where the finish is.
 *
 * # What holds the finish is said where it is fixed
 *
 * A finish tried too early marks what holds it in place (`showErrors`): the
 * part a required value is missing from says what to add under its values,
 * and the contrast pairs not yet confirmed say so under their boxes. Nothing
 * is said as a loose line under the page, and nothing before a finish is
 * tried. The first thing that holds is marked `data-hold`, which is where
 * the setup moves focus.
 */

import type { ReactNode } from "react";
import {
  Button,
  Card,
  Checkbox,
  CheckboxGroup,
  ColorSwatch,
  LogoSlot,
  ReviewList,
  Text,
  TypeSpecimen,
  type ReviewListStatus,
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
import { partProblem } from "~/lib/brand/validate.ts";
import type { BrandAssets } from "~/lib/brand/useBrand.ts";
import type { BrandEdit } from "~/lib/brand/draft.ts";
import { BrandPreview } from "../BrandPreview.tsx";
import { WhenYouAdopt } from "../WhenYouAdopt.tsx";

/** A part's state on Review, as its pill says it. */
export type PartStatus = "complete" | "defaults" | "attention" | "missing";

const STATUS: Readonly<Record<PartStatus, ReviewListStatus>> = {
  complete: { label: "Complete", tone: "success" },
  defaults: { label: "Uses defaults", tone: "neutral" },
  attention: { label: "Needs attention", tone: "warning" },
  missing: { label: "Missing required", tone: "danger" },
};

type Part = Exclude<SetupStepId, "review">;

const PARTS: readonly { part: Part; icon: ReactNode }[] = [
  { part: "identity", icon: <BuildingComplexGlyph size="sm" /> },
  { part: "logos", icon: <ImageGlyph size="sm" /> },
  { part: "colours", icon: <BlendGlyph size="sm" /> },
  { part: "type", icon: <TypeGlyph size="sm" /> },
  { part: "voice", icon: <TextQuoteGlyph size="sm" /> },
];

/** The parts an adoption requires something of. */
const HOLDS_FINISH: ReadonlySet<Part> = new Set(["identity", "logos", "colours"]);

const sameFace = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

/** The key a contrast warning is acknowledged by. */
export function warningKey(pair: BrandContrast): string {
  return `${pair.foreground}/${pair.ground}`;
}

/** The contrast warnings a finish asks to acknowledge, for these colours. */
export function setupWarnings(doc: BrandDocument): BrandContrast[] {
  return warningPairs(brandContrast(doc.colors));
}

/**
 * A part's status: missing what an adoption requires; needing attention while
 * a low-contrast pair of its colours is not confirmed; complete; or leaning on
 * Crewlet's defaults for what it leaves unset (a colour role, a face, the
 * voice agents fall back on).
 */
export function partStatus(
  part: Part,
  doc: BrandDocument,
  acknowledged: ReadonlySet<string> = new Set(),
): PartStatus {
  switch (part) {
    case "identity":
      return doc.name.trim() ? "complete" : "missing";
    case "logos":
      return doc.logos.primary ? "complete" : "missing";
    case "colours":
      if (!doc.colors.primary) return "missing";
      if (setupWarnings(doc).some((pair) => !acknowledged.has(warningKey(pair))))
        return "attention";
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

/** How many lines a voice list holds, in words: "3 lines", "1 line", "none". */
const lines = (n: number) => (n === 0 ? "none" : n === 1 ? "1 line" : `${n} lines`);

/** A list of names in words: "Text", "Text and Dark ground", "Text, Secondary and Dark ground". */
const listed = (names: readonly string[]) =>
  names.length <= 1 ? (names[0] ?? "") : `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`;

/** What a part holds, SHOWN: the values themselves rather than a word for them. */
function PartValues({
  part,
  doc,
  assets,
}: {
  part: Part;
  doc: BrandDocument;
  assets: BrandAssets;
}) {
  switch (part) {
    case "identity":
      return (
        <div className="brand-review-identity">
          <Text as="p" variant="body" className="brand-review-name">
            {doc.name.trim() || "No name yet"}
          </Text>
          <Text as="p" variant="caption" tone="secondary">
            {doc.tagline?.trim() || "No tagline"}
          </Text>
        </div>
      );
    case "logos":
      return (
        <div className="brand-review-logos">
          {BRAND_LOGO_SLOTS.map((spec) => (
            <LogoSlot
              key={spec.slot}
              size="sm"
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
      );
    case "colours": {
      const set = BRAND_COLOR_ROLES.filter((r) => doc.colors[r.role]);
      const unset = BRAND_COLOR_ROLES.filter((r) => !doc.colors[r.role]).map((r) => r.label);
      return (
        <div className="brand-review-stack">
          {set.length > 0 ? (
            <div className="brand-review-swatches">
              {set.map((r) => (
                <ColorSwatch
                  key={r.role}
                  name={r.label}
                  value={doc.colors[r.role]}
                  size="sm"
                  copyable={false}
                />
              ))}
            </div>
          ) : null}
          {unset.length > 0 ? (
            <Text as="p" variant="caption" tone="secondary">
              {`Crewlet's defaults: ${listed(unset)}.`}
            </Text>
          ) : null}
        </div>
      );
    }
    case "type":
      return (
        <div className="brand-review-faces">
          {BRAND_FACE_ROLES.map(({ role, label }) => {
            const face = faceDrawing(doc.type[role], assets.face(role));
            return (
              <TypeSpecimen
                key={role}
                size="sm"
                label={label}
                family={face.name}
                fontFamily={face.stack || undefined}
                sample=""
                weight={role === "heading" ? "semibold" : "regular"}
              />
            );
          })}
        </div>
      );
    case "voice": {
      const like = doc.voice.write_like?.length ?? 0;
      const avoid = doc.voice.avoid?.length ?? 0;
      return (
        <div className="brand-review-stack">
          <Text as="p" variant="body">
            {doc.voice.summary?.trim() || "No summary"}
          </Text>
          <Text as="p" variant="caption" tone="secondary">
            {`Write like this: ${lines(like)}. Avoid: ${lines(avoid)}.`}
          </Text>
        </div>
      );
    }
  }
}

export function ReviewStep({
  edit,
  assets,
  admin,
  onEditStep,
  acknowledged,
  onAcknowledge,
  showErrors,
}: {
  edit: BrandEdit;
  assets: BrandAssets;
  /** An administrator publishes; an editor of the kit saves it for review. */
  admin: boolean;
  onEditStep: (step: SetupStepId) => void;
  acknowledged: ReadonlySet<string>;
  onAcknowledge: (key: string, on: boolean) => void;
  /** A finish was tried: say what holds it, where it is fixed. */
  showErrors: boolean;
}) {
  const doc = edit.document;
  const warnings = setupWarnings(doc);
  const unconfirmed = warnings.filter((pair) => !acknowledged.has(warningKey(pair)));
  // ONLY WHAT AN ADOPTION REQUIRES holds a finish: the name, the primary
  // logo and a readable primary colour (`adoptionProblem`).
  const problems = new Map(
    PARTS.map(({ part }) => [
      part,
      showErrors && HOLDS_FINISH.has(part) ? partProblem(part, edit) : undefined,
    ]),
  );
  // THE FIRST THING THAT HOLDS, in reading order, is where focus goes.
  const firstHeld = PARTS.find(({ part }) => problems.get(part))?.part;
  const firstOpen = unconfirmed[0] ? warningKey(unconfirmed[0]) : undefined;
  const finish = admin ? "publish" : "save it for review";
  const before = admin ? "Before you publish" : "Before you save for review";
  return (
    <div className="brand-review">
      <div className="brand-review-grid">
        <ReviewList label="Brand summary">
          {PARTS.map(({ part, icon }) => {
            const label = stepSpec(part).label;
            const problem = problems.get(part);
            return (
              <ReviewList.Item
                key={part}
                icon={icon}
                title={label}
                status={STATUS[partStatus(part, doc, acknowledged)]}
                error={problem}
                action={
                  <Button
                    size="small"
                    variant="ghost"
                    leadingIcon={<PencilGlyph size="sm" />}
                    aria-label={`Edit ${label}`}
                    data-hold={part === firstHeld ? "" : undefined}
                    onClick={() => onEditStep(part)}
                  >
                    Edit
                  </Button>
                }
              >
                <PartValues part={part} doc={doc} assets={assets} />
              </ReviewList.Item>
            );
          })}
        </ReviewList>
        <div className="brand-review-decide">
          <BrandPreview
            document={doc}
            assets={assets}
            drawn="changes"
            title="Preview of your draft"
            placement="inline"
          />
          <Card as="section" aria-label={before}>
            <Card.Header>
              <Card.Title>{before}</Card.Title>
            </Card.Header>
            <Card.Body>
              <div className="brand-review-before">
                {warnings.length > 0 ? (
                  <CheckboxGroup
                    legend="Confirm low contrast"
                    error={
                      showErrors && unconfirmed.length > 0
                        ? `Confirm ${warnings.length === 1 ? "the pair" : "each pair"} to ${finish}.`
                        : undefined
                    }
                  >
                    {warnings.map((pair) => {
                      const key = warningKey(pair);
                      const open = showErrors && !acknowledged.has(key);
                      return (
                        <Checkbox
                          key={key}
                          label={warningWords(pair)}
                          description={pairReason(pair)}
                          checked={acknowledged.has(key)}
                          aria-invalid={open || undefined}
                          data-hold={open && !firstHeld && key === firstOpen ? "" : undefined}
                          onCheckedChange={(on) => onAcknowledge(key, on)}
                        />
                      );
                    })}
                  </CheckboxGroup>
                ) : null}
                <WhenYouAdopt />
              </div>
            </Card.Body>
          </Card>
        </div>
      </div>
    </div>
  );
}
