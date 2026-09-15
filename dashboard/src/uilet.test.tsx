/**
 * The design system renders under the test runner at all.
 *
 * This is a probe, not a test of uilet: it fails the moment the inline rule in
 * `vitest.config.ts` is dropped, because an externalised `@crewlethq/ui`
 * throws `Unknown file extension ".css"` on its first side-effect import and
 * takes the rest of the suite with it. Without the probe the rule is a comment
 * nobody can check, and the failure it prevents arrives in whichever screen
 * suite happens to render a uilet component first.
 */

import { render, screen } from "@testing-library/react";
import { expect, test } from "vitest";
import { Button } from "@crewlethq/ui";

test("a uilet component renders, so the packages are inlined rather than externalised", () => {
  render(<Button>Save</Button>);
  expect(screen.getByRole("button", { name: "Save" })).toBeDefined();
});
