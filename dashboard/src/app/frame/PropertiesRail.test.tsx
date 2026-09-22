/**
 * What an absent property means, and who is said to have set what.
 *
 * Four different facts share one empty cell, and they are not
 * interchangeable: the object has no such property; it has it and it holds
 * nothing; the emptiness itself means something a reader should be told; or
 * the whole GROUP is empty and means one thing rather than four. A rail that
 * collapsed them would answer "is this task assigned to nobody, or does this
 * kind of task not have an assignee" with the same blank.
 *
 * The other half of this file is the attribution, which has two rules of its
 * own that a screenshot of one create put on trial: one line per CHANGE rather
 * than per row, and never a line under a value that is not there.
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

  test("collapses a group the caller said one thing about, and only that group", () => {
    // THE FOURTH CASE. A heading, a hairline and four dashes cost a reader
    // four lines to learn one fact; the caller's own sentence says it in one.
    // The group beside it has a value, so it keeps every row it has — dash
    // included, because THAT row's absence is still a per-row fact.
    const { container } = render(
      <PropertiesRail
        groups={[
          {
            name: "Plan",
            whenAllAbsent: "Nothing scheduled: no dates, no estimate, no size.",
            properties: [{ label: "Start" }, { label: "Due" }, { label: "Estimate", value: "" }],
          },
          {
            name: "State",
            whenAllAbsent: "Nothing is known about this.",
            properties: [{ label: "Status", value: "todo" }, { label: "Type" }],
          },
        ]}
      />,
    );
    expect(screen.getByText("Nothing scheduled: no dates, no estimate, no size.")).toBeTruthy();
    for (const gone of ["Start", "Due", "Estimate"]) expect(screen.queryByText(gone)).toBeNull();
    // THE HEADING STAYS: the group exists and is empty, which is a different
    // claim from an object that has no plan at all.
    expect(screen.getByText("Plan")).toBeTruthy();
    // And the group that HAS something is untouched, sentence and all.
    expect(screen.queryByText("Nothing is known about this.")).toBeNull();
    expect(valueOf("Status")).toBe("todo");
    expect(valueOf("Type")).toBe(`${EMPTY_VALUE}Not set`);
    expect(container.querySelectorAll(".props-empty")).toHaveLength(1);
  });

  test("keeps every dash when the caller named no sentence for the group", () => {
    // THE PINNED CASE ABOVE, restated as the rule it protects: the collapse
    // is the CALLER's decision like the other three, so a group that did not
    // ask for it keeps its rows however many of them are empty. A rail-wide
    // behaviour would silently fold somebody else's annotation list into one
    // sentence.
    const { container } = render(
      <PropertiesRail
        groups={[{ name: "Plan", properties: [{ label: "Due" }, { label: "Start" }] }]}
      />,
    );
    expect(valueOf("Due")).toBe(`${EMPTY_VALUE}Not set`);
    expect(valueOf("Start")).toBe(`${EMPTY_VALUE}Not set`);
    expect(container.querySelectorAll(".props-empty")).toHaveLength(0);
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

describe("who set it", () => {
  const CREATE = { actor: "agent-ceo", at: "2031-04-16T09:00:00Z", turnId: "t-1", ago: "21h ago" };

  test("says one create once, naming every property it set", () => {
    // ONE LINE PER CHANGE, NOT PER ROW. A create sets status, priority and
    // type in one commit, so three consecutive rows carried three identical
    // copies of "agent-ceo · 21h ago · turn ↗" — the same sentence written out
    // three times, which on a 420px rail turned three rows into six lines. The
    // line names what it covers and sits under the LAST row of the run, where
    // it reads as a footnote to the values above it.
    const { container } = render(
      <PropertiesRail
        groups={[
          {
            name: "State",
            properties: [
              { label: "Status", value: "todo", setBy: CREATE },
              { label: "Priority", value: "none", setBy: CREATE },
              { label: "Type", value: "Task", setBy: CREATE },
            ],
          },
        ]}
      />,
    );
    const lines = [...container.querySelectorAll(".props-setby")];
    expect(lines).toHaveLength(1);
    expect(lines[0]!.textContent).toBe(
      "Status, Priority and Type · set by agent-ceo · 21h ago · turn ",
    );
    // UNDER THE LAST OF THEM: the `dd` holding it is Type's.
    expect(lines[0]!.closest("dd")!.previousElementSibling!.textContent).toBe("Type");
  });

  test("keeps two changes apart, however alike they look", () => {
    // A RUN IS ONE (actor, instant, turn), and an edit an hour after the
    // create is a different commit even when the same seat made both. Folding
    // them would put a sentence on the screen that no single change made.
    const { container } = render(
      <PropertiesRail
        groups={[
          {
            name: "State",
            properties: [
              {
                label: "Status",
                value: "in_progress",
                setBy: { ...CREATE, at: "2031-04-16T10:00:00Z", ago: "20h ago" },
              },
              { label: "Type", value: "Task", setBy: CREATE },
            ],
          },
        ]}
      />,
    );
    const lines = [...container.querySelectorAll(".props-setby")].map((n) => n.textContent);
    expect(lines).toHaveLength(2);
    expect(lines[0]).toContain("20h ago");
    expect(lines[1]).toContain("21h ago");
    // AND NEITHER NAMES A FIELD: a line covering its own row is already
    // labelled by the row it sits under.
    for (const line of lines) expect(line).not.toContain("Status");
  });

  test("draws no attribution under a value that is not there", () => {
    // The screenshot this rule came from: a dashed Priority with "agent-ceo ·
    // 21h ago · turn ↗" under it, which claims the engine recorded somebody
    // setting nothing. A row with no value has nothing for a by-line to be
    // about, and the run it would have joined ends at it rather than reaching
    // over it — a line reading "Status and Type" with a Priority between them
    // is a claim the reader has to check.
    const { container } = render(
      <PropertiesRail
        groups={[
          {
            properties: [
              { label: "Status", value: "todo", setBy: CREATE },
              { label: "Priority", setBy: CREATE },
              { label: "Type", value: "Task", setBy: CREATE },
            ],
          },
        ]}
      />,
    );
    const lines = [...container.querySelectorAll(".props-setby")].map((n) => n.textContent);
    expect(lines).toHaveLength(2);
    expect(lines[0]).toBe("set by agent-ceo · 21h ago · turn ");
    expect(lines[1]).toBe("set by agent-ceo · 21h ago · turn ");
    // Priority's own cell carries the mark and nothing else.
    expect(valueOf("Priority")).toBe(`${EMPTY_VALUE}Not set`);
  });

  test("says who cleared a value, which is the one blank with a record", () => {
    // "Nobody has set a due date" and "ada took the due date off" are
    // different facts about the same empty cell, and the second is a change
    // the log actually witnessed. It reads "cleared by", because "set by" over
    // a blank is the claim the rule above exists to refuse.
    const { container } = render(
      <PropertiesRail
        groups={[
          { properties: [{ label: "Due", setBy: { actor: "ada", ago: "2h ago", cleared: true } }] },
        ]}
      />,
    );
    expect(container.querySelector(".props-setby")!.textContent).toBe("cleared by ada · 2h ago");
    expect(valueOf("Due")).toContain(EMPTY_VALUE);
  });

  test("a collapsed group takes its attributions with it", () => {
    // The sentence replaces the ROWS, and a by-line with no row to sit under
    // is provenance for a value nobody can see.
    const { container } = render(
      <PropertiesRail
        groups={[
          {
            name: "Plan",
            whenAllAbsent: "Nothing scheduled.",
            properties: [{ label: "Due", setBy: { ...CREATE, cleared: true } }],
          },
        ]}
      />,
    );
    expect(container.querySelectorAll(".props-setby")).toHaveLength(0);
    expect(screen.getByText("Nothing scheduled.")).toBeTruthy();
  });

  test("keeps the turn link in one box, so its arrow cannot be orphaned", () => {
    // `svg { display: block }` is the base reset, and a block box inside an
    // inline anchor splits the anchor around it — so the arrow sat on a line
    // of its own, at every width, in every rail and every fact line in the
    // product. The fix is the anchor's own formatting context, which is a
    // CLASS here because jsdom computes no layout: what this pins is that the
    // word and the glyph are in ONE element carrying the rule.
    const { container } = render(
      <PropertiesRail
        groups={[{ properties: [{ label: "Status", value: "todo", setBy: CREATE }] }]}
      />,
    );
    const link = container.querySelector("a.setby-turn")!;
    expect(link.textContent).toContain("turn");
    expect(link.querySelector("svg")).toBeTruthy();
  });
});
