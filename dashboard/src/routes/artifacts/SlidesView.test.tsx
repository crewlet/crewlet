/**
 * A deck presented in the page, as a reader opens it from the view's control:
 * the show takes the keys, and it is never taller than the view it sits in.
 *
 * The deck is the engine's own answer for its test deck
 * (`internal/artifacts/slideformat/testdata/deck.golden.json`).
 */

import { readFileSync } from "node:fs";
import { AppAnnouncer } from "~/app/announcer.tsx";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { LayerHost } from "@crewlethq/ui";
import { SlidesView, deckEditFields } from "./SlidesView.tsx";
import { context, openFrame } from "./draftTesting.ts";
import { draftSnapshot, parseDraftChange } from "~/app/assistant/draft.ts";
import { draftOf } from "~/lib/artifacts/deck.ts";
import { titleOf } from "~/lib/slides.tsx";
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
      <AppAnnouncer />
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

/** The show's stage: the slide, letterboxed into the room the stage measured. */
const stageOf = (show: HTMLElement) => within(show).getByRole("group", { name: /^Slide 1 of / });

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
    return this.classList.contains("crewlet-slide-stage") ? 1143 : 0;
  });
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
    const top = this.tagName === "MAIN" ? 10 : this.classList.contains("slides-show") ? 324 : 0;
    return new DOMRect(0, top, 0, 0);
  });
  mount();
  const show = await present();
  expect(stageOf(show).style.height).toBe("516px");
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
    return this.classList.contains("crewlet-slide-stage") ? 358 : 0;
  });
  vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement,
  ) {
    return new DOMRect(0, this.classList.contains("slides-show") ? 380 : 0, 0, 0);
  });
  mount();
  const show = await present();
  expect(stageOf(show).style.height).toBe("201px");
});

test("the slide on the stage says where it is in the deck, and when Present skips it", async () => {
  const hidden = (deck.slides ?? []).find((s) => s.hidden)!;
  const at = (deck.slides ?? []).indexOf(hidden) + 1;
  location.hash = `#/artifacts/${ID}?slide=${hidden.id}`;
  mount();
  expect(await screen.findByText(`Slide ${at} of ${deck.slides!.length}`)).toBeTruthy();
  expect(screen.getByText("Hidden when presenting")).toBeTruthy();
});

test("Present's controls stop at the ends of the deck, saying why", async () => {
  mount();
  const show = await present();
  const previous = within(show).getByRole("button", { name: "Previous slide" });
  expect(previous.getAttribute("aria-disabled")).toBe("true");
  expect(within(show).getByText("This is the first slide")).toBeTruthy();
  fireEvent.keyDown(show, { key: "End" });
  expect(await within(show).findByText(`${shown} of ${shown}`)).toBeTruthy();
  const next = within(show).getByRole("button", { name: "Next slide" });
  expect(next.getAttribute("aria-disabled")).toBe("true");
});

// A READER WHO MAY NOT PUBLISH HOLDS NO DRAFT: the deck is read, and the
// assistant is told of no edit to change.
test("a deck read rather than edited declares no draft", async () => {
  openFrame("artifact");
  mount();
  await screen.findByText(`Slide 1 of ${deck.slides!.length}`);
  expect(context().draft).toBeUndefined();
  expect(context().accepts).not.toContain("draft.set");
});

// AN EDIT'S FIELDS ARE THE DECK'S TITLE AND EACH SLIDE'S TITLE AND NOTES, each
// set as the editor's own controls write it: the title's run of text in the
// slide's tree, the notes beside it.
test("a deck's edit reads and sets the deck's title and each slide's title and notes", () => {
  const draft = draftOf(deck, 1);
  const spec = {
    kind: "client" as const,
    scope: "Your edit (not saved)",
    value: draft,
    fields: deckEditFields(draft.order),
    apply: () => {},
  };
  const first = draft.slides[draft.order[0]!]!;
  expect(draftSnapshot(spec).fields[1]).toMatchObject({
    path: "slides.1.title",
    value: titleOf(first.root),
    editable: true,
  });
  const parsed = parseDraftChange(spec, {
    values: { title: "Plan for Q3", "slides.1.title": "Where we are", "slides.2.notes": "Pause." },
  });
  if (!parsed.ok) throw new Error(parsed.reason);
  expect(parsed.value.next.title).toBe("Plan for Q3");
  expect(titleOf(parsed.value.next.slides[draft.order[0]!]!.root)).toBe("Where we are");
  expect(parsed.value.next.slides[draft.order[1]!]!.notes).toBe("Pause.");
  expect(parseDraftChange(spec, { values: { "slides.1.notes": "x".repeat(4001) } }).ok).toBe(false);
});
