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

function mount(
  service: Service,
  path: string[],
  services: OrgServices,
  grants: string[],
  vendors: string[] = [],
): void {
  const store = new Store();
  store.applyOrg(withDerived({ name: "Acme", roles: [], services }));
  const socket = new LiveSocket(store);
  const viewer = {
    login: "lee",
    owner: "lee",
    handle: "lee",
    grants,
    acts: [],
    chat_vendors: vendors,
  };
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

const JIRA: OrgServices = { tracker: "jira", knowledge: "native", chat: "native" };

// THE SERVICES' PAGES ARE EVERY READER'S: a reader holding only Tracker who
// followed a link to Work is told in place and offered the page about the
// service that is off, not the directory's front. Mutation: gate the link on
// Admin again and the first case finds none.
test("a reader without Admin is told in place and offered the service's own page", async () => {
  mount("tracker", ["work"], JIRA, ["tracker"]);
  expect(await screen.findByText("Jira handles this company's work")).toBeTruthy();
  expect(screen.getByText("Work is part of Crewlet Works, which is not in use here.")).toBeTruthy();
  const link = await screen.findByRole("link", { name: "See what is built into Crewlet" });
  expect(link.getAttribute("href")).toBe("#/company/integrations/works");
});

test("a hidden knowledge base offers Crewlet Knowledge's page", async () => {
  mount(
    "knowledge",
    ["knowledge"],
    { tracker: "native", knowledge: "confluence", chat: "native" },
    ["admin"],
  );
  const link = await screen.findByRole("link", { name: "See what is built into Crewlet" });
  expect(link.getAttribute("href")).toBe("#/company/integrations/knowledge");
});

// EACH SERVICE OPENS ITS OWN LINE. Mutation: send every service but the
// tracker to Knowledge's line, as the link did before chat was a service,
// and a company talking on Slack is shown Crewlet Knowledge.
test("a chat run elsewhere offers Crewlet Chat's page", async () => {
  mount("chat", ["chat"], { tracker: "native", knowledge: "native", chat: "vendor" }, ["admin"]);
  const link = await screen.findByRole("link", { name: "See what is built into Crewlet" });
  expect(link.getAttribute("href")).toBe("#/company/integrations/chat");
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
  expect(noticeTitle("tracker", { tracker: "none", knowledge: "native", chat: "native" })).toBe(
    "This company runs no work tracker",
  );
  expect(
    noticeTitle("knowledge", { tracker: "native", knowledge: "confluence", chat: "native" }),
  ).toBe("Confluence holds this company's knowledge");
});

// CHAT IS A CREWLET SERVICE LIKE THE OTHER TWO: an address under it, on a
// company talking on a vendor's chat or on none, says where the company talks.
// The vendor is named to a reader the viewer answer names it to, one holding
// Chat, and called a vendor's to anybody else. Mutation: drop the chat arm of
// `noticeTitle` and every case here fails.
test("an address under Chat names where the company talks instead", async () => {
  const slack: OrgServices = { tracker: "native", knowledge: "native", chat: "vendor" };
  mount("chat", ["chat"], slack, ["chat"], ["slack"]);
  expect(await screen.findByText("This company talks on Slack")).toBeTruthy();
  expect(screen.getByText("Chat is part of Crewlet Chat, which is not in use here.")).toBeTruthy();
  expect(noticeTitle("chat", slack)).toBe("This company talks on a vendor's chat");
  expect(noticeTitle("chat", slack, ["mattermost", "slack"])).toBe(
    "This company talks on Mattermost and Slack",
  );
  expect(noticeTitle("chat", { ...slack, chat: "none" })).toBe("This company runs no chat");
});
