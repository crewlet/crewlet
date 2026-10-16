/**
 * What this file adds to `@crewlethq/ui`'s ObjectHeader: the translation from
 * the engine's vocabulary (a glyph by name, a route, a turn by id) into what
 * the kit draws. The drawing's own rules (the two-line title, the fact line's
 * bands and clamps, a fact with no value dropped) are asserted in uilet.
 *
 * One rule still lives here because it spans two of this product's frames:
 * the header and the properties rail say provenance in the same words.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { href } from "../router.tsx";
import { FactLine, ObjectHeader, SetByLine, type SetBy } from "./ObjectHeader.tsx";
import { PropertiesRail } from "./PropertiesRail.tsx";

afterEach(cleanup);

const SET: SetBy = { actor: "ada", at: "2031-04-16T09:00:00Z", ago: "2h ago", turnId: "t-1" };

// ONE WORDING, IN BOTH FRAMES. The fact line said "set by ada" and the rail
// said a bare "ada", the same provenance phrased two ways on one screen,
// where under a value a bare handle reads as who wrote the ROW.
test("the header and the rail say provenance in the same words", () => {
  const { container } = render(
    <>
      <FactLine facts={[{ label: "Status", value: "In progress", setBy: SET }]} />
      <PropertiesRail
        groups={[{ properties: [{ label: "Status", value: "In progress", setBy: SET }] }]}
      />
    </>,
  );
  const header = container.querySelector(".crewlet-fact-line__set-by")!.textContent;
  const rail = container.querySelector(".props-setby")!.textContent;
  expect(header).toBe(rail);
  expect(header).toBe("set by ada · 2h ago · turn ");
});

test("the turn that set a value links to its page", () => {
  render(<SetByLine setBy={SET} fields={["Status", "Priority", "Type"]} className="props-setby" />);
  expect(screen.getByText(/Status, Priority and Type · set by ada/)).toBeTruthy();
  expect(screen.getByRole("link", { name: "turn" }).getAttribute("href")).toBe(
    href(["live", "turns", "t-1"]),
  );
});

test("a fact's route becomes its link, and a named glyph its mark", () => {
  const { container } = render(
    <ObjectHeader
      kind="Unit"
      icon="users"
      title="Engineering"
      facts={[{ label: "Lead", value: "ada", path: ["agents", "seats", "ada"] }]}
    />,
  );
  expect(screen.getByRole("link", { name: "ada" }).getAttribute("href")).toBe(
    href(["agents", "seats", "ada"]),
  );
  expect(container.querySelector(".crewlet-object-header__icon svg")).not.toBeNull();
  expect(screen.getByRole("heading", { level: 2, name: "Engineering" })).toBeTruthy();
});

// THE HEADER DRAWS WHAT IT WAS HANDED. A peek whose body is a properties rail
// passes no facts, and the adapter may not decide that on `size`.
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
  expect(container.querySelector(".crewlet-fact-line")).toBeNull();
  expect(screen.getByText("LEAD-1")).toBeTruthy();
  expect(screen.getByText("Blocked")).toBeTruthy();
});
