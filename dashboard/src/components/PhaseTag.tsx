/**
 * A phase mark.
 *
 * Phase is the one categorical identity this product spends colour on outside
 * a chart, because it is what a reader follows across the Model, Seat,
 * Activity and Trace screens. The three hues are separated from one another
 * and from every status hue under normal, protan and deutan vision, and the
 * word is always present beside the colour.
 *
 * THE NESTED CALLS TAKE NO HUE. A sub-agent, the round-cap judge and a
 * learning worker all run UNDER one of the three phases, and a fourth and
 * fifth categorical colour on one turn card is a legend nobody is reading.
 * They are neutral, and their own word is what tells them apart.
 *
 * It is a wrapper over the design system's tag rather than a component of its
 * own: the engine's claim is WHICH VOCABULARY a phase belongs to, and how a
 * tag is drawn is uilet's.
 */

import type { ReactNode } from "react";
import { EmptyValue, Tag } from "@crewlethq/ui";
import type { TagVariant } from "@crewlethq/ui";

/** The three phases the engine runs, and nothing else. */
const PHASE_VARIANT: Record<string, TagVariant> = {
  onboarding: "phase-onboarding",
  execute: "phase-execute",
  review: "phase-review",
};

export function PhaseTag({ phase, children }: { phase: string; children?: ReactNode }) {
  const key = (phase || "").toLowerCase();
  return (
    <Tag variant={PHASE_VARIANT[key] ?? "neutral"}>
      {children ?? (key || <EmptyValue label="No phase recorded" />)}
    </Tag>
  );
}
