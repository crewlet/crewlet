/**
 * A project's row: its name and its purpose are one two-line cell, the purpose
 * straight under the name as every two-line table cell draws it, on a desktop's
 * columns and on a phone's card rows.
 */
import { afterEach, expect, test } from "vitest";
import { page } from "vitest/browser";
import { flushSync } from "react-dom";
import { createRoot, type Root } from "react-dom/client";
import { LayoutClient } from "~/test/layout.tsx";
import { eventually } from "~/test/settle.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import type { WorkProjectRow } from "~/protocol/index.ts";
import { Projects } from "./Projects.tsx";

let root: Root | null = null;
let host: HTMLElement | null = null;

afterEach(() => {
  flushSync(() => root?.unmount());
  host?.remove();
  root = null;
  host = null;
  location.hash = "#/";
});

const ORG = {
  name: "Acme",
  timezone: "UTC",
  roles: [{ name: "Ada Okonkwo", handle: "ada", kind: "agent" }],
  units: [],
};

const PURPOSE =
  "Plan, design, build, test and ship the control plane across the backend, web and mobile apps";

const CORE: WorkProjectRow = {
  key: "ENG",
  name: "Core",
  purpose: PURPOSE,
  unit: { key: "platform", name: "Platform", resolved: true },
  lead: { handle: "ada", kind: "agent" },
  task_counts: { todo: 5, active: 3, done: 8, closed: 0 },
  version: 1,
};

async function mount(viewport: number, width: number) {
  await page.viewport(viewport, 900);
  location.hash = "#/worklet";
  host = document.createElement("div");
  host.style.width = `${width}px`;
  document.body.append(host);
  root = createRoot(host);
  flushSync(() =>
    root!.render(
      <LayoutClient
        answers={{
          work_projects: {
            projects: [CORE],
            total: 1,
            complete: true,
            census: { active: 1, archived: 0 },
          },
        }}
        org={ORG}
      >
        <ViewerProvider>
          <Projects />
        </ViewerProvider>
      </LayoutClient>,
    ),
  );
  await document.fonts.ready;
}

/** The innermost element drawing exactly this text. */
function drawing(text: string): HTMLElement {
  const all = [...host!.querySelectorAll<HTMLElement>(".crewlet-data-grid__row *")];
  return all.filter((el) => el.textContent === text).at(-1)!;
}

test.each([
  ["on a desktop's columns", 1440, 1200],
  ["on a phone's card rows", 390, 358],
])("a project's purpose stands straight under its name %s", async (_, viewport, width) => {
  await mount(viewport, width);
  await eventually(() => expect(drawing(PURPOSE)).toBeTruthy());
  const name = drawing("Core").getBoundingClientRect();
  const purpose = drawing(PURPOSE).getBoundingClientRect();
  expect(purpose.top - name.bottom).toBeGreaterThanOrEqual(-0.5);
  expect(purpose.top - name.bottom).toBeLessThanOrEqual(1);
  // Cut to its column on a desktop; whole on a phone, where no hover shows the rest.
  const cut = drawing(PURPOSE).scrollWidth > drawing(PURPOSE).clientWidth;
  expect(cut).toBe(viewport > 390);
});
