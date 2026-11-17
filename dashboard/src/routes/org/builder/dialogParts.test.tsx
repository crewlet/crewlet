/**
 * `ScreenLink` is PROSE BY DEFAULT, and that default is load-bearing.
 *
 * Eleven of its fourteen call sites are a remedy trailing the sentence that
 * states the problem ("…No Mattermost bot for this seat. Open Integrations"), so
 * the anchor is a word in a line and takes `.prose-link`, which is the only
 * non-colour cue separating it from the sentence (base.css, WCAG 1.4.1). They
 * carried `.t-link` alone until the reset that stopped links underlining on
 * hover made the gap visible; the hover underline had been standing in for the
 * mark, and no keyboard or touch reader ever saw it.
 *
 * # Why a render test rather than a scan
 *
 * `styles/proselinks.test.ts` scans the tree for an in-sentence anchor that
 * does not carry the mark, and it SKIPS a bare `<ScreenLink>` on the strength
 * of this default. That is a premise about a component, and a source scan
 * cannot hold it: deleting the `cx(…)` and going back to `className="t-link"`
 * left the scan green and every one of those eleven links unmarked — measured,
 * by doing it. The premise is checked here, where it is a fact about what the
 * component renders rather than about how it is spelled.
 */

import { cleanup, fireEvent, render } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import { NodeProblems, ScreenLink } from "./dialogParts.tsx";
import type { PlacedProblem } from "./model/problems.ts";

afterEach(cleanup);

function classesOf(node: HTMLElement, text: string): string[] {
  const anchor = node.querySelector("a");
  expect(anchor, `no anchor rendered for ${text}`).not.toBeNull();
  return [...anchor!.classList].sort();
}

describe("ScreenLink", () => {
  test("is a prose link by default, because most of its call sites are a sentence", () => {
    const { container } = render(<ScreenLink to="integrations">Open Integrations</ScreenLink>);
    expect(classesOf(container, "the default")).toEqual(["prose-link", "t-link"]);
  });

  test("drops the mark only when the caller says it stands alone", () => {
    const { container } = render(
      <ScreenLink to="schedules" standalone>
        Open Schedules
      </ScreenLink>,
    );
    // `.t-link` still draws it — what goes is the underline, because there is
    // no sentence beside it for the underline to separate it from.
    expect(classesOf(container, "standalone")).toEqual(["t-link"]);
  });

  test("and standalone={false} is the default rather than the opt-out", () => {
    // The prop is optional, so an explicitly false value must read the same as
    // an absent one — otherwise a caller threading a boolean through gets the
    // opposite of what it asked for.
    const { container } = render(
      <ScreenLink to="secrets" standalone={false}>
        Open Secrets
      </ScreenLink>,
    );
    expect(classesOf(container, "standalone={false}")).toEqual(["prose-link", "t-link"]);
  });

  test("points at the screen it names", () => {
    // The floor: a test asserting classes on an anchor that goes nowhere would
    // pass just as well.
    const { container } = render(<ScreenLink to="integrations">Open Integrations</ScreenLink>);
    expect(container.querySelector("a")!.getAttribute("href")).toContain("integrations");
  });
});

describe("NodeProblems", () => {
  const placed = (severity: PlacedProblem["severity"], message: string): PlacedProblem =>
    ({
      severity,
      kind: "advisory",
      message,
      node: null,
      field: [],
      link: "schedules",
      source: {},
    }) as unknown as PlacedProblem;

  test("warnings are an advisory: quiet, one line each with its link", () => {
    const { container } = render(
      <NodeProblems problems={[placed("warning", "Runs 24 times a day")]} />,
    );
    const callout = container.querySelector(".crewlet-callout")!;
    expect(callout.className).toContain("crewlet-callout--quiet");
    expect(callout.querySelector(".crewlet-notice-list--compact")).not.toBeNull();
    expect(callout.querySelector("a")!.textContent).toBe("Open Schedules");
  });

  test("a titled warning is its short title, with the engine's sentence opened under it", () => {
    const warning = {
      ...placed("warning", "Learning runs on the same model as the agent's own work."),
      source: { title: "Agents that learn on their main model" },
    } as unknown as PlacedProblem;
    const { getByRole, getByText } = render(<NodeProblems problems={[warning]} />);
    const row = getByRole("button", { name: "Agents that learn on their main model" });
    expect(getByText(/Learning runs on/).closest("[hidden]")).not.toBeNull();
    fireEvent.click(row);
    expect(getByText(/Learning runs on/).closest("[hidden]")).toBeNull();
  });

  test("a refusal stays loud: tinted, announced and in full", () => {
    const { container } = render(
      <NodeProblems problems={[placed("problem", "Cron is not valid")]} />,
    );
    const callout = container.querySelector(".crewlet-callout")!;
    expect(callout.className).not.toContain("crewlet-callout--quiet");
    expect(callout.getAttribute("role")).toBe("alert");
    expect(callout.querySelector(".crewlet-notice-list--compact")).toBeNull();
  });
});
