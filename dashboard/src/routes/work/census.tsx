/**
 * How far along a project is — one reading, drawn in the directory's rows and
 * in the project's own header.
 *
 * # THE SPLIT OF ITS WORK, NOT A SHARE OF ITS HISTORY
 *
 * It was two things in turn. First a share — three segments over open, done
 * and closed, each sized against their own sum — so a project holding one
 * open item drew a FULL bar and read as finished. Then an amount of DONE over
 * everything ever filed, with unfinished work as an untinted track — which
 * answered "how much is done" and drew the two unfinished states, waiting and
 * started, as one blank: a project with nine waiting and nobody on any of them
 * and one with nine under way were the same picture, and those are the two a
 * lead acts on oppositely.
 *
 * The approved design draws the project's WORK split into the three states it
 * is in — done, active, still to do — on the kit's `SegmentedMeter`, whose
 * contract is exactly that: parts in their states' tones, the quiet remainder
 * for the part nobody has started. The whole is `todo + active + done`, the
 * maintained census (`work_projects`' `task_counts`), so the three parts ARE
 * the census and nothing here counts anything.
 *
 * CLOSED IS NOT PART OF IT. A closed task left the question rather than
 * answering it — nobody delivered it and nobody will — so it is neither
 * progress nor work still to do, and a bar that drew it would put abandoned
 * work on one side of the finish line or the other. It has its own column in
 * the directory and its own figure in the header.
 *
 * # One component, two frames
 *
 * The directory draws forty of these in a column and a project's header draws
 * one, and a reader clicks a row in the first to land on the second. Written
 * twice they would disagree the first time either was tuned, and the
 * disagreement would be invisible — both drawings look right on their own.
 *
 * # Colour is on state only
 *
 * Done is the success tone, active the info tone (working — `in_review` is
 * working too, per `docs/reference/dashboard-design.md` §"The one rule"), and
 * still-to-do is the remainder's quiet hairline colour. Nothing is tinted by
 * WHICH project it is.
 */

import { EmptyValue, Legend, SegmentedMeter } from "@crewlethq/ui";
import type { WorkTaskCounts } from "~/protocol/index.ts";
import { unfinished } from "~/lib/work.ts";

/** Everything ever filed in a project — what "nothing filed" is decided against. */
export function filed(counts: WorkTaskCounts): number {
  return unfinished(counts) + counts.done + counts.closed;
}

/**
 * The whole the bar divides: the project's work that is done, active or still
 * to do. Closed work is not in it — see the file's doc.
 */
export function progressWhole(counts: WorkTaskCounts): number {
  return counts.todo + counts.active + counts.done;
}

/**
 * The parts the bar draws, in the order drawn from the leading end: what is
 * done, then what is under way. The rest of the whole — still to do — is the
 * meter's remainder, so the parts and the remainder are the census exactly.
 */
export function progressSegments(counts: WorkTaskCounts) {
  return [
    { id: "done", label: "done", value: counts.done, tone: "success" as const },
    { id: "active", label: "active", value: counts.active, tone: "info" as const },
  ];
}

/**
 * The bar alone.
 *
 * A PROJECT WITH NO WORK IN IT GETS THE ABSENT MARK rather than an empty bar.
 * A split of nothing is undefined rather than zero, and an empty remainder
 * would say "all of it is still to do" about a project nobody has filed
 * anything in. The two callers differ on where that mark belongs: the
 * directory draws it in the cell, and the project page draws a whole empty
 * state in place of its list, so the page gates before it calls.
 */
export function ProjectProgress({
  counts,
  size = "compact",
}: {
  counts: WorkTaskCounts;
  size?: "compact" | "default";
}) {
  const whole = progressWhole(counts);
  if (whole === 0) {
    return (
      <EmptyValue
        label={counts.closed > 0 ? "Only closed work — nothing to do" : "Nothing filed yet"}
      />
    );
  }
  return (
    <SegmentedMeter
      size={size}
      segments={progressSegments(counts)}
      total={whole}
      remainderLabel="to do"
    />
  );
}

/**
 * What the bar's parts are, named.
 *
 * THE COUNTS ARE THE READOUT WHERE A FRAME CAN CARRY THEM: a header is one
 * project, so "Done 40" is a fact about the bar beside it. The directory's is
 * drawn ONCE for a column of forty rows and has no row's numbers to carry,
 * which is why they are optional rather than two components.
 *
 * THE SWATCHES ARE THE METER'S OWN TOKENS — each tone's fill and the
 * remainder's hairline — so a legend can never name a colour the bar is not.
 */
export function ProjectProgressLegend({ counts }: { counts?: WorkTaskCounts }) {
  return (
    <Legend
      items={[
        { id: "done", label: "Done", color: "var(--color-feedback-success)", value: counts?.done },
        {
          id: "active",
          label: "Active",
          color: "var(--color-feedback-info)",
          value: counts?.active,
        },
        { id: "todo", label: "To do", color: "var(--color-border-strong)", value: counts?.todo },
      ]}
    />
  );
}

/**
 * A project's census as a header wears it: the bar, and what it is made of.
 *
 * THE WRAPPER IS THE COMPONENT'S, not each caller's. The page wrapped this in
 * `.work-census` and the peek drew it bare, so the same census had a width and
 * a gap on one surface and neither on the other.
 */
export function ProjectCensus({ counts }: { counts: WorkTaskCounts }) {
  return (
    <div className="work-census">
      <ProjectProgress counts={counts} size="default" />
      <ProjectProgressLegend counts={counts} />
    </div>
  );
}
