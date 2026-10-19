/**
 * A dashboard's gallery cover: its first stats' labels with the datasets they
 * are drawn from, or a schematic of its first widgets, from the version's
 * outline and never a screenshot (D64). It shows no value: a figure is each
 * reader's own, and a gallery card is not the place to read every board's
 * sources at once.
 */

import { DashboardCover, type DashboardCoverWidget } from "@crewlethq/ui";
import type { ArtifactListItem, ArtifactOutline, DashboardWidgetType } from "~/protocol/index.ts";
import { useRest } from "~/lib/useRest.ts";
import { readOutline } from "~/lib/artifacts/api.ts";

/** What each widget type looks like in the schematic. */
const SCHEMATIC: Record<DashboardWidgetType, DashboardCoverWidget["kind"] | null> = {
  stat: "stat",
  meter: "stat",
  timeseries: "chart",
  columns: "chart",
  activity: "chart",
  bars: "chart",
  share: "chart",
  table: "table",
  note: "note",
  group: null,
};

export function DashboardCardCover({
  item,
}: {
  item: Pick<ArtifactListItem, "id" | "head_number">;
}) {
  const outline = useRest<ArtifactOutline>(
    item.head_number > 0 ? `outline:${item.id}:${item.head_number}` : null,
    (signal) => readOutline(item.id, item.head_number, signal),
  );
  const board = outline.data?.dashboard;
  if (!board) return null;
  const layout = board.widgets.flatMap((w) => {
    const kind = SCHEMATIC[w.type];
    return kind ? [{ w: w.w, kind }] : [];
  });
  return (
    <DashboardCover
      stats={board.stats.map((s) => ({ label: s.label, source: s.dataset_title }))}
      layout={layout}
    />
  );
}
