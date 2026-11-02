/**
 * What a brand edit lacks or breaks, part by part, in the sentence the field
 * says: shared by the Brand page's card editors and the setup wizard's steps,
 * so a value is refused the same way wherever it is typed.
 *
 * TWO BARS. A DRAFT is published with no more than a name (`brandformat`'s
 * publish rule), so a part's own fields are held to their bounds and nothing
 * more ([partProblem]); an ADOPTION also needs the primary logo, the primary
 * colour and the reading pair ([adoptionProblem]). The engine holds both
 * again, and its refusal is the one that decides.
 */

import {
  BRAND_NAME_MAX_BYTES,
  BRAND_TAGLINE_MAX_BYTES,
  BRAND_VOICE_EXAMPLE_MAX_BYTES,
  BRAND_VOICE_MAX_EXAMPLES,
  BRAND_VOICE_SUMMARY_MAX_BYTES,
} from "~/contract/brand.ts";
import type { BrandDocument, BrandFace } from "~/protocol/index.ts";
import { utf8Bytes } from "../format.ts";
import { brandContrast, refusalWords, refusingPair, type BrandFaceRole } from "./brand.ts";
import { familyProblem, fontCountProblem, fontProblem, type BrandEdit } from "./draft.ts";

/** The parts of a brand an edit is made in: the wizard's steps, the page's cards. */
export type BrandPart = "identity" | "logos" | "colours" | "type" | "voice";

/** The edit with its document changed by `change`, the picked files as they were. */
export function withDocument<E extends BrandEdit>(
  edit: E,
  change: (doc: BrandDocument) => BrandDocument,
): E {
  return { ...edit, document: change(edit.document) };
}

/** A text that names a place to go, which the format refuses in a brand's words. */
const LINKISH = /(https?:\/\/|www\.)/i;

function textProblem(text: string, max: number, what: string): string | undefined {
  const bytes = utf8Bytes(text.trim());
  if (bytes > max) return `${what} is ${bytes} bytes and the most is ${max}.`;
  if (LINKISH.test(text)) {
    return "Leave links out: a brand's words are read as words, never as places to go.";
  }
  return undefined;
}

/** Why the brand's name cannot be used, or undefined. */
export function nameProblem(doc: BrandDocument): string | undefined {
  if (doc.name.trim() === "") return "Name the brand: the company's name as it should appear.";
  return textProblem(doc.name, BRAND_NAME_MAX_BYTES, "The name");
}

/** Why the tagline cannot be used, or undefined. */
export function taglineProblem(doc: BrandDocument): string | undefined {
  return textProblem(doc.tagline ?? "", BRAND_TAGLINE_MAX_BYTES, "The tagline");
}

/** Why each part of the voice cannot be used, by field. */
export function voiceProblems(voice: BrandDocument["voice"]): {
  summary?: string;
  like?: string;
  avoid?: string;
} {
  const list = (lines: readonly string[] | undefined, what: string) => {
    const kept = (lines ?? []).filter((l) => l.trim() !== "");
    if (kept.length > BRAND_VOICE_MAX_EXAMPLES) {
      return `${what} has more than ${BRAND_VOICE_MAX_EXAMPLES} lines. Remove one.`;
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

/** The files the edit picked for a face, in the face's order. */
export function pickedFaceFiles(edit: BrandEdit, role: BrandFaceRole): File[] {
  return (edit.document.type[role].files ?? []).flatMap((f) => {
    const file = edit.files.get(f.path);
    return file ? [file] : [];
  });
}

/** Why a face cannot be used: an uploaded family's name, files and licence. */
export function faceProblem(face: BrandFace, picked: readonly File[]): string | undefined {
  if (face.kit || face.stack) return undefined;
  const family = familyProblem(face.family ?? "");
  if (family) return family;
  const files = face.files ?? [];
  if (files.length === 0) return "Add the family's WOFF2 files.";
  const wrong = fontCountProblem(files.length) ?? fontProblem(picked);
  if (wrong) return wrong;
  if (!face.licensed) return "Confirm the company holds a licence to embed this family on the web.";
  return undefined;
}

/**
 * Why a part of the edit cannot be saved, in the sentence its field says, or
 * undefined. Required for a draft: the name. Required for its step or card to
 * be done: the primary logo, the primary colour, and a text colour that reads
 * on the light ground, since an adoption is refused without them.
 */
export function partProblem(part: BrandPart, edit: BrandEdit): string | undefined {
  const doc = edit.document;
  switch (part) {
    case "identity":
      return nameProblem(doc) ?? taglineProblem(doc);
    case "logos":
      return doc.logos.primary ? undefined : "Add the primary logo, for light grounds.";
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

/**
 * What a draft lacks that an adoption requires, as the words after "add", or
 * undefined when it has both: the engine accepts a draft without them and
 * refuses to adopt one (`missing` in its answer).
 */
function missingWords(doc: BrandDocument): string | undefined {
  const logo = !doc.logos.primary;
  const colour = !doc.colors.primary;
  if (logo && colour) return "a primary logo and colour";
  if (logo) return "a primary logo";
  if (colour) return "a primary colour";
  return undefined;
}

/**
 * What a brand still needs before it can be adopted, as one sentence, or
 * undefined: the primary logo and colour a draft may lack, then the one
 * contrast pair that refuses an adoption.
 */
export function adoptionProblem(doc: BrandDocument): string | undefined {
  const missing = missingWords(doc);
  if (missing) return `Add ${missing}.`;
  const refusing = refusingPair(brandContrast(doc.colors));
  return refusing ? refusalWords(refusing) : undefined;
}

/**
 * A draft's readiness in one sentence, for where a draft is offered: "Not
 * ready to publish: add a primary logo and colour." for one that lacks what
 * an adoption requires, and the refusing pair's reason for one whose text
 * cannot be read; undefined for a draft ready to publish. PUBLISH IS THE
 * PERSON'S WORD for an adoption: the route and the permission keep "adopt".
 */
export function draftReadiness(doc: BrandDocument): string | undefined {
  const missing = missingWords(doc);
  if (missing) return `Not ready to publish: add ${missing}.`;
  const refusing = refusingPair(brandContrast(doc.colors));
  return refusing ? `Not ready to publish. ${refusalWords(refusing)}` : undefined;
}
