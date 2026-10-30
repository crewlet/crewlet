/**
 * Company › Integrations as a reader meets it: one directory grouped by need,
 * Crewlet's own service first in each group, beside the page of the one
 * chosen.
 *
 * The derivation behind each line is held in `model.test.tsx` and the
 * catalogue's readings in `catalogue.test.tsx`. What only a render can hold is
 * how they meet: which line an address selects, what each kind of reader is
 * shown and, as importantly, never asked, and where the one control a tool
 * offers leads.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { LayerHost } from "@crewlethq/ui";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { Router, useRoute } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store, sessionRestored } from "~/protocol/index.ts";
import type { IntegrationsAnswer } from "~/contract/integrations.ts";
import type { OrgServices } from "~/contract/services.ts";
import type { SetupListing, SetupToolState } from "~/protocol/types.ts";
import { withDerived } from "~/test/org.ts";
import { Integrations } from "./Directory.tsx";
import { CONNECT_HINT, DESCRIPTION } from "./home.ts";

class InertWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  readyState = InertWebSocket.CONNECTING;
  send(): void {}
  close(): void {}
}

const expiring = "The GitLab admin token expires in 3 days.";

/**
 * GitLab connected with its admin token about to lapse, Datadog connected and
 * working, Confluence connected without Jira, and everything else not in use.
 */
const answer: IntegrationsAnswer = {
  integrations: [
    {
      key: "gitlab",
      configured: true,
      inbound: 4,
      reconcile: {
        phase: "ready",
        findings: [
          {
            kind: "credential_expiring",
            subject: "admin_token",
            detail: expiring,
            expires_at: "2026-10-02T00:00:00Z",
            phase: "degraded",
          },
        ],
      },
    },
    { key: "datadog", configured: true, inbound: 0, reconcile: { phase: "ready" } },
    { key: "confluence", configured: true, inbound: 2, reconcile: { phase: "ready" } },
  ],
  tools: [
    {
      key: "gitlab",
      surfaces: ["gitlab"],
      state: "attention",
      label: "Credential expiring",
      reason: expiring,
      surface: "gitlab",
    },
    { key: "datadog", surfaces: ["datadog"], state: "connected", label: "Connected", reason: "" },
    {
      key: "atlassian",
      surfaces: ["confluence"],
      state: "connected",
      label: "Connected",
      reason: "",
    },
    ...["slack", "mattermost", "github"].map((key) => ({
      key,
      surfaces: [key],
      state: "not_in_use" as const,
      label: "Not in use",
      reason: "",
    })),
  ],
  traffic_known: true,
  traffic_since: null,
};

function tool(key: string, configured = false): SetupToolState {
  return {
    key,
    configured,
    enabled: configured,
    satisfied: configured,
    form_complete: configured,
    requirements: [
      {
        field: "admin_token",
        label: "Admin token",
        kind: "secret",
        config_path: `integrations.${key}.admin_token`,
        required: true,
        present: configured,
        resolved: configured,
      },
    ],
  };
}

const listing = {
  tools: [
    { ...tool("gitlab", true), can_provision: true },
    tool("datadog", true),
    tool("confluence", true),
    ...["slack", "mattermost", "atlassian", "jira", "github"].map((k) => tool(k)),
  ],
  external_url: { value: "https://engine.example.com", config_path: "api.external_url" },
} as unknown as SetupListing;

const ADMIN = { login: "ana", owner: "ana", handle: "ana", grants: ["admin"], acts: [] };
const MEMBER = {
  login: "lee",
  owner: "lee",
  handle: "lee",
  grants: ["worklet", "doclet"],
  acts: [],
};
const NOBODY = { login: "", owner: "", grants: [], acts: [] };

const BUILT_IN: OrgServices = { tracker: "worklet", knowledge: "doclet" };

interface Mounted {
  /** Every socket question the page asked. */
  asked: string[];
  /** Every REST path the page fetched, with its method. */
  fetched: string[];
}

/**
 * The selection taken from the address, as App.tsx renders the page: one
 * page, never keyed on the selection, so a move between selections is a
 * change of address under a mounted page rather than a fresh mount.
 */
const ROUTED = Symbol("routed");

function RoutedIntegrations() {
  const { path } = useRoute();
  return <Integrations kind={path[2]} />;
}

/** The page as `viewer`, on a company running `services`, at `kind`. */
function mount(
  viewer: unknown,
  kind?: string | typeof ROUTED,
  /** What the company runs, or null for a node running no company. */
  services: OrgServices | null = BUILT_IN,
  setup: { status: number; body: unknown } = { status: 200, body: listing },
  /** The company document the `config` question answers, or none at all. */
  config?: unknown,
): Mounted {
  const fetched: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = new URL(String(input), "http://engine.test").pathname;
      const method = (init?.method ?? "GET").toUpperCase();
      fetched.push(method === "GET" ? path : `${method} ${path}`);
      if (path === "/setup/integrations") {
        return new Response(JSON.stringify(setup.body), { status: setup.status });
      }
      // AN APP'S AGENTS, as the access route answers them: nobody on it yet.
      const access = /^\/setup\/integrations\/([^/]+)\/access$/.exec(path);
      if (access && method === "GET") {
        return new Response(
          JSON.stringify({
            kind: access[1],
            revision_id: "rev-1",
            syncing: false,
            lanes: [],
            seats: [],
          }),
          { status: 200 },
        );
      }
      return new Response(JSON.stringify({}), { status: 200 });
    }),
  );
  const store = new Store();
  store.applyOrg(withDerived({ name: "Acme", roles: [], ...(services ? { services } : {}) }));
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    asked.push(what);
    if (what === "viewer") return Promise.resolve(viewer);
    if (what === "integrations") return Promise.resolve(answer);
    if (what === "config" && config !== undefined) return Promise.resolve(config);
    if (what === "work_projects") {
      return Promise.resolve({ projects: [], total: 0, census: { active: 0 } });
    }
    return new Promise(() => {});
  };
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <ViewerProvider>
        <Router>{kind === ROUTED ? <RoutedIntegrations /> : <Integrations kind={kind} />}</Router>
      </ViewerProvider>
    </ClientContext.Provider>,
    { wrapper: LayerHost },
  );
  return { asked, fetched };
}

/** The directory's line for `name`. */
async function line(name: RegExp | string): Promise<HTMLElement> {
  const nav = await screen.findByRole("navigation", { name: "Integrations" });
  return within(nav).getByRole("link", { name });
}

beforeEach(() => {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = "#/company/integrations";
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  sessionRestored();
  location.hash = "#/";
});

// GROUPED BY NEED, CREWLET'S OWN FIRST: the built-in answer to a need opens
// its group, beside the apps that answer it. ATLASSIAN IS LISTED BY PRODUCT,
// one line under work tracking and one under docs, because it is one
// connection that serves two needs. A PRODUCT NAMES ITS MAKER beside it:
// Worklet and Doclet name Crewlet as Jira and Confluence name Atlassian, and
// Crewlet Chat, whose name already says it, names nobody. Mutation: drop the
// services' vendor and the two lines lose "Crewlet".
test("the directory groups by need, with Crewlet's own service first in each", async () => {
  mount(ADMIN);
  const nav = await screen.findByRole("navigation", { name: "Integrations" });
  const groups = within(nav)
    .getAllByRole("heading")
    .map((h) => {
      const list = h.parentElement!.querySelector("ul")!;
      return [
        h.textContent,
        within(list)
          .getAllByRole("link")
          .map((a) => {
            const label = a.querySelector(".crewlet-directory__label")!.textContent;
            const hint = a.querySelector(".crewlet-directory__hint")?.textContent;
            return hint ? `${label} (${hint})` : label;
          }),
      ];
    });
  expect(groups).toEqual([
    ["Chat", ["Crewlet Chat", "Slack", "Mattermost"]],
    ["Work tracking", ["Worklet (Crewlet)", "Jira (Atlassian)"]],
    ["Docs and knowledge", ["Doclet (Crewlet)", "Confluence (Atlassian)"]],
    ["Code", ["GitHub", "GitLab"]],
    ["Observability", ["Datadog"]],
  ]);
});

// A PRODUCT'S STATE IS ITS OWN: Confluence is connected and Jira is not, so
// the Jira line says nothing, and an app with no state says nothing rather
// than "Not connected" on every line.
test("each line carries its own state, and a product only its own", async () => {
  mount(ADMIN);
  expect((await line(/Confluence/)).textContent).toContain("Connected");
  expect((await line(/^Jira/)).textContent).not.toContain("Connected");
  expect((await line(/GitLab/)).textContent).toContain("Needs attention");
  expect((await line(/Slack/)).textContent).toBe("Slack");
  expect((await line(/Worklet/)).textContent).toContain("In use");
  expect((await line(/Crewlet Chat/)).textContent).toContain("Soon");
});

// THE OLD ADDRESSES STILL WORK, AS THE SELECTION: an app's key, a surface
// (the relay and Atlassian select Jira's line) or a service.
test("an address selects its line, Atlassian and its relay on Jira's", async () => {
  for (const [kind, name] of [
    ["gitlab", /GitLab/],
    ["atlassian", /^Jira/],
    ["forge", /^Jira/],
    ["confluence", /Confluence/],
    ["worklet", /^Worklet/],
  ] as const) {
    mount(ADMIN, kind);
    expect((await line(name)).getAttribute("aria-current"), kind).toBe("page");
    cleanup();
  }
});

// WITH NOTHING CHOSEN, WHAT NEEDS A LOOK: each issue under its app's name,
// sending the reader to the app's page rather than fixing it out of context.
test("the summary lists what needs a look and links to each app", async () => {
  mount(ADMIN);
  expect(await screen.findByText(expiring)).toBeTruthy();
  const open = screen.getByRole("link", { name: "Open GitLab" });
  expect(open.getAttribute("href")).toBe("#/company/integrations/gitlab?open=rotate");
});

// THE HOME IS WHAT IS IN USE: the connected apps (Confluence as its own
// product) and the services the company runs, each a compact card with its
// state and one action; an app nobody connected is the list's alone.
// Mutation: list every catalogue entry and Slack appears.
test("the home shows only what is in use, each a compact card with one action", async () => {
  mount(ADMIN);
  const inUse = await screen.findByRole("region", { name: /^In use/ });
  const names = [...inUse.querySelectorAll(".int-tile-link")].map((a) => a.textContent);
  expect(names).toEqual(["Worklet", "Doclet", "Confluence", "GitLab", "Datadog"]);
  for (const card of inUse.querySelectorAll(".crewlet-catalog-tile")) {
    expect(card.className).toContain("crewlet-catalog-tile--compact");
  }
  const gitlab = within(inUse).getByRole("link", { name: "GitLab" }).closest("li") as HTMLElement;
  expect(
    within(gitlab).getByRole("link", { name: "Replace key GitLab" }).getAttribute("href"),
  ).toBe("#/company/integrations/gitlab?open=rotate");
  // THE HEADING DESCRIBES THE PAGE, and the count beside In use is of every
  // card under it, the services included.
  expect(screen.getByText(DESCRIPTION)).toBeTruthy();
  expect(within(inUse).getAllByRole("heading")[0]!.textContent).toBe("In use 5");
});

// EVERY CARD'S ACTION NAMES ITS CARD: a reader listing the links in In use
// meets "Open Worklet" and "Replace key GitLab", never a column of
// "Open". Mutation: drop the action's aria-label and the names repeat.
test("each card's action is named for its app, and no two share a name", async () => {
  mount(ADMIN);
  const inUse = await screen.findByRole("region", { name: /^In use/ });
  const cards = [...inUse.querySelectorAll(".crewlet-catalog-tile")] as HTMLElement[];
  const names = cards.map((card) => {
    const title = card.querySelector(".int-tile-link")!.textContent!;
    const links = within(card).getAllByRole("link");
    const action = links[links.length - 1]!;
    const name = action.getAttribute("aria-label") ?? "";
    expect(name.startsWith(action.textContent!.trim()), name).toBe(true);
    expect(name.endsWith(title), name).toBe(true);
    return name;
  });
  expect(new Set(names).size).toBe(names.length);
});

// A FIX LANDS WITH ITS FORM OPEN, read once and taken out of the address.
test("an arrival naming a form opens it, and leaves the address", async () => {
  location.hash = "#/company/integrations/gitlab?open=rotate";
  mount(ADMIN, "gitlab");
  expect(await screen.findByRole("form", { name: "Replace the GitLab key" })).toBeTruthy();
  await waitFor(() => expect(location.hash).not.toContain("open="));
});

// A FIX FOLLOWED FROM THE HOME OPENS ITS FORM: the page stays mounted while
// the address moves, so the intent is read with the selection it arrived on
// rather than once when the page first mounted. Mutation: read the intent
// once at mount and the form never opens.
test("a fix followed from the home opens its form on the app's page", async () => {
  mount(ADMIN, ROUTED);
  const inUse = await screen.findByRole("region", { name: /^In use/ });
  const gitlab = within(inUse).getByRole("link", { name: "GitLab" }).closest("li") as HTMLElement;
  location.hash = within(gitlab)
    .getByRole("link", { name: /^Replace key/ })
    .getAttribute("href")!;
  expect(await screen.findByRole("form", { name: "Replace the GitLab key" })).toBeTruthy();
  await waitFor(() => expect(location.hash).not.toContain("open="));
});

// AN ARRIVAL IS SPENT ON ITS OWN SELECTION: leaving for another app and
// coming back through the list shows the app's page, not the form again.
// Mutation: keep the intent across selections and the form reopens.
test("coming back to an app through the list does not reopen the arrival's form", async () => {
  location.hash = "#/company/integrations/gitlab?open=rotate";
  mount(ADMIN, ROUTED);
  expect(await screen.findByRole("form", { name: "Replace the GitLab key" })).toBeTruthy();
  await waitFor(() => expect(location.hash).not.toContain("open="));
  location.hash = (await line(/GitHub/)).getAttribute("href")!;
  expect(await screen.findByRole("heading", { level: 2, name: "GitHub" })).toBeTruthy();
  location.hash = (await line(/GitLab/)).getAttribute("href")!;
  expect(await screen.findByRole("heading", { level: 2, name: "GitLab" })).toBeTruthy();
  expect(screen.getByRole("tab", { name: /Agents/ })).toBeTruthy();
  expect(screen.queryByRole("form", { name: "Replace the GitLab key" })).toBeNull();
});

// A HOME WITH NOTHING IN USE says so and points at the list.
test("a company with nothing in use is told to pick from the list", async () => {
  mount(MEMBER, undefined, { tracker: "none", knowledge: "none" });
  expect(await screen.findByText("Nothing is in use yet")).toBeTruthy();
  expect(screen.queryByRole("region", { name: /^In use/ })).toBeNull();
});

// AN APP'S PAGE: its header, its tabs, and the one action the engine's state
// asks for. Replace key opens the settings form in place of the tabs, saying
// why, with the directory still beside it.
test("an app's page offers Replace key, which opens its form in the pane", async () => {
  mount(ADMIN, "gitlab");
  expect(await screen.findByRole("heading", { level: 2, name: "GitLab" })).toBeTruthy();
  expect(screen.getByRole("tab", { name: /Agents/, selected: true })).toBeTruthy();
  fireEvent.click(await screen.findByRole("button", { name: "Replace key GitLab" }));
  expect(await screen.findByRole("form", { name: "Replace the GitLab key" })).toBeTruthy();
  expect(screen.queryByRole("tab", { name: /Agents/ })).toBeNull();
  expect(await line(/GitLab/)).toBeTruthy();
});

// CHECK NOW SITS AT THE FAR END OF THE TAB ROW, beside the tabs and never
// among them, on every tab of a connected app, and asks the loop to look.
test("Check now sits beside the tabs on every tab, and asks the loop to look", async () => {
  const { fetched } = mount(ADMIN, "gitlab");
  const tabs = await screen.findByRole("tablist", { name: "GitLab sections" });
  const check = await screen.findByRole("button", { name: "Check now" });
  expect(tabs.contains(check)).toBe(false);
  fireEvent.click(screen.getByRole("tab", { name: /Activity/ }));
  expect(screen.getByRole("button", { name: "Check now" })).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Check now" }));
  await waitFor(() => expect(fetched).toContain("POST /setup/integrations/gitlab/check"));
});

// THE ADDRESS APPS DELIVER TO, said once at the foot of the home rather than
// under the list, with a copy control named for what it copies.
test("the engine's address sits once at the foot of the home, copyable", async () => {
  mount(ADMIN);
  const nav = await screen.findByRole("navigation", { name: "Integrations" });
  await waitFor(() => expect(document.querySelector(".int-landing-foot")).not.toBeNull());
  expect(within(nav).queryByText("Apps reach this engine at")).toBeNull();
  expect(document.querySelectorAll(".int-engine-address")).toHaveLength(1);
  const foot = document.querySelector(".int-landing-foot") as HTMLElement;
  await waitFor(() =>
    expect(
      within(foot).getByRole("button", {
        name: "Copy the engine's address, https://engine.example.com",
      }),
    ).toBeTruthy(),
  );
});

// A MANAGED DOCUMENT HOLDS WHAT WRITES IT and keeps a key's rotation, which
// seals a value the document already names.
test("a managed document holds Connect and keeps Replace key", async () => {
  const managed = { ...ADMIN, config_writer: false, config_managed_by: ["gitops"] };
  mount(managed, "github");
  // THE PAGE SAYS IT ONCE, and each held control carries the same sentence.
  expect(
    (await screen.findAllByText(/Managed by gitops\. Change it there\./)).some(
      (el) => el.closest(".crewlet-callout") !== null,
    ),
  ).toBe(true);
  const connect = await screen.findByRole("button", { name: "Connect GitHub" });
  await waitFor(() => expect(connect.getAttribute("aria-disabled")).toBe("true"));
  fireEvent.click(connect);
  expect(screen.queryByRole("form")).toBeNull();
  cleanup();

  mount(managed, "gitlab");
  fireEvent.click(await screen.findByRole("button", { name: "Replace key GitLab" }));
  expect(await screen.findByRole("form", { name: "Replace the GitLab key" })).toBeTruthy();
});

// AN ADMIN THE ENGINE REFUSES `/setup` is told what it takes, and every form
// action is held with that reason rather than removed.
test("a refused setup listing names the access it takes and holds every form action", async () => {
  mount(ADMIN, "github", BUILT_IN, {
    status: 403,
    body: { error: "unauthorized", reason: "no_grant", grants: ["admin"] },
  });
  expect(await screen.findByText(/Reading the integrations' setup state needs Admin/)).toBeTruthy();
  const connect = await screen.findByRole("button", { name: "Connect GitHub" });
  expect(connect.getAttribute("aria-disabled")).toBe("true");
});

// --- a reader without Admin ---------------------------------------------------- //

// THE PAGE IS EVERY READER'S AND ASKS NOTHING AN ADMIN'S: the services are the
// overview every reader holds, the apps carry no state, and neither the
// `integrations` question nor `/setup` is read. Mutation: build the model for
// every reader and the first list names `integrations`.
test("a reader without Admin is shown the services and asked nothing an Admin's", async () => {
  const { asked, fetched } = mount(MEMBER);
  expect((await line(/Worklet/)).textContent).toContain("In use");
  expect((await line(/GitLab/)).textContent).toBe("GitLab");
  // THE SAME DESCRIPTION, and no hint to connect what an Admin connects.
  expect(screen.getByText(DESCRIPTION)).toBeTruthy();
  expect(screen.queryByText(CONNECT_HINT)).toBeNull();
  expect(screen.getByText("Admins connect and manage apps")).toBeTruthy();
  expect(asked).not.toContain("integrations");
  expect(fetched).not.toContain("/setup/integrations");
  // AND NO ADDRESS: it is the setup listing's, which an Admin reads.
  expect(screen.queryByText("Apps reach this engine at")).toBeNull();
});

test("a reader without Admin is told on an app's page who manages it, and offered nothing", async () => {
  mount(MEMBER, "github");
  expect(await screen.findByRole("heading", { level: 2, name: "GitHub" })).toBeTruthy();
  expect(screen.getByText("Admins connect and manage apps")).toBeTruthy();
  expect(screen.queryByRole("button", { name: /Connect/ })).toBeNull();
  expect(screen.queryByRole("button", { name: "Sign in" })).toBeNull();
});

// THE DOOR FOR A READER WHO IS NOBODY, and none for a signed-in reader, whom
// signing in again as themselves would not change.
test("a reader nobody signed in is offered the sign-in", async () => {
  mount(NOBODY);
  fireEvent.click(await screen.findByRole("button", { name: "Sign in" }));
  expect(location.hash).toMatch(/^#\/login/);
});

// --- Crewlet's own services -------------------------------------------------------- //

// THE INBOX HORIZON IS WORKLET'S OWN POLICY, read where the company document
// keeps it (`tracker.worklet`), and an unset one is the engine's default.
// Mutation: read it off the tracker block itself and the 90 days are never
// said.
test("Worklet's settings read the inbox horizon from its own policy", async () => {
  mount(ADMIN, "worklet", BUILT_IN, undefined, {
    tracker: { backend: "worklet", worklet: { inbox_retention_days: 90 } },
  });
  fireEvent.click(await screen.findByRole("tab", { name: "Settings" }));
  expect(await screen.findByText("90 days")).toBeTruthy();
});

test("a service the company runs is in use, with its way in", async () => {
  mount(MEMBER, "worklet");
  expect(await screen.findByRole("heading", { level: 2, name: "Worklet" })).toBeTruthy();
  expect(screen.getByRole("link", { name: "Open Worklet" }).getAttribute("href")).toBe("#/worklet");
});

// WHAT RUNS INSTEAD, IN THE ENGINE'S WORD, with the way to it, and nothing
// asked of a service the company does not run. Mutation: ask the counts
// whatever runs, and the list names `work_projects`.
test("a service run elsewhere says what replaces it and asks nothing of it", async () => {
  const { asked } = mount(ADMIN, "worklet", { tracker: "jira", knowledge: "doclet" });
  expect(await screen.findByText("Jira does this for your company")).toBeTruthy();
  expect(screen.getByRole("link", { name: "Open Jira" }).getAttribute("href")).toBe(
    "#/company/integrations/jira",
  );
  expect((await line(/Worklet/)).textContent).toContain("Not in use");
  expect(asked).not.toContain("work_projects");
});

test("a service the company runs none of says so, with no product to open", async () => {
  mount(ADMIN, "doclet", { tracker: "worklet", knowledge: "none" });
  expect(await screen.findByText("Nothing does this for your company")).toBeTruthy();
  expect(screen.queryByRole("link", { name: /Open Confluence/ })).toBeNull();
});

// UNTIL THE ENGINE HAS SAID, nothing is guessed: a node running no company
// says so, and no service wears On or Off.
test("a node running no company says so and wears no state", async () => {
  mount(ADMIN, "worklet", null);
  expect(await screen.findByText("This node runs no company yet.")).toBeTruthy();
  // ITS NAME AND ITS MAKER, and nothing after them.
  const worklet = await line(/Worklet/);
  expect(worklet.textContent).toBe("WorkletCrewlet");
});

test("Crewlet Chat is coming soon, pointing at the chat apps meanwhile", async () => {
  mount(MEMBER, "chat");
  const soon = (await screen.findByText("Coming soon")).closest(".int-soon") as HTMLElement;
  expect(within(soon).getByRole("link", { name: "Slack" }).getAttribute("href")).toBe(
    "#/company/integrations/slack",
  );
});
