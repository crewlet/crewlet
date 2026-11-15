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
import {
  ServiceNotice,
  noticeText,
  noticeTitle,
  restartText,
  restartTitle,
} from "./ServiceNotice.tsx";
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

const JIRA: OrgServices = { tracker: "jira", knowledge: "doclet", chat: "chatlet", code: "gitlet" };

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
    { tracker: "worklet", knowledge: "confluence", chat: "chatlet", code: "gitlet" },
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
    { tracker: "worklet", knowledge: "doclet", chat: "vendor", code: "gitlet" },
    ["admin"],
  );
  const link = await screen.findByRole("link", { name: "See what is built into Crewlet" });
  expect(link.getAttribute("href")).toBe("#/company/integrations/chat");
});

// GITLET'S STEP IS THE ONE THAT TURNS IT ON: nothing runs in its place for
// its Integrations page to name, so an admin is sent to Configuration, where
// the company document turns it on, and anybody else is told who can, in
// product words rather than a config key. Mutation: offer Gitlet's
// Integrations page, name the key, or offer a reader without access the
// link, and this fails.
test("a company hosting no code is told so in place, with the step a reader can take", async () => {
  mount(
    "code",
    ["gitlet", "pushes"],
    { tracker: "worklet", knowledge: "doclet", chat: "chatlet", code: "none" },
    ["admin"],
  );
  expect(
    await screen.findByText("Crewlet is not hosting git repositories for this company"),
  ).toBeTruthy();
  expect(screen.queryByText(/code\.backend/)).toBeNull();
  expect(screen.queryByRole("link", { name: "See what is built into Crewlet" })).toBeNull();
  expect(
    (await screen.findByRole("link", { name: "Open Configuration" })).getAttribute("href"),
  ).toBe("#/settings/config");
  cleanup();

  mount(
    "code",
    ["gitlet"],
    { tracker: "worklet", knowledge: "doclet", chat: "chatlet", code: "none" },
    [],
  );
  expect(await screen.findByText("Ask an admin to turn on Gitlet.")).toBeTruthy();
  expect(screen.queryByRole("link", { name: "Open Configuration" })).toBeNull();
});

// WHAT THE READER ASKED FOR IS NAMED: a New design address says why design is
// unavailable, not only that Jira handles work.
test("each hidden place says why it is not here", () => {
  expect(noticeText("tracker", ["artifacts", "new", "design"])).toMatch(
    /^A new design is made by an agent working a task/,
  );
  // A PLACE ONLY SOME SECTIONS OF WHICH READ WORKLET IS NAMED BY THE SECTION
  // ITS ADDRESS SHOWS: My work at #/me is its Queue, and its Reviews tab is
  // Gitlet's, so the workspace as a whole is not part of Worklet.
  expect(noticeText("tracker", ["me"])).toBe("Queue is part of Worklet, which is not in use here.");
  // Settings likewise is not part of Worklet, only its Expensive tasks.
  expect(noticeText("tracker", ["settings", "spend", "tasks"])).toBe(
    "Expensive tasks is part of Worklet, which is not in use here.",
  );
  expect(noticeText("knowledge", ["doclet"])).toMatch(/^Doclet is not in use here\..*Memory tab/);
  expect(
    noticeTitle("tracker", {
      tracker: "none",
      knowledge: "doclet",
      chat: "chatlet",
      code: "gitlet",
    }),
  ).toBe("This company runs no work tracker");
  expect(
    noticeTitle("knowledge", {
      tracker: "worklet",
      knowledge: "confluence",
      chat: "chatlet",
      code: "gitlet",
    }),
  ).toBe("Confluence holds this company's knowledge");
  expect(
    noticeTitle("code", { tracker: "jira", knowledge: "confluence", chat: "none", code: "none" }),
  ).toBe("Crewlet is not hosting git repositories for this company");
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
    code: "gitlet",
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

// A SERVICE THE COMPANY RUNS AND THIS NODE HAS NOT STARTED says so: the company
// turned it on while the node was running, the node starts it when it
// restarts, and a reader who may open the nodes' page is sent there while
// anybody else is told who restarts it. Never "not in use here", which is the
// opposite of what is true, and never the Integrations link, which would
// offer to turn on what is already on. Mutation: drop the restart arm of
// `ServiceNotice` and the company's own Chatlet reads "This company runs no
// chat"; offer the nodes' link to a reader without Admin and the second case
// finds it.
test("a service this node starts on its restart says so, with the step a reader can take", async () => {
  const waiting: OrgServices = {
    tracker: "worklet",
    knowledge: "doclet",
    chat: "chatlet",
    code: "none",
    restart: ["chat"],
  };
  mount("chat", ["chatlet", "activity"], waiting, ["admin"]);
  expect(await screen.findByText("Chatlet starts when this node restarts")).toBeTruthy();
  expect(screen.getByText(restartText("chat"))).toBeTruthy();
  expect(screen.queryByText("Chatlet is not in use here.")).toBeNull();
  expect(screen.queryByRole("link", { name: "See what is built into Crewlet" })).toBeNull();
  expect(
    (await screen.findByRole("link", { name: "See the engine’s nodes" })).getAttribute("href"),
  ).toBe("#/settings/nodes");
  cleanup();

  mount("chat", ["chatlet", "activity"], waiting, ["chatlet"]);
  expect(await screen.findByText("Ask whoever runs the engine to restart it.")).toBeTruthy();
  expect(screen.queryByRole("link", { name: "See the engine’s nodes" })).toBeNull();
});

// EVERY SERVICE HAS THE SENTENCE, in its own name: a tracker, a knowledge base
// or a git host turned on live waits for the same restart.
test("the restart notice names the service it is about", () => {
  expect(restartTitle("tracker")).toBe("Worklet starts when this node restarts");
  expect(restartTitle("code")).toBe("Gitlet starts when this node restarts");
  expect(restartText("knowledge")).toMatch(/turned Doclet on while this node was running/);
  expect(restartText("knowledge")).toMatch(/Restart the engine on each node/);
});
