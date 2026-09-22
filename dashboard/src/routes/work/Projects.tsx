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
 * # The sort is the reader's, it is in the URL, and the ENGINE applies it
 *
 * `DataGrid` writes `sort=`, so a directory ordered by open work can be sent
 * to somebody, survives a reload and comes back from Back the way it went.
 * The key then travels to the engine rather than being applied here, because
 * the answer is a PAGE: the listing stops at the engine's own 200
 * (`MaxProjectsPerAnswer`) and a sort applied after that orders the rows that
 * survived the key order — so `-open` meant "the most open work among the
 * projects whose keys sort first", which reads exactly like the answer to the
 * question it is not. `serverSorted` is what tells the grid not to re-sort
 * the page it was handed.
 *
 * The corollary is that a column the ENGINE cannot order by is not sortable
 * here either: Lead is resolved against the org chart at read time and the
 * tracker holds no chart, so there is no column behind it. Sorting it on the
 * client alone would be the same page-ordering bug wearing one column.
 */

import { useMemo } from "react";
import { buildHash, useParam, useRoute } from "~/app/router.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { usePageCoverage } from "~/app/Shell.tsx";
import {
  ColumnChooser,
  DataGrid,
  columnChoicesOf,
  type GridColumn,
} from "~/app/frame/DataGrid.tsx";
import { DateCell, NumberCell, SeatCell } from "~/app/frame/cells.tsx";
import { peekHref, peekRow, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { QueryState } from "~/components/common.tsx";
import { Button, EmptyState, EmptyValue, Popover, Tag } from "@crewlethq/ui";
import { DashboardGlyph, ViewColumnGlyph } from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, seatLookup } from "~/lib/seats.ts";
import { useNow } from "~/lib/clock.ts";
import { Segmented } from "~/ui/primitives.tsx";
import { filed, ProjectProgress, ProjectProgressLegend } from "./census.tsx";
import type { WorkProjectRow } from "~/protocol/index.ts";

/** Which projects the grid lists, as the one switch this page has. */
const SHOWN = ["active", "archived", "all"] as const;
type Shown = (typeof SHOWN)[number];

/**
 * The segment, as the engine's own `archived=` mode.
 *
 * TWO VOCABULARIES ON PURPOSE. `shown=` is a SCREEN segment — which tab a
 * link opens — and `archived=` is the query parameter the engine's three-mode
 * enum is spelled in (`tracker.ArchivedMode`, the same one the work list
 * takes). Mapping between them is one object; collapsing them would either put
 * `archived=only` in a route people share or invent a second name for a mode
 * the engine already has.
 */
const ASKED: Record<Shown, "false" | "only" | "true"> = {
  active: "false",
  archived: "only",
  all: "true",
};

/**
 * The orderings the ENGINE takes, as the dashboard's own copy.
 *
 * It is a copy by necessity — this is a separate build in a separate language
 * and cannot import `tracker.ProjectSorts` — so a Go gate holds it against
 * that list in both directions (`internal/tracker/client_gate_test.go`). The
 * drift it catches is silent and total: a header carrying a key the engine
 * refuses turns one click into a `bad_params` refusal over the whole screen,
 * and an ordering the engine grew that no header offers is one nobody can
 * reach.
 */
const PROJECT_SORT_KEYS = ["key", "name", "unit", "open", "done", "closed", "last_change"] as const;

/**
 * What the directory opens on.
 *
 * WHERE THE PILE IS, which is the question a directory of containers is
 * opened to answer — and it is the engine's answer over every project now
 * rather than this page's over the first two hundred keys. Both this page and
 * the grid read `sort=` through `useParam` with this same fallback, so the two
 * cannot disagree about what an absent key means.
 */
const DEFAULT_SORT = "-open";

/**
 * What an empty answer means, per segment — and there are only two here.
 *
 * ONE SENTENCE PER QUESTION, because each segment now asks a different one.
 * "No project matches" was the old copy and it named a narrowing this screen
 * does not have: there is no filter box, so a reader told nothing matched had
 * nothing to widen.
 *
 * THE ACTIVE ONE SAYS WHERE THE PROJECTS WENT, and it can, because the answer
 * carries the CENSUS of both sets. An empty Active answer is either a company
 * with no projects or a company that has archived every one of them, and the
 * page used to be unable to tell — so it hedged, naming both ways it happens
 * in one sentence and sending the reader to look. A hedge is what a screen
 * writes when it is missing a number; this one now has the number, so it says
 * how many are archived and offers the segment that holds them.
 *
 * `all` is absent because it is unreachable: an empty answer on ANY segment
 * with a zero census is a company with no projects, which [NoProjectsYet]
 * replaces the whole grid with.
 */
const EMPTY = {
  archived: {
    icon: "view_column" as const,
    title: "No project is archived",
    hint: "An archived project keeps its work and stops taking new items.",
  },
};

/**
 * The Active segment's empty state, which needs the census to write.
 *
 * A FUNCTION RATHER THAN A CONSTANT because the sentence turns on a number the
 * engine sent. With archived work to point at it points at it, by count and
 * with a link; with none, the company has projects that are neither active nor
 * archived, which cannot happen — so the only remaining case is the one the
 * grid is not drawing, and the honest line is the plain one.
 */
function activeEmpty(archived: number, href: string) {
  if (archived > 0) {
    return {
      icon: "view_column" as const,
      title: "No project is active",
      hint: (
        <>
          {/* THE TYPOGRAPHIC APOSTROPHE, which is what every other sentence
              in this product uses — the JSX ones spell it `&rsquo;` and a
              string has to carry the character itself. */}
          {archived === 1
            ? "The company’s one project has been archived"
            : `All ${archived} of the company’s projects have been archived`}
          {" — "}
          <a className="t-link" href={href}>
            see them under Archived
          </a>
          , where each keeps its work.
        </>
      ),
    };
  }
  return {
    icon: "view_column" as const,
    title: "No project is active",
    hint: "A project appears the moment a unit in the company configuration declares its `project` key.",
  };
}

export function Projects() {
  const org = useOrg();
  // THE CHART'S TWO ANSWERS ABOUT A HANDLE, from one lookup: the lead's badge
  // draws the dashed ring off the KIND, and the last change prints the NAME.
  const who = useMemo(() => seatLookup(indexOrg(org)), [org]);
  const now = useNow();
  const [shownRaw, setShown] = useParam("shown", "active", "section");
  const shown: Shown = (SHOWN as readonly string[]).includes(shownRaw)
    ? (shownRaw as Shown)
    : "active";
  const { open: openPeek } = usePeekControls();
  const route = useRoute();
  // WHERE THE ARCHIVED ONES ARE, as a real href rather than a handler: the
  // empty state offers it as a link, so it is middle-clickable and copyable
  // like every other way into a segment. THE REST OF THE QUERY IS KEPT —
  // a reader who sorted, or who arrives with a filter the API takes, does not
  // lose it by following a sentence that was only ever about the segment.
  const archivedHref = useMemo(() => {
    const query = new URLSearchParams(route.query);
    query.set("shown", "archived");
    return buildHash(route.path, query);
  }, [route]);
  // THE GRID'S OWN KEY, READ HERE TOO. `DataGrid` writes `sort=` from a header
  // click and this page sends it to the engine, so both read it through
  // `useParam` with the same fallback — one key, one meaning, one default.
  //
  // AND A KEY THAT IS NOT ONE FALLS BACK, exactly as `shown` above does. A URL
  // is hand-editable and outlives a build, so a link carrying an ordering this
  // engine does not take would otherwise meet a `bad_params` refusal — which
  // the frame renders as the SCREEN being at fault and offers no retry for,
  // over a whole directory, because of one stale query key.
  const [sortRaw] = useParam("sort", DEFAULT_SORT);
  const sort = PROJECT_SORT_KEYS.includes(
    sortRaw.replace(/^-/, "") as (typeof PROJECT_SORT_KEYS)[number],
  )
    ? sortRaw
    : DEFAULT_SORT;

  // THE COLUMN ARRANGEMENT, READ HERE TOO — the same shape `sort=` above takes,
  // and for the same reason: the chooser beside the segment writes this key and
  // [DataGrid] reads it, so both go through `useParam` on ONE name and neither
  // can be looking at a key the other is not writing.
  //
  // THE BARE `cols=`, because this is the screen's PRIMARY and only grid — see
  // [DataGrid]'s own `name`, which says the primary grid takes `sort=` and
  // `cols=` and a SECOND grid on a screen is what earns a suffix. Naming this
  // one would also move `sort=` to `sort.projects=`, and the page reads `sort`
  // itself to send to the engine.
  const [cols, setCols] = useParam("cols", "");

  // EVERY PROJECT IN THE ASKED SET, which is what a directory is. The engine's
  // own limit is what bounds it, and the answer says when it stopped short.
  //
  // THE SEGMENT IS THE QUESTION, not a narrowing of a wider answer. The engine
  // selects exactly the set named (`tracker.ArchivedMode`), so the rows that
  // arrive ARE the segment and `total` counts it. The page used to ask for
  // both sets and filter the answer here: past the engine's 200 the page it
  // filtered held no archived row at all, so the Archived segment said "No
  // project is archived" about a company that had retired dozens.
  //
  // AND THE ORDER IS ASKED FOR TOO, for the same reason — see the module doc.
  const state = useQuery(
    "work_projects",
    { limit: 200, archived: ASKED[shown], sort },
    { pollMs: 60_000 },
  );
  usePageCoverage(state.data);

  const rows = useMemo(() => state.data?.projects ?? [], [state.data]);
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

  // THE ANSWER'S OWN NUMBERS. `work_projects` carries `total` beside a
  // `truncated` that is `total > len(rows)`, and this sentence read neither:
  // past the engine's own 200 it said "200 projects" about a company with
  // three hundred, with nothing on screen to say so.
  //
  // "N of M" NOW READS ON EVERY SEGMENT, which is what selecting rather than
  // widening bought. `total` counts the set this segment asked for, so on
  // Archived it is how many projects are archived — where the widening flag
  // made it a count of the whole company beside a grid drawing the retired
  // half, and the sentence had to drop the comparison on exactly the segment
  // that needed it most.
  const listed = rows.length;
  const answerTotal = state.data?.total ?? listed;
  const short = !!state.data?.truncated;
  const counted = short ? `${listed} of ${answerTotal}` : `${listed}`;
  const noun = (short ? answerTotal : listed) === 1 ? "project" : "projects";

  // THE CENSUS OF BOTH SETS, which is what stops this page guessing. Selecting
  // one set is what makes the listing honest and it is also what makes an
  // empty answer ambiguous — no projects, or every project archived — so the
  // engine sends both counts under the same narrowing the rows were read
  // under. Nothing here derives them from the rows: on the Active segment the
  // archived count has no row on screen to be derived from, which is the whole
  // point.
  const census = state.data?.census;
  const active = census?.active ?? 0;
  const archived = census?.archived ?? 0;
  // A COMPANY WITH NOTHING FILED, on WHICHEVER segment the reader landed on —
  // and they land on Active. Before the census this could only be said from
  // All, so the default segment of a brand-new company showed a grid's empty
  // state instead of the page that says what a project is.
  const nothingAtAll = !!census && active + archived === 0;

  // A HEAD IS A BUTTON WHERE IT CARRIES `sortValue`, so the columns that do
  // are exactly the engine's seven orderings — `Projects.test.tsx` holds the
  // two lists against each other, and the Go gate holds `PROJECT_SORT_KEYS`
  // against the engine's own. Under `serverSorted` the accessors are never
  // called; they stay because they say what each column's value IS.
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
        // NOT SORTABLE, and it cannot be. A project's lead is resolved at
        // READ time against the epoch's org chart — the tracker holds no
        // chart, so there is no column behind this cell and no `sort=lead`
        // for the engine to take. Ordering it here alone would order the
        // PAGE, which is the bug the rest of this screen just stopped
        // doing, wearing one column.
        cell: (row) =>
          row.lead?.handle ? (
            <SeatCell handle={row.lead.handle} {...who(row.lead.handle)} />
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
        // OFF BY DEFAULT, because on a chart-owned company it is the Project
        // column again. The engine mints a project the moment a unit declares
        // its `project` key and names it after the unit, so every row of such
        // a company read `Core` / `Core` and `Executives` / `Executives` —
        // two of nine columns spending their width on one fact, on the screen
        // whose other seven are the answer. The proposal's directory drew
        // Project, Lead, the three counts, Progress and Last change, which is
        // what the default set is now.
        //
        // AND IT IS A COLUMN RATHER THAN A DELETION, because the two names
        // genuinely differ on a company whose projects are a SEAT's: a
        // root-level seat carries its own `project` identity and belongs to no
        // unit, and a unit can be renamed without the project following. Such
        // a company turns it on with `cols=`, which carries the order as well
        // as the selection — so the address names the whole set it wants.
        //
        // THE SORT KEY STAYS EITHER WAY: `sort=unit` is one of the engine's
        // seven orderings, it is what the engine answers whether or not this
        // screen draws the column, and a saved or shared address carrying it
        // must not start refusing.
        optional: true,
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
        cell: (row) => <LastChange row={row} now={now} seatName={(handle) => who(handle).name} />,
      },
    ],
    [who, now],
  );

  // WHAT THE CHOOSER OFFERS, DERIVED FROM THE COLUMNS ABOVE rather than listed
  // beside them — see [columnChoicesOf]. A chooser holding its own list would
  // be a second declaration of this grid's columns, wrong the first time one is
  // added and indistinguishable from a correct one.
  const choices = useMemo(() => columnChoicesOf(columns), [columns]);

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
            The engine answered {listed} of {answerTotal}, in the order asked for, so these counts
            cover only the projects listed.
          </span>
        )}
        <span className="spacer" />
        {/* THE CENSUS ON THE SEGMENTS, so the switch says what is behind each
            option before it is pressed — which is what the counts are for on
            a control whose whole job is to change the set. Only while the
            engine has answered: a count drawn from a missing census would be
            three zeroes that read as a company with nothing.

            ALL CARRIES NONE. Its count is the sum of the two beside it, and a
            third figure that is the other two added up is arithmetic on
            screen rather than a fact. */}
        <Segmented
          value={shown}
          onChange={(value) => setShown(value)}
          ariaLabel="Which projects"
          options={[
            { value: "active", label: "Active", count: census?.active },
            { value: "archived", label: "Archived", count: census?.archived },
            { value: "all", label: "All" },
          ]}
        />
        {/* AN ARRANGEMENT IS A MENU, beside the one control that changes the
            SET. The directory's Unit column is `optional` and this screen had
            no chooser at all, so the column could be turned off in the source
            and never on by anybody — and `sort=unit`, one of the engine's seven
            orderings, was an ordering no head on the screen could reach. Expose
            or collapse: the column earns exposure, because a seat-owned
            project's unit genuinely differs from its name.

            THE SAME COMPONENT THE WORK LIST'S DISPLAY MENU DRAWS, over this
            grid's own columns — see [ColumnChooser], which is where the two
            rules that are easy to get wrong now live once. */}
        <Popover
          role="dialog"
          label="Columns"
          align="end"
          trigger={(open, toggle) => (
            <Button
              size="small"
              variant="tertiary"
              leadingIcon={<ViewColumnGlyph size="sm" />}
              aria-expanded={open}
              aria-haspopup="dialog"
              onClick={toggle}
            >
              Columns
            </Button>
          )}
        >
          {/* THE DISPLAY PANEL'S OWN GEOMETRY, which is what `.work-display`
              is: a padded menu column with a gap between its blocks. The work
              list's Display menu is the other panel wearing it, and this is
              the same kind of thing — the controls that decide how an answer
              is DRAWN — so a second class with the same three declarations
              would be a copy waiting to drift. `.work-menu` is the bare panel
              underneath, which every work menu takes. */}
          <div className="work-menu work-display">
            <ColumnChooser choices={choices} value={cols} onChange={setCols} />
          </div>
        </Popover>
      </div>

      <QueryState error={state.error} loading={state.loading}>
        {/* A COMPANY WITH NO PROJECTS, SAID ON WHATEVER SEGMENT THE READER IS
            ON — and they land on Active. The census is what makes it sayable
            from anywhere: `active + archived === 0` is the company having
            nothing, where an empty ANSWER is only ever the segment having
            nothing. Gated on All, this page greeted a brand-new company with
            a grid's empty state on its own default segment. */}
        {nothingAtAll ? (
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
            defaultSort={DEFAULT_SORT}
            // THE ORDER IS THE ENGINE'S — see the module doc. Without this
            // the grid would re-sort the page it was handed, which on a
            // truncated answer is the ordering bug this screen just stopped
            // making, one layer up.
            serverSorted
            footer={<ProjectProgressLegend />}
            // EACH SEGMENT'S OWN EMPTINESS, because each one asked a
            // different question — and the Active one is WRITTEN FROM THE
            // CENSUS, so it says how many are archived and links to them
            // rather than hedging about which of two things happened. `All`
            // never reaches here: an empty All answer with a zero census is
            // the state above, and a non-zero census cannot answer nothing.
            empty={shown === "archived" ? EMPTY.archived : activeEmpty(archived, archivedHref)}
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
