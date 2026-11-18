/**
 * The design system's `DiffView` under the props the dashboard's screens pass:
 * `hunks` for its `sections`. No diff is drawn anywhere else (`test/diffs.test.ts`).
 */

import { Fragment, useRef, type ReactNode } from "react";
import {
  DiffView as KitDiffView,
  type DiffLine,
  type DiffSection,
  type DiffViewProps as KitDiffViewProps,
} from "@crewlethq/ui";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { useNarrowerThan } from "~/lib/useNarrowerThan.ts";

export type { DiffLayout, DiffLine, DiffSpan } from "@crewlethq/ui";

/** A run of lines and how many unchanged lines were left out before it. */
export type DiffHunk = DiffSection & {
  /** Git's own line opening the hunk (`@@ -14,12 +14,14 @@ func New(…)`), where it came from a patch. */
  header?: string | undefined;
};

/** A gap a host may fill: the unchanged lines above hunk `hunk`, or after the last (`hunk` = the hunks' count). */
export interface DiffGap {
  hunk: number;
  /** How many lines it hides; null where the host does not know. */
  skipped: number | null;
  /** The header of the hunk it opens; none after the last. */
  header: string | undefined;
  /** Whether the kit draws this diff split: asked for, and as wide as a phone or wider. */
  split: boolean;
}

/** The kit's estimate of a row's height before it is measured, which it does not export. */
export const ROW_ESTIMATE_PX = 21;

type Source =
  | { hunks: readonly DiffHunk[]; before?: undefined; after?: undefined; context?: undefined }
  | { before: string; after: string; context?: number | undefined; hunks?: undefined };

export type DiffViewProps = Source &
  Omit<KitDiffViewProps, "sections" | "before" | "after" | "context"> & {
    /** Has no effect: every diff is tinted. Kept so a screen that passes it compiles. */
    hue?: "code" | undefined;
    /** A gap's row, opening each hunk in place of the kit's "N unchanged lines": the first above
     *  the diff, each other one under the line above it. */
    expand?: ((gap: DiffGap) => ReactNode) | undefined;
    /** Unchanged lines after the last hunk, null where unknown; with `expand` only, 0 draws no row. */
    trailing?: number | null | undefined;
  };

/** The line a hunk's last row draws: in split, a longer run of removals ends on its last removal. */
function lastRowLine(lines: readonly DiffLine[], split: boolean): DiffLine | undefined {
  const last = lines.at(-1);
  if (!split || !last || last.kind === "same") return last;
  let added = 0;
  const removed: DiffLine[] = [];
  for (let i = lines.length - 1; i >= 0 && lines[i]!.kind !== "same"; i--) {
    if (lines[i]!.kind === "add") added++;
    else removed.push(lines[i]!);
  }
  return removed.length > added ? removed[0] : last;
}

function ExpandableDiff({
  hunks,
  expand,
  trailing = null,
  notes,
  hue: _hue,
  ...rest
}: DiffViewProps & { hunks: readonly DiffHunk[]; expand: (gap: DiffGap) => ReactNode }) {
  // THE LAYOUT THE KIT DRAWS, decided as it does from its root's width, which this box shares.
  const box = useRef<HTMLDivElement>(null);
  const narrow = useNarrowerThan(box, PHONE_BREAKPOINT);
  const keyed = hunks.some((hunk) => hunk.lines.some((line) => line.path !== undefined));
  const split = rest.layout === "split" && !keyed && !narrow;
  const opens = (hunk: DiffHunk | undefined) =>
    hunk !== undefined && (hunk.skipped > 0 || !!hunk.header);
  const gapAt = (hunk: number, skipped: number | null, header: string | undefined) =>
    expand({ hunk, skipped, header, split });
  const under = new Map<DiffLine, ReactNode>();
  hunks.forEach((hunk, i) => {
    if (i === 0 || !opens(hunk)) return;
    const anchor = lastRowLine(hunks[i - 1]!.lines, split);
    if (anchor) under.set(anchor, gapAt(i, hunk.skipped, hunk.header));
  });
  const first = hunks[0];
  // THE KIT DRAWS NO SKIP ROW here: each gap is the host's row, a note on the line above it.
  const sections = hunks.map((hunk) => ({ lines: hunk.lines, skipped: 0 }));
  const withGaps = (line: DiffLine) => {
    const own = notes?.(line);
    if (!under.has(line)) return own;
    return (
      <Fragment>
        {own}
        {under.get(line)}
      </Fragment>
    );
  };
  return (
    <div ref={box}>
      {opens(first) && gapAt(0, first!.skipped, first!.header)}
      <KitDiffView {...rest} sections={sections} notes={withGaps} />
      {hunks.length > 0 && trailing !== 0 && gapAt(hunks.length, trailing, undefined)}
    </div>
  );
}

export function DiffView(props: DiffViewProps) {
  if (props.hunks !== undefined) {
    const { hunks, hue: _hue, expand, trailing: _trailing, ...rest } = props;
    if (expand) return <ExpandableDiff {...props} hunks={hunks} expand={expand} />;
    return <KitDiffView {...rest} sections={hunks} />;
  }
  const { hunks: _hunks, hue: _hue, expand: _expand, trailing: _trailing, ...rest } = props;
  return <KitDiffView {...rest} />;
}
