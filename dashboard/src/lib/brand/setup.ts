/**
 * The brand setup's steps: what each is, when it is done, where a setup that
 * was left resumes, and which kit it writes.
 *
 * # Done is read off the draft, never remembered
 *
 * Every Continue saves the step into a draft version of the company's kit, so
 * what a step holds is the draft's own: Identity is done with a name, Logos
 * with the primary logo, Colours with the primary colour and a text colour that
 * reads, Typography with a face chosen other than the kit's own, and Voice
 * with any line of it. A setup opened again, on any browser, resumes at the
 * first step that is not done ([resumeStep]); a step skipped for now is not
 * done, and a reader who skipped both optional steps lands on Typography,
 * where Skip for now takes them on.
 */

import type { ArtifactListItem, BrandDocument } from "~/protocol/index.ts";
import { roleIncludes } from "../artifacts/words.ts";
import { DEFAULT_FACE, brandContrast, refusingPair, warningPairs } from "./brand.ts";
import { partProblem, type BrandPart } from "./validate.ts";
import type { BrandEdit } from "./draft.ts";

/** A step of the setup. */
export type SetupStepId = "identity" | "logos" | "colours" | "type" | "voice" | "review";

/** What a step is: its name, whether it may be skipped, the part of the brand it edits and why it matters. */
export interface SetupStepSpec {
  id: SetupStepId;
  label: string;
  optional: boolean;
  part?: BrandPart;
  /** One sentence over the step: why it matters to agents and what they make. */
  lead: string;
}

export const SETUP_STEPS: readonly SetupStepSpec[] = [
  {
    id: "identity",
    label: "Identity",
    optional: false,
    part: "identity",
    lead: "This is how agents name the company in what they make.",
  },
  {
    id: "logos",
    label: "Logos",
    optional: false,
    part: "logos",
    lead: "Agents put these on every doc header, slide and page they make.",
  },
  {
    id: "colours",
    label: "Colours",
    optional: false,
    part: "colours",
    lead: "Every branded doc, deck and page is painted in these, so each must read on its ground.",
  },
  {
    id: "type",
    label: "Typography",
    optional: true,
    part: "type",
    lead: "Headings and body text in what agents make are set in these faces.",
  },
  {
    id: "voice",
    label: "Voice",
    optional: true,
    part: "voice",
    lead: "How the company sounds, which agents follow when they write an artifact.",
  },
  {
    id: "review",
    label: "Review",
    optional: false,
    lead: "Check the brand before it becomes the look of everything agents make.",
  },
];

/** A step's spec by id. */
export function stepSpec(id: SetupStepId): SetupStepSpec {
  return SETUP_STEPS.find((s) => s.id === id)!;
}

/** Whether a face is anything other than the kit's own. */
function chosen(face: BrandDocument["type"]["heading"]): boolean {
  return JSON.stringify(face) !== JSON.stringify(DEFAULT_FACE);
}

/** Whether a step's part of the draft is done (Review never is: it is where a setup ends). */
export function stepDone(id: SetupStepId, edit: BrandEdit): boolean {
  const doc = edit.document;
  const spec = stepSpec(id);
  if (!spec.part || partProblem(spec.part, edit)) return false;
  switch (id) {
    case "type":
      return chosen(doc.type.heading) || chosen(doc.type.body);
    case "voice":
      return Boolean(
        doc.voice.summary?.trim() || doc.voice.write_like?.length || doc.voice.avoid?.length,
      );
    default:
      return true;
  }
}

/** Whether a step holds something the setup allows but a person should look at: a low contrast pair, a refused file. */
export function stepAttention(id: SetupStepId, edit: BrandEdit, refusedLogo: boolean): boolean {
  if (id === "logos") return refusedLogo;
  if (id !== "colours") return false;
  const results = brandContrast(edit.document.colors);
  return warningPairs(results).length > 0 || refusingPair(results) !== undefined;
}

/** Where a setup opened on this draft resumes: the first step that is not done, or Review. */
export function resumeStep(edit: BrandEdit): SetupStepId {
  return SETUP_STEPS.find((s) => s.id !== "review" && !stepDone(s.id, edit))?.id ?? "review";
}

/** The step after `id`, or Review. */
export function nextStep(id: SetupStepId): SetupStepId {
  const at = SETUP_STEPS.findIndex((s) => s.id === id);
  return SETUP_STEPS[Math.min(at + 1, SETUP_STEPS.length - 1)]!.id;
}

/** The step before `id`, or the first. */
export function previousStep(id: SetupStepId): SetupStepId {
  const at = SETUP_STEPS.findIndex((s) => s.id === id);
  return SETUP_STEPS[Math.max(at - 1, 0)]!.id;
}

/** A brand with nothing chosen but the company's name and the kit's own faces. */
export function blankBrand(name: string): BrandDocument {
  return {
    v: 1,
    name,
    logos: {},
    colors: {},
    type: { heading: DEFAULT_FACE, body: DEFAULT_FACE },
    voice: {},
  };
}

/**
 * The kit a setup writes when no brand is adopted: the most recently changed
 * brand kit the reader may write (an administrator may write any), which is
 * the draft a setup left; null when there is none and the first Continue
 * makes one.
 */
export function draftKit(
  kits: readonly ArtifactListItem[],
  admin: boolean,
): ArtifactListItem | null {
  return (
    kits.find(
      (k) => k.kind === "brand" && k.status === "live" && (admin || roleIncludes(k.role, "editor")),
    ) ?? null
  );
}
