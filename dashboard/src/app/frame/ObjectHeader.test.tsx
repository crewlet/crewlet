/**
 * What an object's header says, and what it deliberately does not.
 *
 * Two rules live here and both were broken on one screen at once. The header
 * and the properties rail render the SAME provenance line, so it has one
 * wording and one markup rather than two that drift. And a header stacked
 * above a rail in one column may not restate what the rail is about to say —
 * which is a rule about the CALLER, so what is pinned is that the component
 * draws exactly what it was handed.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { FactLine, ObjectHeader, SetByLine, type SetBy } from "./ObjectHeader.tsx";
import { PropertiesRail } from "./PropertiesRail.tsx";

afterEach(cleanup);

const SET: SetBy = { actor: "ada", at: "2031-04-16T09:00:00Z", ago: "2h ago", turnId: "t-1" };

// ONE WORDING, IN BOTH FRAMES. The fact line said "set by ada" and the rail
// said a bare "ada" — the same provenance phrased two ways on one screen,
// where under a value a bare handle reads as who wrote the ROW rather than as
// who set the field. Two copies of one sentence is how a wording drifts with
// nothing to catch it, so there is one component and this is what says so.
test("the header and the rail say provenance in the same words", () => {
  const { container } = render(
    <>
      <FactLine facts={[{ label: "Status", value: "In progress", setBy: SET }]} />
      <PropertiesRail
        groups={[{ properties: [{ label: "Status", value: "In progress", setBy: SET }] }]}
      />
    </>,
  );
  const header = container.querySelector(".fact-setby")!.textContent;
  const rail = container.querySelector(".props-setby")!.textContent;
  expect(header).toBe(rail);
  expect(header).toBe("set by ada · 2h ago · turn ");
});

// A LIST OF FIELDS IS THE RAIL'S OWN CASE and it is written by hand rather
// than by `Intl.ListFormat`, which is locale-keyed: `en-US` writes an Oxford
// comma and `en-GB` does not, so the string the product renders would depend
// on the reader's browser and no case could name it.
test("names the properties one line covers, without a locale deciding how", () => {
  render(<SetByLine setBy={SET} fields={["Status", "Priority", "Type"]} className="props-setby" />);
  expect(screen.getByText(/Status, Priority and Type · set by ada/)).toBeTruthy();
});

// NEVER "SET BY —". A fact with no value is dropped from the line entirely,
// so its attribution goes with it: a by-line under nothing claims the engine
// recorded somebody setting nothing.
test("drops a fact that has no value, attribution and all", () => {
  const { container } = render(
    <FactLine
      facts={[
        { label: "Status", value: "In progress", setBy: SET },
        { label: "Due", value: undefined, setBy: SET },
      ]}
    />,
  );
  expect(screen.queryByText("Due")).toBeNull();
  expect(container.querySelectorAll(".fact-setby")).toHaveLength(1);
});

// THE HEADER DRAWS WHAT IT WAS HANDED. A peek whose body is a properties rail
// passes no facts — the rail states every property once, a hundred pixels
// below — and a peek with no rail under it passes its facts as any page does.
// The component may not decide that on `size`: it would take a rule about one
// frame's BODY and apply it to every frame's header, silently emptying the
// fact line of every peek in the product that has no rail.
test("draws no fact line when the caller passes none, and keeps the state marks", () => {
  const { container } = render(
    <ObjectHeader
      size="peek"
      kind="Item"
      identifier="LEAD-1"
      title="Test purpose"
      status={<span>Blocked</span>}
    />,
  );
  expect(container.querySelector(".fact-line")).toBeNull();
  expect(screen.getByText("LEAD-1")).toBeTruthy();
  expect(screen.getByText("Blocked")).toBeTruthy();
});

test("draws the fact line a peek with no rail under it still needs", () => {
  const { container } = render(
    <ObjectHeader
      size="peek"
      kind="Unit"
      title="Engineering"
      facts={[{ label: "Lead", value: "ada" }]}
    />,
  );
  expect(container.querySelector(".fact-line")).not.toBeNull();
  expect(screen.getByText("Lead")).toBeTruthy();
});
