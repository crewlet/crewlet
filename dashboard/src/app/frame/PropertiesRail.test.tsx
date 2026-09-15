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
    expect(valueOf("Due")).toBe("—");
    expect(valueOf("Start")).toBe("—");
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
    // A wrapper that laid out would restart the column per group and the rail
    // would read as several rails stacked.
    const { container } = render(
      <PropertiesRail
        groups={[
          { name: "State", properties: [{ label: "Status", value: "todo" }] },
          { name: "Plan", properties: [{ label: "Due", value: "Friday" }] },
        ]}
      />,
    );
    const rail = container.querySelector("dl.props");
    expect(rail).not.toBeNull();
    for (const wrapper of rail!.querySelectorAll<HTMLElement>(":scope > div:not(.props-section)")) {
      expect(wrapper.style.display).toBe("contents");
    }
    expect(within(rail as HTMLElement).getByText("State")).toBeTruthy();
    expect(within(rail as HTMLElement).getByText("Plan")).toBeTruthy();
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
