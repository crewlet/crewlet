/**
 * A person's edit of a doc's tabs: the tab operations, the publish a save
 * sends, and the merge a raced save is moved onto the newer version by.
 */

import { describe, expect, test } from "vitest";
import type { ArtifactDoc } from "~/protocol/index.ts";
import {
  addTab,
  addTabProblem,
  docChangesOf,
  docDraftOf,
  docPublishPlan,
  indexSource,
  mergeDoc,
  newTabPath,
  removeTab,
  renameTab,
  reorderTabs,
  resolveDoc,
  setTabText,
  tabTitleProblem,
  withFreePaths,
  type DocDraft,
} from "./docDraft.ts";

const tabbed: ArtifactDoc = {
  indexed: true,
  tabs: [
    { path: "tabs/plan.md", title: "Plan", text: "# Plan\n\nShip in May.\n" },
    { path: "tabs/risks.md", title: "Risks", text: "# Risks\n\nVendor lock-in.\n" },
    { path: "tabs/team.md", title: "Team", text: "# Team\n\nMaya, Sam.\n" },
  ],
};

const base = docDraftOf(tabbed, 4);
const raw = indexSource(base);

/** What the engine does with an edit: replace its one occurrence. */
function apply(text: string, e: { old_str: string; new_str: string }): string {
  const at = text.indexOf(e.old_str);
  expect(at).toBeGreaterThanOrEqual(0);
  expect(text.indexOf(e.old_str, at + 1)).toBe(-1);
  return text.slice(0, at) + e.new_str + text.slice(at + e.old_str.length);
}

describe("the tab operations", () => {
  test("add a tab after another, on a path of its own, headed by its title", () => {
    const { draft, path } = addTab(base, "Plan", ["images/plan.png"], "tabs/plan.md");
    expect(path).toBe("tabs/plan-2.md");
    expect(draft.order).toEqual([
      "tabs/plan.md",
      "tabs/plan-2.md",
      "tabs/risks.md",
      "tabs/team.md",
    ]);
    expect(draft.tabs[path]!.text).toBe("# Plan\n");
    expect(newTabPath("Q3 / Launch!", [])).toBe("tabs/q3-launch.md");
    expect(newTabPath("???", ["tabs/tab.md"])).toBe("tabs/tab-2.md");
  });

  test("rename, reorder and remove, and never remove the last tab", () => {
    let d = renameTab(base, "tabs/risks.md", "Open risks");
    d = reorderTabs(d, ["tabs/team.md", "tabs/plan.md", "tabs/risks.md"]);
    expect(reorderTabs(d, ["tabs/team.md"])).toBe(d);
    d = removeTab(d, "tabs/plan.md");
    expect(d.order).toEqual(["tabs/team.md", "tabs/risks.md"]);
    expect(d.tabs["tabs/risks.md"]!.title).toBe("Open risks");
    const one = removeTab(d, "tabs/team.md");
    expect(removeTab(one, "tabs/risks.md").order).toEqual(["tabs/risks.md"]);
  });

  test("hold a title and the tab count to the engine's rules", () => {
    expect(tabTitleProblem(" ")).toBe("Give the tab a title.");
    expect(tabTitleProblem("x".repeat(121))).toContain("121 bytes");
    expect(tabTitleProblem("A\u0007")).toContain("control");
    expect(tabTitleProblem("Plan")).toBeNull();
    let d: DocDraft = base;
    for (let i = d.order.length; i < 32; i++) d = addTab(d, `T${i}`, []).draft;
    expect(addTabProblem(d)).toContain("at most 32");
  });
});

describe("the publish", () => {
  test("moves a new tab off a file the version already holds, and leaves the base's tabs alone", () => {
    const { draft, path } = addTab(base, "Appendix", []);
    expect(path).toBe("tabs/appendix.md");
    const free = withFreePaths(base, draft, ["tabs/appendix.md", "tabs/plan.md", "doc.json"]);
    expect(free.order).toContain("tabs/appendix-2.md");
    expect(free.order).not.toContain("tabs/appendix.md");
    expect(free.tabs["tabs/appendix-2.md"]!.title).toBe("Appendix");
    expect(free.order.slice(0, 3)).toEqual(base.order);
  });

  test("sends a changed tab as its edit, and the index only when the tabs changed", () => {
    const d = setTabText(base, "tabs/risks.md", "# Risks\n\nVendor lock-in, and hiring.\n");
    const plan = docPublishPlan(base, d, raw);
    expect(plan.uploads).toEqual([]);
    expect(plan.removes).toEqual([]);
    expect(plan.edits).toHaveLength(1);
    expect(plan.edits[0]!.path).toBe("tabs/risks.md");
    expect(apply(base.tabs["tabs/risks.md"]!.text, plan.edits[0]!)).toBe(
      d.tabs["tabs/risks.md"]!.text,
    );
  });

  test("adds, renames, reorders and removes as one version: uploads, an index edit and a removal", () => {
    let d = addTab(base, "Budget", []).draft;
    d = renameTab(d, "tabs/plan.md", "The plan");
    d = reorderTabs(d, ["tabs/team.md", "tabs/plan.md", "tabs/budget.md", "tabs/risks.md"]);
    d = removeTab(d, "tabs/risks.md");
    const plan = docPublishPlan(base, d, raw);
    expect(plan.removes).toEqual(["tabs/risks.md"]);
    expect(plan.uploads).toEqual([{ path: "tabs/budget.md", text: "# Budget\n" }]);
    expect(plan.entry).toBeUndefined();
    const index = plan.edits.find((e) => e.path === "doc.json")!;
    expect(JSON.parse(apply(raw, index))).toEqual({
      v: 1,
      tabs: [
        { path: "tabs/team.md", title: "Team" },
        { path: "tabs/plan.md", title: "The plan" },
        { path: "tabs/budget.md", title: "Budget" },
      ],
    });
  });

  test("gives a doc of one file its index, and opens it there, when it gains a tab", () => {
    const one = docDraftOf(
      { indexed: false, tabs: [{ path: "index.md", title: "index", text: "# Runbook\n" }] },
      1,
    );
    const d = renameTab(addTab(one, "On call", []).draft, "index.md", "Runbook");
    const plan = docPublishPlan(one, d, null);
    expect(plan.entry).toBe("doc.json");
    const index = plan.uploads.find((u) => u.path === "doc.json")!;
    expect(JSON.parse(index.text).tabs).toEqual([
      { path: "index.md", title: "Runbook" },
      { path: "tabs/on-call.md", title: "On call" },
    ]);
  });

  test("sends an empty tab's new text whole, since no edit can address an empty file", () => {
    const empty = docDraftOf({ indexed: true, tabs: [{ path: "a.md", title: "A", text: "" }] }, 1);
    const plan = docPublishPlan(empty, setTabText(empty, "a.md", "# A\n"), indexSource(empty));
    expect(plan.edits).toEqual([]);
    expect(plan.uploads).toEqual([{ path: "a.md", text: "# A\n" }]);
  });

  test("says nothing changed for an untouched draft", () => {
    const c = docChangesOf(base, base);
    expect(c).toEqual({ texts: [], added: [], removed: [], index: false });
  });
});

describe("the merge", () => {
  const live = docDraftOf(
    {
      indexed: true,
      tabs: [
        { path: "tabs/plan.md", title: "Plan", text: "# Plan\n\nShip in June.\n" },
        { path: "tabs/risks.md", title: "Risks", text: "# Risks\n\nVendor lock-in.\n" },
        { path: "tabs/team.md", title: "People", text: "# Team\n\nMaya, Sam.\n" },
        { path: "tabs/notes.md", title: "Notes", text: "# Notes\n" },
      ],
    },
    5,
  );

  // A TAB ADDED is put back after the tab it followed in its own side's order.
  test("keeps both sides' changes to different tabs and different lines, on the newer base", () => {
    let mine = setTabText(base, "tabs/risks.md", "# Risks\n\nVendor lock-in.\n\nHiring.\n");
    mine = addTab(mine, "Budget", []).draft;
    const m = mergeDoc(base, mine, live);
    expect(m.conflicts).toEqual([]);
    expect(m.draft.base).toBe(5);
    expect(m.draft.tabs["tabs/plan.md"]!.text).toContain("June");
    expect(m.draft.tabs["tabs/risks.md"]!.text).toContain("Hiring.");
    expect(m.draft.tabs["tabs/team.md"]!.title).toBe("People");
    expect(m.draft.order).toEqual([
      "tabs/plan.md",
      "tabs/risks.md",
      "tabs/team.md",
      "tabs/budget.md",
      "tabs/notes.md",
    ]);
    expect(m.theirs).toEqual(["Plan", "People", "Notes"]);
  });

  test("asks the person where both changed the same lines, and settles it place by place", () => {
    const mine = setTabText(base, "tabs/plan.md", "# Plan\n\nShip in April.\n");
    const m = mergeDoc(base, mine, live);
    expect(m.conflicts.map((c) => c.kind)).toEqual(["text"]);
    const settled = resolveDoc(m, m.conflicts[0]!, ["theirs"]);
    expect(settled.conflicts).toEqual([]);
    expect(settled.draft.tabs["tabs/plan.md"]!.text).toContain("June");
  });

  test("a tab one side removed and the other changed is the person's to keep or drop", () => {
    const gone = removeTab(live, "tabs/risks.md");
    const mine = setTabText(base, "tabs/risks.md", "# Risks\n\nHiring.\n");
    const m = mergeDoc(base, mine, gone);
    expect(m.conflicts).toEqual([{ kind: "tab", path: "tabs/risks.md", title: "Risks" }]);
    expect(m.draft.order).toContain("tabs/risks.md");
    expect(resolveDoc(m, m.conflicts[0]!, "theirs").draft.order).not.toContain("tabs/risks.md");

    const dropped = removeTab(base, "tabs/plan.md");
    const back = mergeDoc(base, dropped, live);
    expect(back.conflicts).toEqual([{ kind: "tab", path: "tabs/plan.md", title: "Plan" }]);
    expect(back.draft.order).not.toContain("tabs/plan.md");
    const theirs = resolveDoc(back, back.conflicts[0]!, "theirs");
    expect(theirs.draft.order[0]).toBe("tabs/plan.md");
    expect(theirs.draft.tabs["tabs/plan.md"]!.text).toContain("June");
  });

  test("two different renames and two different moves are each the person's to settle", () => {
    let mine = renameTab(base, "tabs/team.md", "Crew");
    mine = reorderTabs(mine, ["tabs/team.md", "tabs/plan.md", "tabs/risks.md"]);
    const moved = reorderTabs(live, [
      "tabs/risks.md",
      "tabs/plan.md",
      "tabs/team.md",
      "tabs/notes.md",
    ]);
    const m = mergeDoc(base, mine, moved);
    expect(m.conflicts.map((c) => c.kind).sort()).toEqual(["order", "title"]);
    const title = m.conflicts.find((c) => c.kind === "title")!;
    expect(resolveDoc(m, title, "theirs").draft.tabs["tabs/team.md"]!.title).toBe("People");
    expect(resolveDoc(m, title, "mine").draft.tabs["tabs/team.md"]!.title).toBe("Crew");
  });

  test("a tab both sides added at one path keeps both, mine on a path of its own", () => {
    const mine = addTab(base, "Notes", []).draft;
    const m = mergeDoc(base, mine, live);
    expect(m.conflicts).toEqual([]);
    expect(m.draft.order).toContain("tabs/notes.md");
    expect(m.draft.order).toContain("tabs/notes-2.md");
    expect(m.draft.tabs["tabs/notes-2.md"]!.title).toBe("Notes");
  });
});
