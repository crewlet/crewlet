/**
 * The editor each brand card shows while an administrator changes it, and why
 * its Save is held.
 *
 * Every editor is CONTROLLED: it is handed the edit as it stands and reports
 * the next one, so the preview beside the cards draws every keystroke and the
 * page holds the one edit a save publishes. Nothing here writes; the page's
 * Save does, once, for the whole edit.
 *
 * A value the format refuses is said under its field, in the format's bound,
 * and holds Save with the same sentence ([cardProblem]); the engine checks
 * everything again at publish.
 */

import { useState } from "react";
import {
  Checkbox,
  ColorField,
  FilePickButton,
  FormField,
  IconButton,
  Input,
  ListInput,
  LogoSlot,
  Select,
  Text,
  Textarea,
  TypeSpecimen,
  type PickedFile,
} from "@crewlethq/ui";
import { TrashGlyph, UploadGlyph } from "@crewlethq/icons/glyphs";
import {
  BRAND_FONT_STYLES,
  BRAND_KIT_FACES,
  BRAND_NAME_MAX_BYTES,
  BRAND_SYSTEM_STACKS,
  BRAND_TAGLINE_MAX_BYTES,
  BRAND_VOICE_EXAMPLE_MAX_BYTES,
  BRAND_VOICE_MAX_EXAMPLES,
  BRAND_VOICE_SUMMARY_MAX_BYTES,
} from "~/contract/artifacts.ts";
import { utf8Bytes } from "~/lib/format.ts";
import type { BrandDocument, BrandFace } from "~/protocol/index.ts";
import {
  BRAND_COLOR_ROLES,
  BRAND_FACE_ROLES,
  BRAND_LOGO_SLOTS,
  FACE_NAMES,
  brandContrast,
  faceDrawing,
  refusalWords,
  refusingPair,
  type BrandFaceRole,
  type BrandLogoSlot,
} from "~/lib/brand/brand.ts";
import {
  FONT_HINT,
  FONT_WEIGHTS,
  LOGO_HINT,
  familyProblem,
  fontCountProblem,
  fontPath,
  fontProblem,
  logoPath,
  logoProblem,
  type BrandEdit,
} from "~/lib/brand/draft.ts";
import type { BrandAssets } from "~/lib/brand/useBrand.ts";
import { ContrastRow, type BrandCardKey } from "./BrandCards.tsx";

/** What every editor is handed. */
export interface BrandEditorProps {
  edit: BrandEdit;
  onEdit: (next: BrandEdit) => void;
  /** The edit's logos and faces, loaded: what the editor's own previews draw. */
  assets: BrandAssets;
}

/** The edit with its document changed by `change`, the picked files as they were. */
function withDocument(edit: BrandEdit, change: (doc: BrandDocument) => BrandDocument): BrandEdit {
  return { document: change(edit.document), files: edit.files };
}

/** A text that names a place to go, which the format refuses in a brand's words. */
const LINKISH = /(https?:\/\/|www\.)/i;

function textProblem(text: string, max: number, what: string): string | undefined {
  const bytes = utf8Bytes(text.trim());
  if (bytes > max) return `${what} is ${bytes} bytes and the most is ${max}.`;
  if (LINKISH.test(text))
    return "Leave links out: a brand's words are read as words, never as places to go.";
  return undefined;
}

function nameProblem(doc: BrandDocument): string | undefined {
  if (doc.name.trim() === "") return "Name the brand: the company's name as it should appear.";
  return textProblem(doc.name, BRAND_NAME_MAX_BYTES, "The name");
}

function taglineProblem(doc: BrandDocument): string | undefined {
  return textProblem(doc.tagline ?? "", BRAND_TAGLINE_MAX_BYTES, "The tagline");
}

function voiceProblems(voice: BrandDocument["voice"]): {
  summary?: string;
  like?: string;
  avoid?: string;
} {
  const list = (lines: readonly string[] | undefined, what: string) => {
    const kept = (lines ?? []).filter((l) => l.trim() !== "");
    if (kept.length > BRAND_VOICE_MAX_EXAMPLES) {
      return `${what} can hold at most ${BRAND_VOICE_MAX_EXAMPLES} lines.`;
    }
    for (const line of kept) {
      const wrong = textProblem(line, BRAND_VOICE_EXAMPLE_MAX_BYTES, "A line");
      if (wrong) return wrong;
    }
    return undefined;
  };
  const summary = textProblem(voice.summary ?? "", BRAND_VOICE_SUMMARY_MAX_BYTES, "The summary");
  const like = list(voice.write_like, "Write like this");
  const avoid = list(voice.avoid, "Avoid");
  return {
    ...(summary ? { summary } : {}),
    ...(like ? { like } : {}),
    ...(avoid ? { avoid } : {}),
  };
}

function faceProblem(face: BrandFace, picked: readonly File[]): string | undefined {
  if (face.kit || face.stack) return undefined;
  const family = familyProblem(face.family ?? "");
  if (family) return family;
  const files = face.files ?? [];
  if (files.length === 0) return "Add the family's WOFF2 files.";
  const wrong = fontCountProblem(files.length) ?? fontProblem(picked);
  if (wrong) return wrong;
  if (!face.licensed) return "Confirm the company may embed this family in what it publishes.";
  return undefined;
}

/** The files the edit picked for a face, in the face's order. */
function pickedFaceFiles(edit: BrandEdit, role: BrandFaceRole): File[] {
  return (edit.document.type[role].files ?? []).flatMap((f) => {
    const file = edit.files.get(f.path);
    return file ? [file] : [];
  });
}

/**
 * Why a card's Save is held, in the sentence its field says, or undefined when
 * the engine will take the edit. Colours are held for the one contrast pair
 * that refuses an adoption.
 */
export function cardProblem(card: BrandCardKey, edit: BrandEdit): string | undefined {
  const doc = edit.document;
  switch (card) {
    case "logos":
      return (
        nameProblem(doc) ??
        taglineProblem(doc) ??
        (doc.logos.primary ? undefined : "Add the primary logo.")
      );
    case "colours": {
      if (!doc.colors.primary) return "Choose the primary colour.";
      const refusing = refusingPair(brandContrast(doc.colors));
      return refusing ? refusalWords(refusing) : undefined;
    }
    case "type":
      return (
        faceProblem(doc.type.heading, pickedFaceFiles(edit, "heading")) ??
        faceProblem(doc.type.body, pickedFaceFiles(edit, "body"))
      );
    case "voice": {
      const wrong = voiceProblems(doc.voice);
      return wrong.summary ?? wrong.like ?? wrong.avoid;
    }
  }
}

/** The name, the tagline, and the three logos, each on its own ground with a picker. */
export function LogosEditor({ edit, onEdit, assets }: BrandEditorProps) {
  const doc = edit.document;
  const [refused, setRefused] = useState<Partial<Record<BrandLogoSlot, string>>>({});
  const name = nameProblem(doc);
  const tagline = taglineProblem(doc);

  const pick = (slot: BrandLogoSlot, file: File) => {
    const problem = logoProblem(file);
    setRefused((was) => ({ ...was, [slot]: problem ?? undefined }));
    if (problem) return;
    const path = logoPath(slot, file);
    const files = new Map(edit.files);
    const old = doc.logos[slot];
    if (old) files.delete(old);
    files.set(path, file);
    onEdit({ document: { ...doc, logos: { ...doc.logos, [slot]: path } }, files });
  };
  const remove = (slot: BrandLogoSlot) => {
    const files = new Map(edit.files);
    const old = doc.logos[slot];
    if (old) files.delete(old);
    const logos = { ...doc.logos };
    delete logos[slot];
    onEdit({ document: { ...doc, logos }, files });
  };

  return (
    <div className="brand-editor">
      <div className="brand-editor-pair">
        <FormField label="Name" required error={name}>
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid}
              width="full"
              value={doc.name}
              onChange={(e) => onEdit(withDocument(edit, (d) => ({ ...d, name: e.target.value })))}
            />
          )}
        </FormField>
        <FormField label="Tagline" optional error={tagline} helper="One line under the name.">
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              aria-invalid={field.invalid}
              width="full"
              value={doc.tagline ?? ""}
              onChange={(e) =>
                onEdit(withDocument(edit, (d) => ({ ...d, tagline: e.target.value })))
              }
            />
          )}
        </FormField>
      </div>
      <div className="brand-logo-slots">
        {BRAND_LOGO_SLOTS.map((spec) => (
          <LogoSlot
            key={spec.slot}
            label={spec.label}
            description={spec.description}
            ground={spec.ground}
            groundColor={spec.ground === "dark" ? doc.colors.dark_ground : doc.colors.light_ground}
            shape={spec.shape}
            src={assets.logo(spec.slot)}
            alt={`${doc.name} ${spec.label.toLowerCase()} logo`}
            editable
            error={refused[spec.slot]}
            onFile={(file) => pick(spec.slot, file)}
            {...(spec.slot === "primary" ? {} : { onRemove: () => remove(spec.slot) })}
            emptyNote={spec.slot === "primary" ? undefined : spec.emptyNote}
            labels={{ hint: LOGO_HINT }}
          />
        ))}
      </div>
    </div>
  );
}

/** Every colour role as a field, and how the pairs read while they are chosen. */
export function ColoursEditor({ edit, onEdit }: BrandEditorProps) {
  const colors = edit.document.colors;
  const results = brandContrast(colors);
  const refusing = refusingPair(results);
  const set = (role: keyof BrandDocument["colors"], value: string) =>
    onEdit(
      withDocument(edit, (d) => {
        const next = { ...d.colors };
        if (value === "" && role !== "primary") delete next[role];
        else next[role] = value;
        return { ...d, colors: next };
      }),
    );
  return (
    <div className="brand-editor">
      <div className="brand-color-fields">
        {BRAND_COLOR_ROLES.map((spec) => (
          <ColorField
            key={spec.role}
            label={spec.label}
            value={colors[spec.role] ?? ""}
            required={spec.required}
            optional={!spec.required}
            helper={spec.description}
            onChange={(value) => set(spec.role, value)}
          />
        ))}
      </div>
      <ContrastRow results={results} />
      {refusing ? (
        <Text as="p" variant="body" className="brand-refusal" role="alert">
          {refusalWords(refusing)}
        </Text>
      ) : null}
    </div>
  );
}

/** The value the face Select holds for a face. */
function sourceOf(face: BrandFace): string {
  if (face.kit) return `kit:${face.kit}`;
  if (face.stack) return `stack:${face.stack}`;
  return "upload";
}

const FACE_OPTIONS = [
  ...BRAND_KIT_FACES.map((kit) => ({ value: `kit:${kit}`, label: FACE_NAMES[kit] })),
  ...BRAND_SYSTEM_STACKS.map((stack) => ({ value: `stack:${stack}`, label: FACE_NAMES[stack] })),
  { value: "upload", label: "An uploaded family (WOFF2)" },
];

const WEIGHT_OPTIONS = FONT_WEIGHTS.map((w) => ({ value: String(w), label: String(w) }));
/** What each style a face's file may draw is called where it is chosen. */
const STYLE_WORDS: Readonly<Record<(typeof BRAND_FONT_STYLES)[number], string>> = {
  normal: "Upright",
  italic: "Italic",
};
const STYLE_OPTIONS = BRAND_FONT_STYLES.map((style) => ({
  value: style,
  label: STYLE_WORDS[style],
}));

/** The heading face and the body face: a kit face, a system face, or a family the company uploads. */
export function TypographyEditor({ edit, onEdit, assets }: BrandEditorProps) {
  return (
    <div className="brand-editor">
      {BRAND_FACE_ROLES.map(({ role, label }) => (
        <FaceEditor
          key={role}
          role={role}
          label={label}
          edit={edit}
          onEdit={onEdit}
          assets={assets}
        />
      ))}
    </div>
  );
}

function FaceEditor({
  role,
  label,
  edit,
  onEdit,
  assets,
}: BrandEditorProps & { role: BrandFaceRole; label: string }) {
  const face = edit.document.type[role];
  const [refused, setRefused] = useState<string | null>(null);
  const setFace = (next: BrandFace, files: Map<string, File> = new Map(edit.files)) =>
    onEdit({
      document: { ...edit.document, type: { ...edit.document.type, [role]: next } },
      files,
    });
  const choose = (value: string) => {
    // LEAVING AN UPLOADED FAMILY drops the files picked for it: a face
    // names one source.
    const files = new Map(edit.files);
    for (const f of face.files ?? []) files.delete(f.path);
    if (value.startsWith("kit:")) setFace({ kit: value.slice(4) }, files);
    else if (value.startsWith("stack:")) setFace({ stack: value.slice(6) }, files);
    else setFace({ family: "", files: [], licensed: false }, files);
  };
  const add = (picked: PickedFile[]) => {
    const list = face.files ?? [];
    const incoming = picked.map((p) => p.file);
    const problem =
      fontCountProblem(list.length + incoming.length) ??
      fontProblem([...pickedFaceFiles(edit, role), ...incoming]);
    setRefused(problem);
    if (problem) return;
    const used = new Set(list.map((f) => f.path));
    const files = new Map(edit.files);
    const added = incoming.map((file) => {
      let i = 0;
      while (used.has(fontPath(role, i))) i++;
      const path = fontPath(role, i);
      used.add(path);
      files.set(path, file);
      return { path, weight: 400, style: "normal" as const };
    });
    setFace({ ...face, files: [...list, ...added] }, files);
  };
  const removeFile = (path: string) => {
    const files = new Map(edit.files);
    files.delete(path);
    setFace({ ...face, files: (face.files ?? []).filter((f) => f.path !== path) }, files);
  };
  const setFile = (path: string, change: { weight?: number; style?: "normal" | "italic" }) =>
    setFace({
      ...face,
      files: (face.files ?? []).map((f) => (f.path === path ? { ...f, ...change } : f)),
    });

  const drawn = faceDrawing(face, assets.face(role));
  const uploading = sourceOf(face) === "upload";
  const family = uploading ? familyProblem(face.family ?? "") : null;
  return (
    <fieldset className="brand-face-editor">
      <legend className="brand-face-legend">{label}</legend>
      <FormField label="Face">
        {(field) => (
          <Select
            id={field.id}
            ariaLabel={`${label} face`}
            value={sourceOf(face)}
            options={FACE_OPTIONS}
            onChange={(value) => choose(String(value))}
          />
        )}
      </FormField>
      {uploading ? (
        <>
          <FormField
            label="Family name"
            required
            error={face.family ? (family ?? undefined) : undefined}
            helper="As its publisher names it. Letters, digits, hyphens and single spaces."
          >
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                aria-invalid={field.invalid}
                width="full"
                value={face.family ?? ""}
                onChange={(e) => setFace({ ...face, family: e.target.value })}
              />
            )}
          </FormField>
          <ul className="brand-font-files" aria-label={`${label} files`}>
            {(face.files ?? []).map((f, i) => (
              <li key={f.path} className="brand-font-file">
                <span className="brand-font-file-name">
                  {edit.files.get(f.path)?.name ?? `File ${i + 1}`}
                </span>
                <Select
                  ariaLabel={`Weight of file ${i + 1}`}
                  size="sm"
                  value={String(f.weight)}
                  options={WEIGHT_OPTIONS}
                  onChange={(value) => setFile(f.path, { weight: Number(value) })}
                />
                <Select
                  ariaLabel={`Style of file ${i + 1}`}
                  size="sm"
                  value={f.style}
                  options={STYLE_OPTIONS}
                  onChange={(value) =>
                    setFile(f.path, { style: value === "italic" ? "italic" : "normal" })
                  }
                />
                <IconButton
                  label={`Remove file ${i + 1}`}
                  icon={<TrashGlyph size="sm" />}
                  size="sm"
                  variant="ghost"
                  onClick={() => removeFile(f.path)}
                />
              </li>
            ))}
          </ul>
          <div className="brand-font-pick">
            <FilePickButton
              size="small"
              variant="secondary"
              leadingIcon={<UploadGlyph size="sm" />}
              accept=".woff2,font/woff2"
              multiple
              onFiles={add}
            >
              Add WOFF2 files
            </FilePickButton>
            <Text as="span" variant="caption" tone="secondary">
              {FONT_HINT}
            </Text>
          </div>
          {refused ? (
            <Text as="p" variant="body" className="brand-refusal" role="alert">
              {refused}
            </Text>
          ) : null}
          <Checkbox
            label="The company holds a licence to embed this family in what it publishes"
            checked={face.licensed === true}
            onCheckedChange={(checked) => setFace({ ...face, licensed: checked })}
          />
        </>
      ) : null}
      <TypeSpecimen
        label={label}
        family={drawn.name || "Unnamed family"}
        fontFamily={drawn.stack || undefined}
        detail={drawn.detail}
        weight={role === "heading" ? "semibold" : "regular"}
        size="sm"
      />
    </fieldset>
  );
}

/** The voice: a summary, and up to five lines each of what to write like and what to avoid. */
export function VoiceEditor({ edit, onEdit }: BrandEditorProps) {
  const voice = edit.document.voice;
  const wrong = voiceProblems(voice);
  const setVoice = (next: BrandDocument["voice"]) =>
    onEdit(withDocument(edit, (d) => ({ ...d, voice: next })));
  return (
    <div className="brand-editor">
      <FormField
        label="Summary"
        optional
        error={wrong.summary}
        helper="How the company sounds, in a sentence or two."
      >
        {(field) => (
          <Textarea
            id={field.id}
            aria-describedby={field.describedBy}
            aria-invalid={field.invalid}
            rows={3}
            autoResize
            value={voice.summary ?? ""}
            onChange={(e) => setVoice({ ...voice, summary: e.target.value })}
          />
        )}
      </FormField>
      <div className="brand-voice-lists">
        <ListInput
          label="Write like this"
          itemName="line"
          optional
          value={voice.write_like ?? []}
          onChange={(next) => setVoice({ ...voice, write_like: next })}
          error={wrong.like}
          helper={`Up to ${BRAND_VOICE_MAX_EXAMPLES} short lines.`}
        />
        <ListInput
          label="Avoid"
          itemName="line"
          optional
          value={voice.avoid ?? []}
          onChange={(next) => setVoice({ ...voice, avoid: next })}
          error={wrong.avoid}
          helper={`Up to ${BRAND_VOICE_MAX_EXAMPLES} short lines.`}
        />
      </div>
    </div>
  );
}
