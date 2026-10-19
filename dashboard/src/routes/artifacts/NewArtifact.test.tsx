/**
 * New artifact: each kind it starts is named by the kind's own word, and every
 * control stands under a visible name.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { LayerHost, ToastProvider } from "@crewlethq/ui";

import { NewArtifact } from "./NewArtifact.tsx";
import { Router } from "~/app/router.tsx";
import { KIND_WORDS } from "~/lib/artifacts/words.ts";

vi.mock("~/lib/artifacts/access.ts", async () => {
  const actual = await vi.importActual<typeof import("~/lib/artifacts/access.ts")>(
    "~/lib/artifacts/access.ts",
  );
  return { ...actual, useArtifactAccess: () => ({ can: true, as: "ada", acts: [] }) };
});

afterEach(cleanup);

function mount(hash: string) {
  location.hash = hash;
  return render(
    <Router>
      <LayerHost>
        <ToastProvider>
          <NewArtifact />
        </ToastProvider>
      </LayerHost>
    </Router>,
  );
}

// THE GALLERY'S TYPE FILTER AND COLUMN say "Slides"; the switch said "Deck".
test("the type switch names every kind by the word the gallery uses", () => {
  mount("#/artifacts/new");
  const types = screen.getByRole("radiogroup", { name: "Type" });
  const names = [...types.querySelectorAll('[role="radio"]')].map((r) => r.textContent);
  expect(names).toEqual([KIND_WORDS.sheet, KIND_WORDS.doc, KIND_WORDS.slides]);
});

test("a sheet's templates stand under a visible name", () => {
  mount("#/artifacts/new?kind=sheet");
  const group = screen.getByRole("group", { name: "Start from" });
  expect(group.querySelector("legend")?.textContent).toContain("Start from");
});
