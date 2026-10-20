/**
 * New artifact: each kind it starts is named by the kind's own word, and every
 * control stands under a visible name.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { NewArtifact } from "./NewArtifact.tsx";
import { Router } from "~/app/router.tsx";
import { NewTaskOpener } from "~/app/newTask.ts";
import { KIND_WORDS } from "~/lib/artifacts/words.ts";

vi.mock("~/lib/artifacts/access.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/access.ts")>(
    "~/lib/artifacts/access.ts",
  );
  return { ...actual, useArtifactAccess: () => ({ can: true, as: "ada", acts: [] }) };
});

afterEach(cleanup);

function mount(hash: string, openNewTask = vi.fn()) {
  location.hash = hash;
  return render(
    <Router>
      <NewTaskOpener.Provider value={openNewTask}>
        <LayerHost>
          <ToastProvider>
            <NewArtifact />
          </ToastProvider>
        </LayerHost>
      </NewTaskOpener.Provider>
    </Router>,
  );
}

// THE GALLERY'S TYPE FILTER AND COLUMN say "Slides"; the switch said "Deck".
test("the type switch names every kind by the word the gallery uses", () => {
  mount("#/artifacts/new");
  const types = screen.getByRole("radiogroup", { name: "Type" });
  const names = [...types.querySelectorAll('[role="radio"]')].map((r) => r.textContent);
  expect(names).toEqual([
    KIND_WORDS.doc,
    KIND_WORDS.slides,
    KIND_WORDS.design,
    KIND_WORDS.dashboard,
    KIND_WORDS.html,
    KIND_WORDS.sheet,
  ]);
});

// THE GALLERY'S MAKE ROW opens New on every kind; an agent publishes three of
// them, so New on one says so and opens the New task sheet rather than
// offering a title field that could make nothing.
test.each(["design", "dashboard", "html"])("New on a %s offers to ask an agent", (kind) => {
  const openNewTask = vi.fn();
  mount(`#/artifacts/new?kind=${kind}`, openNewTask);
  expect(screen.queryByRole("textbox", { name: "Title" })).toBeNull();
  expect(
    screen.getByRole("radio", { name: KIND_WORDS[kind as "design"] }).getAttribute("aria-checked"),
  ).toBe("true");
  fireEvent.click(screen.getByRole("button", { name: "Ask an agent" }));
  expect(openNewTask).toHaveBeenCalledTimes(1);
});

test("a sheet's templates stand under a visible name", () => {
  mount("#/artifacts/new?kind=sheet");
  const group = screen.getByRole("group", { name: "Start from" });
  expect(group.querySelector("legend")?.textContent).toContain("Start from");
});
