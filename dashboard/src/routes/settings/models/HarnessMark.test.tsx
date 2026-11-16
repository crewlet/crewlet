/**
 * Every built-in harness draws the kit's mark for its vendor, or is listed as having none the
 * product may use, so a harness the engine adds or a mark the kit adds cannot fall to the glyph.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { VENDORS } from "@crewlethq/icons";
import { HARNESS_PRODUCTS } from "~/contract/models.ts";
import { GLYPH_HARNESSES, HARNESS_MARKS, HarnessMark } from "./HarnessMark.tsx";

afterEach(cleanup);

const KIT: readonly string[] = VENDORS;

test("every built-in harness has a kit mark or is a glyph harness, and only built-ins do", () => {
  const decided = [...Object.keys(HARNESS_MARKS), ...GLYPH_HARNESSES].sort();
  expect(decided).toEqual(Object.keys(HARNESS_PRODUCTS).sort());
  for (const vendor of Object.values(HARNESS_MARKS)) expect(KIT).toContain(vendor);
});

// The kit names a harness's mark after the harness or its first word (gemini-cli, qwen).
test.each(GLYPH_HARNESSES)("the kit ships no mark named after %s", (harness) => {
  expect(KIT).not.toContain(harness);
  expect(KIT).not.toContain(harness.split("-")[0]);
});

test.each(Object.entries(HARNESS_MARKS))("%s draws the kit's %s mark", (harness, vendor) => {
  const { container } = render(<HarnessMark harness={harness} />);
  expect(container.querySelector(`svg.crewlet-vendor-mark--${vendor}`)).not.toBeNull();
});

test.each([...GLYPH_HARNESSES, "custom"])("%s draws the terminal glyph", (harness) => {
  const { container } = render(<HarnessMark harness={harness} />);
  expect(container.querySelector("svg")).not.toBeNull();
  expect(container.querySelector(".crewlet-vendor-mark")).toBeNull();
});
