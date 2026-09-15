/**
 * One sidebar per workspace, each built from the answers that already exist.
 *
 * # Built from live answers, never from a table
 *
 * A workspace's tree is the company's own shape — the projects it has, the
 * units the chart declares, the containers this node knows about. A
 * hand-maintained copy of any of those would be a second list that is wrong
 * the first time somebody adds a project, and it would look exactly like a
 * correct one.
 *
 * What IS hand-written is the fixed furniture: the lists and landing pages
 * that exist whatever a company contains. Those come from `nav.ts`'s
 * destinations, so the palette and the sidebar cannot disagree about where a
 * screen lives.
 *
 * # Every row is a path
 *
 * See the grammar in `nav.ts`. The one admitted exception is a row whose
 * destination IS a filtered list — a seat's turns are `#/activity/turns?seat=`
 * and there is no other address for them — and it is admitted because the row
 * is not a filter on the page you are on: it is a different list.
 */

import { useMemo } from "react";
import { destinationsOf } from "../nav.ts";
import type { SidebarSection, SidebarRow } from "../frame/WorkspaceSidebar.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useAgents, useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, seatTone } from "~/lib/seats.ts";
import { useSandboxes } from "~/lib/store-hooks.ts";
import { useStarred } from "~/lib/starred.ts";
import { useRecents } from "~/lib/recents.ts";
import type { OrgUnit, WorkProjectRow } from "~/protocol/index.ts";

/**
 * A project's sprint rows: the one that is running, and the list.
 *
 * A PROJECT THAT RUNS NO SPRINTS gets neither — `sprints` is absent on such a
 * project, which is a different fact from a project that runs them and has
 * none right now, and offering a sprint list for a team that does not sprint
 * is an invitation to a screen that can only ever be empty.
 */
function sprintRows(p: WorkProjectRow): SidebarRow[] {
  if (!p.sprints) return [];
  const rows: SidebarRow[] = [];
  const active = p.sprints.active;
  if (active) {
    rows.push({
      key: `${p.key}-sprint-${active.number}`,
      label: active.name || `Sprint ${active.number}`,
      path: ["work", p.key, "sprints", String(active.number)],
      tone: "info",
    });
  }
  rows.push({
    key: `${p.key}-sprints`,
    label: "All sprints",
    path: ["work", p.key, "sprints"],
  });
  return rows;
}

/** The fixed rows of a workspace, from the one destinations table. */
function fixed(workspace: Parameters<typeof destinationsOf>[0]): SidebarRow[] {
  return destinationsOf(workspace).map((d) => ({
    key: d.key,
    label: d.label,
    path: d.path,
    icon: d.icon,
  }));
}

/**
 * Work: every project, its sprints, the goals and the saved views.
 *
 * A PROJECT EXPANDS TO ITS SPRINTS AND NOTHING ELSE. Its board, list,
 * calendar and backlog are tabs of the project — they are views OF the object
 * you are on, and a sidebar row for each is what turns two levels into three.
 */
export function useWorkSidebar(): SidebarSection[] {
  const projects = useQuery("work_projects", { limit: 200 }, { pollMs: 120_000 });
  const goals = useQuery("work_goals", undefined, { pollMs: 120_000 });
  const views = useQuery("work_views", { container: "workspace" }, { pollMs: 300_000 });

  return useMemo(() => {
    const projectRows: SidebarRow[] = (projects.data?.projects ?? []).map((p) => ({
      key: p.key,
      label: p.name || p.key,
      path: ["work", p.key],
      sub: p.lead?.handle || undefined,
      // THE MAINTAINED COLUMN, not a count over the page: the engine keeps
      // open/done/closed on the project row itself precisely so a sidebar
      // does not have to aggregate per poll.
      count: p.task_counts?.open,
      countTitle: "open items — the engine's own maintained count",
      children: sprintRows(p),
    }));

    const goalRows: SidebarRow[] = (goals.data?.goals ?? []).map((g) => ({
      key: g.id,
      label: g.name,
      path: ["goals", g.id],
      sub: g.group || undefined,
    }));

    // SAVED VIEWS ONLY. The builtin rows a container has without anybody
    // saving one are the list's own view strip, not destinations — a builtin
    // has no id, so there is nothing to address.
    const viewRows: SidebarRow[] = (views.data?.views ?? [])
      .filter((v) => !v.builtin && v.id)
      .map((v) => ({
        key: v.id as string,
        label: v.name,
        path: ["work", "views", v.id as string],
        sub: v.owner || undefined,
      }));

    return [
      { key: "fixed", rows: fixed("work") },
      {
        key: "projects",
        label: "Projects",
        rows: projectRows,
        empty: "No project has been created yet.",
      },
      { key: "goals", label: "Goals", rows: goalRows, empty: "No goal has been set." },
      {
        key: "views",
        label: "Saved views",
        rows: viewRows,
        empty: "Nobody has saved a view yet.",
      },
    ];
  }, [projects.data, goals.data, views.data]);
}

/** Company: the charter, the directory and the unit tree. */
export function useCompanySidebar(): SidebarSection[] {
  const org = useOrg();
  return useMemo(() => {
    const index = indexOrg(org);
    function rowsFor(units: OrgUnit[] | undefined): SidebarRow[] {
      return (units ?? []).map((unit) => {
        const key = unit.id || unit.name;
        return {
          key,
          label: unit.name,
          path: ["company", "units", key],
          // THE EFFECTIVE LEAD, which is the nearest ancestor's where a unit
          // declares none: it behaves identically everywhere in the engine,
          // and hiding the difference is how somebody concludes a team is
          // unmanaged.
          sub: index.seats.find((s) => s.unit?.name === unit.name)?.unitLead || undefined,
          count: index.seats.filter((s) => s.unitChain.some((u) => u.name === unit.name)).length,
          countTitle: "seats in this unit and everything under it",
          children: rowsFor(unit.children),
        };
      });
    }
    return [
      { key: "fixed", rows: fixed("company") },
      {
        key: "units",
        label: "Units",
        rows: rowsFor(org?.units),
        empty: "This company declares no units — every seat sits at the top level.",
      },
    ];
  }, [org]);
}

/** Knowledge: the containers this node knows about. */
export function useKnowledgeSidebar(): SidebarSection[] {
  const containers = useQuery("containers", undefined, { pollMs: 300_000 });
  return useMemo(
    () => [
      { key: "fixed", rows: fixed("knowledge") },
      {
        key: "containers",
        label: "Containers",
        rows: (containers.data?.containers ?? []).map((c) => ({
          key: c.key,
          label: c.name || c.key,
          path: ["knowledge", c.key],
          // HOW MUCH IS IN IT, which is what makes a rail of forty spaces
          // navigable: the one with four hundred pages and the one with
          // two looked identical. Trashed pages are not counted, so the
          // number and the list behind it agree.
          count: c.pages,
          countTitle: "pages in this container, trashed ones excluded",
        })),
        empty: "This node knows about no containers yet.",
      },
    ],
    [containers.data],
  );
}

/**
 * Activity: the kinds of thing the company does, and the seats doing them.
 *
 * A SEAT ROW IS THAT SEAT'S TURNS, which is a different list from the one you
 * are on rather than a filter on it — the admitted exception to "a row differs
 * by its path". There is no other address for one seat's turns.
 */
export function useActivitySidebar(): SidebarSection[] {
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const org = useOrg();

  return useMemo(() => {
    const index = indexOrg(org);
    const seats: SidebarRow[] = index.seats
      .filter((s) => s.kind !== "human")
      .map((seat) => {
        const live = agents.find((a) => a.role === seat.name);
        return {
          key: seat.handle,
          label: seat.name,
          path: ["activity", "turns"],
          query: { seat: seat.handle },
          tone: toneOf(seatTone(live, sandboxes)),
        };
      });
    return [
      { key: "fixed", rows: fixed("activity") },
      {
        key: "seats",
        label: "Seats",
        rows: seats,
        empty: "This company has no agent seats.",
      },
    ];
  }, [agents, sandboxes, org]);
}

/** Cost: two lists, and nothing that is a filter wearing a row's clothes. */
export function useCostSidebar(): SidebarSection[] {
  return useMemo(() => [{ key: "fixed", rows: fixed("cost") }], []);
}

/** Admin: the estates, each expanding to what it holds. */
export function useAdminSidebar(): SidebarSection[] {
  const fleet = useQuery("fleet", undefined, { pollMs: 30_000 });
  const integrations = useQuery("integrations", undefined, { pollMs: 120_000 });

  return useMemo(() => {
    const nodes: SidebarRow[] = (fleet.data?.nodes ?? []).map((n) => ({
      key: n.id,
      label: n.id,
      path: ["admin", "fleet", n.id],
      count: n.seats,
      countTitle: "seats this node holds",
    }));
    // UNCONFIGURED IS NOT A FAULT, so it draws no dot at all. A surface
    // nobody has wired is a decision rather than a failure, and a neutral
    // dot beside a green one reads as "something is wrong here".
    const kinds: SidebarRow[] = (integrations.data?.integrations ?? []).map((i) => ({
      key: i.key,
      label: i.label || i.key,
      path: ["admin", "integrations", i.key],
      tone: i.configured ? ("positive" as const) : undefined,
    }));
    const rows = fixed("admin").map((row) =>
      row.key === "fleet"
        ? { ...row, children: nodes }
        : row.key === "integrations"
          ? { ...row, children: kinds }
          : row,
    );
    return [{ key: "fixed", rows }];
  }, [fleet.data, integrations.data]);
}

/** The seat tone vocabulary, as the sidebar's one-word dot. */
function toneOf(tone: string): SidebarRow["tone"] {
  if (tone === "working") return "info";
  if (tone === "needs") return "caution";
  if (tone === "broken") return "critical";
  return undefined;
}

/**
 * The two sections every workspace gets for free.
 *
 * WHAT YOU KEPT AND WHAT YOU OPENED, and they are opposites: a star is a
 * decision that does not decay, a recent is a side effect that does. A rail
 * built only from recents loses the board somebody checks every morning the
 * first day they spend somewhere else, and one built only from stars never
 * offers the thing they were reading five minutes ago.
 *
 * SCOPED TO THIS WORKSPACE. Both records carry the workspace they were made
 * in, so the Work sidebar offers work and the Admin sidebar offers nodes —
 * a single global list would put a config revision under Knowledge.
 *
 * Neither section renders empty. There is nothing to say about a reader who
 * has kept nothing: the control that fills it is in the page bar, not here,
 * so an empty-state sentence would point at a button in another component.
 *
 * `lib/starred.ts` and `lib/recents.ts` were both written whole — the caps,
 * the refusals, the storage guards, their suites — and until the page bar
 * grew a star and this grew its sections, only the command palette read
 * either, so a star had no producer at all and a recent had one reader.
 */
export function useKeptSections(workspace: string): SidebarSection[] {
  const stars = useStarred();
  const recents = useRecents();
  return useMemo(() => {
    const out: SidebarSection[] = [];
    const kept = stars.filter((s) => s.workspace === workspace);
    if (kept.length > 0) {
      out.push({
        key: "starred",
        label: "Starred",
        collapsible: true,
        rows: kept.map((s) => ({
          key: `star-${s.path.join("/")}`,
          label: s.label,
          path: s.path,
          icon: "star" as const,
        })),
      });
    }
    const seen = recents.filter((r) => r.workspace === workspace);
    if (seen.length > 0) {
      out.push({
        key: "recents",
        label: "Recent",
        collapsible: true,
        rows: seen.map((r) => ({
          key: `recent-${r.path.join("/")}`,
          label: r.label,
          path: r.path,
          icon: "clock" as const,
        })),
      });
    }
    return out;
  }, [stars, recents, workspace]);
}
