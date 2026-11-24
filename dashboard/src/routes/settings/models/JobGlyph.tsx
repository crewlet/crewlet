/**
 * Each use of a model by its glyph, one per use wherever it is named: a
 * model's page, Who runs' job rows and the seats' grid. Lucide
 * outlines from @crewlethq/icons, drawn in the text's ink; the Assistant takes
 * its own product mark, the spark the icon rules keep for it alone.
 */

import type { ComponentType } from "react";
import { Tooltip } from "@crewlethq/ui";
import { ProductMark } from "@crewlethq/icons/product-mark";
import {
  CodeGlyph,
  LayoutGridGlyph,
  ListChecksGlyph,
  RefreshCwGlyph,
  type GlyphSize,
} from "@crewlethq/icons/glyphs";
import type { JobCategory } from "~/lib/modelUses.ts";
import { ADVISOR_USE, ASSISTANT_USE, CATEGORY_WORDS, EVERYTHING, categoryTip } from "./words.ts";

export type Job = JobCategory | "assistant" | "advisor" | "everything";

const GLYPHS: Record<
  Exclude<Job, "assistant" | "advisor">,
  ComponentType<{ size?: GlyphSize | number }>
> = {
  coding: CodeGlyph,
  reviews: ListChecksGlyph,
  routines: RefreshCwGlyph,
  everything: LayoutGridGlyph,
};

export function JobGlyph({ job, size = "md" }: { job: Job; size?: GlyphSize }) {
  // THE HELPERS ARE CREWLET'S OWN PRODUCTS, each drawn by its product mark.
  if (job === "assistant" || job === "advisor") return <ProductMark product={job} size={size} />;
  const Glyph = GLYPHS[job];
  return <Glyph size={size} />;
}

/** A use by its glyph and name, with the engine fields it sets in a tooltip: a column's head. */
export function JobName({ job }: { job: JobCategory | "assistant" | "advisor" | "everything" }) {
  const label =
    job === "assistant"
      ? ASSISTANT_USE.label
      : job === "advisor"
        ? ADVISOR_USE.label
        : job === "everything"
          ? EVERYTHING.label
          : CATEGORY_WORDS[job].label;
  const tip =
    job === "assistant"
      ? ASSISTANT_USE.tip
      : job === "advisor"
        ? ADVISOR_USE.tip
        : job === "everything"
          ? EVERYTHING.hint
          : categoryTip(job);
  return (
    <Tooltip content={tip} placement="top">
      <span className="models-job">
        <JobGlyph job={job} size="sm" />
        <span>{label}</span>
      </span>
    </Tooltip>
  );
}
