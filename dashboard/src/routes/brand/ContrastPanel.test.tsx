/**
 * Every colour pair an adoption measures, under the scale it is graded on.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { ContrastPanel } from "./ContrastPanel.tsx";

afterEach(cleanup);

// A GRADE IS READ AGAINST ITS RULE. One ratio earned AA on one row and AAA on
// the next with nothing saying the two rows are graded on different scales.
// Mutation: draw every pair in one list.
test("each pair stands under its scale, and the scale names its floors", () => {
  render(<ContrastPanel colors={{ primary: "#0f7b5f" }} />);
  const body = screen.getByRole("list", { name: /Body text/ });
  const marks = screen.getByRole("list", { name: /Large text and marks/ });
  expect(body.getAttribute("aria-labelledby")).toBeTruthy();
  expect(within(body).getByText(/White on primary|Black on primary/)).toBeTruthy();
  expect(within(body).getByText("Text on light ground")).toBeTruthy();
  expect(within(marks).getByText("Primary on light ground")).toBeTruthy();
  expect(within(marks).queryByText("Text on light ground")).toBeNull();
  expect(screen.getByText("AA from 4.5:1, AAA from 7:1")).toBeTruthy();
  expect(screen.getByText("AA from 3:1, AAA from 4.5:1")).toBeTruthy();
});
