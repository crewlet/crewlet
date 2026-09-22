/**
 * Every project the company has, as one sortable grid.
 *
 * # Where the overview went
 *
 * The four workspace totals and the ranked bar chart of open work used to sit
 * above every board and every list on `#/work`, on every visit. They are an
 * OVERVIEW — a question about the containers rather than about the work — so
 * they are a page, and the totals are one sentence in this page's own bar
 * rather than four tiles, because they are context for the rows rather than
 * the point of the screen.
 *
 * # A row is a project and a meter is its shape
 *
 * The bar chart ranked projects by open work alone, which answers "where is
 * the pile" and nothing else: a project with four open and two hundred done is
 * a different situation from one with four open and nothing else, and the
 * chart drew them identically. Every row carries the three maintained counts
 * AND the proportion, so both readings are on the same line.
 *
 * # The sort is the reader's and it is in the URL
 *
 * `DataGrid` writes it, so a directory ordered by open work can be sent to
 * somebody, survives a reload and comes back from Back the way it went.
 */

import { useMemo } from "react";
import { href, useParam } from "~/app/router.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { DateCell, NumberCell, SeatCell } from "~/app/frame/cells.tsx";
import { usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { QueryState } from "~/components/common.tsx";
import { Coverage } from "~/components/work.tsx";
import { EmptyValue, Legend, StackedBar, Tag, DATA_COLOR_OTHER } from "@crewlethq/ui";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { Segmented } from "~/ui/primitives.tsx";
import { NoWorkYet } from "./ItemsView.tsx";
import type { WorkProjectRow } from "~/protocol/index.ts";

/** Which projects the grid lists, as the one switch this page has. */
const SHOWN = ["active", "archived", "all"] as const;
type Shown = (typeof SHOWN)[number];

export function Projects() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  const [shownRaw, setShown] = useParam("shown", "active", "section");
  const shown: Shown = (SHOWN as readonly string[]).includes(shownRaw)
    ? (shownRaw as Shown)
    : "active";
  const { open: openPeek } = usePeekControls();

  // EVERY PROJECT, which is what a directory is. The engine's own limit is
  // what bounds it, and the answer says when it stopped short.
  //
  // `archived` IS ASKED FOR, never filtered out of an answer that never had
  // them: the listing excludes archived rows unless the question says
  // otherwise (`internal/tracker/projectsread.go` ANDs `p.archived = 0`), so a
  // client-side narrowing over the default answer left the Archived segment
  // permanently empty — and, on a company whose every project is archived, it
  // said the company had filed nothing at all.
  //
  // IT WIDENS RATHER THAN SELECTS: the engine's flag INCLUDES the archived
  // ones, so "archived only" is still this screen's own narrowing over the
  // wider answer.
  const state = useQuery(
    "work_projects",
    { limit: 200, ...(shown === "active" ? {} : { archived: true }) },
    { pollMs: 60_000 },
  );
  usePageCoverage(state.data);

  const all = useMemo(() => state.data?.projects ?? [], [state.data]);
  const rows = useMemo(
    () =>
      all.filter((p) =>
        shown === "all" ? true : shown === "archived" ? !!p.archived : !p.archived,
      ),
    [all, shown],
  );
  // WHAT `[` AND `]` WALK, in the order the grid is in — published by the list
  // that holds it, which is the only thing that knows that order.
  usePeekNeighbours(
    useMemo(() => rows.map((p) => ({ kind: "project" as const, id: p.key })), [rows]),
  );

  const totals = useMemo(
    () =>
      rows.reduce(
        (acc, p) => ({
          open: acc.open + p.task_counts.open,
          done: acc.done + p.task_counts.done,
          closed: acc.closed + p.task_counts.closed,
        }),
        { open: 0, done: 0, closed: 0 },
      ),
    [rows],
  );

  const columns = useMemo<GridColumn<WorkProjectRow>[]>(
    () => [
      {
        key: "key",
        header: "Key",
        shrink: true,
        sortValue: (row) => row.key,
        cell: (row) => <span className="mono key-mark">{row.key}</span>,
      },
      {
        key: "name",
        header: "Project",
        sortValue: (row) => row.name || row.key,
        cell: (row) => (
          <span className="col">
            <span className="truncate">{row.name || row.key}</span>
            {row.purpose && <span className="t-caption truncate">{row.purpose}</span>}
          </span>
        ),
      },
      {
        key: "lead",
        header: "Lead",
        shrink: true,
        sortValue: (row) => row.lead?.handle ?? "",
        cell: (row) =>
          row.lead?.handle ? (
            <SeatCell handle={row.lead.handle} name={index.byHandle.get(row.lead.handle)?.name} />
          ) : (
            // A PROJECT WITH NO LEAD ROUTES ITS UNASSIGNED WORK TO NOBODY,
            // which is a finding rather than a blank cell.
            <EmptyValue label="Nobody leads this" />
          ),
      },
      {
        key: "unit",
        header: "Unit",
        shrink: true,
        sortValue: (row) => row.unit?.name ?? row.unit?.key ?? "",
        cell: (row) =>
          row.unit?.resolved === false ? (
            <Tag
              variant="warning"
              appearance="outline"
              title="The current org chart has no such unit"
            >
              {row.unit.key}
            </Tag>
          ) : (
            row.unit?.name || row.unit?.key || <EmptyValue label="No unit owns this" />
          ),
      },
      {
        key: "open",
        header: "Open",
        align: "right",
        shrink: true,
        sortValue: (row) => row.task_counts.open,
        cell: (row) => <NumberCell value={row.task_counts.open} />,
      },
      {
        key: "done",
        header: "Done",
        align: "right",
        shrink: true,
        sortValue: (row) => row.task_counts.done,
        cell: (row) => <NumberCell value={row.task_counts.done} />,
      },
      {
        key: "closed",
        header: "Closed",
        align: "right",
        shrink: true,
        optional: true,
        sortValue: (row) => row.task_counts.closed,
        cell: (row) => <NumberCell value={row.task_counts.closed} />,
      },
      {
        key: "progress",
        header: "Progress",
        // NOT SORTABLE. A proportion over four tasks and one over four hundred
        // are the same number and not the same fact, so ordering by it would
        // rank a project nobody has started below one with a single task
        // closed. The counts beside it are what the ordering is for.
        cell: (row) => <ProjectMeter row={row} />,
      },
      {
        key: "last_change",
        header: "Last change",
        shrink: true,
        // SORTED BY WHEN, which is the question this column is for: a
        // directory read to find what has gone quiet is read newest-last.
        sortValue: (row) => row.last_change?.at ?? "",
        cell: (row) => (
          <LastChange
            row={row}
            now={now}
            seatName={(handle) => index.byHandle.get(handle)?.name ?? handle}
          />
        ),
      },
    ],
    [index, now],
  );

  return (
    <>
      <PageNote>
        Every project in the company, who leads it and how far along it is. A project appears the
        moment a unit in the company configuration declares its{" "}
        <span className="mono">project</span> key.
      </PageNote>

      <div className="toolbar">
        {/* THE TOTALS AS ONE SENTENCE, not four tiles. They are context for the
            rows under them rather than the point of the page — and they are
            over WHAT IS SHOWN, which is why the switch beside them changes
            them. */}
        <span className="work-summary" style={{ marginLeft: 0 }}>
          {rows.length} project{rows.length === 1 ? "" : "s"} · {totals.open} open · {totals.done}{" "}
          done · {totals.closed} closed
        </span>
        <span className="spacer" />
        <Coverage answer={state.data} />
        <Segmented
          value={shown}
          onChange={(value) => setShown(value)}
          ariaLabel="Which projects"
          options={[
            { value: "active", label: "Active" },
            { value: "archived", label: "Archived" },
            { value: "all", label: "All" },
          ]}
        />
      </div>

      <QueryState error={state.error} loading={state.loading}>
        {state.data && all.length === 0 ? (
          <NoWorkYet />
        ) : (
          <DataGrid
            rows={rows}
            columns={columns}
            rowKey={(row) => row.key}
            // A ROW OPENS THE PEEK the directory was written for: the question
            // a reader asks here is "is this the one I meant", which the rail
            // answers without leaving the list.
            onRowActivate={(row) => openPeek({ kind: "project", id: row.key })}
            rowHref={(row) => href(["work", row.key])}
            defaultSort="-open"
            footer={<ProjectsLegend />}
            empty={{
              icon: "view_column",
              title: shown === "archived" ? "No project is archived" : "No project matches",
              hint:
                shown === "archived"
                  ? "An archived project keeps its work and stops taking new items."
                  : "Every project this company has is listed under All.",
            }}
          />
        )}
      </QueryState>
    </>
  );
}

/**
 * How far along a project is, as a proportion with its legend.
 *
 * IN THE STATUS TONES the badges use rather than the chart hues, so the same
 * fact is not two colours on one screen. A project with nothing filed draws no
 * bar at all: three zero segments are an empty track that reads as a chart
 * which failed to load.
 */
function ProjectMeter({ row }: { row: WorkProjectRow }) {
  const counts = row.task_counts;
  const whole = counts.open + counts.done + counts.closed;
  if (whole === 0) return <EmptyValue label="Nothing filed yet" />;
  const segments = [
    { id: "done", label: "Done", value: counts.done, color: "var(--positive)" },
    { id: "open", label: "Open", value: counts.open, color: "var(--info)" },
    { id: "closed", label: "Closed", value: counts.closed, color: DATA_COLOR_OTHER },
  ];
  return (
    <span className="work-meter" title={`${counts.done} done of ${whole}`}>
      <StackedBar segments={segments} />
      {/* THE LEGEND IS DRAWN ONCE FOR THE COLUMN, not once per row — see
          [ProjectsLegend] below the grid. An unlabelled stack of three colours
          is three colours, and forty legends is forty. */}
    </span>
  );
}

/**
 * When this project's work last changed, and who changed it.
 *
 * THE ENGINE'S OWN MAINTAINED COLUMN where it has one. A node built before the
 * applier maintained it, or a project nothing has ever been filed in, carries
 * nothing — and the two are different facts: one is "this build cannot say"
 * and the other is "nothing has happened". Only the second is something a
 * reader should act on, so the absent case says which it is rather than
 * drawing a dash for both.
 */
function LastChange({
  row,
  now,
  seatName,
}: {
  row: WorkProjectRow;
  now: number;
  seatName: (handle: string) => string;
}) {
  const change = row.last_change;
  if (!change) {
    return row.task_counts.open + row.task_counts.done + row.task_counts.closed === 0 ? (
      <EmptyValue label="Nothing has been filed here" />
    ) : (
      <EmptyValue label="Filed before this node recorded one" />
    );
  }
  return (
    <span className="col">
      <DateCell at={change.at} now={now} />
      {/* A COMMIT CAN NAME NOBODY, and the wire says so by leaving the actor
          out — the engine did it. A handle is resolved through the chart like
          everywhere else, and an operator's is a TOKEN's label rather than a
          seat, which is why the kind travels beside it. */}
      <span className="t-caption truncate">
        {change.actor ? seatName(change.actor) : "the engine"}
        {change.actor_kind && change.actor_kind !== "agent" ? ` (${change.actor_kind})` : ""}
      </span>
    </span>
  );
}

/**
 * The one legend the Progress column needs, drawn once under the grid.
 *
 * An unlabelled stack of three colours is three colours; a legend per row is
 * forty of them. The grid's own footer is where a fact about a COLUMN belongs.
 */
function ProjectsLegend() {
  return (
    <Legend
      items={[
        { id: "done", label: "Done", color: "var(--positive)" },
        { id: "open", label: "Open", color: "var(--info)" },
        { id: "closed", label: "Closed", color: DATA_COLOR_OTHER },
      ]}
    />
  );
}
