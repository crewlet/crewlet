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

const JIRA: OrgServices = { tracker: "jira", knowledge: "doclet", chat: "chatlet", code: "native" };

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
  mount(
    "knowledge",
    ["doclet"],
    { tracker: "worklet", knowledge: "confluence", chat: "chatlet", code: "native" },
    ["admin"],
  );
  const link = await screen.findByRole("link", { name: "See what is built into Crewlet" });
  expect(link.getAttribute("href")).toBe("#/company/integrations/doclet");
});

// EACH SERVICE OPENS ITS OWN LINE. Mutation: send every service but the
// tracker to Doclet's line, as the link did before chat was a service,
// and a company talking on Slack is shown Doclet.
test("a chat run elsewhere offers Chatlet's page", async () => {
  mount(
    "chat",
    ["chatlet"],
    { tracker: "worklet", knowledge: "doclet", chat: "vendor", code: "native" },
    ["admin"],
  );
  const link = await screen.findByRole("link", { name: "See what is built into Crewlet" });
  expect(link.getAttribute("href")).toBe("#/company/integrations/chat");
});

// THE CODE HOST HAS NO CARD UNDER INTEGRATIONS: it is set in the company
// document, which the sentence names, and a link to a page about something
// else would send the reader looking there. Mutation: give the code host a
// card, and the link below is drawn.
test("a company hosting no code is told so in place, and offered no card", async () => {
  mount(
    "code",
    ["code", "pushes"],
    { tracker: "worklet", knowledge: "doclet", chat: "chatlet", code: "none" },
    ["admin"],
  );
  expect(await screen.findByText("This company does not host its code here")).toBeTruthy();
  expect(screen.getByText(/does not set code\.native/)).toBeTruthy();
  expect(screen.queryByRole("link", { name: "See what is built into Crewlet" })).toBeNull();
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
  // A PLACE ONLY ONE SECTION OF WHICH READS WORKLET IS NAMED BY THAT SECTION,
  // never by its workspace: Settings is not part of Worklet.
  expect(noticeText("tracker", ["settings", "spend", "tasks"])).toBe(
    "Expensive tasks is part of Worklet, which is not in use here.",
  );
  expect(noticeText("knowledge", ["doclet"])).toMatch(/^Doclet is not in use here\..*Memory tab/);
  expect(
    noticeTitle("tracker", {
      tracker: "none",
      knowledge: "doclet",
      chat: "chatlet",
      code: "native",
    }),
  ).toBe("This company runs no work tracker");
  expect(
    noticeTitle("knowledge", {
      tracker: "worklet",
      knowledge: "confluence",
      chat: "chatlet",
      code: "native",
    }),
  ).toBe("Confluence holds this company's knowledge");
  expect(
    noticeTitle("code", { tracker: "jira", knowledge: "confluence", chat: "none", code: "none" }),
  ).toBe("This company does not host its code here");
});

// CHATLET IS A CREWLET SERVICE LIKE THE OTHER TWO: an address under it, on a
// company talking on a vendor's chat or on none, says where the company talks.
// The vendor is named to a reader the viewer answer names it to, one holding
// Chatlet, and called a vendor's to anybody else. Mutation: drop the chat arm of
// `noticeTitle` and every case here fails.
test("an address under Chatlet names where the company talks instead", async () => {
  const slack: OrgServices = {
    tracker: "worklet",
    knowledge: "doclet",
    chat: "vendor",
    code: "native",
  };
  mount("chat", ["chatlet"], slack, ["chatlet"], ["slack"]);
  expect(await screen.findByText("This company talks on Slack")).toBeTruthy();
  expect(screen.getByText("Chatlet is not in use here.")).toBeTruthy();
  expect(noticeTitle("chat", slack)).toBe("This company talks on a vendor's chat");
  expect(noticeTitle("chat", slack, ["mattermost", "slack"])).toBe(
    "This company talks on Mattermost and Slack",
  );
  expect(noticeTitle("chat", { ...slack, chat: "none" })).toBe("This company runs no chat");
});
