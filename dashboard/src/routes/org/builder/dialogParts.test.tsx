/**
 * `ScreenLink` is PROSE BY DEFAULT, and that default is load-bearing.
 *
 * Eleven of its fourteen call sites are a remedy trailing the sentence that
 * states the problem — "…so it has no channel to set. Open Integrations" — so
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

import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import { ReadOnlyFact, ScreenLink } from "./dialogParts.tsx";

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

/*
 * A READ-ONLY FACT'S LINK IS A PROMISE that the screen it opens changes the
 * value. The seat editor's "Edit in the configuration document" opened
 * Settings › Configuration, which reads and never writes, and half a dozen
 * facts beside it said "set in the configuration document" over the same
 * link. So `where` takes a screen that writes, or "revision" for a value no
 * screen writes, which says the write that does.
 */
describe("ReadOnlyFact", () => {
  test("a value no screen writes says how it is changed and links nowhere", () => {
    const { container } = render(
      <ReadOnlyFact label="Learning" reason="It overrides the company's." where="revision">
        Off
      </ReadOnlyFact>,
    );
    expect(container.querySelector("p")!.textContent).toBe(
      "It overrides the company's. No screen edits this: it is changed with crewlet config import or PUT /config.",
    );
    expect(container.querySelector("a")).toBeNull();
  });

  test("a value a screen writes links to that screen", () => {
    const { container } = render(
      <ReadOnlyFact
        label="Datadog fallback"
        reason="Chosen from Integrations."
        where={{ to: "integrations", label: "Open Integrations" }}
      >
        Not the fallback.
      </ReadOnlyFact>,
    );
    expect(container.querySelector("a")!.getAttribute("href")).toBe("#/settings/integrations");
  });

  test("a screen that only reads cannot be named as where a value is edited", () => {
    // Held by the compiler: `tsc --noEmit` fails on an unused directive, so
    // widening `where` back to every screen turns this line red.
    const reading = (
      // @ts-expect-error -- Settings › Configuration reads; it edits nothing.
      <ReadOnlyFact label="Model" reason="Per phase." where={{ to: "config", label: "Edit" }}>
        review: smart
      </ReadOnlyFact>
    );
    expect(reading).toBeDefined();
  });
});
