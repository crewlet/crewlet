/**
 * How far along a project is — one reading, drawn in the directory's rows and
 * in the project's own header.
 *
 * # AN AMOUNT, NOT A SHARE
 *
 * It was a share: three segments over open, done and closed, each sized
 * against their own sum. So a project holding one open item and nothing else
 * drew a FULL, solid bar — 100% of its work is open — under a legend naming
 * three colours, and read as a project that had finished everything. Every
 * project whose work sits in one state drew the same complete-looking bar,
 * which is the state a young company's projects are all in.
 *
 * A progress meter answers "how much of this is done", so the WHOLE is what
 * has been filed and the FILL is what is done. Closed work is a muted segment
 * beside it, because it left the question rather than answering it, and open
 * work is the TRACK — untinted, because it is precisely what has not been
 * filled. A project with nothing done now draws an empty track, which is the
 * fact, and one that is finished draws a full one.
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
 * `--positive` for done and the data ramp's neutral residual for closed, per
 * `docs/reference/dashboard-design.md` §"The one rule". Nothing here is tinted
 * by WHICH project it is.
 */

import { EmptyValue, Legend, StackedBar, DATA_COLOR_OTHER } from "@crewlethq/ui";
import type { WorkTaskCounts } from "~/protocol/index.ts";

/** Everything ever filed in a project, which is this meter's whole. */
export function filed(counts: WorkTaskCounts): number {
  return counts.open + counts.done + counts.closed;
}

/**
 * The bar alone: done fills, closed is muted beside it, open is the track.
 *
 * A PROJECT WITH NOTHING FILED GETS THE ABSENT MARK rather than a track. A
 * proportion of nothing is undefined rather than zero, and an empty track
 * would say "nothing is done yet" about a project nobody has filed anything
 * in — which is the one distinction this meter now turns on. The two callers
 * differ on where that mark belongs: the directory draws it in the cell, and
 * the project page draws a whole empty state in place of its list, so the
 * page gates before it calls.
 */
export function ProjectProgress({ counts }: { counts: WorkTaskCounts }) {
  const whole = filed(counts);
  if (whole === 0) return <EmptyValue label="Nothing filed yet" />;
  return (
    <StackedBar
      segments={[
        { id: "done", label: "Done", value: counts.done, color: "var(--positive)" },
        { id: "closed", label: "Closed", value: counts.closed, color: DATA_COLOR_OTHER },
        // THE REMAINDER IS A SEGMENT, and it is transparent so the track shows
        // through it. It has to be here: the bar sizes every part against the
        // sum of the parts it is GIVEN, so leaving open out would fill `done`
        // against done + closed — the share reading this meter exists to stop
        // being.
        { id: "open", label: "Open", value: counts.open, color: "transparent" },
      ]}
      // WHAT THE PICTURE SAYS, for a reader who cannot see it — and it is the
      // amount rather than three percentages, because that is the sentence the
      // bar is drawing.
      summary={() =>
        `${counts.done} done of ${whole} filed, ${counts.closed} closed, ${counts.open} open.`
      }
    />
  );
}

/**
 * What FILLS the bar, named.
 *
 * ONLY THE PARTS THAT FILL IT. Open work is the untinted track, so a swatch
 * for it would be a colour that is not on the bar — and the open count is
 * already a fact in the header's own line and a column of its own in the
 * directory.
 *
 * THE COUNTS ARE THE READOUT WHERE A FRAME CAN CARRY THEM: a header is one
 * project, so "Done 40" is a fact about the bar beside it. The directory's is
 * drawn ONCE for a column of forty rows and has no row's numbers to carry,
 * which is why they are optional rather than two components.
 */
export function ProjectProgressLegend({ counts }: { counts?: WorkTaskCounts }) {
  return (
    <Legend
      items={[
        { id: "done", label: "Done", color: "var(--positive)", value: counts?.done },
        { id: "closed", label: "Closed", color: DATA_COLOR_OTHER, value: counts?.closed },
      ]}
    />
  );
}

/**
 * A project's census as a header wears it: the bar, and what fills it.
 *
 * THE WRAPPER IS THE COMPONENT'S, not each caller's. The page wrapped this in
 * `.work-census` and the peek drew it bare, so the same census had a width and
 * a gap on one surface and neither on the other.
 */
export function ProjectCensus({ counts }: { counts: WorkTaskCounts }) {
  return (
    <div className="work-census">
      <ProjectProgress counts={counts} />
      <ProjectProgressLegend counts={counts} />
    </div>
  );
}
