/**
 * The design system's `DiffView` under the props the dashboard's screens pass:
 * `hunks` for its `sections`. No diff is drawn anywhere else (`test/diffs.test.ts`).
 */

import {
  DiffView as KitDiffView,
  type DiffSection,
  type DiffViewProps as KitDiffViewProps,
} from "@crewlethq/ui";

export type { DiffLayout, DiffLine, DiffSpan } from "@crewlethq/ui";

/** A run of lines and how many unchanged lines were left out before it. */
export type DiffHunk = DiffSection;

type Source =
  | { hunks: readonly DiffHunk[]; before?: undefined; after?: undefined; context?: undefined }
  | { before: string; after: string; context?: number | undefined; hunks?: undefined };

export type DiffViewProps = Source &
  Omit<KitDiffViewProps, "sections" | "before" | "after" | "context"> & {
    /** Has no effect: every diff is tinted. Kept so a screen that passes it compiles. */
    hue?: "code" | undefined;
  };

export function DiffView(props: DiffViewProps) {
  if (props.hunks !== undefined) {
    const { hunks, hue: _hue, ...rest } = props;
    return <KitDiffView {...rest} sections={hunks} />;
  }
  const { hunks: _hunks, hue: _hue, ...rest } = props;
  return <KitDiffView {...rest} />;
}
