/**
 * What an address under a service the company does not run draws: a notice
 * in place that every signed-in reader can read, naming what runs instead,
 * and offering Company › Integrations only to a reader who may open it.
 */

import { cleanup, render, screen } from "@testing-library/react";
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

const JIRA: OrgServices = { tracker: "jira", knowledge: "doclet" };

// THE SERVICES' PAGES ARE EVERY READER'S: a reader holding only Worklet who
// followed a link to Worklet is told in place and offered the page about the
// service that is off, not the directory's front. Mutation: gate the link on
// Admin again and the first case finds none.
test("a reader without Admin is told in place and offered the service's own page", async () => {
  mount("tracker", ["worklet"], JIRA, ["worklet"]);
  expect(await screen.findByText("Jira handles this company's work")).toBeTruthy();
  expect(screen.getByText("Worklet is not in use here.")).toBeTruthy();
  const link = await screen.findByRole("link", { name: "See what is built into Crewlet" });
  expect(link.getAttribute("href")).toBe("#/company/integrations/worklet");
});

test("a hidden knowledge base offers Doclet's page", async () => {
  mount("knowledge", ["doclet"], { tracker: "worklet", knowledge: "confluence" }, ["admin"]);
  const link = await screen.findByRole("link", { name: "See what is built into Crewlet" });
  expect(link.getAttribute("href")).toBe("#/company/integrations/doclet");
});

// WHAT THE READER ASKED FOR IS NAMED: a New design address says why design is
// unavailable, not only that Jira handles work.
test("each hidden place says why it is not here", () => {
  expect(noticeText("tracker", ["artifacts", "new", "design"])).toMatch(
    /^A new design is made by an agent working a task/,
  );
  expect(noticeText("tracker", ["me"])).toBe(
    "My work is part of Worklet, which is not in use here.",
  );
  expect(noticeText("knowledge", ["doclet"])).toMatch(/^Doclet is not in use here\..*Memory tab/);
  expect(noticeTitle("tracker", { tracker: "none", knowledge: "doclet" })).toBe(
    "This company runs no work tracker",
  );
  expect(noticeTitle("knowledge", { tracker: "worklet", knowledge: "confluence" })).toBe(
    "Confluence holds this company's knowledge",
  );
});
