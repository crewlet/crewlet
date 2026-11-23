/**
 * The builder's confirmation dialogs with long names, laid out: nothing runs
 * past the dialog sideways, and every name cut with an ellipsis says itself on hover.
 */
import "~/test/layout.tsx";
import { cleanup, fireEvent } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { page } from "vitest/browser";
import { overflowing } from "~/test/layout.tsx";
import { scenes } from "./dialogScenes.tsx";
import { renderInBuilder } from "./viewTestkit.tsx";

afterEach(() => {
  cleanup();
  document.documentElement.removeAttribute("data-theme");
});

/** The kit's one-line cuts inside the change and consequence lists. */
const CUTS = [
  ".crewlet-change-list__subject",
  ".crewlet-change-list__from",
  ".crewlet-change-list__to",
  ".crewlet-change-list__removed",
  ".crewlet-change-list__fact",
  ".crewlet-resource-name",
].join(", ");

for (const width of [1440, 390]) {
  for (const theme of ["light", "dark"]) {
    test(`at ${width} in ${theme}, each dialog fits and says a cut name on hover`, async () => {
      await page.viewport(width, 900);
      document.documentElement.setAttribute("data-theme", theme);
      let cuts = 0;
      for (const scene of scenes()) {
        renderInBuilder(scene.state, scene.ui);
        scene.prepare?.();
        await document.fonts.ready;
        const surface = document.querySelector(".crewlet-modal, .crewlet-drawer, [role=dialog]")!;
        expect(surface, scene.name).not.toBeNull();
        expect(overflowing(surface), scene.name).toEqual([]);
        for (const cut of surface.querySelectorAll<HTMLElement>(CUTS)) {
          if (cut.scrollWidth <= cut.clientWidth) continue;
          cuts += 1;
          fireEvent.mouseEnter(cut);
          expect(cut.getAttribute("title"), `${scene.name}: ${cut.textContent}`).toBe(
            (cut.textContent ?? "").replace(/\s+/g, " ").trim(),
          );
        }
        cleanup();
      }
      // The long seat, unit and secret names do not fit a phone's dialog.
      if (width === 390) expect(cuts).toBeGreaterThan(0);
    });
  }
}
