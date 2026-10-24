/**
 * Setting the company's brand up: what a brand cannot be without (its name,
 * its primary logo and its primary colour), saved as the company's first kit
 * and adopted in one gesture. Everything else is added card by card after.
 */

import { useMemo, useState } from "react";
import { Button, ColorField, FormField, Input, LogoSlot, Modal, Text } from "@crewlethq/ui";
import type { BrandDocument } from "~/protocol/index.ts";
import { BRAND_NAME_MAX_BYTES } from "~/contract/artifacts.ts";
import { utf8Bytes } from "~/lib/format.ts";
import { DEFAULT_FACE, brandContrast, refusalWords, refusingPair } from "~/lib/brand/brand.ts";
import {
  LOGO_HINT,
  draftSource,
  logoPath,
  logoProblem,
  saveFiles,
  type BrandEdit,
} from "~/lib/brand/draft.ts";
import { useBrandAssets } from "~/lib/brand/useBrand.ts";
import { useBrandSave } from "~/lib/brand/useBrandSave.ts";
import { PUBLIC_FACE } from "./AdoptDialog.tsx";

/** A brand with nothing chosen but the company's name and the kit's own face. */
function blank(name: string): BrandDocument {
  return {
    v: 1,
    name,
    logos: { primary: "" },
    colors: { primary: "" },
    type: { heading: DEFAULT_FACE, body: DEFAULT_FACE },
    voice: {},
  };
}

export function SetupDialog({
  company,
  onClose,
  onAdopted,
}: {
  /** The company's name, which the brand starts with. */
  company: string;
  onClose: () => void;
  /** The brand was adopted, or a version saved and not adopted, in `notice`'s words. */
  onAdopted: (notice: string | null) => void;
}) {
  const [edit, setEdit] = useState<BrandEdit>(() => ({
    document: blank(company),
    files: new Map(),
  }));
  const [refused, setRefused] = useState<string | null>(null);
  const [failed, setFailed] = useState<string | null>(null);
  const save = useBrandSave();
  const source = useMemo(() => draftSource(null, edit), [edit]);
  const assets = useBrandAssets(source);
  const doc = edit.document;

  const name = doc.name.trim();
  const nameWrong =
    name === ""
      ? "Name the brand: the company's name as it should appear."
      : utf8Bytes(name) > BRAND_NAME_MAX_BYTES
        ? `The name is ${utf8Bytes(name)} bytes and the most is ${BRAND_NAME_MAX_BYTES}.`
        : undefined;
  const refusing = refusingPair(brandContrast(doc.colors));
  const hold =
    nameWrong ??
    (doc.logos.primary ? undefined : "Add the primary logo.") ??
    (doc.colors.primary ? undefined : "Choose the primary colour.") ??
    (refusing ? refusalWords(refusing) : undefined);

  const pick = (file: File) => {
    const problem = logoProblem(file);
    setRefused(problem);
    if (problem) return;
    const path = logoPath("primary", file);
    setEdit({ document: { ...doc, logos: { primary: path } }, files: new Map([[path, file]]) });
  };

  const adopt = async () => {
    if (hold) return;
    setFailed(null);
    let files;
    try {
      files = await saveFiles(null, edit);
    } catch (err) {
      setFailed(err instanceof Error ? err.message : String(err));
      return;
    }
    const answer = await save.run({
      kit: null,
      head: 0,
      title: `${name} brand`,
      document: { ...doc, name },
      files,
    });
    if (answer?.kind === "adopted") onAdopted(null);
    else if (answer?.kind === "published") onAdopted(answer.text);
  };

  const answer = save.answer;
  const said =
    failed ??
    (answer && (answer.kind === "refused" || answer.kind === "unknown") ? answer.text : null);
  return (
    <Modal
      open
      onClose={onClose}
      title="Set up your brand"
      subtitle="Start with the name, the logo and the main colour. Add the rest card by card."
      placement="top"
      footer={
        <>
          <Button size="small" variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            size="small"
            variant="primary"
            loading={save.busy}
            disabledReason={hold}
            onClick={() => void adopt()}
          >
            Adopt as the company brand
          </Button>
        </>
      }
    >
      <div className="brand-setup">
        <FormField label="Name" required error={doc.name === company ? undefined : nameWrong}>
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid}
              width="full"
              value={doc.name}
              onChange={(e) => setEdit({ ...edit, document: { ...doc, name: e.target.value } })}
            />
          )}
        </FormField>
        <LogoSlot
          label="Primary logo"
          description="On light grounds"
          ground="light"
          src={assets.logo("primary")}
          alt={`${name || company} logo`}
          editable
          error={refused}
          onFile={pick}
          labels={{ hint: LOGO_HINT }}
        />
        <ColorField
          label="Primary colour"
          required
          helper="Links, buttons and accents"
          value={doc.colors.primary}
          onChange={(value) =>
            setEdit({ ...edit, document: { ...doc, colors: { ...doc.colors, primary: value } } })
          }
        />
        <Text as="p" variant="body" tone="secondary">
          {PUBLIC_FACE}
        </Text>
        {said ? (
          <Text as="p" variant="body" className="brand-refusal" role="alert">
            {said}
          </Text>
        ) : null}
      </div>
    </Modal>
  );
}
