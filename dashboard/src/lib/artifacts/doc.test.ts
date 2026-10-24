/**
 * A doc as the dashboard reads and edits it: the save it sends, the pictures
 * it loads, and the presence region that names a tab.
 */

import { describe, expect, test } from "vitest";
import type { ArtifactDoc } from "~/protocol/index.ts";
import { resolveVersionPath } from "./versionImages.ts";
import { currentTab, docImagePaths, editOf, tabImages, tabOfRegion, tabRegion } from "./doc.ts";

/** What the engine does with an edit: replace its one occurrence. */
function apply(base: string, e: { old_str: string; new_str: string }): string {
  const at = base.indexOf(e.old_str);
  expect(at).toBeGreaterThanOrEqual(0);
  expect(base.indexOf(e.old_str, at + 1)).toBe(-1);
  return base.slice(0, at) + e.new_str + base.slice(at + e.old_str.length);
}

describe("editOf", () => {
  const base = "# Runbook\n\nPage the lead within ten minutes.\n\nThen wait.\n";

  test("sends the changed passage, not the whole tab, and it applies", () => {
    const draft = base.replace("ten", "five");
    const e = editOf("tabs/oncall.md", base, draft)!;
    expect(e.path).toBe("tabs/oncall.md");
    expect(e.old_str.length).toBeLessThan(base.length);
    expect(apply(base, e)).toBe(draft);
  });

  test("widens a span that is not unique until it is", () => {
    const repeated = "- item\n- item\n- item\n";
    const draft = "- item\n- item two\n- item\n";
    const e = editOf("a.md", repeated, draft)!;
    expect(apply(repeated, e)).toBe(draft);
  });

  test("an insertion and a deletion each apply", () => {
    const inserted = base.replace("Then wait.", "Then wait.\n\nThen call.");
    expect(apply(base, editOf("a.md", base, inserted)!)).toBe(inserted);
    const deleted = base.replace("\n\nThen wait.", "");
    expect(apply(base, editOf("a.md", base, deleted)!)).toBe(deleted);
  });

  test("never splits a character", () => {
    const b = "Owner: 👩‍💻 Maya\n";
    const d = "Owner: 👩‍🔬 Maya\n";
    const e = editOf("a.md", b, d)!;
    expect(apply(b, e)).toBe(d);
    // A LONE SURROGATE IS NOT TEXT: encoding one throws.
    expect(() => encodeURIComponent(e.old_str)).not.toThrow();
    expect(() => encodeURIComponent(e.new_str)).not.toThrow();
  });

  test("answers nothing when nothing changed or the tab is empty", () => {
    expect(editOf("a.md", base, base)).toBeNull();
    expect(editOf("a.md", "", "# New")).toBeNull();
  });
});

describe("pictures", () => {
  const doc: ArtifactDoc = {
    indexed: true,
    tabs: [
      {
        path: "tabs/start.md",
        title: "Start",
        text: "![Team](../images/team.png)\n\n`![x](no.png)`",
      },
      {
        path: "tabs/more.md",
        title: "More",
        text: "![Again](../images/team.png) ![Far](https://example.com/a.png) ![Up](../../a.png)",
      },
    ],
  };

  test("are the version files each tab shows, once each, and never an address", () => {
    expect(docImagePaths(doc)).toEqual(["images/team.png"]);
  });

  test("are shown from what was loaded, by the path relative to the tab", () => {
    const show = tabImages(doc.tabs[0]!, (p) => (p === "images/team.png" ? "blob:team" : null));
    expect(show("../images/team.png")).toBe("blob:team");
    expect(show("https://example.com/a.png")).toBeNull();
  });

  test("resolve relative to the file and stay inside the version", () => {
    expect(resolveVersionPath("tabs/a.md", "./img/x.png")).toBe("tabs/img/x.png");
    expect(resolveVersionPath("a.md", "../x.png")).toBeNull();
    expect(resolveVersionPath("a.md", "/x.png")).toBeNull();
    expect(resolveVersionPath("a.md", "data:image/png;base64,AA")).toBeNull();
    expect(resolveVersionPath("a.md", "x.png?v=1")).toBeNull();
  });
});

test("a tab is named to the room by its path, and an address names one tab", () => {
  expect(tabOfRegion(tabRegion("tabs/plan.md"))).toBe("tabs/plan.md");
  expect(tabOfRegion("slide:cover")).toBe("");
  const doc: ArtifactDoc = {
    indexed: true,
    tabs: [
      { path: "a.md", title: "A", text: "" },
      { path: "b.md", title: "B", text: "" },
    ],
  };
  expect(currentTab(doc, "b.md").title).toBe("B");
  expect(currentTab(doc, "gone.md").title).toBe("A");
});
