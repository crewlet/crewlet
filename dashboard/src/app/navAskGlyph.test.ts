/**
 * Ask Crewlet wears one glyph wherever the navigation names it: its sidebar
 * row and its Settings section, which the palette and the Settings column
 * draw from the same rows.
 */

import { expect, test } from "vitest";
import { ASK_CREWLET_GLYPH, sectionOf, workspaceRow } from "./nav.ts";

test("the Settings section for Ask Crewlet wears the sidebar row's glyph", () => {
  const sidebar = workspaceRow("ask");
  const settings = sectionOf(["settings", "ask-crewlet"]);
  expect(settings?.label).toBe("Ask Crewlet");
  expect(sidebar?.icon).toBe(ASK_CREWLET_GLYPH);
  expect(settings?.icon).toBe(sidebar?.icon);
});
