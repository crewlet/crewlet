/**
 * A place that runs on a model, in words: a seat by its name and a job by the
 * words its category uses, or the Dashboard Assistant.
 *
 * NEVER A SEAT KEY OR A FIELD IN PROSE (design QA M8). The engine addresses a
 * place by a seat's handle and a job (`executor`, `llm_review`); a person
 * reads "Backend Engineer, coding". The handle is kept for the title a long
 * name's ellipsis reveals, and config paths stay inside the list of every
 * change a revision makes.
 */

import type { ModelJob } from "~/contract/models.ts";
import type { Seat } from "~/lib/seats.ts";

/**
 * Each job as a sentence names it, in the words its category's label uses
 * (`CATEGORY_WORDS`), so "Backend Engineer, delegations" reads as part of the
 * category the page offers it under, whichever category that is.
 */
export const JOB_WORDS: Record<ModelJob, string> = {
  executor: "coding",
  review: "reviewing",
  judge: "evaluations",
  subagent: "delegations",
  auxiliary: "routines",
  assistant: "Ask Crewlet's chat",
  deep_work: "Ask Crewlet's deep work",
};

/** A seat's display name, or its handle where the chart does not hold it. */
export function seatName(seats: readonly Seat[], handle: string): string {
  return seats.find((s) => s.handle === handle)?.name || handle;
}

/** Where a reference or a use is, by identity. */
export interface PlaceAt {
  seat?: string | undefined;
  job?: ModelJob | undefined;
  assistant?: "chat" | "deep_work" | undefined;
}

/** "Backend Engineer, coding", or "The Dashboard Assistant's chat". */
export function placeWords(seats: readonly Seat[], at: PlaceAt): string {
  if (at.assistant === "chat") return "Ask Crewlet's chat";
  if (at.assistant === "deep_work") return "Ask Crewlet's deep work";
  const job = at.job ? JOB_WORDS[at.job] : "";
  const seat = at.seat ? seatName(seats, at.seat) : "";
  return [seat, job].filter(Boolean).join(", ");
}
