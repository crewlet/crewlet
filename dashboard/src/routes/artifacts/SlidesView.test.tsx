/**
 * A deck presented in the page, as a reader opens it from the view's control:
 * the show takes the keys, and it is never taller than the view it sits in.
 *
 * The deck is the engine's own answer for its test deck
 * (`internal/artifacts/slideformat/testdata/deck.golden.json`).
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { LayerHost } from "@crewlethq/ui";
import { SlidesView } from "./SlidesView.tsx";
import { WithKindBar } from "~/test/kindControls.tsx";
import { Router } from "~/app/router.tsx";
import type { ArtifactDeck, ArtifactDetail } from "~/protocol/index.ts";

// A READER WHO MAY NOT PUBLISH: the deck shows its slides rather than the editor.
vi.mock("~/lib/artifacts/access.ts", () => ({ useArtifactAccess: () => ({ can: false }) }));

const ID = "0192f2c4-1111-7000-8000-000000000002";
const deck = JSON.parse(
  readFileSync(
    join(process.cwd(), "..", "internal/artifacts/slideformat/testdata/deck.golden.json"),
    "utf8",
  ),
) as ArtifactDeck;
const shown = (deck.slides ?? []).filter((s) => !s.hidden).length;

const detail = {
  id: ID,
  kind: "slides",
  title: "Quarterly plan",
  head_number: 1,
  role: "viewer",
} as unknown as ArtifactDetail;

beforeEach(() => {
  location.hash = `#/artifacts/${ID}`;
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      const body =
        path === `/artifacts/${ID}/versions/1/deck` ? { artifact: ID, version: 1, deck } : null;
      return new Response(JSON.stringify(body ?? {}), {
        status: body ? 200 : 404,
        headers: { "Content-Type": "application/json" },
      });
    }),
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "";
});

const mount = () =>
  render(
    <Router>
      <LayerHost>
        <WithKindBar>
          <main style={{ overflowY: "auto" }}>
            <SlidesView
              detail={detail}
              version={1}
              presence={{ at: () => {} }}
              onPublished={() => {}}
            />
          </main>
        </WithKindBar>
      </LayerHost>
    </Router>,
  );

/** The reader's press on Present: focus on the radio, then the click. */
async function present() {
  const radio = await screen.findByRole("radio", { name: "Present" });
  radio.focus();
  fireEvent.click(radio);
  return screen.findByRole("region", { name: "Slide show" });
}

test("Present takes the keys: the arrows and Page keys step slides, not the view control", async () => {
  mount();
  const show = await present();
  expect(document.activeElement).toBe(show);
  expect(screen.getByText(`1 of ${shown}`)).toBeTruthy();
  fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" });
  expect(await screen.findByText(`2 of ${shown}`)).toBeTruthy();
  fireEvent.keyDown(document.activeElement!, { key: "PageDown" });
  expect(await screen.findByText(`3 of ${shown}`)).toBeTruthy();
  fireEvent.keyDown(document.activeElement!, { key: "PageUp" });
  expect(await screen.findByText(`2 of ${shown}`)).toBeTruthy();
  expect(screen.getByRole("radio", { name: "Present" }).getAttribute("aria-checked")).toBe("true");
});

test("Present is never taller than the view: the slide is letterboxed into the room below the controls", async () => {
  // THE PAGE AT 1440 x 900: a scrollport 830px tall from y 10, the show's top
  // at y 324 and its width 1143px, so the slide at that width (643px) is
  // taller than the 516px left, and the show takes the 516px.
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockImplementation(function (
    this: HTMLElement,
  ) {
    return this.tagName === "MAIN" ? 830 : 0;
  });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(function (
    this: HTMLElement,
  ) {
    return this.classList.contains("slides-show") ? 1143 : 0;
  });
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
    const top = this.tagName === "MAIN" ? 10 : this.classList.contains("slides-show") ? 324 : 0;
    return new DOMRect(0, top, 0, 0);
  });
  mount();
  const show = await present();
  expect(show.style.getPropertyValue("--slides-show-height")).toBe("516px");
});

test("a show with room to spare asks only for the slide at its width", async () => {
  // A PHONE: a 358px column whose slide is 201px tall, with 400px below it.
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockImplementation(function (
    this: HTMLElement,
  ) {
    return this.tagName === "MAIN" ? 780 : 0;
  });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(function (
    this: HTMLElement,
  ) {
    return this.classList.contains("slides-show") ? 358 : 0;
  });
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
    return new DOMRect(0, this.classList.contains("slides-show") ? 380 : 0, 0, 0);
  });
  mount();
  const show = await present();
  expect(show.style.getPropertyValue("--slides-show-height")).toBe("201px");
});
