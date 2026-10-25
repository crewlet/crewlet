/**
 * Reviewing one version of a brand kit and adopting it: the version's
 * `brand.json` read, compared card by card with the adopted brand (each logo by
 * its bytes), and adopted as it is (`POST /brand/adoptions`).
 *
 * Opened from the newer-version notice on Company › Brand and from a kit's own
 * page. Nothing is published: the version already exists, and adopting it is
 * the one write.
 */

import { useMemo } from "react";
import { Modal, Skeleton, useToast } from "@crewlethq/ui";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { adoptBrand } from "~/lib/brand/api.ts";
import type { Adoption } from "~/lib/brand/brand.ts";
import { adoptionSource, logoDigests } from "~/lib/brand/useBrand.ts";
import { useBrandVersion } from "~/lib/brand/useBrandVersion.ts";
import { AdoptDialog } from "./AdoptDialog.tsx";

export function ReviewAdopt({
  kit,
  version,
  adoption,
  onClose,
  onAdopted,
}: {
  kit: string;
  version: number;
  /** The brand adopted now, or null when none is. */
  adoption: Adoption | null;
  onClose: () => void;
  onAdopted: () => void;
}) {
  const toast = useToast();
  const gesture = useKeyedGesture();
  const read = useBrandVersion(kit, version);
  const doc = read.document;
  const after = useMemo(() => logoDigests(read.source), [read.source]);
  const before = useMemo(() => logoDigests(adoption ? adoptionSource(adoption) : null), [adoption]);

  if (!doc || read.loading) {
    return (
      <Modal open onClose={onClose} title={`Version ${version}`} placement="top">
        {read.error ? (
          <p className="brand-refusal" role="alert">
            {read.error.message}
          </p>
        ) : (
          <Skeleton variant="text" rows={4} label={`Reading version ${version}`} />
        )}
      </Modal>
    );
  }

  const adopt = async () => {
    const answer = await gesture.run(adoptBrand(kit, version));
    if (answer?.kind === "done") {
      toast.ok(`The ${doc.name} brand is adopted`);
      onAdopted();
    }
  };

  return (
    <AdoptDialog
      open
      onClose={onClose}
      title={`Adopt version ${version} as the ${doc.name} brand?`}
      before={adoption?.document ?? null}
      after={doc}
      digests={{ before, after }}
      confirmLabel={`Adopt version ${version}`}
      busy={gesture.busy}
      answer={gesture.answer && gesture.answer.kind !== "done" ? gesture.answer.text : undefined}
      onConfirm={() => void adopt()}
    />
  );
}
