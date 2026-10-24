/**
 * The org chart's company node and Company › Principles, end to end: the one
 * way the builder has to change the company's mission, vision and policies is
 * to send the person to the page that writes them, and that page opens in edit
 * for a person who may write them and simply reads for one who may not.
 *
 * The builder's own suites pin that its Edit leaves for an address
 * (`Builder.test.tsx`, `editWiring.test.tsx`) and the page's pin what an
 * address opens (`edit/usePrinciplesEdit.test.tsx`); this is where the two
 * meet, so an address one side writes and the other no longer reads is a
 * failing test rather than a page that opens with nothing in edit.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeAll, beforeEach, expect, test, vi } from "vitest";
import { ToastProvider } from "@crewlethq/ui";
import { screenFor } from "~/app/App.tsx";
import { loadChunk } from "~/app/lazyScreen.ts";
import { Router, useRoute } from "~/app/router.tsx";
import { resolve } from "~/app/routes.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { company, EDITOR, Engine, InertWebSocket } from "~/routes/org/builder/testkit.tsx";
import { charterEngine, charterRoute, type CharterEngine } from "~/test/charterEngine.ts";
import { healthFrame } from "~/test/health.ts";
import { withDerived } from "~/test/org.ts";

const CHARTER = {
  mission: "Split costs fairly",
  vision: "Every shared bill settles itself",
  policies: ["Every amount is exact to the cent."],
};

let charter: CharterEngine;
let builder: Engine;

// The two chunks these addresses draw, in before a case starts, so a case
// times what a screen draws rather than a cold `import()`.
beforeAll(async () => {
  await Promise.all([loadChunk("org"), loadChunk("settings")]);
});

beforeEach(() => {
  charter = charterEngine(CHARTER);
  builder = new Engine({ ...company(), ...CHARTER });
  // THE CHARTER ROUTE IS ITS OWN, answered beside the builder's configuration.
  builder.script = (request) =>
    request.path === "/config/charter"
      ? charterRoute(charter)(`http://engine${request.path}`, {
          method: request.method,
          headers: request.headers,
          ...(request.body === undefined ? {} : { body: JSON.stringify(request.body) }),
        })
      : null;
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  location.hash = "#/";
});

/** The screen the address resolves to, as the frame draws it, following every move. */
function Screens() {
  const route = resolve(useRoute().path);
  return route.resolved ? screenFor(route) : null;
}

/** The builder's table view, for a reader holding `grants`. */
function mount(grants: string[]) {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/company/org/edit?view=table";
  builder.install();
  const store = new Store();
  store.applyHealth(healthFrame());
  store.applyOrg(withDerived({ name: "Acme", ...CHARTER, roles: [] }));
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(what === "viewer" ? { ...EDITOR, grants, leads: [] } : null);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <ToastProvider>
            <Router>
              <Screens />
            </Router>
          </ToastProvider>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

// AN ADMINISTRATOR'S EDIT OF THE COMPANY LANDS IN THE MISSION, on the page
// that writes it, with the engine's words in the field and the focus in it.
// Mutation: send the company's Edit to `edit=charter` (the address the builder
// used to read) and the page opens nothing.
test("the company's Edit principles opens Company › Principles with the mission in edit", async () => {
  mount(["admin", "tracker", "knowledge"]);
  fireEvent.click(await screen.findByRole("button", { name: "Edit principles" }));
  const mission = await within(await screen.findByRole("region", { name: "Mission" })).findByRole(
    "textbox",
    { name: "Mission" },
  );
  await waitFor(() => expect(mission).toHaveProperty("value", CHARTER.mission));
  await waitFor(() => expect(document.activeElement).toBe(mission));
  expect(location.hash).toBe("#/company/principles?edit=mission");
  // THE BUILDER WROTE NOTHING on the way (a dry run of its unchanged draft
  // is a check, not a write): the principles are the page's to save.
  const writes = builder.requests.filter(
    (r) => r.method !== "GET" && r.path.startsWith("/config") && r.path !== "/config/charter",
  );
  expect(writes.filter((r) => r.query.get("dry_run") !== "true")).toEqual([]);
});

// A READER WHO MAY NOT WRITE THEM is offered to open them, and reads them:
// nothing opens, and the guarded route is never asked.
test("a reader who may not write them opens Company › Principles to read", async () => {
  mount(["tracker", "knowledge"]);
  fireEvent.click(await screen.findByRole("button", { name: "Open principles" }));
  const region = await screen.findByRole("region", { name: "Mission" });
  expect(within(region).getByText(CHARTER.mission)).toBeDefined();
  expect(location.hash).toBe("#/company/principles");
  expect(screen.queryByRole("textbox")).toBeNull();
  expect(charter.gets).toBe(0);
});
