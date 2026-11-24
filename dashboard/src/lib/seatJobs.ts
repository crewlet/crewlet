/**
 * A seat's model jobs in words: the one map the org editor's model fields,
 * Who runs' job rows and every move confirm name them by. The jobs themselves
 * are the engine's (`SEAT_JOB_FIELDS`, held to it by a Go gate).
 */

import { SEAT_JOB_FIELDS } from "~/contract/models.ts";
import type { SeatJob } from "./modelUses.ts";

/** A job written beside the executor's `llm`, in a flat field of its own. */
export type SatelliteJob = Exclude<SeatJob, "executor">;

export const SEAT_JOB_WORDS = {
  executor: { label: "Model" },
  review: { label: "Reviewer", runs: "Checks the executor’s work before the turn ends." },
  subagent: { label: "Sub-agents", runs: "Runs the sub-agents this seat starts." },
  auxiliary: {
    label: "Auxiliary",
    runs: "Runs learning’s summaries and reflection. A fast model keeps them cheap.",
  },
  judge: {
    label: "Round-cap judge",
    runs: "Decides whether a phase that ran out of tool rounds is still making progress.",
  },
  triage: {
    label: "Wake triage",
    runs: "Sorts a message to this seat from someone else while its run works. A fast model keeps it cheap.",
  },
} as const satisfies Record<SeatJob, { label: string }> &
  Record<SatelliteJob, { label: string; runs: string }>;

/** The satellite job a seat field (`llm_review`) is written for. */
export function jobOfField(field: (typeof SEAT_JOB_FIELDS)[SatelliteJob]): SatelliteJob {
  const hit = Object.entries(SEAT_JOB_FIELDS).find(([, f]) => f === field);
  return hit![0] as SatelliteJob;
}
