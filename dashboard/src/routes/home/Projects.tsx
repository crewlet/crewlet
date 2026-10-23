/**
 * "Projects" — how far along each project is: its done, active and to-do work
 * as one bar, its lead and the date its lead means it to be finished.
 *
 * THE COUNTS ARE THE ENGINE'S MAINTAINED CENSUS (`task_counts`), the same
 * numbers the sidebar's project rows draw, never a count this screen took.
 * Percent done is done over the work that is not closed — closed work is out
 * of the way rather than finished, and counting it would move a project
 * forward every time somebody tidied its board.
 */

import { useServicesRun } from "~/lib/services.ts";
import { useMemo } from "react";
import { Card, SegmentedMeter, StatusDot } from "@crewlethq/ui";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { href } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { TRACKER, needsWords } from "~/lib/access.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import type { AgentAvatar } from "~/lib/avatar.ts";
import { indexOrg } from "~/lib/seats.ts";
import { companyDateLabel, plural } from "~/lib/format.ts";
import type { WorkProjectRow } from "~/protocol/index.ts";
import { projectPath } from "~/lib/work.ts";

/** How many projects the card draws before "All projects". */
export const PROJECT_ROWS = 3;

/** The sidebar's own listing, asked with the same arguments so the two read
 *  one answer. */
const PROJECTS_PAGE = 200;

export function Projects() {
  // THE TRACKER'S LIST, asked only of a reader holding Tracker, of a company
  // running Crewlet Works (the card is not drawn elsewhere).
  const viewer = useViewer();
  const runs = useServicesRun();
  const read = useQuery(
    "work_projects",
    { limit: PROJECTS_PAGE },
    { pollMs: 120_000, enabled: viewer.tracker && runs.tracker },
  );
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const rows = read.data?.projects?.filter((p) => !p.archived) ?? [];
  // THE MOST OPEN WORK FIRST: the projects with the most still to do are
  // the ones a reader looks at this card for.
  const shown = [...rows]
    .sort((a, b) => open(b) - open(a) || a.key.localeCompare(b.key))
    .slice(0, PROJECT_ROWS);
  return (
    <Card padding="none" className="home-card">
      <Card.Header
        actions={
          <span className="home-legend" aria-hidden="true">
            <span>
              <StatusDot tone="success" /> Done
            </span>
            <span>
              <StatusDot tone="info" /> Active
            </span>
            <span>
              <StatusDot tone="neutral" /> To do
            </span>
          </span>
        }
      >
        <Card.Title as="h3">Projects</Card.Title>
      </Card.Header>
      {!viewer.loading && !viewer.tracker ? (
        <p className="home-card-note">{needsWords("Projects", [TRACKER])}</p>
      ) : (
        <QueryState
          error={read.error}
          refusal={read.refusal}
          loading={read.loading && !read.data}
          empty={
            read.data && rows.length === 0
              ? {
                  title: "No projects yet",
                  hint: "Add one with New project in Work.",
                }
              : undefined
          }
        >
          <ul className="project-list">
            {shown.map((p) => (
              <ProjectRow
                key={p.key}
                project={p}
                leadName={
                  p.lead.handle ? (index.byHandle.get(p.lead.handle)?.name ?? p.lead.handle) : ""
                }
                leadAvatar={
                  p.lead.handle ? (index.byHandle.get(p.lead.handle)?.avatar ?? null) : null
                }
              />
            ))}
          </ul>
          {rows.length > PROJECT_ROWS && (
            <a className="home-card-more t-link" href={href(["work"])}>
              {`All ${plural(rows.length, "project")}`}
            </a>
          )}
        </QueryState>
      )}
    </Card>
  );
}

function open(p: WorkProjectRow): number {
  return p.task_counts.todo + p.task_counts.active;
}

function ProjectRow({
  project,
  leadName,
  leadAvatar,
}: {
  project: WorkProjectRow;
  leadName: string;
  leadAvatar: AgentAvatar | null;
}) {
  const { todo, active, done } = project.task_counts;
  const whole = todo + active + done;
  const pct = whole > 0 ? Math.round((done / whole) * 100) : null;
  return (
    <li>
      <a className="project-row" href={href(projectPath(project.key))}>
        <span className="project-row-head">
          <span className="project-key mono">{project.key}</span>
          <span className="project-name">{project.name}</span>
          {pct !== null && <span className="project-pct">{pct}%</span>}
        </span>
        <SegmentedMeter
          size="compact"
          total={whole}
          remainderLabel="to do"
          segments={[
            { id: "done", value: done, tone: "success", label: "done" },
            { id: "active", value: active, tone: "info", label: "active" },
          ]}
        />
        <span className="project-row-foot">
          <span>
            {whole === 0 ? "No open or finished work yet" : censusLine(done, active, todo)}
          </span>
          <span className="project-row-when">
            {leadName && (
              <SeatAvatar
                name={leadName}
                size="xs"
                kind={project.lead.kind === "human" ? "human" : "agent"}
                avatar={leadAvatar}
              />
            )}
            {project.target_date && <span>{companyDateLabel(project.target_date)}</span>}
          </span>
        </span>
      </a>
    </li>
  );
}

/** "38 done · 12 active · 11 to do", naming only the parts that hold work. */
export function censusLine(done: number, active: number, todo: number): string {
  return [
    done && `${done.toLocaleString()} done`,
    active && `${active.toLocaleString()} active`,
    todo && `${todo.toLocaleString()} to do`,
  ]
    .filter(Boolean)
    .join(" · ");
}
