/**
 * What an absent property means.
 *
 * Three different facts share one empty cell, and they are not
 * interchangeable: the object has no such property; it has it and it holds
 * nothing; or the emptiness itself means something a reader should be told.
 * A rail that collapsed them would answer "is this task assigned to nobody, or
 * does this kind of task not have an assignee" with the same blank.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import { EMPTY_VALUE } from "@crewlethq/ui";

import { PropertiesRail } from "./PropertiesRail.tsx";

afterEach(cleanup);

/** The value rendered beside a label, or null when the label is not there. */
function valueOf(label: string): string | null {
  const dt = screen.queryByText(label);
  const dd = dt?.nextElementSibling;
  return dd ? dd.textContent : null;
}

describe("an absent property", () => {
  test("renders a dash when the property exists and holds nothing", () => {
    // A task with no due date HAS a due date, unset. Dropping the row would
    // tell the reader this kind of object has no such field.
    render(
      <PropertiesRail
        groups={[{ properties: [{ label: "Due" }, { label: "Start", value: undefined }] }]}
      />,
    );
    // The mark plus the sentence that says WHICH absence it is, which is what
    // the product draws everywhere now — a bare em dash in a `.muted` span said
    // nothing at all to a reader who could not hover it.
    expect(valueOf("Due")).toBe(`${EMPTY_VALUE}Not set`);
    expect(valueOf("Start")).toBe(`${EMPTY_VALUE}Not set`);
  });

  test("is simply not there when the caller leaves the row out", () => {
    // A task nobody is collaborating on has no collaborators, which is not
    // the same as an empty set of them.
    render(<PropertiesRail groups={[{ properties: [{ label: "Assignee", value: "ada" }] }]} />);
    expect(screen.queryByText("Collaborators")).toBeNull();
  });

  test("says what the caller wrote when the emptiness means something", () => {
    render(
      <PropertiesRail
        groups={[
          {
            properties: [{ label: "Auxiliary model", value: "none — reflection uses the default" }],
          },
        ]}
      />,
    );
    expect(valueOf("Auxiliary model")).toBe("none — reflection uses the default");
  });

  test("keeps zero and false, which are values a property can hold", () => {
    // A truthiness test here renders "no hand-offs" and "not archived" as
    // "nothing set", which is a different claim about the object.
    render(
      <PropertiesRail
        groups={[
          {
            properties: [
              { label: "Hand-offs", value: 0 },
              { label: "Archived", value: "no" },
            ],
          },
        ]}
      />,
    );
    expect(valueOf("Hand-offs")).toBe("0");
    expect(valueOf("Archived")).toBe("no");
  });
});

describe("the rail's shape", () => {
  test("puts every group's rows in ONE grid, so the labels share a column", () => {
    // EVERY ROW IS A DIRECT CHILD OF THE `dl`, which is the only shape the
    // rail's own rules are written against: `.props > dt` and `.props > dd`
    // carry the label face and cancel the user agent's 40px indent on a
    // definition, and `.props-section:first-child` drops the separator above
    // the rail's FIRST heading and only that one.
    //
    // This was asserted as "every wrapper is `display: contents`", which is
    // the shape that broke it: contents removes the BOX, never the node, so
    // every one of those rules silently stopped matching while the assertion
    // stayed green. A group that laid out and a group with no element at all
    // are told apart by where the rows are, not by a style property.
    const { container } = render(
      <PropertiesRail
        groups={[
          { name: "State", properties: [{ label: "Status", value: "todo" }] },
          { name: "Plan", properties: [{ label: "Due", value: "Friday" }] },
        ]}
      />,
    );
    const rail = container.querySelector<HTMLElement>("dl.props");
    expect(rail).not.toBeNull();
    const rows = [...rail!.querySelectorAll("dt, dd")];
    expect(rows).toHaveLength(4);
    for (const row of rows) expect(row.parentElement).toBe(rail);
    expect(rail!.querySelectorAll(":scope > div:not(.props-section)")).toHaveLength(0);
    expect(within(rail!).getByText("State")).toBeTruthy();
    expect(within(rail!).getByText("Plan")).toBeTruthy();
  });

  test("only the rail's first heading is its first child", () => {
    // Which is what decides whether a group draws its separator. Under a
    // wrapper per group `:first-child` was true of every heading, so the
    // hairline between "Dates", "People" and "Tracking" was suppressed
    // everywhere and no group in any rail ever drew one.
    const { container } = render(
      <PropertiesRail
        groups={[
          { name: "State", properties: [{ label: "Status", value: "todo" }] },
          { name: "Plan", properties: [{ label: "Due", value: "Friday" }] },
        ]}
      />,
    );
    const headings = [...container.querySelectorAll(".props-section")];
    expect(headings).toHaveLength(2);
    expect(headings[0]!.matches(":first-child")).toBe(true);
    expect(headings[1]!.matches(":first-child")).toBe(false);
  });

  test("an unnamed first group leaves the next heading its separator", () => {
    // The work item's own shape: a run of unlabelled rows, then "Fields",
    // "Cost" and "Record". That first heading HAS rows above it, so it is not
    // the rail's first child and must keep its hairline.
    const { container } = render(
      <PropertiesRail
        groups={[
          { properties: [{ label: "Status", value: "todo" }] },
          { name: "Fields", properties: [{ label: "Due", value: "Friday" }] },
        ]}
      />,
    );
    const heading = container.querySelector(".props-section");
    expect(heading).not.toBeNull();
    expect(heading!.matches(":first-child")).toBe(false);
  });

  test("links a value the caller gave a path, and nothing else", () => {
    const { container } = render(
      <PropertiesRail
        groups={[
          {
            properties: [
              { label: "Turn", value: "t-1", path: ["activity", "turns", "t-1"] },
              { label: "Points", value: 3 },
            ],
          },
        ]}
      />,
    );
    const links = container.querySelectorAll("dd a");
    expect(links.length).toBe(1);
    expect(links[0]!.getAttribute("href")).toContain("/activity/turns/t-1");
  });

  test("renders an identifier label in the mono face, and a word not", () => {
    // An environment variable's name is code; "Reports to" is a word. A rail
    // that set one face for both makes one of them harder to read.
    const { container } = render(
      <PropertiesRail
        groups={[
          {
            properties: [
              { label: "GITHUB_TOKEN", value: "${GH}", code: true },
              { label: "Reports to", value: "ada" },
            ],
          },
        ]}
      />,
    );
    const labels = [...container.querySelectorAll("dt")];
    expect(labels.find((l) => l.textContent === "GITHUB_TOKEN")!.className).toContain("mono");
    expect(labels.find((l) => l.textContent === "Reports to")!.className).not.toContain("mono");
  });
});
