/**
 * Company › Integrations' first row: the services built into Crewlet, each
 * saying what the company runs from the engine's own word, and nothing a
 * service the company does not run would have to answer.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";
import { BuiltIn } from "./BuiltIn.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type { OrgServices } from "~/contract/services.ts";
import { withDerived } from "~/test/org.ts";

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
  location.hash = "#/company/integrations";
});

afterEach(() => {
  cleanup();
  location.hash = "#/";
});

const ADMIN = { login: "ana", owner: "ana", handle: "ana", grants: ["admin"], acts: [] };

/** Mount the cards as an admin of a company running `services`; returns what was asked. */
function mount(services: OrgServices | undefined) {
  const store = new Store();
  store.applyOrg(withDerived({ name: "Acme", roles: [], ...(services ? { services } : {}) }));
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    asked.push(what);
    if (what === "viewer") return Promise.resolve(ADMIN);
    if (what === "work_projects") return Promise.resolve({ projects: [], total: 4, census: {} });
    if (what === "containers") return Promise.resolve({ containers: [{ key: "ENG" }] });
    return new Promise(() => {});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>
          <BuiltIn />
        </Router>
      </ViewerProvider>
    </ClientContext.Provider>,
  );
  return asked;
}

const card = (name: string) => screen.getByRole("article", { name }) as HTMLElement;

test("a company running Crewlet's own says so, with what each holds and the way in", async () => {
  mount({ tracker: "native", knowledge: "native" });
  expect(await screen.findByText("4 projects")).toBeTruthy();
  expect(await screen.findByText("1 space")).toBeTruthy();
  const works = card("Crewlet Works");
  expect(within(works).getByText("On")).toBeTruthy();
  expect(within(works).getByRole("link", { name: "Open Work" }).getAttribute("href")).toBe(
    "#/work",
  );
  expect(
    within(card("Crewlet Knowledge")).getByRole("link", { name: "Open Knowledge" }),
  ).toBeTruthy();
  expect(within(card("Crewlet Chat")).getByText("Coming soon")).toBeTruthy();
});

// WHAT RUNS INSTEAD, IN THE ENGINE'S WORD, and nothing asked of a service the
// company does not run. Mutation: ask the counts whatever runs, and the first
// list names them.
test("a company on Jira and Confluence is told what runs instead, and asked nothing of them", async () => {
  const asked = mount({ tracker: "jira", knowledge: "confluence" });
  expect(await screen.findByText("Jira handles work items.")).toBeTruthy();
  expect(screen.getByText("Confluence holds the knowledge base.")).toBeTruthy();
  expect(within(card("Crewlet Works")).getByText("Not in use")).toBeTruthy();
  expect(within(card("Crewlet Works")).queryByRole("link")).toBeNull();
  expect(asked.filter((q) => q === "work_projects" || q === "containers")).toEqual([]);
});

test("a company running neither is told it runs no tracker and no knowledge base", async () => {
  mount({ tracker: "none", knowledge: "none" });
  expect(await screen.findByText("The company runs no work tracker.")).toBeTruthy();
  expect(screen.getByText("The company runs no knowledge base.")).toBeTruthy();
  expect(within(card("Crewlet Knowledge")).getByText("Off")).toBeTruthy();
});

// THE PROJECTION NAMES NO SERVICES only on a node running no company, and the
// cards say so rather than guessing either way.
test("a node running no company says so on both cards", async () => {
  mount(undefined);
  expect((await screen.findAllByText("This node runs no company yet.")).length).toBe(2);
});

// A TILE WITH NO WAY IN DRAWS NO FOOTER: a rule with nothing under it read as
// a missing control, beside siblings whose footer holds a count and a button.
// And Coming soon wears Not in use's neutral outline, one look for one state.
test("a tile with no way in draws no footer, and Coming soon matches Not in use", async () => {
  mount({ tracker: "jira", knowledge: "native" });
  expect(await screen.findByText("1 space")).toBeTruthy();
  expect(card("Crewlet Works").querySelector(".int-tile-foot")).toBeNull();
  expect(card("Crewlet Chat").querySelector(".int-tile-foot")).toBeNull();
  expect(card("Crewlet Knowledge").querySelector(".int-tile-foot")).not.toBeNull();
  const tagClass = (name: string, word: string) =>
    within(card(name)).getByText(word).closest(".crewlet-tag")?.className ?? "";
  expect(tagClass("Crewlet Chat", "Coming soon")).toMatch(/outline/);
  expect(tagClass("Crewlet Works", "Not in use")).toMatch(/outline/);
});
