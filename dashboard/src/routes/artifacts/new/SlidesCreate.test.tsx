/**
 * The new slide deck page: a title, a theme and the slides it opens with, sent
 * as one plan the engine composes; and its loading, failing and leaving.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { rest } from "~/protocol/index.ts";
import { readSlidesStarters } from "~/lib/artifacts/api.ts";
import type { SlidesStartersAnswer } from "~/protocol/index.ts";
import { SlidesCreate } from "./SlidesCreate.tsx";

const starters = JSON.parse(
  readFileSync(
    join(process.cwd(), "..", "internal/api/artifactsapi/testdata/slides-starters.golden.json"),
    "utf8",
  ),
) as SlidesStartersAnswer;

vi.mock("~/lib/artifacts/api.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/artifacts/api.ts")>("~/lib/artifacts/api.ts");
  return { ...actual, readSlidesStarters: vi.fn() };
});

// THE ENGINE'S ANSWER TO A CREATE, at the transport every keyed write goes through.
const request = vi.spyOn(rest, "request");

beforeEach(() => {
  vi.mocked(readSlidesStarters).mockResolvedValue(starters);
  request.mockReset();
  request.mockResolvedValue({ status: 201, body: { artifact: "a-1" } } as never);
});
afterEach(cleanup);

function mount() {
  location.hash = "#/artifacts/new/slides";
  return render(
    <Router>
      <LayerHost>
        <ToastProvider>
          <SlidesCreate />
        </ToastProvider>
      </LayerHost>
    </Router>,
  );
}

const title = () => screen.findByRole("textbox", { name: "Title" });

test("it opens on the title, in Nightshade, with the engine's four starting slides", async () => {
  mount();
  const field = await title();
  await waitFor(() => expect(document.activeElement).toBe(field));
  expect(screen.getByRole("radio", { name: /^Nightshade,/ }).getAttribute("aria-checked")).toBe(
    "true",
  );
  expect(document.querySelectorAll(".crewlet-slide-filmstrip__thumb")).toHaveLength(4);
  expect(screen.getByRole("button", { name: "Create deck" }).getAttribute("aria-disabled")).toBe(
    "true",
  );
});

test("Create deck sends the title, the theme and every slide as one plan, then opens the deck", async () => {
  mount();
  fireEvent.change(await title(), { target: { value: "Board review" } });
  fireEvent.click(screen.getByRole("radio", { name: /^Grove,/ }));
  fireEvent.click(screen.getByRole("button", { name: "Slide 2, Agenda", exact: true } as never));
  fireEvent.click(screen.getByRole("button", { name: "New slide" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: /Big number/ }));
  fireEvent.change(screen.getByRole("textbox", { name: "Slide title" }), {
    target: { value: "Uptime" },
  });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: "Create deck" }));
  });
  await waitFor(() => expect(location.hash).toBe("#/artifacts/a-1"));
  const [method, path, sent] = request.mock.calls[0]!;
  expect([method, path]).toEqual(["POST", "/artifacts"]);
  expect((sent as { body: unknown }).body).toEqual({
    kind: "slides",
    title: "Board review",
    starter: true,
    deck: {
      theme: "grove",
      slides: [
        { layout: "cover" },
        { layout: "agenda" },
        { layout: "number", title: "Uptime" },
        { layout: "columns" },
        { layout: "closing" },
      ],
    },
  });
});

test("leaving with a title typed asks first, and keeping on stays", async () => {
  mount();
  fireEvent.change(await title(), { target: { value: "Board review" } });
  fireEvent.click(screen.getAllByRole("button", { name: "Cancel" })[0]!);
  expect(await screen.findByText("Leave without creating the deck?")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Keep editing" }));
  expect(location.hash).toBe("#/artifacts/new/slides");
});

test("themes that did not load say so and offer to ask again", async () => {
  vi.mocked(readSlidesStarters).mockRejectedValueOnce(new Error("down"));
  mount();
  const retry = await screen.findByRole("button", { name: "Try again" });
  fireEvent.click(retry);
  expect(await title()).toBeTruthy();
});
