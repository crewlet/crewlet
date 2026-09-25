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
import { href } from "~/app/router.tsx";
import type { SidebarSection, SidebarRow } from "../frame/WorkspaceSidebar.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { unfinished } from "~/lib/work.ts";
import { useAgents, useOrg } from "~/lib/store-hooks.ts";
import { indexOrg, seatTone, unitTally, UNIT_TOTAL_HINT, type Unit } from "~/lib/seats.ts";
import { useSandboxes } from "~/lib/store-hooks.ts";
import { useStarred } from "~/lib/starred.ts";
import { useRecents } from "~/lib/recents.ts";

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
 * Work: every project and the saved views.
 *
 * A PROJECT IS A LEAF. Its board, list and calendar are tabs of the project —
 * they are views OF the object you are on, and a sidebar row for each is what
 * turns two levels into three.
 */
export function useWorkSidebar(): SidebarSection[] {
  const projects = useQuery("work_projects", { limit: 200 }, { pollMs: 120_000 });
  const views = useQuery("work_views", { container: "workspace" }, { pollMs: 300_000 });

  return useMemo(() => {
    const projectRows: SidebarRow[] = (projects.data?.projects ?? []).map((p) => ({
      key: p.key,
      label: p.name || p.key,
      path: ["work", p.key],
      sub: p.lead?.handle || undefined,
      // THE MAINTAINED COLUMNS, not a count over the page: the engine keeps
      // the census on the project row itself precisely so a sidebar does not
      // have to aggregate per poll. The rail's number is everything not yet
      // finished — waiting and started together.
      count:
        p.task_counts == null
          ? undefined
          : {
              value: unfinished(p.task_counts),
              of: "unfinished items — the engine's own maintained count",
            },
    }));

    // HOW MANY ARE ARCHIVED, where EVERY one of them is — the same census the
    // directory and `#/work` read, so the three surfaces cannot disagree about
    // whether a company has projects. Zero while the read is in flight or
    // refused, which leaves the rail on the sentence that claims nothing: an
    // answer with no census in it is not a census of nothing.
    const census = projects.data?.census;
    const archived = census && census.active === 0 ? census.archived : 0;

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
        // AN EMPTY ACTIVE SET IS TWO STATES, and this rail read both as the
        // first: beside a `#/work/projects` saying "All 4 of the company's
        // projects have been archived", it said "No project has been created
        // yet" about the same four. The listing is the ACTIVE set — the
        // engine's own default — so the rows cannot tell them apart and the
        // CENSUS is what does, which is why the answer carries it whatever
        // the limit. Said with the count and the way to them, on the same
        // terms the directory's own empty state uses.
        empty:
          archived > 0 ? (
            <>
              {archived === 1
                ? "The company’s one project is archived."
                : `All ${archived} of the company’s projects are archived.`}{" "}
              <a className="prose-link" href={href(["work", "projects"], { shown: "archived" })}>
                See them under Archived
              </a>
              .
            </>
          ) : (
            "No project has been created yet."
          ),
      },
      {
        key: "views",
        label: "Saved views",
        rows: viewRows,
        empty: "Nobody has saved a view yet.",
      },
    ];
  }, [projects.data, views.data]);
}

/** Company: the charter, the directory and the unit tree. */
export function useCompanySidebar(): SidebarSection[] {
  const org = useOrg();
  return useMemo(() => {
    const index = indexOrg(org);
    // THE INDEXED TREE, not `org.units` off the wire. The raw projection
    // carries neither the subtree membership nor the effective lead, so this
    // walked it and re-derived both by hand — a filter over every seat per
    // unit for the count, and "the first seat whose unit is this one" for the
    // lead, which answers nothing for a unit with no seats of its own.
    function rowsFor(units: Unit[] | undefined): SidebarRow[] {
      return (units ?? []).map((unit) => {
        // BY NAME. A unit's stable `id:` is guarded and the anonymous org
        // projection does not carry it, so every route to a unit is its name.
        const key = unit.name;
        return {
          key,
          label: unit.name,
          path: ["company", "units", key],
          // THE EFFECTIVE LEAD, which is the nearest ancestor's where a unit
          // declares none: it behaves identically everywhere in the engine,
          // and hiding the difference is how somebody concludes a team is
          // unmanaged.
          sub: unit.effectiveLead?.handle || undefined,
          // THE SUBTREE, THROUGH `unitTally`, and its own sentence from
          // `UNIT_TOTAL_HINT` — because the number this rail draws was the one
          // contradicted elsewhere: "Leadership 5" here against "2 seats" on
          // the org chart's block for the same unit. Derived rather than
          // re-counted, so the three surfaces cannot drift again.
          count: { value: unitTally(unit).total, of: UNIT_TOTAL_HINT },
          children: rowsFor(unit.children),
        };
      });
    }
    return [
      { key: "fixed", rows: fixed("company") },
      {
        key: "units",
        label: "Units",
        rows: rowsFor(index.topUnits),
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
          count: { value: c.pages, of: "pages in this container, trashed ones excluded" },
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
/**
 * The Admin tree.
 *
 * SCOPED TO READERS WHO ARE IN ADMIN, which is the one thing a sidebar hook
 * has to do for itself. Every one of them runs on every screen — React's rules
 * make that unavoidable, and `useSidebar` says why — so a hook that polls
 * unconditionally polls for readers who will never see its rows. Both answers
 * here are registered operator-only, so for everybody else that was a refusal
 * every thirty seconds, for the whole life of the tab, on screens that do not
 * have this tree.
 */
export function useAdminSidebar(here: boolean): SidebarSection[] {
  const fleet = useQuery("fleet", undefined, { enabled: here, pollMs: 30_000 });
  const integrations = useQuery("integrations", undefined, { enabled: here, pollMs: 120_000 });
  // THE STATE-LOG DOMAINS, from the document the Infrastructure page already
  // reads. Slow, because a domain's floor moves on the trim's own tick.
  const retention = useQuery("retention", undefined, { enabled: here, pollMs: 60_000 });

  return useMemo(() => {
    const nodes: SidebarRow[] = (fleet.data?.nodes ?? []).map((n) => ({
      key: n.id,
      label: n.id,
      path: ["admin", "fleet", n.id],
      count: { value: n.seats, of: "seats this node holds" },
    }));
    // UNCONFIGURED IS NOT A FAULT, so it draws no dot at all. A surface
    // nobody has wired is a decision rather than a failure, and a neutral
    // dot beside a green one reads as "something is wrong here".
    const kinds: SidebarRow[] = (integrations.data?.integrations ?? []).map((i) => ({
      key: i.key,
      // The key, because it is all the row carries: a `label` was read here
      // that the engine has never sent.
      label: i.key,
      path: ["admin", "integrations", i.key],
      tone: i.configured ? ("positive" as const) : undefined,
    }));
    // THE NODES, THEN THE DOMAINS. A node is a machine and a domain is a log,
    // and the trim's floor is the minimum across every node's position in one
    // domain — so the two belong under Infrastructure together and neither is
    // a filter on the other. The tone is STATE, never identity: a domain
    // draws a mark only when something is holding it.
    const domains: SidebarRow[] = (retention.data?.domains ?? []).map((d) => ({
      key: `domain-${d.domain}`,
      label: d.domain,
      path: ["admin", "fleet", "domains", d.domain],
      tone: d.blocked_by ? ("caution" as const) : undefined,
    }));
    const rows = fixed("admin").map((row) =>
      row.key === "fleet"
        ? { ...row, children: [...nodes, ...domains] }
        : row.key === "integrations"
          ? { ...row, children: kinds }
          : row,
    );
    return [{ key: "fixed", rows }];
  }, [fleet.data, integrations.data, retention.data]);
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
 * NEITHER SECTION MOVES WHILE IT IS BEING READ. A star is a decision, so its
 * row is where the reader put it; and `useRecents` is ARRIVAL order rather
 * than visit order precisely so that opening a row from this rail does not
 * send it to the top and slide the rows above it down — which is what it did,
 * at the instant the pointer landed. `lib/recents.ts` carries the reasoning,
 * and the command palette takes the other order from the same store.
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
          // THE SAME STAR THE PAGE BAR'S CONTROL DRAWS, which is the whole
          // requirement: the mark that fills this section and the mark you
          // press to fill it have to be one drawing. `@crewlethq/icons` has
          // not vendored a star, so it is one of the four this build holds —
          // see `src/ui/symbols/README.md`.
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
          icon: "schedule" as const,
        })),
      });
    }
    return out;
  }, [stars, recents, workspace]);
}
