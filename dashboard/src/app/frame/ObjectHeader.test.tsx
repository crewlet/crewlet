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

import { readFileSync } from "node:fs";
import { join } from "node:path";
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

// A FACT GETS TWO LINES BEFORE IT IS CUT. At one line, "not running on this
// node" in an 8rem track (7.5rem now) read "not running on this no…" at 1440 — the phrase
// was the whole fact and its ending was what went.
test("a fact's value clamps at two lines rather than truncating at one", () => {
  const { container } = render(
    <FactLine facts={[{ label: "Runtime", value: "not running on this node" }]} />,
  );
  const value = container.querySelector(".fact-value")!;
  expect(value.classList.contains("clamp")).toBe(true);
  expect(value.classList.contains("truncate")).toBe(false);
});

// A TOKEN IS NEVER SPLIT. The clamp wraps where it must, and an event's type
// came out "turn.guard_breac" over "h" in a 7.5rem track — an identifier a
// reader can neither read nor search for. A token fact takes two tracks and
// one line, and the stylesheet (`.fact.is-token`, `.fact-token`) is what says
// how; here, that the fact is marked for it and not clamped.
//
// Mutation: drop `token` from `FactLine`'s class choice, and the value clamps.
test("a token fact takes two tracks on one line rather than wrapping mid-token", () => {
  const { container } = render(
    <FactLine facts={[{ label: "Type", value: "turn.guard_breach", token: true }]} />,
  );
  expect(container.querySelector(".fact")!.classList.contains("is-token")).toBe(true);
  const value = container.querySelector(".fact-value")!;
  expect(value.classList.contains("fact-token")).toBe(true);
  expect(value.classList.contains("clamp")).toBe(false);
});

// A SET OF CHIPS TAKES TWO TRACKS AND IS NEVER CLAMPED. A node's three roles in
// one `7.5rem` track stood two over one, a fact taller than every fact beside
// it; clamped, a chip would be cut in half.
//
// Mutation: drop `set` from `FactLine`'s class choice, and the fact is one
// track and clamped.
test("a set fact takes two tracks and wraps between its chips, never inside one", () => {
  const { container } = render(
    <FactLine facts={[{ label: "Roles", value: <span>ingress seats workers</span>, set: true }]} />,
  );
  expect(container.querySelector(".fact")!.classList.contains("is-set")).toBe(true);
  const value = container.querySelector(".fact-value")!;
  expect(value.classList.contains("clamp")).toBe(false);
  expect(value.classList.contains("fact-token")).toBe(false);
  const css = readFileSync(join(process.cwd(), "src/styles/frame.css"), "utf8");
  expect(css).toMatch(/\.fact\.is-set\s*\{\s*grid-column:\s*span 2;/);
});

// A NOTE IS ITS VALUE'S. On a band of its own, a neighbour's value wrapping to
// two lines pushed every note in the row down, and "37%" stood a blank line
// above the note saying what it was 37% of. So the value and its footnotes
// share one cell, and the fact is two bands: its label, then that cell.
test("a fact's note and set-by sit in the same cell as its value", () => {
  const { container } = render(
    <FactLine
      facts={[
        { label: "Cache", value: "37%", note: "16.8k of 46.0k input read from cache", setBy: SET },
      ]}
    />,
  );
  const fact = container.querySelector(".fact")!;
  expect([...fact.children].map((c) => c.className)).toEqual(["fact-label", "fact-body"]);
  const body = fact.querySelector(".fact-body")!;
  expect([...body.children].map((c) => c.className.split(" ")[0])).toEqual([
    "fact-value",
    "fact-note",
    "fact-setby",
  ]);
});
