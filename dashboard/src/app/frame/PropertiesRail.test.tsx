/**
 * What this file adds to `@crewlethq/ui`'s PropertyList: the translation from
 * the engine's vocabulary (a route, a turn by id, the change log's instant)
 * into what the kit draws. The drawing's own rules (the four absences, one
 * term column down every section, one by-line per change and never one under
 * a dash) are asserted in uilet.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { EMPTY_VALUE } from "@crewlethq/ui";

import { href } from "../router.tsx";
import { PropertiesRail } from "./PropertiesRail.tsx";

afterEach(cleanup);

const setBys = (container: HTMLElement) =>
  [...container.querySelectorAll(".crewlet-property-list__set-by")].map((line) => line.textContent);

const CREATE = { actor: "agent-ceo", at: "2031-04-16T09:00:00Z", turnId: "t-1", ago: "21h ago" };

test("a value's route becomes its link, and a turn by id the by-line's", () => {
  const { container } = render(
    <PropertiesRail
      groups={[
        {
          properties: [
            { label: "Turn", value: "t-1", path: ["live", "turns", "t-1"], setBy: CREATE },
            { label: "Points", value: 3 },
          ],
        },
      ]}
    />,
  );
  expect(screen.getByRole("link", { name: "t-1" }).getAttribute("href")).toBe(
    href(["live", "turns", "t-1"]),
  );
  expect(screen.getByRole("link", { name: "turn" }).getAttribute("href")).toBe(
    href(["live", "turns", "t-1"]),
  );
  expect(container.querySelectorAll("dd a")).toHaveLength(2);
});

// ONE CHANGE IS ONE (actor, kind, instant, turn), and an edit an hour after the
// create is a different commit even where the same seat made both and both
// read alike. Folding them would put a sentence on the screen no single change
// made; this is the half of the key the kit cannot see.
test("one create is one by-line, and two changes that read alike keep one each", () => {
  const { container, rerender } = render(
    <PropertiesRail
      groups={[
        {
          properties: [
            { label: "Status", value: "todo", setBy: CREATE },
            { label: "Type", value: "Task", setBy: CREATE },
          ],
        },
      ]}
    />,
  );
  expect(setBys(container)).toEqual(["Status and Type · set by agent-ceo · 21h ago · turn "]);
  rerender(
    <PropertiesRail
      groups={[
        {
          properties: [
            { label: "Status", value: "todo", setBy: { ...CREATE, at: "2031-04-16T09:20:00Z" } },
            { label: "Type", value: "Task", setBy: CREATE },
          ],
        },
      ]}
    />,
  );
  expect(setBys(container)).toEqual([
    "set by agent-ceo · 21h ago · turn ",
    "set by agent-ceo · 21h ago · turn ",
  ]);
});

test("an absent value is the kit's dash, and a collapsed group the caller's sentence", () => {
  render(
    <PropertiesRail
      groups={[
        { properties: [{ label: "Due" }] },
        { name: "Plan", whenAllAbsent: "Nothing scheduled.", properties: [{ label: "Start" }] },
      ]}
    />,
  );
  expect(screen.getByText("Due").nextElementSibling!.textContent).toBe(`${EMPTY_VALUE}Not set`);
  expect(screen.getByText("Nothing scheduled.")).toBeTruthy();
  expect(screen.queryByText("Start")).toBeNull();
});

test("an identifier label wears the mono face, and the headings the case asked for", () => {
  const { container } = render(
    <PropertiesRail
      headings="sentence"
      groups={[
        {
          name: "Environment",
          properties: [{ label: "GITHUB_TOKEN", value: "${GH}", identifierLabel: true }],
        },
      ]}
    />,
  );
  expect(screen.getByText("GITHUB_TOKEN").className).toBe("crewlet-property-list__identifier");
  expect(container.firstElementChild!.className).toContain("crewlet-property-list--sentence");
});
