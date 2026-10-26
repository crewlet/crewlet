/**
 * Publishing a brand (adopting it, in the engine's word): what changes, which
 * contrast pairs fall short, and what publishing makes public, confirmed once.
 *
 * AN ADMINISTRATOR'S, A PERSON'S, WITH A RECENT PROOF. The engine asks for the
 * proof when it is stale (`StepUp.tsx`) and the adoption goes through once the
 * person confirms. A pair that refuses an adoption holds the confirm with the
 * engine's own reason; the others are listed for the person to accept. A
 * draft that lacks a primary logo or colour holds it too, saying which.
 */

import { useId, type ReactNode } from "react";
import { Button, Callout, ContrastHelp, Modal, Tag, Text, VisuallyHidden } from "@crewlethq/ui";
import { TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import type { BrandDocument } from "~/protocol/index.ts";
import {
  brandChanges,
  brandContrast,
  warningPairs,
  warningWords,
  type LogoDigests,
} from "~/lib/brand/brand.ts";
import { adoptionProblem } from "~/lib/brand/validate.ts";
import { WhenYouAdopt } from "./WhenYouAdopt.tsx";

export interface AdoptDialogProps {
  open: boolean;
  onClose: () => void;
  /** The dialog's question: "Publish version 9 as the Acme brand?". */
  title: string;
  /** The adopted brand, or null when none is adopted yet. */
  before: BrandDocument | null;
  after: BrandDocument;
  digests: { before: LogoDigests; after: LogoDigests };
  /** The confirm's words: "Publish version 9", "Save and publish". */
  confirmLabel: string;
  busy: boolean;
  /** What the last attempt answered, where it did not adopt. */
  answer?: ReactNode;
  onConfirm: () => void;
}

export function AdoptDialog({
  open,
  onClose,
  title,
  before,
  after,
  digests,
  confirmLabel,
  busy,
  answer,
  onConfirm,
}: AdoptDialogProps) {
  const changes = brandChanges(before, after, digests);
  const warningsHeading = useId();
  const results = brandContrast(after.colors);
  // WHAT HOLDS THE ADOPTION, in the engine's order: a missing primary logo or
  // colour, then the one pair that refuses.
  const hold = adoptionProblem(after);
  const missing = !after.logos.primary || !after.colors.primary;
  const warnings = warningPairs(results);
  return (
    <Modal
      open={open}
      onClose={onClose}
      title={title}
      placement="top"
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            size="small"
            variant="primary"
            loading={busy}
            disabledReason={hold}
            onClick={onConfirm}
          >
            {confirmLabel}
          </Button>
        </>
      }
    >
      <div className="brand-adopt">
        {changes.length > 0 ? (
          <dl className="brand-diff" aria-label="What changes">
            {changes.map((c) => (
              <div key={c.key} className="brand-diff-row">
                <dt>{c.label}</dt>
                <dd>
                  {c.was ? (
                    <>
                      <del className="brand-diff-was">{c.was}</del>
                      <span aria-hidden="true">{" → "}</span>
                      <VisuallyHidden> to </VisuallyHidden>
                    </>
                  ) : null}
                  {c.now}
                </dd>
              </div>
            ))}
          </dl>
        ) : (
          <Text as="p" variant="body">
            Nothing changes from the published brand.
          </Text>
        )}
        {hold ? (
          <Callout
            variant={missing ? "warning" : "danger"}
            title={missing ? "Draft, not ready to publish" : "This brand cannot be published yet"}
          >
            {hold}
          </Callout>
        ) : null}
        {warnings.length > 0 ? (
          <div className="brand-contrast-head">
            <Text as="span" variant="label" id={warningsHeading}>
              Contrast warnings
            </Text>
            <ContrastHelp />
          </div>
        ) : null}
        {warnings.length > 0 ? (
          <ul className="brand-warnings" aria-labelledby={warningsHeading}>
            {warnings.map((w) => (
              <li key={`${w.foreground}/${w.ground}`}>
                <Tag size="sm" variant="warning" leadingIcon={<TriangleAlertGlyph size="xs" />}>
                  {warningWords(w)}
                </Tag>
              </li>
            ))}
          </ul>
        ) : null}
        <WhenYouAdopt />
        {answer ? (
          <Text as="p" variant="body" className="brand-refusal" role="alert">
            {answer}
          </Text>
        ) : null}
      </div>
    </Modal>
  );
}
