/**
 * A deck grows in the editor's strip: New slide adds from the layouts of the
 * theme the deck's index names, or a copy of the slide on the stage where it
 * names none; Command or Control+D copies and Delete removes on a thumbnail.
 */

import { readFileSync } from "node:fs";
import { AppAnnouncer } from "~/app/announcer.tsx";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { SlidesView } from "./SlidesView.tsx";
import { WithKindBar } from "~/test/kindControls.tsx";
import { Router } from "~/app/router.tsx";
import type { ArtifactDeck, ArtifactDetail } from "~/protocol/index.ts";

// AN EDITOR: the deck opens in Edit, its strip grows and shrinks.
vi.mock("~/lib/artifacts/access.ts", () => ({ useArtifactAccess: () => ({ can: true }) }));

const ID = "0192f2c4-1111-7000-8000-000000000003";
const read = (rel: string) => JSON.parse(readFileSync(join(process.cwd(), "..", rel), "utf8"));
const golden = read("internal/artifacts/slideformat/testdata/deck.golden.json") as ArtifactDeck;
const starters = read("internal/api/artifactsapi/testdata/slides-starters.golden.json");

const detail = {
  id: ID,
  kind: "slides",
  title: "Quarterly plan",
  head_number: 1,
  role: "editor",
} as unknown as ArtifactDetail;

function serve(deck: ArtifactDeck) {
  Object.defineProperty(globalThis, "fetch", {
    writable: true,
    value: vi.fn(async (input: RequestInfo | URL) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      const body =
        path === `/artifacts/${ID}/versions/1/deck`
          ? { artifact: ID, version: 1, deck }
          : path === "/artifacts/starters/slides"
            ? starters
            : null;
      return new Response(JSON.stringify(body ?? {}), {
        status: body ? 200 : 404,
        headers: { "Content-Type": "application/json" },
      });
    }),
  });
}

beforeEach(() => {
  location.hash = `#/artifacts/${ID}`;
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
        <ToastProvider>
          <WithKindBar>
            <SlidesView
              detail={detail}
              version={1}
              presence={{ at: () => {} }}
              onPublished={() => {}}
            />
          </WithKindBar>
        </ToastProvider>
      </LayerHost>
    </Router>,
  );

const thumbs = () => document.querySelectorAll(".crewlet-slide-filmstrip__thumb");
const onStage = () =>
  document
    .querySelector(
      '.crewlet-slide-filmstrip__item[data-selected="true"] .crewlet-slide-filmstrip__thumb',
    )
    ?.getAttribute("aria-label");

test("a deck that names no theme adds a copy of the slide on the stage, after it", async () => {
  serve(golden);
  mount();
  const add = await screen.findByRole("button", { name: "New slide" });
  const before = thumbs().length;
  fireEvent.click(add);
  expect(thumbs()).toHaveLength(before + 1);
  expect(onStage()).toBe(`Slide 2, ${golden.slides![0]!.title}`);
});

test("a deck that names a theme adds from its layouts, drawn in that theme", async () => {
  serve({ ...golden, theme: "paper" });
  mount();
  // The theme's layouts arrive after the deck: until then New slide copies.
  const add = await waitFor(() => {
    const b = screen.getByRole("button", { name: "New slide" });
    expect(b.getAttribute("aria-haspopup")).toBe("menu");
    return b;
  });
  fireEvent.click(add);
  const quote = await screen.findByRole("menuitem", { name: /Quote/ });
  fireEvent.click(quote);
  expect(onStage()).toBe("Slide 2, A quote from a customer, in their own words");
});

test("Command+D copies a slide and Delete removes it, and the last slide stays", async () => {
  serve({ ...golden, slides: golden.slides!.slice(0, 2) });
  mount();
  await screen.findByRole("button", { name: "New slide" });
  const first = thumbs()[0] as HTMLElement;
  fireEvent.keyDown(first, { key: "d", metaKey: true });
  expect(thumbs()).toHaveLength(3);
  fireEvent.keyDown(thumbs()[1] as HTMLElement, { key: "Delete" });
  fireEvent.keyDown(thumbs()[0] as HTMLElement, { key: "Delete" });
  fireEvent.keyDown(thumbs()[0] as HTMLElement, { key: "Delete" });
  expect(thumbs()).toHaveLength(1);
});
