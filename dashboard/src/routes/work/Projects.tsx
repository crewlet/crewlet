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
 * # A row is a project, and a meter is how far along it is
 *
 * The bar chart ranked projects by open work alone, which answers "where is
 * the pile" and nothing else: a project with four open and two hundred done is
 * a different situation from one with four open and nothing else, and the
 * chart drew them identically. Every row carries the three maintained counts
 * AND how much of the whole is done, so both readings are on the same line —
 * the meter is `census.tsx`'s, which is the same one the project's own header
 * wears.
 *
 * # A row peeks, because a directory is read to RECOGNISE something
 *
 * "Is this the one I meant" is answered beside the list; the project's page is
 * one click further, from the panel's own `Open ↗` or from any click the
 * browser treats as "open elsewhere".
 *
 * # The sort is the reader's and it is in the URL
 *
 * `DataGrid` writes it, so a directory ordered by open work can be sent to
 * somebody, survives a reload and comes back from Back the way it went.
 */

import { useMemo } from "react";
import { useParam } from "~/app/router.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { DateCell, NumberCell, SeatCell } from "~/app/frame/cells.tsx";
import { peekHref, peekRow, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { QueryState } from "~/components/common.tsx";
import { EmptyState, EmptyValue, Tag } from "@crewlethq/ui";
import { DashboardGlyph } from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { Segmented } from "~/ui/primitives.tsx";
import { filed, ProjectProgress, ProjectProgressLegend } from "./census.tsx";
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

  // HOW MANY PROJECTS THE COMPANY HAS, which is the answer's own number and
  // not this page's. `work_projects` carries `total` beside a `truncated` that
  // is `total > len(rows)` (`internal/tracker/projectsread.go`), and the
  // sentence read neither: past the engine's own 200 it said "200 projects"
  // about a company with three hundred, with nothing on screen to say so.
  //
  // THE ARCHIVED SEGMENT IS THE ONE THAT CANNOT USE IT. The engine's flag
  // INCLUDES the archived rows rather than selecting them, so the total it
  // counted covers the active ones too while this page shows only the archived
  // half — printing it there would be a number about a set the grid is not
  // drawing. What is honest on that segment is the count on screen, and the
  // note below carries the rest.
  const listed = rows.length;
  const answered = all.length;
  const answerTotal = state.data?.total ?? answered;
  const short = !!state.data?.truncated;
  // THE "N of M" FORM ONLY WHERE M IS THIS PAGE'S OWN QUESTION: on the Archived
  // segment the sentence says what is on screen and the note carries the total,
  // because "12 of 340 projects" there would compare an archived set against a
  // count of the whole company.
  const counted = short && shown !== "archived" ? `${listed} of ${answerTotal}` : `${listed}`;
  const noun =
    (short && shown !== "archived" ? answerTotal : listed) === 1 ? "project" : "projects";

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
        // NOT OPTIONAL. It was, and this screen has no Display menu — the
        // design gave the directory the segment and nothing else — so `cols=`
        // was reachable only by hand-editing the URL and the grid's one column
        // control is a RESET. The column could be turned off and never on,
        // while the sentence directly above the grid quoted its number. A
        // right-aligned integer is also nowhere near the width `optional`
        // exists to ration (`SHRINK_CAP` in `DataGrid`).
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
        cell: (row) => (
          // IN A CELL IT IS THE BAR ALONE — the legend is drawn once under the
          // grid, because forty legends is not forty facts.
          <span
            className="work-meter"
            title={`${row.task_counts.done} done of ${filed(row.task_counts)} filed`}
          >
            <ProjectProgress counts={row.task_counts} />
          </span>
        ),
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
      {/* WHAT THIS PAGE IS, once. Where a project COMES FROM is the sentence
          somebody needs when there are none, so it lives in the empty state
          below rather than being printed twice on the one screen that shows
          both. */}
      <PageNote>Every project in the company, who leads it and how far along its work is.</PageNote>

      <div className="toolbar">
        {/* THE TOTALS AS ONE SENTENCE, not four tiles. They are context for the
            rows under them rather than the point of the page — and the counts
            are over WHAT IS SHOWN, which is why the switch beside them changes
            them and why a short page says so. */}
        <span className="work-summary" style={{ marginLeft: 0 }}>
          {counted} {noun} · {totals.open} open · {totals.done} done · {totals.closed} closed
        </span>
        {short && (
          <span className="t-caption">
            The engine answered {answered} of the company&rsquo;s {answerTotal}, ordered by key, so
            these counts cover only the projects listed.
          </span>
        )}
        <span className="spacer" />
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
          <NoProjectsYet />
        ) : (
          <DataGrid
            rows={rows}
            columns={columns}
            rowKey={(row) => row.key}
            // A ROW OPENS THE PEEK the directory was written for: the question
            // a reader asks here is "is this the one I meant", which the rail
            // answers without leaving the list — and the peek's own `Open ↗`
            // is the way to the page.
            //
            // THROUGH `peekRow`, which is the one thing that calls
            // `preventDefault`. A bare handler beside a `rowHref` opened the
            // peek and then let the browser follow the anchor, so the rail was
            // pushed and destroyed by one click and the reader landed on the
            // page every time.
            //
            // AND THE HREF IS THE FRAME'S OWN ANSWER to where a project lives
            // rather than a second copy of the route, so a row and the panel it
            // opens can never name different pages.
            rowHref={(row) => peekHref({ kind: "project", id: row.key })}
            onRowActivate={peekRow<WorkProjectRow>((row) =>
              openPeek({ kind: "project", id: row.key }),
            )}
            defaultSort="-open"
            footer={<ProjectProgressLegend />}
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
    // ONE LINE, because two made every row in the directory a line and a half
    // tall — the instant above the actor, on a grid whose other eight columns
    // are single values, so the rhythm a reader scans down was set by the one
    // column they scan last.
    <span className="work-lastchange">
      <DateCell at={change.at} now={now} />
      {/* A COMMIT CAN NAME NOBODY, and the wire says so by leaving the actor
          out — the engine did it. A handle is resolved through the chart like
          everywhere else, so this column says what the Lead column one cell
          over says; and an operator's is a TOKEN's label rather than a seat,
          which is why the kind travels beside it — in the same line, because
          who made a change is one fact. */}
      <span className="t-caption truncate">
        {"· "}
        {change.actor ? seatName(change.actor) : "the engine"}
        {change.actor_kind && change.actor_kind !== "agent" ? ` (${change.actor_kind})` : ""}
      </span>
    </span>
  );
}

/**
 * A COMPANY WITH NO PROJECTS, said on a page about projects.
 *
 * This drew `NoWorkYet` — "No work has been filed yet" — which is a sentence
 * about ITEMS on the one screen whose rows are containers. A reader with three
 * projects and nothing filed in them would have been told the opposite of what
 * the grid was showing, and a reader with no projects was told to go looking
 * for work rather than for the configuration that mints one.
 */
function NoProjectsYet() {
  return (
    <EmptyState
      icon={<DashboardGlyph size={32} />}
      title="No project has been created yet"
      description="A project is where the company files its work: a key, a lead and its own statuses, types and labels. One appears here the moment a unit in the company configuration declares its `project` key — the engine mints it, so there is nothing to create by hand."
    />
  );
}
