/**
 * "Who builds it": the agent a create page asks, and the project its task is
 * filed in, for every kind whose page offers to ask an agent.
 *
 * # The agent
 *
 * Every agent seat, each drawn as its Crewlet character with its unit under
 * its name, the suggested one first and tagged (`lib/artifacts/builders.ts`
 * decides the order). A company with a handful of agents sees them all as a
 * list; a larger chart is a searchable field grouped as Suggested and Every
 * agent, so a page never grows by thirty rows.
 *
 * # The project
 *
 * The task's project follows the agent until the reader picks one: the
 * project of the agent's own unit, else the reader's, else the first
 * (`startingProject`, the New task sheet's rule). A page opened from a task
 * (`item=`) starts on that task's project.
 *
 * A page holds the choice with [useWhoBuildsIt] and draws it with
 * [WhoBuildsIt], or with [AgentPicker] and [ProjectPicker] where its layout
 * places the two apart, and files the task with `useFileArtifactTask`.
 */

import { useMemo, useState } from "react";
import { ChoiceList, FormField, Select, type SelectOption } from "@crewlethq/ui";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { indexOrg } from "~/lib/seats.ts";
import { projectOptions, startingProject } from "~/lib/startingProject.ts";
import { artifactBuilders, type Builder } from "~/lib/artifacts/builders.ts";
import type { WorkProjectRow } from "~/protocol/index.ts";
import type { MakeKind } from "~/lib/artifacts/words.ts";
import { useWorkItem } from "./create.ts";

/** Up to this many agents are drawn as a list; more are a searchable field. */
const LISTED_AGENTS = 4;

/** What a create page holds for "who builds it". */
export interface WhoBuildsItState {
  /** The agents offered, the suggested one first. */
  builders: Builder[];
  /** The chosen agent's handle, "" while the chart has none. */
  seat: string;
  setSeat: (seat: string) => void;
  /** The project the task is filed in, "" while none is known. */
  project: string;
  setProject: (project: string) => void;
  projects: readonly WorkProjectRow[];
  projectsLoading: boolean;
}

/**
 * The choice of agent and project for a kind, with its defaults: the
 * suggested agent (`suggested` names one, else the best match), and the
 * project that follows the agent until the reader picks one.
 */
export function useWhoBuildsIt(kind: MakeKind, suggested?: string): WhoBuildsItState {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const viewer = useViewer();
  const item = useWorkItem();
  const builders = useMemo(
    () => artifactBuilders(index, kind, suggested),
    [index, kind, suggested],
  );
  const [picked, setSeat] = useState("");
  const seat = builders.some((b) => b.seat.handle === picked)
    ? picked
    : (builders[0]?.seat.handle ?? "");
  const listed = useQuery("work_projects", { limit: 200 }, { pollMs: 120_000 });
  const projects = useMemo(() => listed.data?.projects ?? [], [listed.data]);
  const [chosenProject, setProject] = useState("");
  const project =
    chosenProject ||
    startingProject(
      {
        assignee: seat || undefined,
        project: item ? item.slice(0, item.lastIndexOf("-")) : undefined,
      },
      viewer.project,
      projects,
      index,
    );
  return {
    builders,
    seat,
    setSeat,
    project,
    setProject,
    projects,
    projectsLoading: listed.loading,
  };
}

/** The agents, each as its character, the suggested one first. */
export function AgentPicker({
  builders,
  value,
  onChange,
  label = "Agent",
}: {
  builders: readonly Builder[];
  value: string;
  onChange: (seat: string) => void;
  label?: string;
}) {
  if (builders.length === 0) {
    return (
      <FormField label={label}>
        <p className="artifact-note">
          No agent is on the chart yet. Add one in Edit org, then ask it here.
        </p>
      </FormField>
    );
  }
  const face = (b: Builder, size: "sm" | "xs") => (
    <SeatAvatar name={b.seat.name} kind="agent" avatar={b.seat.avatar} size={size} />
  );
  const where = (b: Builder) => b.seat.unit?.name || "Reports to the company";
  if (builders.length <= LISTED_AGENTS) {
    return (
      <FormField as="fieldset" label={label}>
        <ChoiceList
          label={label}
          value={value}
          onChange={onChange}
          options={builders.map((b) => ({
            value: b.seat.handle,
            label: b.seat.name,
            detail: where(b),
            media: face(b, "sm"),
            tag: b.suggested ? "Suggested" : undefined,
          }))}
        />
      </FormField>
    );
  }
  const options: SelectOption[] = builders.map((b) => ({
    value: b.seat.handle,
    label: b.seat.name,
    description: where(b),
    leading: face(b, "xs"),
    group: b.suggested ? "Suggested" : "Every agent",
    text: `${b.seat.name} ${b.seat.handle} ${where(b)}`,
  }));
  return (
    <FormField label={label}>
      {(field) => (
        <Select
          id={field.id}
          ariaLabel={label}
          searchable
          searchPlaceholder="Find an agent"
          width="full"
          value={value || undefined}
          options={options}
          onChange={(next) => onChange(String(next))}
        />
      )}
    </FormField>
  );
}

/** The project the task is filed in. */
export function ProjectPicker({
  projects,
  value,
  onChange,
  loading = false,
}: {
  projects: readonly WorkProjectRow[];
  value: string;
  onChange: (project: string) => void;
  loading?: boolean;
}) {
  return (
    <FormField label="Project">
      {(field) => (
        <Select
          id={field.id}
          ariaLabel="Project"
          searchable={projects.length > 8}
          searchPlaceholder="Find a project"
          width="full"
          placeholder={loading ? "Reading the projects…" : "Choose a project"}
          value={value || undefined}
          options={projectOptions(projects, value)}
          onChange={(next) => onChange(String(next))}
        />
      )}
    </FormField>
  );
}

/** The agent and, unless a page leaves it out, the project, together. */
export function WhoBuildsIt({
  who,
  withProject = true,
}: {
  who: WhoBuildsItState;
  withProject?: boolean;
}) {
  return (
    <div className="artifact-who">
      <AgentPicker builders={who.builders} value={who.seat} onChange={who.setSeat} />
      {withProject && (
        <ProjectPicker
          projects={who.projects}
          value={who.project}
          onChange={who.setProject}
          loading={who.projectsLoading}
        />
      )}
    </div>
  );
}
