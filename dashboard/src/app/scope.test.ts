/**
 * Work's scope: which scope and lens a route is on, and where a switch lands.
 */

import { describe, expect, test } from "vitest";
import {
  ALL_PROJECTS_LENSES,
  PROJECT_LENSES,
  allProjectsPath,
  sameScope,
  scopeAt,
  switchTarget,
  type Scope,
  type ScopeAt,
} from "./scope.ts";
import { WORKSPACES } from "./nav.ts";

const ALL: Scope = { kind: "all" };
const FAIR: Scope = { kind: "project", key: "FAIR" };
const SPLIT: Scope = { kind: "project", key: "SPLIT" };
const q = (query = "") => new URLSearchParams(query);

describe("which scope and lens a route is on", () => {
  test("All projects' lenses are Work's own sections, Projects first", () => {
    expect(scopeAt(["worklet"], q())).toEqual({ scope: ALL, lens: "projects" });
    expect(scopeAt(["worklet", "tasks"], q())).toEqual({ scope: ALL, lens: "tasks" });
    expect(scopeAt(["worklet", "history"], q())).toEqual({ scope: ALL, lens: "history" });
    // AND THEY ARE THE SECTIONS THE BAR DRAWS, in that order.
    const work = WORKSPACES.find((w) => w.key === "worklet")!;
    expect(work.sections.filter((s) => s.tab !== false).map((s) => s.key)).toEqual([
      ...ALL_PROJECTS_LENSES,
    ]);
    for (const lens of ALL_PROJECTS_LENSES) {
      expect(work.sections.find((s) => s.key === lens)?.path).toEqual(allProjectsPath(lens));
    }
  });

  test("a project's lens is its `lens=` key, and an unknown one is its first", () => {
    expect(scopeAt(["worklet", "FAIR"], q())).toEqual({ scope: FAIR, lens: "tasks" });
    expect(scopeAt(["worklet", "FAIR"], q("lens=about"))).toEqual({ scope: FAIR, lens: "about" });
    expect(scopeAt(["worklet", "FAIR"], q("lens=items"))).toEqual({ scope: FAIR, lens: "tasks" });
    expect(PROJECT_LENSES[0]).toBe("tasks");
  });

  test("a task, a saved view, the search and every other workspace are no scope", () => {
    for (const path of [
      ["worklet", "FAIR-12"],
      ["worklet", "views"],
      ["worklet", "search"],
      ["worklet", "projects"],
      ["home"],
      ["company", "org", "roster"],
    ]) {
      expect(scopeAt(path, q()), path.join("/")).toBeNull();
    }
  });
});

describe("where a switch lands", () => {
  const at = (scope: Scope, lens: ScopeAt["lens"]): ScopeAt => ({ scope, lens });

  // THE APPROVED TABLE: on this lens, switching to All projects opens / to a
  // project opens.
  test.each([
    ["projects", ALL, ["worklet"], {}, ["worklet", "SPLIT"], {}],
    ["tasks", ALL, ["worklet", "tasks"], {}, ["worklet", "SPLIT"], {}],
    ["history", ALL, ["worklet", "history"], {}, ["worklet", "SPLIT"], { lens: "history" }],
    ["about", FAIR, ["worklet"], {}, ["worklet", "SPLIT"], { lens: "about" }],
    ["files", FAIR, ["worklet"], {}, ["worklet", "SPLIT"], { lens: "files" }],
  ] as const)("from %s", (lens, from, allPath, allQuery, projectPath, projectQuery) => {
    expect(switchTarget(at(from, lens), ALL, q())).toEqual({ path: allPath, query: allQuery });
    expect(switchTarget(at(from, lens), SPLIT, q())).toEqual({
      path: projectPath,
      query: projectQuery,
    });
  });

  test("only the list's shape travels, and only with Tasks", () => {
    const query = q("shape=board&view=overdue&status=todo&assignee=pm");
    expect(switchTarget(at(ALL, "tasks"), SPLIT, query)).toEqual({
      path: ["worklet", "SPLIT"],
      query: { shape: "board" },
    });
    expect(switchTarget(at(FAIR, "tasks"), ALL, query)).toEqual({
      path: ["worklet", "tasks"],
      query: { shape: "board" },
    });
    expect(switchTarget(at(FAIR, "history"), SPLIT, query).query).toEqual({ lens: "history" });
  });

  test("from off Work's scope pages, a scope opens on its first lens", () => {
    expect(switchTarget(null, ALL, q())).toEqual({ path: ["worklet"], query: {} });
    expect(switchTarget(null, FAIR, q("shape=board"))).toEqual({
      path: ["worklet", "FAIR"],
      query: {},
    });
  });

  test("two scopes are the same one by kind and key", () => {
    expect(sameScope(ALL, { kind: "all" })).toBe(true);
    expect(sameScope(FAIR, { kind: "project", key: "FAIR" })).toBe(true);
    expect(sameScope(FAIR, SPLIT)).toBe(false);
    expect(sameScope(FAIR, ALL)).toBe(false);
  });
});
