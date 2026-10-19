/**
 * A dashboard, drawn here: its filters, its board of widgets and the Sources
 * rail that says where every figure came from and when it was true.
 *
 * NATIVE, NEVER FRAMED (D61). A board runs no author code: the engine answers
 * the version's spec held to the board format, and every widget is drawn by
 * the design system's own primitive for its type.
 *
 * EVERY FIGURE IS THIS READER'S (ADR-0043). The board carries no row, each
 * dataset is read through the engine as whoever is signed in, and a dataset
 * the reader may not read is drawn as the engine's refusal, never as an empty
 * figure. Two people reading one board see what each may read.
 *
 * READ ONLY IN THIS RELEASE: a board is arranged by publishing a new version of
 * `dashboard.json` (an agent's `publish_artifact`, or an upload), so what a
 * person sees is always a version every other reader can open.
 */

import { useLayoutEffect, useRef, useState } from "react";
import {
  ArrangeBoard,
  ArrangeFrame,
  ArrangeGrid,
  ArrangeGroup,
  Button,
  Card,
  DatasetSource,
  DatasetSourceList,
  FormField,
  Select,
  Skeleton,
  type SurfaceTone,
} from "@crewlethq/ui";
import { DASHBOARD_COLUMNS } from "~/contract/artifacts.ts";
import type {
  ArtifactDetail,
  DashboardBoardDataset,
  DashboardWidget as Widget,
} from "~/protocol/index.ts";
import { QueryState } from "~/components/common.tsx";
import type { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import {
  boardSpan,
  datasetStatus,
  definitionWords,
  refreshWords,
  rowsWords,
} from "~/lib/artifacts/dashboard.ts";
import { useBoardData, type BoardDataOptions } from "~/lib/artifacts/useBoardData.ts";
import { useNarrowerThan } from "~/lib/useNarrowerThan.ts";
import { useWidth } from "~/lib/useWidth.ts";
import { DashboardWidget, RowsTable } from "./DashboardWidget.tsx";
import type { PresenceReport } from "./presence.ts";

/** The height of one board row, in pixels: the Home board's. */
const ROW = 16;

/** Below this width the board draws every widget across the page. */
const STACKS_BELOW = 720;

/** How many rows the Sources rail previews of a selected dataset. */
const PREVIEW_ROWS = 5;

export function DashboardView({
  detail,
  version,
  store,
  presence,
  options,
}: {
  detail: ArtifactDetail;
  version: number;
  store: ArtifactDataStore | null;
  presence?: PresenceReport | undefined;
  /** The refresh timings, for a suite. */
  options?: BoardDataOptions;
}) {
  const data = useBoardData(detail.id, version, store, options);
  const [selected, setSelected] = useState<string | null>(null);
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set());
  const root = useRef<HTMLDivElement>(null);
  const boardRef = useRef<HTMLDivElement>(null);
  const stacked = useNarrowerThan(root, STACKS_BELOW, data.board.data !== null);
  const boardWidth = useWidth(boardRef, data.board.data !== null);
  // The grid's gap, as its stylesheet sets it (it follows the density), read
  // whenever the board's width moves.
  const [gap, setGap] = useState(0);
  useLayoutEffect(() => {
    const grid = boardRef.current?.querySelector(".crewlet-arrange-grid");
    if (grid) setGap(Number.parseFloat(getComputedStyle(grid).columnGap) || 0);
  }, [boardWidth]);
  const now = Date.now();
  const answer = data.board.data;

  if (!answer) {
    return data.board.loading ? (
      <Skeleton variant="grid" count={4} label="Loading the dashboard" />
    ) : (
      <QueryState error={data.board.code} refusal={data.board.refusal} loading={false} />
    );
  }
  const spec = answer.board;
  const byId = new Map(answer.datasets.map((d) => [d.id, d]));
  /*
   * A widget's span on this board's width (see boardSpan). A group's children
   * stand on the group's own twelve columns, inside its inset, which is the
   * board's gap a side.
   */
  const spanOf = (w: Widget, inGroup: boolean) =>
    stacked
      ? DASHBOARD_COLUMNS
      : boardSpan(w.w, inGroup ? boardWidth - 2 * gap : boardWidth, gap, DASHBOARD_COLUMNS);
  const statusOf = (d: DashboardBoardDataset | undefined) =>
    d ? datasetStatus(d, data.datasets.get(d.id), now) : "loading";

  const renderLeaf = (w: Widget, inGroup = false) => {
    const d = w.dataset ? byId.get(w.dataset) : undefined;
    const state = d ? data.datasets.get(d.id) : undefined;
    const got = state?.answer ?? null;
    return (
      <ArrangeFrame
        key={w.id}
        name={w.title ?? w.id}
        editing={false}
        bare={w.type === "note"}
        columns={spanOf(w, inGroup)}
        rows={w.h ?? null}
        rowHeight={ROW}
        selected={d !== undefined && selected === d.id}
        className="dashboard-cell"
      >
        <DashboardWidget
          boardTitle={spec.title ?? detail.title}
          widget={w}
          dataset={d}
          answer={got}
          status={statusOf(d)}
          detail={state?.failure ?? got?.detail}
          asOf={got?.as_of}
          onCite={d ? () => setSelected(d.id) : undefined}
          onFocus={() => presence?.at({ region: `widget:${w.id}` }, false)}
        />
      </ArrangeFrame>
    );
  };

  return (
    <div className="dashboard-view" ref={root}>
      <div className="dashboard-bar">
        {(spec.params ?? []).map((p) => (
          <FormField key={p.key} label={p.label} className="dashboard-filter">
            <Select
              value={data.params[p.key]}
              options={p.options.map((o) => ({ value: o.value, label: o.label }))}
              onChange={(v) => data.setParam(p.key, String(v))}
            />
          </FormField>
        ))}
        <Button variant="secondary" onClick={() => data.refresh()}>
          Refresh
        </Button>
      </div>
      <div className="dashboard-body">
        <div className="dashboard-board" ref={boardRef}>
          <ArrangeBoard editing={false} columns={DASHBOARD_COLUMNS}>
            <ArrangeGrid>
              {spec.widgets.map((w) =>
                w.type === "group" ? (
                  <ArrangeFrame
                    key={w.id}
                    name={w.title ?? w.id}
                    editing={false}
                    container
                    columns={spanOf(w, false)}
                    rows={null}
                    rowHeight={ROW}
                    className="dashboard-cell"
                  >
                    <ArrangeGroup
                      title={w.title ?? ""}
                      tone={(w.tone ?? "none") as SurfaceTone}
                      collapsed={collapsed.has(w.id)}
                      onCollapsedChange={(c) =>
                        setCollapsed((prev) => {
                          const next = new Set(prev);
                          if (c) next.add(w.id);
                          else next.delete(w.id);
                          return next;
                        })
                      }
                      count={w.children?.length ?? 0}
                      columns={DASHBOARD_COLUMNS}
                      empty="This group holds no widgets."
                    >
                      {(w.children ?? []).map((c) => renderLeaf(c, true))}
                    </ArrangeGroup>
                  </ArrangeFrame>
                ) : (
                  renderLeaf(w)
                ),
              )}
            </ArrangeGrid>
          </ArrangeBoard>
        </div>
        {/* ONE CARD, as every panel beside a board is: the rail's heading is
            the card's, and the entries are rows of it rather than loose
            blocks on the page, its top level with the board's first row. */}
        <aside className="dashboard-sources" aria-label="Sources">
          <Card padding="none">
            <Card.Header>
              <Card.Title as="h2">Sources</Card.Title>
            </Card.Header>
            <DatasetSourceList
              label="Datasets on this dashboard"
              className="dashboard-sources-list"
            >
              {spec.datasets.map((spec) => {
                const d = byId.get(spec.id);
                const state = data.datasets.get(spec.id);
                const got = state?.answer ?? null;
                const isSelected = selected === spec.id;
                return (
                  <DatasetSource
                    key={spec.id}
                    title={spec.title}
                    kind={spec.source}
                    definition={definitionWords(spec, answer.board.params)}
                    refresh={d ? refreshWords(d.freshness) : undefined}
                    asOf={got?.as_of}
                    status={statusOf(d)}
                    detail={state?.failure ?? got?.detail}
                    rows={rowsWords(got)}
                    selected={isSelected}
                    onSelect={() => setSelected(isSelected ? null : spec.id)}
                    now={now}
                  >
                    {isSelected && got?.status === "ready" && got.table ? (
                      <RowsTable
                        columns={got.table.columns}
                        shown={got.table.columns.map((c) => c.key)}
                        rows={got.table.rows.slice(0, PREVIEW_ROWS)}
                      />
                    ) : null}
                  </DatasetSource>
                );
              })}
            </DatasetSourceList>
          </Card>
        </aside>
      </div>
    </div>
  );
}
