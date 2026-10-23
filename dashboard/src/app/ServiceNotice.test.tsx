/**
 * What an address under a service the company does not run draws: a notice
 * in place that every signed-in reader can read, naming what runs instead,
 * and offering Company › Integrations only to a reader who may open it.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Router } from "./router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { SessionReading } from "~/lib/frameSession.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { OrgServices } from "~/contract/services.ts";
import { withDerived } from "~/test/org.ts";
import { ServiceNotice, noticeText, noticeTitle } from "./ServiceNotice.tsx";
import type { Service } from "./nav.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  // THE SESSION READ IS REFUSED, so what the reader holds is the viewer's.
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response("{}", { status: 404 })),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function mount(service: Service, path: string[], services: OrgServices, grants: string[]): void {
  const store = new Store();
  store.applyOrg(withDerived({ name: "Acme", roles: [], services }));
  const socket = new LiveSocket(store);
  const viewer = { login: "lee", owner: "lee", handle: "lee", grants, acts: [] };
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    what === "viewer" ? Promise.resolve(viewer) : new Promise(() => {});
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <SessionReading>
        <ViewerProvider>
          <Router>
            <ServiceNotice service={service} path={path} />
          </Router>
        </ViewerProvider>
      </SessionReading>
    </ClientContext.Provider>,
  );
}

const JIRA: OrgServices = { tracker: "jira", knowledge: "native" };

// INTEGRATIONS IS AN ADMINISTRATOR'S PAGE: a reader holding only Tracker who
// followed a link to Work was sent there and refused. They are told in place,
// and offered no link they could not follow. Mutation: drop the access check
// and the first case finds a link.
test("a reader who cannot open Integrations is told in place and offered no link", async () => {
  mount("tracker", ["work"], JIRA, ["tracker"]);
  expect(await screen.findByText("Jira handles this company's work")).toBeTruthy();
  expect(screen.getByText("Work is part of Crewlet Works, which is not in use here.")).toBeTruthy();
  // INSIDE `act`, so every render the reads cause has landed before the absence is read.
  await act(async () => {
    await new Promise((r) => setTimeout(r, 0));
  });
  expect(screen.queryByRole("link")).toBeNull();
});

test("an administrator is offered the services built into Crewlet", async () => {
  mount("tracker", ["me"], JIRA, ["admin"]);
  const link = await screen.findByRole("link", { name: "See what is built into Crewlet" });
  expect(link.getAttribute("href")).toBe("#/company/integrations");
});

// WHAT THE READER ASKED FOR IS NAMED: a New design address says why design is
// unavailable, not only that Jira handles work.
test("each hidden place says why it is not here", () => {
  expect(noticeText("tracker", ["artifacts", "new", "design"])).toMatch(
    /^A new design is made by an agent working a task/,
  );
  expect(noticeText("tracker", ["me"])).toBe(
    "My work is part of Crewlet Works, which is not in use here.",
  );
  expect(noticeText("knowledge", ["knowledge"])).toMatch(/Memory tab/);
  expect(noticeTitle("tracker", { tracker: "none", knowledge: "native" })).toBe(
    "This company runs no work tracker",
  );
  expect(noticeTitle("knowledge", { tracker: "native", knowledge: "confluence" })).toBe(
    "Confluence holds this company's knowledge",
  );
});
