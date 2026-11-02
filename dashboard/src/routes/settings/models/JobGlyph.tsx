/**
 * Each use of a model by its glyph, one per use wherever it is named: the Use
 * it for list, the per-seat grid, Used by and its tooltip, and By job. Lucide
 * outlines from @crewlethq/icons, drawn in the text's ink; the Assistant takes
 * its own product mark, the spark the icon rules keep for it alone.
 */

import type { ComponentType } from "react";
import { Tooltip } from "@crewlethq/ui";
import { ProductMark } from "@crewlethq/icons/product-mark";
import {
  CircleDashedGlyph,
  CodeGlyph,
  LayoutGridGlyph,
  ListChecksGlyph,
  RefreshCwGlyph,
  type GlyphSize,
} from "@crewlethq/icons/glyphs";
import type { JobCategory } from "~/lib/modelUses.ts";
import { ASSISTANT_USE, CATEGORY_WORDS, categoryTip } from "./words.ts";

export type Job = JobCategory | "assistant" | "everything" | "none";

const GLYPHS: Record<Exclude<Job, "assistant">, ComponentType<{ size?: GlyphSize | number }>> = {
  coding: CodeGlyph,
  reviews: ListChecksGlyph,
  routines: RefreshCwGlyph,
  everything: LayoutGridGlyph,
  none: CircleDashedGlyph,
};

export function JobGlyph({ job, size = "md" }: { job: Job; size?: GlyphSize }) {
  if (job === "assistant") return <ProductMark product="assistant" size={size} />;
  const Glyph = GLYPHS[job];
  return <Glyph size={size} />;
}

/** A use by its glyph and name, with the engine fields it sets in a tooltip: a column's head. */
export function JobName({ job }: { job: JobCategory | "assistant" }) {
  const label = job === "assistant" ? ASSISTANT_USE.label : CATEGORY_WORDS[job].label;
  const tip = job === "assistant" ? ASSISTANT_USE.tip : categoryTip(job);
  return (
    <Tooltip content={tip} placement="top">
      <span className="models-job">
        <JobGlyph job={job} size="sm" />
        <span>{label}</span>
      </span>
    </Tooltip>
  );
}
