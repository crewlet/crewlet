/**
 * Switching Worklet's scope: All projects, or one project.
 *
 * # One panel, opened from the trail or by a key
 *
 * Worklet's pages end their trail in the scope they show, drawn as the kit's
 * Switcher button ([ScopeSwitcher], which the page header puts in the trail's
 * heading). G then P opens the same panel from anywhere: under that button
 * where the page has one, and centred over the page everywhere else, where
 * nothing behind it moves until a row is chosen. Both read ONE provider
 * ([ProjectSwitcherProvider]), so the panel's state, its search and the
 * listing it reads are never two.
 *
 * # What it reads
 *
 * The company's ACTIVE projects (`work_projects`, the engine's own default
 * set) at the listing's cap of 200. Past the cap the answer says it was cut
 * short and so does the panel, and a search is sent to the engine once typing
 * rests, so every project is found by its key, its name or its purpose, not
 * only the ones listed. Archived projects are never offered here: they are
 * listed under Projects, and the panel says so where a reader would look.
 *
 * The same read gives All projects' lenses their figures ([useScopeFigures]),
 * so the trail's lenses and the panel's first row cannot disagree.
 *
 * # Order
 *
 * `lib/projectRank.ts`, the order ⌘K uses too.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  Switcher,
  type SwitcherGroup,
  type SwitcherMessage,
  type SwitcherOption,
  type SwitcherTrigger,
} from "@crewlethq/ui";
import {
  Columns3Glyph,
  KeyGlyph,
  LayersGlyph,
  PlusGlyph,
  RefreshCwGlyph,
  SearchGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useNavigator, useRoute } from "../router.tsx";
import { KEY_SHAPE } from "../routes.ts";
import { sequenceWords, useKeymap } from "../keymap.ts";
import { useServicesAsk, useServicesRun } from "~/lib/services.ts";
import { LayerBoundary } from "../boundaries.tsx";
import { lazyScreen, preload } from "../lazyScreen.ts";
import {
  ALL_PROJECTS,
  sameScope,
  scopeAt,
  switchTarget,
  type Scope,
  type ScopeAt,
} from "../scope.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useRecents, type Recent } from "~/lib/recents.ts";
import { useConnection } from "~/lib/store-hooks.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useConfigWriteAccess, type ConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { PROJECT_PAGE, unfinished } from "~/lib/work.ts";
import { rankProjects } from "~/lib/projectRank.ts";
import type { WorkProjectRow, WorkProjectsAnswer } from "~/protocol/index.ts";

/** How long typing rests before a search is sent to the engine. */
const SEARCH_DELAY_MS = 150;
/** Past this many active projects the panel leads with the ones opened last. */
const RECENT_FROM = 8;
/** How many recent projects it leads with. */
const RECENT_ROWS = 3;
/** The panel's name, which the button's tooltip leads with too. */
const LABEL = "Switch project";

const ALL: Scope = { kind: "all" };

/** The button's tooltip: what it does and the keys that do it from anywhere. */
const TRIGGER_TITLE = `${LABEL} (${sequenceWords("project")})`;

/** All projects' figures, as the trail's lenses and the panel's first row draw them. */
export interface ScopeFigures {
  /** How many projects are active. */
  projects?: number;
  /** The open work across them, written as drawn: a floor ("200+") past the cap. */
  open?: number | string;
}

/**
 * The figures the listing supports. An answer with no census in it is not a
 * census of nothing, so a missing one draws no figure rather than a zero.
 */
export function figuresOf(answer: WorkProjectsAnswer | null): ScopeFigures {
  if (!answer) return {};
  const sum = answer.projects.reduce(
    (n, p) => n + (p.task_counts ? unfinished(p.task_counts) : 0),
    0,
  );
  return {
    ...(answer.census ? { projects: answer.census.active } : {}),
    open: answer.truncated ? `${sum}+` : sum,
  };
}

/** What the panel draws, from the reads and the reader's place in the product. */
export interface PanelInput {
  /** The active projects, unsearched. */
  base: { data: WorkProjectsAnswer | null; error: string | null };
  /** The engine's answer to the query, asked once the unsearched listing was cut short. */
  searched: WorkProjectsAnswer | null;
  query: string;
  /** Where the reader is, or null away from Worklet's scope pages. */
  at: ScopeAt | null;
  recents: readonly Recent[];
  /**
   * Whether this viewer can make a project, and why not: the company grant's
   * gate (`useConfigWriteAccess`), never a lead's. A project key is one of the
   * KEYS another system finds a team by and it outlives the team that declared
   * it, so the engine refuses a lead every write that sets one
   * (`config.DiffOrg`'s keyed fields) — offered to a lead, New project would
   * end on a Project field Edit org draws locked.
   */
  newProject: ConfigWriteAccess;
}

/** What taking a row does. */
export interface PanelActions {
  go: (to: Scope) => void;
  hrefOf: (to: Scope) => string;
  browse: () => void;
  searchTasks: (query: string) => void;
  newProject: () => void;
  retry: () => void;
}

export interface Panel {
  pinned?: SwitcherOption;
  note?: string;
  message?: SwitcherMessage;
  groups?: SwitcherGroup[];
  footer: SwitcherOption[];
  loading?: boolean;
  announcement?: string;
}

/**
 * The panel, as data: which rows, which state, which words. A pure function
 * of what was read, so every state is held by the suite without a socket.
 */
export function panelOf(input: PanelInput, act: PanelActions): Panel {
  const { base, searched, at, recents, newProject } = input;
  const q = input.query.trim();
  const answer = base.data;
  const current = at?.scope ?? null;
  const census = answer?.census;
  const total = census ? census.active + census.archived : (answer?.projects.length ?? 0);

  const footer: SwitcherOption[] = [];
  if (answer && total > 0) {
    footer.push({
      id: "browse",
      label: "Browse all projects",
      icon: <Columns3Glyph size="md" />,
      href: href(["worklet"]),
      onSelect: act.browse,
    });
  }
  footer.push({
    id: "new",
    label: "New project…",
    icon: <PlusGlyph size="md" />,
    // HELD WITH THE GATE'S OWN SENTENCE, the one Add seat and every org chart
    // write give: a key mark only where a grant is what is missing.
    ...(newProject.can
      ? { onSelect: act.newProject }
      : {
          held: newProject.reason,
          ...(newProject.block === "no_grant"
            ? { meta: <KeyGlyph size="xs" aria-hidden="true" /> }
            : {}),
        }),
  });

  if (!answer) {
    if (base.error) {
      return {
        footer,
        message: {
          title: "The project list did not load",
          body: "The page behind it is unchanged.",
          actions: [
            {
              id: "retry",
              label: "Try again",
              icon: <RefreshCwGlyph size="md" />,
              keepsOpen: true,
              onSelect: act.retry,
            },
          ],
        },
      };
    }
    return { footer, loading: true };
  }

  const row = (p: WorkProjectRow): SwitcherOption => {
    const name = p.name || p.key;
    const open = p.task_counts ? unfinished(p.task_counts) : undefined;
    const to: Scope = { kind: "project", key: p.key };
    return {
      id: p.key,
      label: name,
      keyChip: p.key,
      meta: open,
      accessibleLabel: open === undefined ? `${name}, ${p.key}` : `${name}, ${p.key}, ${open} open`,
      href: act.hrefOf(to),
      current: current ? sameScope(current, to) : false,
      onSelect: () => act.go(to),
    };
  };

  // THE ENGINE'S ANSWER TO THE QUERY once the listing was cut short, and
  // until it lands the listed projects ranked here, so typing never waits on
  // a round trip to show what is already on hand.
  const pending = q !== "" && answer.truncated === true && searched === null;
  const rows = searched
    ? rankProjects(searched.projects, q, { matched: true })
    : rankProjects(answer.projects, q);

  const lower = q.toLowerCase();
  const figures = figuresOf(answer);
  const pinned: SwitcherOption | undefined =
    !q || ALL_PROJECTS.toLowerCase().startsWith(lower) || "projects".startsWith(lower)
      ? {
          id: "all",
          label: ALL_PROJECTS,
          icon: <LayersGlyph size="md" />,
          meta: figures.open,
          accessibleLabel:
            figures.open === undefined ? ALL_PROJECTS : `${ALL_PROJECTS}, ${figures.open} open`,
          href: act.hrefOf(ALL),
          current: current?.kind === "all",
          onSelect: () => act.go(ALL),
        }
      : undefined;

  const groups: SwitcherGroup[] = [];
  if (!q && (census?.active ?? 0) > RECENT_FROM) {
    const recent = recentProjects(recents)
      .filter((key) => !(current?.kind === "project" && current.key === key))
      .map((key) => answer.projects.find((p) => p.key === key))
      .filter((p): p is WorkProjectRow => p !== undefined)
      .slice(0, RECENT_ROWS);
    if (recent.length > 0) groups.push({ id: "recent", label: "Recent", options: recent.map(row) });
  }
  if (rows.length > 0) {
    groups.push({ id: "projects", label: "Projects", count: rows.length, options: rows.map(row) });
  }

  let message: SwitcherMessage | undefined;
  if (!q && total === 0) {
    message = {
      title: "No projects yet",
      body: "Create one with New project.",
    };
  } else if (!q && census && census.active === 0) {
    message = {
      title: "No active projects",
      body: "Archived projects are under Projects.",
    };
  } else if (q && rows.length === 0 && !pending) {
    message = {
      title: `No project matches “${q}”`,
      body: "Archived projects are listed under Projects.",
      actions: [
        {
          id: "search-tasks",
          label: `Search tasks for “${q}”`,
          icon: <SearchGlyph size="md" />,
          href: href(["worklet", "search"], { q }),
          onSelect: () => act.searchTasks(q),
        },
      ],
    };
  }

  const note =
    answer.truncated && !q && census
      ? `${answer.projects.length} of ${census.active}. Type to search.`
      : undefined;

  const announcement = !q
    ? undefined
    : rows.length > 0
      ? `${rows.length} ${rows.length === 1 ? "project" : "projects"}`
      : pending
        ? undefined
        : "No project matches";

  return {
    ...(pinned ? { pinned } : {}),
    ...(note ? { note } : {}),
    ...(message ? { message } : {}),
    groups,
    footer,
    ...(pending && rows.length === 0 ? { loading: true } : {}),
    ...(announcement ? { announcement } : {}),
  };
}

/** The projects this reader opened last, most recent first, by key. */
function recentProjects(recents: readonly Recent[]): string[] {
  return recents
    .filter(
      (r) => r.path.length === 2 && r.path[0] === "worklet" && KEY_SHAPE.test(r.path[1] ?? ""),
    )
    .map((r) => r.path[1]!);
}

interface SwitcherContextValue {
  /** Whether the panel is open, and whether it opened centred. */
  open: boolean;
  centred: boolean;
  setOpen: (open: boolean) => void;
  /** Open the panel: under the trail's button on Worklet's scope pages, centred elsewhere. */
  openSwitcher: () => void;
  openNewProject: () => void;
  query: string;
  setQuery: (query: string) => void;
  panel: Panel;
  figures: ScopeFigures;
}

const SwitcherContext = createContext<SwitcherContextValue | null>(null);

/** What the openers are outside the frame: nothing to open, so nothing happens. */
const nothing = () => {};

/**
 * Open the switcher: G then P, and ⌘K's "Switch project…". A screen rendered
 * outside the frame (a suite) gets a function that does nothing rather than
 * one that reaches a panel that is not there, as `newTask.ts`'s opener does.
 */
export function useOpenProjectSwitcher(): () => void {
  return useContext(SwitcherContext)?.openSwitcher ?? nothing;
}

/** Open the New project dialog: the switcher's footer and the Projects lens. */
export function useOpenNewProject(): () => void {
  return useContext(SwitcherContext)?.openNewProject ?? nothing;
}

/** All projects' figures, for the trail's lenses; none outside the frame. */
export function useScopeFigures(): ScopeFigures {
  return useContext(SwitcherContext)?.figures ?? {};
}

/** The New project dialog, out of the Worklet chunk: a Worklet form, like New task. */
const NewProjectDialog = lazyScreen("worklet", (module) => module.NewProjectDialog);

export function ProjectSwitcherProvider({ children }: { children: ReactNode }) {
  const route = useRoute();
  const nav = useNavigator();
  const recents = useRecents();
  const newProjectAccess = useConfigWriteAccess();
  // A BROWSER THE ENGINE REFUSED ASKS IT NOTHING, as the palette does not:
  // the listing would wait on a socket nobody dials and draw a panel that
  // never loads. NOR DOES A READER WITHOUT WORKLET, whose every project the
  // engine would refuse: the palette offers them no switch to open.
  const worklet = useViewer().worklet;
  // AND NONE WHERE THE COMPANY RUNS NO WORKLET: a hidden Worklet address
  // still names a scope, and the projects were asked on it and refused.
  const ask = useServicesAsk();
  const served = useConnection().accessRefused === null && worklet && ask.tracker;
  const at = useMemo(() => scopeAt(route.path, route.query), [route.path, route.query]);

  const [state, setState] = useState({ open: false, centred: false });
  const [query, setQuery] = useState("");
  const [sent, setSent] = useState("");
  const [newProject, setNewProject] = useState(false);
  // A SWITCH FROM THE CENTRED PANEL lands on a page whose trail has the
  // button; focus goes there, so the reader is on the name of the place they
  // moved to, as a switch from the trail leaves them.
  const focusTrail = useRef(false);

  // READ WHILE IT CAN BE SEEN: the panel open, or a page whose trail carries
  // All projects' lenses or a project's switcher.
  const base = useQuery(
    "work_projects",
    { limit: PROJECT_PAGE },
    { enabled: served && (state.open || at !== null), pollMs: 120_000 },
  );
  const truncated = base.data?.truncated === true;
  const search = useQuery(
    "work_projects",
    { limit: PROJECT_PAGE, q: sent },
    { enabled: served && state.open && truncated && sent !== "" },
  );

  // THE ENGINE SEARCHES ONLY WHAT THE LISTING COULD NOT HOLD, once typing
  // rests: under the cap every project is on hand and ranked here at once.
  useEffect(() => {
    const q = query.trim();
    if (!truncated || q === "") {
      setSent("");
      return;
    }
    const timer = window.setTimeout(() => setSent(q), SEARCH_DELAY_MS);
    return () => window.clearTimeout(timer);
  }, [query, truncated]);

  const close = useCallback(() => {
    setState((was) => (was.open ? { ...was, open: false } : was));
    setQuery("");
  }, []);

  // A NEW PAGE CLOSES IT, however the reader got there (Back, a link), as the
  // palette closes; and a switch from the centred panel focuses the trail's
  // button once the page it lands on is drawn.
  useEffect(() => {
    close();
    if (!focusTrail.current) return;
    focusTrail.current = false;
    const frame = requestAnimationFrame(() =>
      document
        .querySelector<HTMLElement>(".crewlet-breadcrumb__here .crewlet-switcher-trigger")
        ?.focus(),
    );
    return () => cancelAnimationFrame(frame);
  }, [route.hash, close]);

  const openSwitcher = useCallback(() => {
    // Every scope's page is in the Worklet chunk: fetched while the reader looks.
    preload(["worklet"]);
    setState({ open: true, centred: at === null });
  }, [at]);
  // G THEN P, from anywhere (`keymap.ts`), where the company runs Worklet:
  // its projects are what the switcher lists.
  const runs = useServicesRun();
  useKeymap({ project: { run: openSwitcher, when: runs.tracker } });

  const setOpen = useCallback(
    (open: boolean) => {
      if (open) {
        preload(["worklet"]);
        setState({ open: true, centred: false });
      } else close();
    },
    [close],
  );

  const centred = state.centred;
  const actions: PanelActions = useMemo(
    () => ({
      go: (to) => {
        if (at && sameScope(at.scope, to)) return;
        const target = switchTarget(at, to, route.query);
        if (centred) focusTrail.current = true;
        nav.to(target.path, target.query);
      },
      hrefOf: (to) => {
        const target = switchTarget(at, to, route.query);
        return href(target.path, target.query);
      },
      browse: () => nav.to(["worklet"]),
      searchTasks: (q) => nav.to(["worklet", "search"], { q }),
      newProject: () => setNewProject(true),
      retry: base.refetch,
    }),
    [at, route.query, centred, nav, base.refetch],
  );

  const searched = sent !== "" && sent === query.trim() ? search.data : null;
  const panel = panelOf(
    { base, searched, query, at, recents, newProject: newProjectAccess },
    actions,
  );

  const value: SwitcherContextValue = {
    open: state.open,
    centred,
    setOpen,
    openSwitcher,
    openNewProject: () => setNewProject(true),
    query,
    setQuery,
    panel,
    figures: figuresOf(base.data),
  };

  return (
    <SwitcherContext.Provider value={value}>
      {children}
      {/* CENTRED, for G then P away from Worklet's scope pages. */}
      <Switcher
        label={LABEL}
        open={state.open && centred}
        onOpenChange={(open) => {
          if (!open) close();
        }}
        query={query}
        onQueryChange={setQuery}
        placeholder="Find a project"
        listLabel="Projects"
        loadingLabel="Loading projects"
        {...panel}
      />
      {newProject && (
        <LayerBoundary title="New project" onClose={() => setNewProject(false)}>
          <NewProjectDialog onClose={() => setNewProject(false)} />
        </LayerBoundary>
      )}
    </SwitcherContext.Provider>
  );
}

/**
 * The trail's last crumb on Worklet's scope pages: the scope's name, as the
 * button that switches it, and the panel under it.
 */
export function ScopeSwitcher({ trigger }: { trigger: Omit<SwitcherTrigger, "title"> }) {
  const ctx = useContext(SwitcherContext);
  // THE PANEL IS THE FRAME'S, and a trail drawn without it would draw a
  // button that opens nothing: loud, as `useViewer` is outside its provider.
  if (ctx === null) {
    throw new Error(
      "ScopeSwitcher outside a ProjectSwitcherProvider: the frame mounts one (app/Shell.tsx), and a suite that draws Worklet's trail without the frame mounts one around it",
    );
  }
  return (
    <Switcher
      label={LABEL}
      open={ctx.open && !ctx.centred}
      onOpenChange={ctx.setOpen}
      trigger={{ ...trigger, title: TRIGGER_TITLE }}
      query={ctx.query}
      onQueryChange={ctx.setQuery}
      placeholder="Find a project"
      listLabel="Projects"
      loadingLabel="Loading projects"
      {...ctx.panel}
    />
  );
}
