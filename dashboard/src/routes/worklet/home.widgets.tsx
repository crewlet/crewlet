/**
 * Worklet's widgets: a saved view, one project, and the company's blocked and
 * overdue work, each Worklet's own question asked the way its screens ask it.
 *
 * Tasks are drawn as `taskRows.tsx`'s one-line rows, as My work's are.
 */

import { useMemo } from "react";
import { SegmentedMeter } from "@crewlethq/ui";
import {
  CalendarClockGlyph,
  FolderKanbanGlyph,
  LayoutDashboardGlyph,
  BanGlyph,
} from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { companyDateLabel, relTime } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { projectPath, viewRun } from "~/lib/work.ts";
import { censusLine } from "~/routes/home/Projects.tsx";
import type { WorkView } from "~/protocol/index.ts";
import {
  ChoiceNote,
  WidgetCard,
  WidgetNote,
  WidgetQuery,
  defineWidgets,
  gapsOf,
  type WidgetChoices,
} from "~/routes/home/board/kit.tsx";
import { MoreLink, ROWS, TaskList, countLabel } from "~/routes/worklet/taskRows.tsx";

const POLL_MS = 60_000;

// ---- A saved view ------------------------------------------------------------

/** Every saved view the reader can see, as Saved views lists them. */
function useSavedViews() {
  const read = useQuery("work_saved_views", {}, { pollMs: 120_000 });
  const views = useMemo(
    () => (read.data?.views ?? []).filter((v) => !v.builtin && v.id),
    [read.data],
  );
  return { read, views };
}

function useViewChoices(): WidgetChoices {
  const { read, views } = useSavedViews();
  return {
    loading: read.loading && !read.data,
    options: views.map((v) => ({ value: v.id as string, label: v.name })),
    empty: "No saved views yet. Save one from a list in Worklet.",
  };
}

/** A view's container as `work_items` takes it: `workspace`, or `project:ENG`. */
const containerOf = (view: WorkView) =>
  view.container.id ? `${view.container.kind}:${view.container.id}` : view.container.kind;

function SavedView({ params }: { params: Readonly<Record<string, string>> }) {
  const { read, views } = useSavedViews();
  const view = views.find((v) => v.id === params.view);
  if (!view) {
    return (
      <WidgetCard title="Saved view" from="worklet-views">
        {read.error ? (
          <WidgetQuery error={read.error} refusal={read.refusal} loading={false} gaps={[]} />
        ) : read.loading && !read.data ? null : (
          <ChoiceNote thing="saved view" />
        )}
      </WidgetCard>
    );
  }
  return <ViewRows view={view} />;
}

/** One view, run as its own page runs it: its saved query over its own container. */
function ViewRows({ view }: { view: WorkView }) {
  const run = viewRun(view);
  const read = useQuery(
    "work_items",
    { ...view.params, container: containerOf(view), subtasks: "separate", limit: ROWS },
    { pollMs: POLL_MS },
  );
  const rows = read.data?.items ?? [];
  return (
    <WidgetCard
      title={view.name}
      from="worklet-views"
      actions={
        <a className="t-link home-widget-open" href={href(run.path, run.query)}>
          Open view
        </a>
      }
    >
      <WidgetQuery
        error={read.error}
        refusal={read.refusal}
        loading={read.loading && !read.data}
        gaps={gapsOf([read.data])}
        empty={
          read.data && rows.length === 0
            ? { title: "Nothing in this view", hint: "No task matches its filters right now." }
            : undefined
        }
      >
        <TaskList rows={rows} />
        <MoreLink
          shown={rows.length}
          total={
            read.data ? { total: read.data.total_hint, capped: read.data.total_capped } : undefined
          }
          to={run.path}
          query={run.query}
          noun="task"
        />
      </WidgetQuery>
    </WidgetCard>
  );
}

// ---- One project ---------------------------------------------------------------

const PROJECTS_PAGE = 200;

function useProjects() {
  const viewer = useViewer();
  const read = useQuery(
    "work_projects",
    { limit: PROJECTS_PAGE },
    { pollMs: 120_000, enabled: viewer.worklet },
  );
  const projects = useMemo(
    () => (read.data?.projects ?? []).filter((p) => !p.archived),
    [read.data],
  );
  return { read, projects };
}

function useProjectChoices(): WidgetChoices {
  const { read, projects } = useProjects();
  return {
    loading: read.loading && !read.data,
    options: projects.map((p) => ({ value: p.key, label: `${p.key} · ${p.name}` })),
    empty: "No projects yet. Add one with New project in Worklet.",
    partial: read.data?.truncated
      ? `Only the first ${PROJECTS_PAGE} projects are offered.`
      : undefined,
  };
}

/**
 * One project, read BY ITS KEY, as the project's own page reads it
 * (`work_project`).
 *
 * NEVER LOOKED UP IN THE PICKER'S LIST. The picker offers one page of the
 * company's projects, and the widget used to find its project in that page: a
 * project chosen while it was on the page, and pushed past it as the company
 * grew, was not in it, so the widget said "Choose a project" for a project
 * that was there all along. The question that answers one project answers
 * this one.
 */
function Project({ params }: { params: Readonly<Record<string, string>> }) {
  const viewer = useViewer();
  const key = params.project ?? "";
  const read = useQuery("work_project", key ? { key } : undefined, {
    pollMs: 120_000,
    enabled: key !== "" && viewer.worklet,
  });
  const project = read.data;
  const now = useNow();
  const open = useQuery(
    "work_items",
    project
      ? { container: `project:${project.key}`, subtasks: "separate", limit: ROWS }
      : undefined,
    { pollMs: POLL_MS, enabled: project !== null },
  );
  if (!project) {
    return (
      <WidgetCard title="Project" from="worklet">
        {key === "" ? (
          <ChoiceNote thing="project" />
        ) : read.error === "not_found" ? (
          // A KEY NOTHING ANSWERS TO is a project removed since it was chosen,
          // said with its key, which is the half a reader can act on.
          <WidgetNote>{`There’s no project ${key} any more. Choose another in Edit Home.`}</WidgetNote>
        ) : (
          <WidgetQuery error={read.error} refusal={read.refusal} loading={read.loading} gaps={[]} />
        )}
      </WidgetCard>
    );
  }
  const { todo, active, done } = project.task_counts;
  const whole = todo + active + done;
  const pct = whole > 0 ? Math.round((done / whole) * 100) : null;
  const rows = open.data?.items ?? [];
  return (
    <WidgetCard
      title={`${project.key} · ${project.name}`}
      from="worklet"
      actions={
        <a className="t-link home-widget-open" href={href(projectPath(project.key))}>
          Open project
        </a>
      }
    >
      <div className="home-widget-pad home-tasks-project">
        <div className="home-tasks-meter">
          <SegmentedMeter
            size="compact"
            total={whole}
            remainderLabel="to do"
            segments={[
              { id: "done", value: done, tone: "success", label: "done" },
              { id: "active", value: active, tone: "info", label: "active" },
            ]}
          />
          {pct !== null && <span className="home-tasks-pct">{pct}%</span>}
        </div>
        <p className="home-tasks-meta home-tasks-census">
          {[
            whole === 0 ? "No work yet" : censusLine(done, active, todo),
            project.target_date && `Target ${companyDateLabel(project.target_date)}`,
            project.last_change && `Changed ${relTime(project.last_change.at, now)}`,
          ]
            .filter(Boolean)
            .join(" · ")}
        </p>
      </div>
      <WidgetQuery
        error={open.error}
        refusal={open.refusal}
        loading={open.loading && !open.data}
        gaps={gapsOf([read.data, open.data])}
      >
        {rows.length > 0 ? <TaskList rows={rows} /> : null}
        <MoreLink
          shown={rows.length}
          total={
            open.data ? { total: open.data.total_hint, capped: open.data.total_capped } : undefined
          }
          to={projectPath(project.key)}
          noun="open task"
        />
      </WidgetQuery>
    </WidgetCard>
  );
}

// ---- The company's stuck work ---------------------------------------------------

/**
 * Work matching one of Worklet's own filters, company-wide: the list Worklet's
 * Tasks draws with that filter set, and its link opens exactly that list.
 */
function Filtered({
  title,
  filter,
  empty,
}: {
  title: string;
  filter: { key: "blocked" | "due"; wire: unknown; address: string };
  empty: { title: string; hint: string };
}) {
  const viewer = useViewer();
  const read = useQuery(
    "work_items",
    { container: "workspace", subtasks: "separate", limit: ROWS, [filter.key]: filter.wire },
    { pollMs: POLL_MS, enabled: viewer.worklet },
  );
  const rows = read.data?.items ?? [];
  const total = read.data
    ? { total: read.data.total_hint, capped: read.data.total_capped }
    : undefined;
  const to = ["worklet", "tasks"];
  const query = { [filter.key]: filter.address };
  return (
    <WidgetCard
      title={total ? `${title} · ${countLabel(total.total, total.capped)}` : title}
      from="worklet-tasks"
      query={query}
    >
      <WidgetQuery
        error={read.error}
        refusal={read.refusal}
        loading={read.loading && !read.data}
        gaps={gapsOf([read.data])}
        empty={read.data && rows.length === 0 ? empty : undefined}
      >
        <TaskList rows={rows} />
        <MoreLink shown={rows.length} total={total} to={to} query={query} noun="task" />
      </WidgetQuery>
    </WidgetCard>
  );
}

export const widgets = defineWidgets([
  {
    key: "worklet.view",
    from: "worklet-views",
    label: "Saved view",
    description: "The top tasks in a saved view",
    icon: LayoutDashboardGlyph,
    hue: "purple",
    size: { w: 6, minW: 3, minH: 8, vertical: true },
    params: [
      {
        key: "view",
        label: "Saved view",
        helper: "Yours and those shared with you.",
        useChoices: useViewChoices,
      },
    ],
    Component: SavedView,
  },
  {
    key: "worklet.project",
    from: "worklet",
    label: "Project",
    description: "One project's progress and open tasks",
    icon: FolderKanbanGlyph,
    hue: "green",
    size: { w: 4, minW: 3, minH: 10, vertical: true },
    params: [{ key: "project", label: "Project", useChoices: useProjectChoices }],
    nameOf: (p) => (p.project ? `Project ${p.project}` : undefined),
    Component: Project,
  },
  {
    key: "worklet.blocked",
    from: "worklet-tasks",
    label: "Blocked work",
    description: "Open tasks waiting on another task",
    icon: BanGlyph,
    hue: "rose",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: function BlockedWork() {
      return (
        <Filtered
          title="Blocked work"
          filter={{ key: "blocked", wire: true, address: "true" }}
          empty={{ title: "Nothing is blocked", hint: "No open task is waiting on another." }}
        />
      );
    },
  },
  {
    key: "worklet.overdue",
    from: "worklet-tasks",
    label: "Overdue work",
    description: "Open tasks past their due date",
    icon: CalendarClockGlyph,
    hue: "amber",
    size: { w: 4, minW: 3, minH: 8, vertical: true },
    once: true,
    Component: function OverdueWork() {
      return (
        <Filtered
          title="Overdue work"
          filter={{ key: "due", wire: "overdue", address: "overdue" }}
          empty={{
            title: "Nothing is overdue",
            hint: "Every open task with a due date is on time.",
          }}
        />
      );
    },
  },
]);
