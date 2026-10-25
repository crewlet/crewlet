/**
 * The brand setup's steps, each the editor of one part of the brand: the same
 * fields the Brand page's cards edit, so a value is chosen and refused the
 * same way in both places, under a lead sentence saying why the part matters
 * to agents and what they make.
 */

import type { ReactNode } from "react";
import { Button, Card, Checkbox, List, ListItem, Text } from "@crewlethq/ui";
import { PencilGlyph } from "@crewlethq/icons/glyphs";
import type { BrandDocument } from "~/protocol/index.ts";
import {
  BRAND_COLOR_ROLES,
  BRAND_FACE_ROLES,
  BRAND_LOGO_SLOTS,
  brandContrast,
  faceDrawing,
  warningPairs,
  warningWords,
  type BrandColorRole,
  type BrandContrast,
  type BrandLogoSlot,
} from "~/lib/brand/brand.ts";
import { pairReason } from "~/lib/brand/suggest.ts";
import { stepSpec, type SetupStepId } from "~/lib/brand/setup.ts";
import { withDocument } from "~/lib/brand/validate.ts";
import type { BrandAssets } from "~/lib/brand/useBrand.ts";
import type { BrandEdit } from "~/lib/brand/draft.ts";
import {
  ColourFields,
  IdentityFields,
  LogoFields,
  TypographyEditor,
  VoiceEditor,
  type BrandEditorProps,
} from "../BrandEditors.tsx";
import { ContrastPanel } from "../ContrastPanel.tsx";
import { BrandPreview } from "../BrandPreview.tsx";
import { PUBLIC_FACE } from "../AdoptDialog.tsx";

/** The note under the voice: where a rule for every message lives instead. */
export const VOICE_NOTE =
  "Agents read this when they write an artifact. To shape every message they write, add a standing policy in Principles.";

/** One step's body: its lead sentence over its fields. */
function StepBody({ step, children }: { step: SetupStepId; children: ReactNode }) {
  return (
    <div className="brand-setup-step">
      <Text as="p" variant="body" tone="secondary" className="brand-setup-lead">
        {stepSpec(step).lead}
      </Text>
      {children}
    </div>
  );
}

export function IdentityStep({ edit, onEdit }: Omit<BrandEditorProps, "assets">) {
  return (
    <StepBody step="identity">
      <IdentityFields edit={edit} onEdit={onEdit} />
    </StepBody>
  );
}

export function LogosStep({
  edit,
  onEdit,
  assets,
  refused,
}: BrandEditorProps & { refused: Partial<Record<BrandLogoSlot, string>> }) {
  return (
    <StepBody step="logos">
      <LogoFields edit={edit} onEdit={onEdit} assets={assets} purposes refused={refused} />
    </StepBody>
  );
}

export function ColoursStep({ edit, onEdit }: Omit<BrandEditorProps, "assets">) {
  const apply = (set: Partial<Record<BrandColorRole, string>>) =>
    onEdit(withDocument(edit, (d) => ({ ...d, colors: { ...d.colors, ...set } })));
  return (
    <StepBody step="colours">
      <ColourFields edit={edit} onEdit={onEdit} />
      <ContrastPanel colors={edit.document.colors} onApply={apply} />
    </StepBody>
  );
}

export function TypographyStep(props: BrandEditorProps) {
  return (
    <StepBody step="type">
      <TypographyEditor {...props} />
    </StepBody>
  );
}

export function VoiceStep(props: BrandEditorProps) {
  return (
    <StepBody step="voice">
      <Text as="p" variant="body" className="brand-setup-note">
        {VOICE_NOTE}
      </Text>
      <VoiceEditor {...props} />
    </StepBody>
  );
}

/** A summary card's lines: what is set, what uses the default, what is missing. */
interface SummaryLine {
  key: string;
  label: string;
  value: string;
}

function summaries(doc: BrandDocument): Record<Exclude<SetupStepId, "review">, SummaryLine[]> {
  return {
    identity: [
      { key: "name", label: "Name", value: doc.name.trim() || "Missing" },
      { key: "tagline", label: "Tagline", value: doc.tagline?.trim() || "None" },
    ],
    logos: BRAND_LOGO_SLOTS.map((s) => ({
      key: s.slot,
      label: s.changeLabel,
      value: doc.logos[s.slot] ? "Added" : s.required ? "Missing" : "Not added",
    })),
    colours: BRAND_COLOR_ROLES.map((r) => ({
      key: r.role,
      label: r.label,
      value: doc.colors[r.role] ?? (r.required ? "Missing" : "Uses the default"),
    })),
    type: BRAND_FACE_ROLES.map(({ role, label }) => ({
      key: role,
      label,
      value: faceDrawing(doc.type[role]).name,
    })),
    voice: [
      { key: "summary", label: "Summary", value: doc.voice.summary?.trim() ? "Written" : "None" },
      {
        key: "like",
        label: "Write like this",
        value: doc.voice.write_like?.length ? `${doc.voice.write_like.length} lines` : "None",
      },
      {
        key: "avoid",
        label: "Avoid",
        value: doc.voice.avoid?.length ? `${doc.voice.avoid.length} lines` : "None",
      },
    ],
  };
}

/** The key a contrast warning is acknowledged by. */
export function warningKey(pair: BrandContrast): string {
  return `${pair.foreground}/${pair.ground}`;
}

/** The contrast warnings a finish asks to acknowledge, for these colours. */
export function setupWarnings(doc: BrandDocument): BrandContrast[] {
  return warningPairs(brandContrast(doc.colors));
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
  const cards = summaries(doc);
  const warnings = setupWarnings(doc);
  return (
    <StepBody step="review">
      <div className="brand-setup-summaries">
        {(Object.keys(cards) as (keyof typeof cards)[]).map((step) => (
          <Card as="section" key={step} aria-label={stepSpec(step).label}>
            <Card.Header
              actions={
                <Button
                  size="small"
                  variant="ghost"
                  leadingIcon={<PencilGlyph size="sm" />}
                  aria-label={`Edit ${stepSpec(step).label}`}
                  onClick={() => onEditStep(step)}
                >
                  Edit
                </Button>
              }
            >
              <Card.Title>{stepSpec(step).label}</Card.Title>
            </Card.Header>
            <Card.Body>
              <dl className="brand-setup-summary">
                {cards[step].map((line) => (
                  <div key={line.key} className="brand-setup-summary-row">
                    <dt>{line.label}</dt>
                    <dd>{line.value}</dd>
                  </div>
                ))}
              </dl>
            </Card.Body>
          </Card>
        ))}
      </div>
      <BrandPreview document={doc} assets={assets} draft title="Preview of your draft" />
      {warnings.length > 0 ? (
        <section className="brand-setup-warnings" aria-label="Contrast warnings">
          <Text as="p" variant="body">
            These colours are allowed, and some readers may find them hard to read. Confirm each
            before you finish.
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
      <Text as="p" variant="body" tone="secondary">
        {PUBLIC_FACE}
      </Text>
    </StepBody>
  );
}
