// WHAT CONNECTING GIVES, LAID OUT in an integration's pane at 1440 and 390px in
// light and dark: the four sections flow through columns no narrower than a
// readable line, so none waits under a taller one, and one column on a phone.
import { afterEach, describe, expect, test } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { overflowing } from "~/test/layout.tsx";
import { STORIES, type Tool } from "./model.ts";
import { BeforeYouStart } from "./parts.tsx";

/** Google Cloud's own story and what its form asks for, as the page shows them before a connect. */
const GOOGLE_CLOUD = {
  story: STORIES.google_cloud!,
  haveReady: [{ label: "Provisioner service account" }, { label: "Workload identity provider" }],
} as unknown as Tool;

let host: HTMLElement | null = null;
let root: Root | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
});

/** The pane's width beside the directory at each page width. */
const PANE = { 1440: 840, 390: 358 } as const;

async function mount(width: 1440 | 390, theme: "light" | "dark") {
  await page.viewport(width, 900);
  document.documentElement.setAttribute("data-theme", theme);
  host = document.createElement("div");
  host.style.cssText = `width: ${PANE[width]}px;`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() => root!.render(<BeforeYouStart tool={GOOGLE_CLOUD} />));
  await document.fonts.ready;
}

describe.each(["light", "dark"] as const)("on a %s document", (theme) => {
  // Mutation: lay the sections out as 14rem grid columns again and each one is
  // under 20rem wide, and "You do by hand" starts below the list's foot.
  test("at 1440 the sections share two columns of readable lines, none waiting under another", async () => {
    await mount(1440, theme);
    const sections = [...host!.querySelectorAll<HTMLElement>("section")].map((s) =>
      s.getBoundingClientRect(),
    );
    expect(sections).toHaveLength(4);
    for (const box of sections) expect(box.width).toBeGreaterThanOrEqual(320);
    const columns = new Set(sections.map((box) => Math.round(box.left)));
    expect(columns.size).toBe(2);
    // The second column starts level with the first.
    const tops = [...columns].map((left) =>
      Math.min(...sections.filter((b) => Math.round(b.left) === left).map((b) => b.top)),
    );
    expect(Math.abs(tops[0]! - tops[1]!)).toBeLessThan(1);
    expect(overflowing(host!)).toEqual([]);
  });

  test("on a phone the sections stand in one column, inside the width", async () => {
    await mount(390, theme);
    const lefts = new Set(
      [...host!.querySelectorAll<HTMLElement>("section")].map((s) =>
        Math.round(s.getBoundingClientRect().left),
      ),
    );
    expect(lefts.size).toBe(1);
    expect(overflowing(host!)).toEqual([]);
  });
});
