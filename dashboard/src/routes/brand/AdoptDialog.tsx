/**
 * Adopting a brand: what changes, which contrast pairs fall short, and what
 * adopting makes public, confirmed once.
 *
 * AN ADMINISTRATOR'S, A PERSON'S, WITH A RECENT PROOF. The engine asks for the
 * proof when it is stale (`StepUp.tsx`) and the adoption goes through once the
 * person confirms. A pair that refuses an adoption holds the confirm with the
 * engine's own reason; the others are listed for the person to accept.
 */

import type { ReactNode } from "react";
import { Button, Callout, Modal, Tag, Text, VisuallyHidden } from "@crewlethq/ui";
import { TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import type { BrandDocument } from "~/protocol/index.ts";
import {
  brandChanges,
  brandContrast,
  refusalWords,
  refusingPair,
  warningPairs,
  warningWords,
  type LogoDigests,
} from "~/lib/brand/brand.ts";

/** What adopting makes public, said in every adopt dialog. */
export const PUBLIC_FACE =
  "Everything here becomes visible to everyone in the company, to every artifact and to agents, including agents talking to people outside the company. Docs and dashboards change now, pages and designs change when they are next published, and existing decks keep their look.";

export interface AdoptDialogProps {
  open: boolean;
  onClose: () => void;
  /** The dialog's question: "Adopt version 9 as the Acme brand?". */
  title: string;
  /** The adopted brand, or null when none is adopted yet. */
  before: BrandDocument | null;
  after: BrandDocument;
  digests: { before: LogoDigests; after: LogoDigests };
  /** The confirm's words: "Adopt version 9", "Save and adopt". */
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
  const results = brandContrast(after.colors);
  const refusing = refusingPair(results);
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
            disabledReason={refusing ? refusalWords(refusing) : undefined}
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
              <div key={c.label} className="brand-diff-row">
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
            Nothing changes from the adopted brand.
          </Text>
        )}
        {refusing ? (
          <Callout variant="danger" title="This brand cannot be adopted yet">
            {refusalWords(refusing)}
          </Callout>
        ) : null}
        {warnings.length > 0 ? (
          <ul className="brand-warnings" aria-label="Contrast warnings">
            {warnings.map((w) => (
              <li key={`${w.foreground}/${w.ground}`}>
                <Tag size="sm" variant="warning" leadingIcon={<TriangleAlertGlyph size="xs" />}>
                  {warningWords(w)}
                </Tag>
              </li>
            ))}
          </ul>
        ) : null}
        <Text as="p" variant="body" tone="secondary">
          {PUBLIC_FACE}
        </Text>
        {answer ? (
          <Text as="p" variant="body" className="brand-refusal" role="alert">
            {answer}
          </Text>
        ) : null}
      </div>
    </Modal>
  );
}
