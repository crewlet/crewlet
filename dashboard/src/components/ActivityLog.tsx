/**
 * An executor's activity transcript drawn as a `ToolCallLog`: its words, each
 * call with its input and result, and the engine's notes (`lib/activity.ts`).
 */

import { useMemo, type ReactNode } from "react";
import { ToolCallLog, type ToolCallLogLabels } from "@crewlethq/ui";
import { activityEntries } from "~/lib/activity.ts";
import { fmtDuration } from "~/lib/format.ts";

const LABELS: Partial<ToolCallLogLabels> = { duration: fmtDuration };

export function ActivityLog({
  text,
  label,
  live = false,
  base = 0,
  caption,
}: {
  text: string;
  label: string;
  /** The run is going: a call with no result is running, and the end is followed. */
  live?: boolean;
  /** The bytes of the transcript before `text`, so rows keep their keys as a live view drops its start. */
  base?: number;
  caption?: ReactNode;
}) {
  const entries = useMemo(() => activityEntries(text, live, base), [text, live, base]);
  return (
    <ToolCallLog
      entries={entries}
      label={label}
      live={live}
      copyText={text}
      caption={caption}
      labels={LABELS}
    />
  );
}
