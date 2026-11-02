/**
 * Worklet's SCOPE: All projects, or one project. The trail names it and switches
 * it (`header/ProjectSwitcher.tsx`), and each scope carries its lenses beside
 * that name.
 *
 * ONE MODULE for which scope and lens a route is on and where a switch lands,
 * because the trail, the switcher, the browser tab's title and the palette all
 * ask, and four answers to "which lens is this" is how a switch opens the
 * wrong one.
 *
 * # Two kinds of lens, drawn alike
 *
 * All projects' lenses are Worklet's own SECTIONS (`nav.ts`), so each is a
 * path: `#/worklet`, `#/worklet/tasks`, `#/worklet/history`. A project's are
 * keys on its address (`#/worklet/{KEY}?lens=about`), its first lens taking no
 * key at all.
 * The header draws both as the same chips, so the reader meets one bar.
 */

import { resolve } from "./routes.ts";

/** All projects' lenses, in the order the bar draws them; the first is Worklet's landing. */
export const ALL_PROJECTS_LENSES = ["projects", "tasks", "history"] as const;

/** A project's lenses, in the order its bar draws them; the first is its landing. */
export const PROJECT_LENSES = ["tasks", "about", "history", "files"] as const;

export type AllProjectsLens = (typeof ALL_PROJECTS_LENSES)[number];
export type ProjectLens = (typeof PROJECT_LENSES)[number];
export type Lens = AllProjectsLens | ProjectLens;

/** What each lens is called, in either scope: a lens of one name means one thing. */
export const LENS_LABELS: Record<Lens, string> = {
  projects: "Projects",
  tasks: "Tasks",
  history: "History",
  about: "About",
  files: "Files",
};

/** What the trail calls the scope that is every project. */
export const ALL_PROJECTS = "All projects";

export type Scope = { kind: "all" } | { kind: "project"; key: string };

/** A scope and the lens the reader has open on it. */
export interface ScopeAt {
  scope: Scope;
  lens: Lens;
}

/**
 * The scope and lens a route is on, or null for a page that is neither (a
 * task, a saved view, the search, any other workspace).
 *
 * A PROJECT'S UNKNOWN `lens=` IS ITS FIRST LENS, the rule `frame/tabs.ts`
 * draws by, so this never names a lens the page is not showing.
 */
export function scopeAt(path: string[], query: URLSearchParams): ScopeAt | null {
  const where = resolve(path);
  if (!where.resolved) return null;
  switch (where.screen) {
    case "worklet-projects":
      return { scope: { kind: "all" }, lens: "projects" };
    case "worklet-tasks":
      return { scope: { kind: "all" }, lens: "tasks" };
    case "worklet-history":
      return { scope: { kind: "all" }, lens: "history" };
    case "project": {
      const asked = query.get("lens") ?? "";
      const lens = (PROJECT_LENSES as readonly string[]).includes(asked)
        ? (asked as ProjectLens)
        : PROJECT_LENSES[0];
      return { scope: { kind: "project", key: where.key }, lens };
    }
    default:
      return null;
  }
}

/** The path of one of All projects' lenses. */
export function allProjectsPath(lens: AllProjectsLens): string[] {
  return lens === "projects" ? ["worklet"] : ["worklet", lens];
}

/**
 * Where switching to `to` lands, from where the reader is.
 *
 * TASKS AND HISTORY CARRY OVER, because each is the same question about any
 * scope. ABOUT AND FILES CARRY BETWEEN TWO PROJECTS, and every other lens
 * opens the next scope's first lens. A switch from off Worklet's scope pages (the
 * switcher opened centred) opens the scope's first lens too.
 *
 * ONLY THE LIST'S SHAPE TRAVELS, and only with Tasks: a saved view and the
 * filters belong to the scope they were set in, because a project's
 * statuses, labels and fields are its own.
 */
export function switchTarget(
  from: ScopeAt | null,
  to: Scope,
  query: URLSearchParams,
): { path: string[]; query: Record<string, string> } {
  const lens = nextLens(from, to);
  const shape = query.get("shape");
  const keep: Record<string, string> =
    lens === "tasks" && from?.lens === "tasks" && shape ? { shape } : {};
  if (to.kind === "all") {
    return { path: allProjectsPath(lens as AllProjectsLens), query: keep };
  }
  return { path: ["worklet", to.key], query: lens === PROJECT_LENSES[0] ? keep : { lens } };
}

function nextLens(from: ScopeAt | null, to: Scope): Lens {
  const first = to.kind === "all" ? ALL_PROJECTS_LENSES[0] : PROJECT_LENSES[0];
  if (!from) return first;
  if (from.lens === "tasks" || from.lens === "history") return from.lens;
  if (from.scope.kind === "project" && to.kind === "project") return from.lens;
  return first;
}

/** Whether two scopes are the same one. */
export function sameScope(a: Scope, b: Scope): boolean {
  return a.kind === "all" ? b.kind === "all" : b.kind === "project" && a.key === b.key;
}
