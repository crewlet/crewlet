/**
 * The brand setup's steps, each the editor of one part of the brand: the same
 * fields the Brand page's cards edit, so a value is chosen and refused the
 * same way in both places.
 */

import type { ReactNode } from "react";
import { Text } from "@crewlethq/ui";
import type { BrandLogoSlot } from "~/lib/brand/brand.ts";
import {
  ColoursEditor,
  IdentityFields,
  LogoFields,
  TypographyEditor,
  VoiceEditor,
  type BrandEditorProps,
} from "../BrandEditors.tsx";

/** The note under the voice: where a rule for every message lives instead. */
export const VOICE_NOTE =
  "Agents read this when they write an artifact. To shape every message they write, add a standing policy in Principles.";

/**
 * One step's fields, stacked. The step's heading and lead sentence are the
 * wizard's (`SetupWizard heading` and `lead`), which focuses the heading when
 * the step changes.
 */
function StepBody({ children }: { children: ReactNode }) {
  return <div className="brand-setup-step">{children}</div>;
}

export function IdentityStep({ edit, onEdit }: Omit<BrandEditorProps, "assets">) {
  return (
    <StepBody>
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
    <StepBody>
      <LogoFields edit={edit} onEdit={onEdit} assets={assets} purposes refused={refused} />
    </StepBody>
  );
}

export function ColoursStep(props: BrandEditorProps) {
  return (
    <StepBody>
      <ColoursEditor {...props} />
    </StepBody>
  );
}

export function TypographyStep(props: BrandEditorProps) {
  return (
    <StepBody>
      <TypographyEditor {...props} />
    </StepBody>
  );
}

export function VoiceStep(props: BrandEditorProps) {
  return (
    <StepBody>
      <Text as="p" variant="body" className="brand-setup-note">
        {VOICE_NOTE}
      </Text>
      <VoiceEditor {...props} />
    </StepBody>
  );
}
