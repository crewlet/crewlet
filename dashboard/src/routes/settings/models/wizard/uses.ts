/**
 * What a model's uses change: every seat and helper that moves when they go
 * from `was` to `next`, for Review to list before a write, and the helpers a
 * company's first model takes on with everything else.
 */

import type { ChangeGroup, ChangeListRow } from "@crewlethq/ui";
import { listed } from "~/lib/format.ts";
import { CATEGORY_JOBS, CATEGORY_ORDER, type SeatJob, type Uses } from "~/lib/modelUses.ts";
import type { ModelsData } from "../data.ts";
import { ADVISOR_USE, ASSISTANT_USE, CATEGORY_WORDS } from "../words.ts";
import { factsOf } from "./draft.ts";

/** The model each seat runs a job on now, by handle, as the engine reports it. */
function currentFor(data: Pick<ModelsData, "models">, job: SeatJob): Map<string, string> {
  const out = new Map<string, string>();
  for (const m of data.models) for (const seat of m.uses.jobs[job]) out.set(seat, m.key);
  return out;
}

/**
 * Every seat that changes model when `key`'s uses go from `was` to `next`: a
 * group per seat by its name, a row per category, from the model it runs on
 * today to `key`, or taken off `key`. Ask Crewlet and the Advisor under Helpers.
 */
export function seatChangesOf(
  data: Pick<ModelsData, "models" | "seats" | "assistant" | "advisor">,
  key: string,
  was: Uses,
  next: Uses,
): ChangeGroup[] {
  const name = key || "this model";
  const groups: ChangeGroup[] = [];
  for (const s of data.seats) {
    const rows: ChangeListRow[] = [];
    for (const category of CATEGORY_ORDER) {
      const jobs = CATEGORY_JOBS[category];
      const added = jobs.filter(
        (j) => next.jobs[j].includes(s.handle) && !was.jobs[j].includes(s.handle),
      );
      const removed = jobs.filter(
        (j) => was.jobs[j].includes(s.handle) && !next.jobs[j].includes(s.handle),
      );
      const label = CATEGORY_WORDS[category].label;
      if (added.length > 0) {
        const today = [
          ...new Set(
            added.flatMap((j) => {
              const now = currentFor(data, j).get(s.handle);
              return now && now !== key ? [now] : [];
            }),
          ),
        ];
        rows.push(
          today.length > 0
            ? { kind: "changed", label, from: listed(today), to: name }
            : { kind: "added", label, to: name },
        );
      }
      if (removed.length > 0) rows.push({ kind: "removed", label, from: name });
    }
    if (rows.length > 0) groups.push({ key: s.handle, subject: s.name || s.handle, rows });
  }
  const helpers: ChangeListRow[] = [];
  const moved = (label: string, today: string | null): ChangeListRow =>
    today && today !== key
      ? { kind: "changed", label, from: today, to: name }
      : { kind: "added", label, to: name };
  if (next.assistant && !was.assistant)
    helpers.push(moved(ASSISTANT_USE.label, data.assistant.key));
  if (next.advisor && !was.advisor) helpers.push(moved(ADVISOR_USE.label, data.advisor.key));
  if (!next.advisor && was.advisor)
    helpers.push({
      kind: "removed",
      label: ADVISOR_USE.label,
      from: name,
      note: "Its checks run with no model",
    });
  if (helpers.length > 0) groups.push({ key: "helpers", subject: "Helpers", rows: helpers });
  return groups;
}

/**
 * The helpers a model on `harness` takes with everything else: each that runs
 * on none of the company's models (it names none, or one gone), where the
 * harness runs helpers. One on a model keeps it.
 */
export function everythingHelpers(
  data: Pick<ModelsData, "models" | "assistant" | "advisor">,
  harness: string,
): { assistant: boolean; advisor: boolean } {
  const open = (now: string | null) => !now || !data.models.some((m) => m.key === now);
  const runs = !!factsOf(harness).assistant;
  return { assistant: runs && open(data.assistant.key), advisor: runs && open(data.advisor.key) };
}
