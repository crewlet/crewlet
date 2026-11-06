import { TestClient } from "~/test/client.tsx";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { installWindow } from "~/testing.tsx";
import { cleanup, render, screen } from "@testing-library/react";
import { LayerHost, ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { HtmlCreate } from "./HtmlCreate.tsx";

/**
 * A DEPLOYMENT THAT CANNOT SHOW PAGES SAYS SO BEFORE A PUBLISH IS SPENT. The
 * frame's own refusal arrived after the draft was made, and handed every reader
 * a config key only an admin can act on.
 */

let reader = { admin: false, artifactPages: false as boolean | undefined };
vi.mock("~/lib/viewer.ts", async (original) => ({
  ...(await original<typeof import("~/lib/viewer.ts")>()),
  useViewer: () => reader,
}));
vi.mock("~/lib/artifacts/access.ts", () => ({ useArtifactAccess: () => ({ can: true }) }));
vi.mock("./WhoBuildsIt.tsx", () => ({
  useWhoBuildsIt: () => ({
    builders: [{ seat: { handle: "pixel", name: "Pixel" }, suggested: true }],
    seat: "pixel",
    setSeat: () => {},
    project: "CRW",
    setProject: () => {},
    projects: [{ key: "CRW", name: "Crewlet" }],
    projectsLoading: false,
  }),
  WhoBuildsIt: () => null,
}));

let win: ReturnType<typeof installWindow>;
beforeEach(() => {
  win = installWindow(1440);
});
afterEach(() => win.restore());
afterEach(cleanup);

function mount() {
  location.hash = "#/artifacts/new/html?panel=closed";
  render(
    <TestClient>
      <Router>
        <LayerHost>
          <ToastProvider>
            <HtmlCreate />
          </ToastProvider>
        </LayerHost>
      </Router>
    </TestClient>,
  );
}

test("a reader is told to ask, and Preview is held", () => {
  reader = { admin: false, artifactPages: false };
  mount();
  expect(
    screen.getAllByText(
      "This deployment is not set up to show pages yet. Ask an admin to set it up.",
    ).length,
  ).toBeGreaterThan(0);
  expect(document.body.textContent).not.toContain("external_url");
  const preview = screen.getAllByRole("button", { name: "Preview privately" })[0]!;
  expect(preview.getAttribute("aria-disabled")).toBe("true");
});

test("an admin is told the setting", () => {
  reader = { admin: true, artifactPages: false };
  mount();
  expect(document.body.textContent).toContain("api.artifacts.external_url");
});

test("a deployment that can show pages, or has not said, draws no notice", () => {
  for (const artifactPages of [true, undefined]) {
    reader = { admin: true, artifactPages };
    mount();
    expect(document.body.textContent).not.toContain("not set up to show pages");
    cleanup();
  }
});
