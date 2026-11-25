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

import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
import { datadogAnswer } from "./access/fixtures.ts";
import { Integrations } from "./Directory.tsx";
import { installWindow } from "~/testing.tsx";
import { settled } from "~/test/settle.ts";
import { CONNECT_HINT } from "./home.ts";

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
    ...[
      "slack",
      "mattermost",
      "github",
      "cloudflare",
      "expo",
      "digitalocean",
      "scaleway",
      "aws",
      "google_cloud",
      "ssh",
    ].map((key) => ({
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
    ...[
      "slack",
      "mattermost",
      "atlassian",
      "jira",
      "github",
      "cloudflare",
      "expo",
      "digitalocean",
      "scaleway",
      "aws",
      "google_cloud",
      "ssh",
    ].map((k) => tool(k)),
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

const BUILT_IN: OrgServices = {
  tracker: "worklet",
  knowledge: "doclet",
  chat: "chatlet",
  code: "gitlet",
};

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
  /**
   * What the page reads besides the defaults: each access read's answer for a
   * kind (nobody on it otherwise), and the integrations answer.
   */
  extra: {
    access?: (kind: string) => unknown;
    integrations?: IntegrationsAnswer;
    /** The chart's seats, where a case needs agents on it. */
    roles?: unknown[];
    /** The `limits` answer, the engine's rows of the settings a pane changes. */
    limits?: unknown;
    /** The `settings` answer, the engine's rows of the settings that are not numbers. */
    settings?: unknown;
  } = {},
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
          JSON.stringify(
            extra.access?.(access[1]!) ?? {
              kind: access[1],
              revision: "rev-1",
              syncing: false,
              options: [],
              seats: [],
            },
          ),
          { status: 200 },
        );
      }
      return new Response(JSON.stringify({}), { status: 200 });
    }),
  );
  const store = new Store();
  store.applyOrg(
    withDerived({
      name: "Acme",
      roles: (extra.roles ?? []) as never[],
      ...(services ? { services } : {}),
    }),
  );
  const socket = new LiveSocket(store);
  const asked: string[] = [];
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) => {
    asked.push(what);
    if (what === "viewer") return Promise.resolve(viewer);
    // A NEW OBJECT ON EVERY ASK, as the wire decodes one.
    if (what === "integrations")
      return Promise.resolve(structuredClone(extra.integrations ?? answer));
    if (what === "config" && config !== undefined) return Promise.resolve(config);
    if (what === "limits" && extra.limits !== undefined) return Promise.resolve(extra.limits);
    if (what === "settings" && extra.settings !== undefined) return Promise.resolve(extra.settings);
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
// Chatlet, Worklet, Doclet and Gitlet name Crewlet as Jira and Confluence
// name Atlassian. Mutation: drop the services' vendor and the four lines lose
// "Crewlet".
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
    ["Chat", ["Chatlet (Crewlet)", "Slack", "Mattermost"]],
    ["Work tracking", ["Worklet (Crewlet)", "Jira (Atlassian)"]],
    ["Docs and knowledge", ["Doclet (Crewlet)", "Confluence (Atlassian)"]],
    ["Code", ["Gitlet (Crewlet)", "GitHub", "GitLab"]],
    ["Observability", ["Datadog"]],
    [
      "Cloud and Infrastructure",
      ["AWS", "Cloudflare", "DigitalOcean", "Google", "Google Cloud", "Scaleway", "SSH"],
    ],
    ["Mobile apps", ["Expo", "App Store Connect", "Google Play"]],
    ["Advertising", ["Google Ads", "Meta Ads Manager", "Apple Ads"]],
    ["Analytics", ["Google Analytics", "Search Console"]],
    ["Ad revenue", ["AdSense", "AdMob"]],
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
  expect((await line(/Chatlet/)).textContent).toContain("In use");
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
    ["gitlet", /^Gitlet/],
  ] as const) {
    mount(ADMIN, kind);
    expect((await line(name)).getAttribute("aria-current"), kind).toBe("page");
    cleanup();
  }
});

// WITH NOTHING CHOSEN, WHAT NEEDS A LOOK: each issue under its app's name,
// sending the reader to the app's page rather than fixing it out of context,
// on a button that says the fix it opens there.
test("the summary lists what needs a look and links to each app", async () => {
  mount(ADMIN);
  expect(await screen.findByText(expiring)).toBeTruthy();
  const tile = screen.getByText(expiring).closest(".int-issue") as HTMLElement;
  const open = within(tile).getByRole("link", { name: "Replace key GitLab" });
  expect(open.textContent).toBe("Replace key");
  expect(open.getAttribute("href")).toBe("#/company/integrations/gitlab?edit=key");
});

// ON A PHONE THE BARE ADDRESS LEADS WITH WHAT NEEDS A LOOK, then the
// directory: one pane showed the list alone, and a step waiting on the reader
// was a dot in it. Mutation: show the list pane on the bare address and the
// summary is in a pane a phone never draws.
test("on a phone the bare address shows what needs a look above the directory", async () => {
  const screenAt = installWindow(390);
  try {
    mount(ADMIN);
    expect(await screen.findByText(expiring)).toBeTruthy();
    const split = document.querySelector(".crewlet-list-detail")!;
    expect(split.className).toContain("crewlet-list-detail--showing-detail");
    const detail = split.querySelector(".crewlet-list-detail__detail")!;
    expect(detail.contains(screen.getByText(expiring))).toBe(true);
    expect(
      within(detail as HTMLElement).getByRole("navigation", { name: "Integrations" }),
    ).toBeTruthy();
  } finally {
    screenAt.restore();
  }
});

// A SERVICE'S AGENTS ARE THE PRODUCT'S DATA GRID: a head over them, the
// agent sortable, each row its agent's page, and the whole name in the row.
// Mutation: back on the hand-built lines there is no table and no link.
test("a service's agents are a sortable grid whose rows open each agent", async () => {
  const long = "Principal Product Manager for Learner Journeys and Phrase Discovery";
  mount(ADMIN, "worklet", BUILT_IN, undefined, undefined, {
    roles: [{ name: long, handle: "pm", llm: "zulu" }],
  });
  const grid = await screen.findByRole("table", { name: "Agents in Worklet" });
  expect(within(grid).getByRole("button", { name: /Agent/ })).toBeTruthy();
  // THE STATE KEEPS ITS WORDS: its track is its content, never a capped share.
  const tracks = (grid.closest("[style]") as HTMLElement | null)?.style.gridTemplateColumns ?? "";
  expect(tracks.trim().endsWith("max-content")).toBe(true);
  expect(within(grid).getByText(long)).toBeTruthy();
  const open = within(grid)
    .getAllByRole("link")
    .find((a) => a.getAttribute("href") === "#/company/org/seats/pm");
  expect(open).toBeTruthy();
});

// A TOOL WHOSE EVERY PRODUCT IS HELD BY A CREWLET SERVICE HAS NOTHING TO
// CONNECT: the page says what holds each and where that is changed, in place
// of the form, and offers no Connect. Mutation: drop the check and the page
// offers Connect over an organization that would serve nothing.
test("an integration whose every product a Crewlet service holds says so and offers no Connect", async () => {
  const held = {
    ...listing,
    tools: listing.tools.map((t) =>
      t.key === "jira"
        ? { ...t, held_by: "worklet" }
        : t.key === "confluence"
          ? { ...t, configured: false, held_by: "doclet" }
          : t,
    ),
  } as unknown as SetupListing;
  mount(ADMIN, "atlassian", BUILT_IN, { status: 200, body: held }, undefined, {
    integrations: {
      ...answer,
      integrations: answer.integrations.filter((r) => r.key !== "confluence"),
      tools: (answer.tools ?? []).filter((t) => t.key !== "atlassian"),
    },
  });
  expect(await screen.findByText(/Doclet is this company.s knowledge base/)).toBeTruthy();
  expect(screen.getByText(/Worklet is this company.s work tracker/)).toBeTruthy();
  expect(screen.queryByRole("button", { name: /Connect/ })).toBeNull();
  expect(
    screen.getAllByRole("link").some((a) => a.getAttribute("href") === "#/settings/config"),
  ).toBe(true);
});

// A RE-READ THAT SAYS THE SAME THING DRAWS NO ROW AGAIN: the readings keep
// their identity while unchanged and the table is `memo` on stable props, so
// an idle Agents tab writes nothing into its inputs. Mutation: drop `memo` on
// AgentsAccess, or hand it a new closure per render, and every switch's
// attributes are written again on each re-read.
test("a re-read of an unchanged reading draws no row of the agents again", async () => {
  // THE SAME READING EACH TIME, as a new object: the fixture's times are relative.
  const reading = datadogAnswer();
  const { fetched } = mount(ADMIN, "datadog", BUILT_IN, undefined, undefined, {
    access: () => structuredClone(reading),
  });
  await screen.findByRole("switch", { name: "SRE on Datadog" });
  await settled(() => fetched.length);
  const writes: MutationRecord[] = [];
  const observer = new MutationObserver((records) => writes.push(...records));
  observer.observe(document.body, { attributes: true, subtree: true });
  const before = fetched.length;
  act(() => void document.dispatchEvent(new Event("visibilitychange")));
  await settled(() => fetched.length);
  observer.disconnect();
  expect(fetched.length).toBeGreaterThan(before);
  expect(writes.filter((r) => (r.target as Element).tagName === "INPUT").length).toBe(0);
});

// THE HOME IS WHAT IS IN USE: the connected apps (Confluence as its own
// product) and the services the company runs, each a compact card with its
// state and one action; an app nobody connected is the list's alone.
// Mutation: list every catalogue entry and Slack appears.
test("the home shows only what is in use, each a compact card with one action", async () => {
  mount(ADMIN);
  const inUse = await screen.findByRole("region", { name: /^In use/ });
  const names = [...inUse.querySelectorAll(".int-tile-link")].map((a) => a.textContent);
  expect(names).toEqual([
    "Chatlet",
    "Worklet",
    "Doclet",
    "Confluence",
    "Gitlet",
    "GitLab",
    "Datadog",
  ]);
  for (const card of inUse.querySelectorAll(".crewlet-catalog-tile")) {
    expect(card.className).toContain("crewlet-catalog-tile--compact");
  }
  const gitlab = within(inUse).getByRole("link", { name: "GitLab" }).closest("li") as HTMLElement;
  expect(
    within(gitlab).getByRole("link", { name: "Replace key GitLab" }).getAttribute("href"),
  ).toBe("#/company/integrations/gitlab?edit=key");
  // THE COUNT BESIDE IN USE is of every card under it, the services included.
  expect(within(inUse).getAllByRole("heading")[0]!.textContent).toBe("In use 7");
});

// EVERY STATE IS DRAWN ONE WAY: a service's "In use" and an app's
// "Connected" are each the tone's dot and the word, on the tile and on the
// directory's row alike. A service's was a pill beside the apps' dots.
// Mutation: hand a service's tile or row a Tag again and this goes red.
test("a service's state and an app's are drawn the same way, tile and row", async () => {
  mount(ADMIN);
  const inUse = await screen.findByRole("region", { name: /^In use/ });
  for (const name of ["Worklet", "Datadog"]) {
    const card = within(inUse).getByRole("link", { name }).closest(".crewlet-catalog-tile")!;
    const state = card.querySelector(".crewlet-catalog-tile__status")!;
    expect(state.querySelector(".crewlet-status-dot"), name).toBeTruthy();
    expect(card.querySelector(".crewlet-tag"), name).toBeNull();
  }
  expect(inUse.querySelector(".crewlet-catalog-tile__status")?.textContent?.trim()).toBe("In use");
  for (const name of [/Worklet/, /Datadog/]) {
    const row = await line(name);
    expect(row.querySelector(".crewlet-status-dot"), String(name)).toBeTruthy();
    expect(row.querySelector(".crewlet-tag"), String(name)).toBeNull();
  }
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

// A FIX LANDS ON ITS FORM'S OWN ADDRESS, which stays: a reload or a copied
// link opens the same form, inside the Settings tab under the page's header.
test("an address naming a form opens it in place, and keeps it", async () => {
  location.hash = "#/company/integrations/gitlab?edit=key";
  mount(ADMIN, ROUTED);
  expect(await screen.findByRole("form", { name: "Replace the GitLab key" })).toBeTruthy();
  expect(screen.getByRole("heading", { level: 2, name: "GitLab" })).toBeTruthy();
  expect(screen.getByRole("tab", { name: "Settings", selected: true })).toBeTruthy();
  await settled();
  expect(location.hash).toBe("#/company/integrations/gitlab?edit=key");
});

// A FIX FOLLOWED FROM THE HOME OPENS ITS FORM: the page stays mounted while
// the address moves, and the form is the address's, not a mount's.
test("a fix followed from the home opens its form on the app's page", async () => {
  mount(ADMIN, ROUTED);
  const inUse = await screen.findByRole("region", { name: /^In use/ });
  const gitlab = within(inUse).getByRole("link", { name: "GitLab" }).closest("li") as HTMLElement;
  location.hash = within(gitlab)
    .getByRole("link", { name: /^Replace key/ })
    .getAttribute("href")!;
  expect(await screen.findByRole("form", { name: "Replace the GitLab key" })).toBeTruthy();
});

// COMING BACK THROUGH THE LIST LANDS ON THE PAGE, not the form: the list links
// the bare address, which for a working app is its Agents tab.
test("coming back to an app through the list does not reopen its form", async () => {
  location.hash = "#/company/integrations/gitlab?edit=key";
  mount(ADMIN, ROUTED);
  expect(await screen.findByRole("form", { name: "Replace the GitLab key" })).toBeTruthy();
  location.hash = (await line(/GitHub/)).getAttribute("href")!;
  expect(await screen.findByRole("heading", { level: 2, name: "GitHub" })).toBeTruthy();
  location.hash = (await line(/GitLab/)).getAttribute("href")!;
  expect(await screen.findByRole("heading", { level: 2, name: "GitLab" })).toBeTruthy();
  expect(screen.getByRole("tab", { name: /Agents/, selected: true })).toBeTruthy();
  expect(screen.queryByRole("form", { name: "Replace the GitLab key" })).toBeNull();
});

// EACH TAB IS AN ADDRESS: a tab pressed is written into the address (so Back,
// a reload and a copied link land on it), and an address naming a tab opens
// on it. Mutation: hold the tab in state again and the address never moves.
test("each tab of an app's page has its own address", async () => {
  location.hash = "#/company/integrations/gitlab?tab=activity";
  mount(ADMIN, ROUTED);
  expect(await screen.findByRole("tab", { name: "Activity", selected: true })).toBeTruthy();
  fireEvent.click(screen.getByRole("tab", { name: "Settings" }));
  await waitFor(() => expect(location.hash).toBe("#/company/integrations/gitlab?tab=settings"));
  expect(await screen.findByRole("tab", { name: "Settings", selected: true })).toBeTruthy();
  location.hash = "#/company/integrations/gitlab?tab=agents";
  expect(await screen.findByRole("tab", { name: /Agents/, selected: true })).toBeTruthy();
});

// EDIT SETTINGS OPENS IN PLACE: the header, its facts and the tabs stay, the
// form is inside the Settings tab at its own address, and Cancel returns to
// the tab's read view. Mutation: draw the form in place of the page again and
// the tabs are gone.
test("Edit settings opens the form inside the Settings tab, and Cancel returns to it", async () => {
  location.hash = "#/company/integrations/gitlab?tab=settings";
  mount(ADMIN, ROUTED);
  fireEvent.click(await screen.findByRole("button", { name: "Edit settings" }));
  expect(await screen.findByRole("form", { name: "GitLab settings" })).toBeTruthy();
  expect(screen.getByRole("heading", { level: 2, name: "GitLab" })).toBeTruthy();
  expect(screen.getByRole("tab", { name: "Settings", selected: true })).toBeTruthy();
  expect(screen.getByRole("tab", { name: /Agents/ })).toBeTruthy();
  expect(location.hash).toBe("#/company/integrations/gitlab?edit=settings");
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  await waitFor(() => expect(location.hash).toBe("#/company/integrations/gitlab?tab=settings"));
  expect(screen.queryByRole("form", { name: "GitLab settings" })).toBeNull();
  expect(await screen.findByRole("button", { name: "Edit settings" })).toBeTruthy();
});

/** The listing with GitLab's own connection set but not finished. */
const unfinished = {
  ...listing,
  tools: listing.tools.map((t) =>
    t.key === "gitlab" ? { ...t, satisfied: false, form_complete: false } : t,
  ),
} as unknown as SetupListing;

// A SAVE THAT FINISHES THE SETUP LANDS ON AGENTS, decided from the reading that
// shows the save, never from the one before it, which would open the form again.
// Mutation: close the form at once on an unfinished connection and the page
// lands back on the form.
test("saving the last answer of an unfinished connection lands on Agents", async () => {
  location.hash = "#/company/integrations/gitlab";
  // THE READ AFTER THE SAVE STILL PREDATES IT, as an engine's may; the next has it.
  let after = -1;
  const setup = {
    status: 200,
    get body(): unknown {
      if (after < 0) return unfinished;
      return after++ === 0 ? unfinished : listing;
    },
  };
  mount(ADMIN, ROUTED, BUILT_IN, setup);
  const form = await screen.findByRole("form", { name: "GitLab settings" });
  fireEvent.change(within(form).getByLabelText(/Admin token/), { target: { value: "glpat-new" } });
  after = 0;
  fireEvent.click(within(form).getByRole("button", { name: "Save" }));
  await waitFor(() => expect(location.hash).toBe("#/company/integrations/gitlab"));
  expect(await screen.findByRole("tab", { name: /Agents/, selected: true })).toBeTruthy();
  expect(screen.queryByRole("form", { name: "GitLab settings" })).toBeNull();
});

// AN UNFINISHED CONNECTION LEADS WITH ITS SETUP: the bare address opens the
// form (written into the address), and its Agents tab says what is next
// rather than offering agents it cannot give access to yet.
test("an unfinished connection opens on its form, and its Agents tab leads with the next step", async () => {
  location.hash = "#/company/integrations/gitlab";
  mount(ADMIN, ROUTED, BUILT_IN, { status: 200, body: unfinished });
  expect(await screen.findByRole("form", { name: "GitLab settings" })).toBeTruthy();
  await waitFor(() => expect(location.hash).toBe("#/company/integrations/gitlab?edit=settings"));
  location.hash = "#/company/integrations/gitlab?tab=agents";
  expect(await screen.findByText("Finish setting up GitLab")).toBeTruthy();
  expect(screen.queryByRole("form", { name: "GitLab settings" })).toBeNull();
  expect(screen.queryByRole("table")).toBeNull();
});

// A HOME WITH NOTHING IN USE says so and points at the list.
test("a company with nothing in use is told to pick from the list", async () => {
  mount(MEMBER, undefined, { tracker: "none", knowledge: "none", chat: "none", code: "none" });
  expect(await screen.findByText("Nothing is in use yet")).toBeTruthy();
  expect(screen.queryByRole("region", { name: /^In use/ })).toBeNull();
});

// AN APP'S PAGE: its header, its tabs, and the one action the engine's state
// asks for. Replace key opens the settings form in place inside the Settings
// tab, saying why, with the header, the tabs and the directory still there.
test("an app's page offers Replace key, which opens its form in the Settings tab", async () => {
  location.hash = "#/company/integrations/gitlab";
  mount(ADMIN, ROUTED);
  expect(await screen.findByRole("heading", { level: 2, name: "GitLab" })).toBeTruthy();
  expect(screen.getByRole("tab", { name: /Agents/, selected: true })).toBeTruthy();
  fireEvent.click(await screen.findByRole("button", { name: "Replace key GitLab" }));
  expect(await screen.findByRole("form", { name: "Replace the GitLab key" })).toBeTruthy();
  expect(screen.getByRole("tab", { name: "Settings", selected: true })).toBeTruthy();
  expect(location.hash).toBe("#/company/integrations/gitlab?edit=key");
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
  // THE BUTTON'S OWN BUSY STATE IS THE ANSWER: nothing is said beside it
  // unless the loop refused.
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Check now" }).getAttribute("aria-busy")).not.toBe(
      "true",
    ),
  );
  expect(screen.queryByText("Crewlet is looking now.")).toBeNull();
});

// THE AGENTS TAB COUNTS THE AGENTS THAT ARE ON, once the access answer has
// said: one of Datadog's three. Mutation: count every agent and it reads 3.
test("the Agents tab counts the agents that are on", async () => {
  mount(ADMIN, "datadog", BUILT_IN, undefined, undefined, { access: () => datadogAnswer() });
  const tab = await screen.findByRole("tab", { name: /Agents/ });
  // THE TAB BAR AND ITS PANEL share one `.tab-detail` column, one shared
  // step apart, rather than standing the pane's own gap from each other.
  expect(tab.closest(".tab-detail")?.querySelector(".int-tab")).toBeTruthy();
  await waitFor(() => expect(within(tab).getByText("1")).toBeTruthy());
  expect(within(tab).queryByText("3")).toBeNull();
});

// THE COUNT AND THE ATTENTION DOT ARE TWO FACTS: GitLab has an issue, so its
// tab carries the dot, and still counts the one agent that is on, before it.
// Mutation: let the marker stand in for the count and the "1" goes.
test("an Agents tab that needs attention still counts the agents that are on", async () => {
  mount(ADMIN, "gitlab", BUILT_IN, undefined, undefined, { access: () => datadogAnswer() });
  const tab = await screen.findByRole("tab", { name: /Agents/ });
  const count = await within(tab).findByText("1");
  const dot = tab.querySelector(".crewlet-tabs__marker--warning");
  expect(dot).not.toBeNull();
  expect(count.compareDocumentPosition(dot!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
});

// AN AGENT'S OWN WAIT IS SAID ON ITS ROW, with the step's own control, and not
// again above the table as the integration's: the engine marks it `seat`. The
// tab still says something needs attention. Mutation: list every finding
// above the table and the wait is drawn twice.
test("an agent's own wait is left to its row, not listed above the agents", async () => {
  const waiting: IntegrationsAnswer = {
    ...answer,
    integrations: answer.integrations.map((row) =>
      row.key === "datadog"
        ? {
            ...row,
            reconcile: {
              phase: "awaiting_admin",
              actor: "admin",
              findings: [
                {
                  kind: "approval_required",
                  subject: "sre",
                  detail: "Ask an administrator to approve SRE's role",
                  phase: "awaiting_admin",
                  seat: true,
                },
              ],
            },
          }
        : row,
    ),
  };
  mount(ADMIN, "datadog", BUILT_IN, undefined, undefined, {
    access: () => datadogAnswer(),
    integrations: waiting,
  });
  const tab = await screen.findByRole("tab", { name: /Agents/ });
  await waitFor(() => expect(tab.querySelector(".crewlet-tabs__marker--warning")).not.toBeNull());
  await screen.findByRole("table", { name: "Agents on Datadog" });
  expect(document.querySelector(".int-issue")).toBeNull();
  expect(screen.queryByText(/approve SRE's role/)).toBeNull();
});

// A NOTE SITS UNDER THE AGENTS, its own row led by its title, the sentence
// with its key and address set as code, and what would change it beneath.
// Mutation: draw the notes above the table, or drop the lead, and it fails.
test("a working integration's notes sit under its agents, each led by its title", async () => {
  const noted: IntegrationsAnswer = {
    ...answer,
    integrations: answer.integrations.map((row) =>
      row.key === "datadog"
        ? {
            ...row,
            reconcile: {
              phase: "ready",
              findings: [
                {
                  kind: "coverage_partial",
                  subject: "api.external_url",
                  title: "Datadog can't reach this engine",
                  detail: "`api.external_url` is `http://localhost:8200`, so no events arrive",
                  remedy: "Set `api.external_url` to a public address to receive events",
                  phase: "ready",
                },
              ],
            },
          }
        : row,
    ),
  };
  mount(ADMIN, "datadog", BUILT_IN, undefined, undefined, {
    access: () => datadogAnswer(),
    integrations: noted,
  });
  const notes = await screen.findByRole("region", { name: "Notes" });
  const lead = within(notes).getByText("Datadog can't reach this engine.");
  expect(lead.tagName).toBe("STRONG");
  expect(within(notes).getByText("http://localhost:8200").tagName).toBe("CODE");
  expect(within(notes).getByText(/to a public address to receive events\./)).toBeTruthy();
  const table = await screen.findByRole("table", { name: "Agents on Datadog" });
  expect(table.compareDocumentPosition(notes) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
});

// A SYNC THAT ENDS HAS BEEN CHECKED: the engine looked again before it let the
// sync end, so the page reads the integration's rows again the moment it does,
// with nobody pressing Check now. Mutation: drop the re-read and the second
// integrations question never goes out.
test("the integration is read again the moment a sync ends", async () => {
  let syncing = true;
  const { asked } = mount(ADMIN, "datadog", BUILT_IN, undefined, undefined, {
    access: () => ({ ...datadogAnswer(), syncing }),
  });
  await screen.findByRole("table", { name: "Agents on Datadog" });
  const before = asked.filter((q) => q === "integrations").length;
  syncing = false;
  await waitFor(() =>
    expect(asked.filter((q) => q === "integrations").length).toBeGreaterThan(before),
  );
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

// AN APP'S PAGE SAYS EACH FACT ONCE: its name, never its key beside it
// ("Integration slack" over "Slack"); what it covers under the name, and how
// each agent gets in only under "Each agent"; and the step a person takes by
// hand apart from the values to have ready. Mutation: put the key back on the
// header, or the step back in the list, and the case fails.
test("an app's page names it once, and keeps what to do apart from what to have", async () => {
  mount(ADMIN, "slack");
  expect(await screen.findByRole("heading", { level: 2, name: "Slack" })).toBeTruthy();
  expect(screen.queryByText("slack", { exact: true })).toBeNull();
  expect(screen.getAllByText(/its own Slack app/)).toHaveLength(1);
  const need = screen.getByRole("region", { name: "What you need" });
  expect(within(need).queryByText(/manifest/)).toBeNull();
  const byHand = screen.getByRole("region", { name: "What you do by hand" });
  expect(within(byHand).getByText(/Create one Slack app per agent/)).toBeTruthy();
});

// AN ADDRESS ONLY THIS MACHINE REACHES SAYS SO, on the engine's word: drawn
// bare, `http://127.0.0.1:…` read as the address every app was set up on,
// though no app outside the machine can deliver to it. Mutation: drop the
// warning and the case fails; draw it for a public address and the first half
// does.
test("a loopback address apps are given is said to be unreachable from outside", async () => {
  mount(ADMIN);
  await waitFor(() => expect(document.querySelector(".int-landing-foot")).not.toBeNull());
  expect(screen.queryByText("Only this machine can reach this address")).toBeNull();
  cleanup();
  const loopback = {
    ...listing,
    external_url: {
      value: "http://127.0.0.1:8080",
      config_path: "api.external_url",
      loopback: true,
    },
  };
  mount(ADMIN, undefined, BUILT_IN, { status: 200, body: loopback });
  expect(await screen.findByText("Only this machine can reach this address")).toBeTruthy();
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
  // NO HINT to connect what an Admin connects.
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

test("a service the company runs is in use, with its way in", async () => {
  mount(MEMBER, "worklet");
  expect(await screen.findByRole("heading", { level: 2, name: "Worklet" })).toBeTruthy();
  expect(screen.getByRole("link", { name: "Open Worklet" }).getAttribute("href")).toBe("#/worklet");
});

// A SERVICE OPENS ON ITS AGENTS, with no Overview: what that tab listed (a
// service's projects or spaces) is the service's own landing, so the header
// counts them as a link to that list. Mutation: bring the Overview tab back
// and the tab list goes red; drop the fact's path and the link does.
test.each([
  ["worklet", "Worklet", "#/worklet"],
  ["doclet", "Doclet", "#/doclet"],
])(
  "%s opens on its agents, with what it holds counted as a way to its list",
  async (kind, name, list) => {
    mount(
      { ...MEMBER, grants: [...MEMBER.grants, "worklet", "doclet"], worklet: true, doclet: true },
      kind,
    );
    const tabs = await screen.findByRole("tablist", { name: `${name} sections` });
    expect(within(tabs).queryByRole("tab", { name: /Overview/ })).toBeNull();
    expect(
      within(tabs)
        .getByRole("tab", { name: /^Agents/ })
        .getAttribute("aria-selected"),
    ).toBe("true");
    const counted = [...document.querySelectorAll<HTMLAnchorElement>(`a[href="${list}"]`)].filter(
      (a) => a.textContent !== `Open ${name}`,
    );
    expect(counted, "the header's count links to the service's own list").toHaveLength(1);
  },
);

// WHAT RUNS INSTEAD, IN THE ENGINE'S WORD, with the way to it, and nothing
// asked of a service the company does not run. Mutation: ask the counts
// whatever runs, and the list names `work_projects`.
test("a service run elsewhere says what replaces it and asks nothing of it", async () => {
  const { asked } = mount(ADMIN, "worklet", {
    tracker: "jira",
    knowledge: "doclet",
    chat: "chatlet",
    code: "none",
  });
  expect(await screen.findByText("Jira does this for your company")).toBeTruthy();
  expect(screen.getByRole("link", { name: "Open Jira" }).getAttribute("href")).toBe(
    "#/company/integrations/jira",
  );
  expect((await line(/Worklet/)).textContent).toContain("Not in use");
  expect(asked).not.toContain("work_projects");
});

// THE INBOX HORIZON IS THE COMPANY'S, read where its document keeps it — at the
// top level, `inbox.retention_days` — with the engine's default where it is
// unset. It was read off the `tracker` block, under a key no document wrote
// there, so the pane said the default whatever a company set. Mutation: read
// it under `tracker` again, and a company keeping ninety days is told the
// engine's default.
test("Worklet's settings say how long the company's inbox keeps a notice, and change it", async () => {
  // TWO UNITS FILING UNDER ONE PROJECT name it once.
  const doc = {
    name: "Acme",
    inbox: { retention_days: 90 },
    units: [
      { id: "web", name: "Web", project: "PROD" },
      { id: "api", name: "API", project: "PROD" },
      { id: "eng", name: "Engineering", project: "ENG" },
    ],
  };
  mount(ADMIN, "worklet", BUILT_IN, undefined, doc, {
    limits: {
      limits: [
        {
          key: "inbox.retention_days",
          group: "inbox",
          unit: "days",
          value: 90,
          default: 365,
          off: false,
          default_off: false,
          settable: true,
          range: { min: 30, max: 3650 },
        },
      ],
    },
  });
  fireEvent.click(await screen.findByRole("tab", { name: "Settings" }));
  const row = await screen.findByRole("row", { name: /Inbox keeps a notice for/ });
  await waitFor(() => expect(row.textContent).toContain("90 days"));
  expect(within(row).getByText("Changed from 365 days")).toBeTruthy();
  expect(within(row).getByRole("button", { name: "Change Inbox keeps a notice for" })).toBeTruthy();
  // WHERE EACH LIVES, said truly: the inbox is not under tracker.
  expect(screen.getByText(/the inbox under inbox/)).toBeTruthy();
  const projects = screen.getByRole("row", { name: /Projects the chart declares/ });
  await waitFor(() => expect(projects.textContent).toContain("ENG"));
  expect(projects.textContent).not.toContain("PROD, PROD");
});

// DOCLET'S SETTINGS ARE THE ENGINE'S ROWS (`settings`, group `knowledge`): each
// the value in force beside its default, said in the page's words, with the
// pencil that changes it. They were read off the document with a dashboard
// sentence for every unset key ("The engine's default"), so a company never
// learned which space its pages lived in. Mutation: read them off `config`
// again, and the defaults' values are gone.
test("Doclet's settings are the engine's rows, each beside its default, and change in place", async () => {
  const knowledge = (
    key: string,
    kind: string,
    value: unknown,
    def: unknown,
    over: Record<string, unknown> = {},
  ) => ({
    key,
    group: "knowledge",
    kind,
    value,
    default: def,
    set: false,
    off: false,
    has_off: false,
    ...over,
  });
  mount(
    ADMIN,
    "doclet",
    BUILT_IN,
    undefined,
    { name: "Acme" },
    {
      settings: {
        settings: [
          knowledge("knowledge.root_space", "container", "HOME", "HOME", { has_off: true }),
          knowledge("knowledge.skills_container", "container", "", "TS", {
            has_off: true,
            off: true,
            set: true,
          }),
          knowledge("knowledge.scope", "containers", ["ENG", "PROD"], [], { set: true }),
          knowledge("knowledge.vectors", "switch", true, true),
        ],
      },
    },
  );
  fireEvent.click(await screen.findByRole("tab", { name: "Settings" }));
  await waitFor(() =>
    expect(screen.getByRole("row", { name: /Company pages/ }).textContent).toContain("HOME"),
  );
  const pages = screen.getByRole("row", { name: /Company pages/ });
  expect(within(pages).getByText("Default")).toBeTruthy();
  const skills = screen.getByRole("row", { name: /Tool skills/ });
  expect(within(skills).getByText("Off")).toBeTruthy();
  expect(within(skills).getByText("Changed")).toBeTruthy();
  const reach = screen.getByRole("row", { name: /Search reaches/ });
  expect(within(reach).getByText("ENG, PROD")).toBeTruthy();
  const meaning = screen.getByRole("row", { name: /Search by meaning/ });
  expect(within(meaning).getByText("On")).toBeTruthy();
  for (const label of ["Company pages", "Tool skills", "Search reaches", "Search by meaning"]) {
    expect(screen.getByRole("button", { name: `Change ${label}` })).toBeTruthy();
  }
});

/** App Store Connect connected as the setup listing sends it: the hidden toggle and the team's Issuer ID. */
function appStoreConnected(): SetupToolState {
  const requirements = [
    {
      field: "enabled",
      label: "Connect App Store Connect",
      kind: "toggle",
      hidden: true,
      present: true,
      default: "true",
      config_path: "integrations.app_store_connect.enabled",
    },
    {
      field: "main_issuer_id",
      label: "Issuer ID",
      kind: "id",
      connect: true,
      present: true,
      value: "57246542-96fe-1a63-e053-0824d011072a",
      config_path: "integrations.app_store_connect.connections.main.issuer_id",
    },
  ].map((r) => ({ required: true, ...r }));
  return {
    key: "app_store_connect",
    configured: true,
    enabled: true,
    satisfied: true,
    form_complete: true,
    requirements,
  } as unknown as SetupToolState;
}

// APP STORE CONNECT KEEPS NO COMPANY KEY: every call is signed by the agent's
// own, so its Settings tab is the team's Issuer ID and Edit settings under it,
// with no key row and no card asking for a key. Mutation: draw a key list or an
// Admin key card back on the tab, and a case fails.
test("App Store Connect's Settings tab holds the Issuer ID and asks for no key", async () => {
  mount(
    ADMIN,
    "app_store_connect",
    BUILT_IN,
    { status: 200, body: { ...listing, tools: [...listing.tools, appStoreConnected()] } },
    undefined,
    {
      integrations: {
        ...answer,
        integrations: [
          ...answer.integrations,
          { key: "app_store_connect", configured: true, inbound: 0, reconcile: { phase: "ready" } },
        ],
      },
    },
  );
  fireEvent.click(await screen.findByRole("tab", { name: "Settings" }));
  const term = await screen.findByText("Issuer ID", { selector: "dt" });
  const summary = term.closest("dl") as HTMLElement;
  expect([...summary.querySelectorAll("dt")].map((dt) => dt.textContent)).toEqual(["Issuer ID"]);
  const edit = screen.getByRole("button", { name: "Edit settings" });
  expect(term.compareDocumentPosition(edit) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  expect(screen.queryByText(/API keys|Admin key|App Store analytics/)).toBeNull();
  expect(screen.queryByRole("button", { name: /Remove|Turn on App Store analytics/ })).toBeNull();
});

test("a reader without Admin is told Doclet's settings are an admin's, and asked nothing of them", async () => {
  const { asked } = mount(MEMBER, "doclet");
  fireEvent.click(await screen.findByRole("tab", { name: "Settings" }));
  const pages = await screen.findByRole("row", { name: /Company pages/ });
  expect(within(pages).getByText("Admins see this")).toBeTruthy();
  expect(asked).not.toContain("settings");
});

// THE COMPANY'S MESSAGE HORIZON IS SHOWN AND CHANGED ON CHATLET'S PAGE: the
// engine's row (`limits`, group `chat`), whose 0 keeps messages for ever and
// reads so. Its value was never drawn anywhere: a room said "as long as the
// company default" and no screen said how long that was. (What its field
// writes is `components/LimitSetting.test.tsx`'s.)
test("Chatlet's settings say how long the company keeps messages, and change it", async () => {
  mount(
    { ...ADMIN, grants: [...ADMIN.grants, "chatlet"] },
    "chat",
    BUILT_IN,
    undefined,
    undefined,
    {
      limits: {
        limits: [
          {
            key: "chat.chatlet.message_retention_days",
            group: "chat",
            unit: "days",
            value: 0,
            default: 365,
            off: true,
            default_off: false,
            settable: true,
            range: { min: 30, max: 3650, or_zero: true },
          },
        ],
      },
    },
  );
  // THE ROW IS DRAWN AGAIN once the engine's answer lands, so it is found then.
  await waitFor(() =>
    expect(screen.getByRole("row", { name: /Messages are kept for/ }).textContent).toContain(
      "For ever",
    ),
  );
  const row = screen.getByRole("row", { name: /Messages are kept for/ });
  expect(within(row).getByText("Changed from 365 days")).toBeTruthy();
  expect(within(row).getByRole("button", { name: "Change Messages are kept for" })).toBeTruthy();
});

test("a service the company runs none of says so, with no product to open", async () => {
  mount(ADMIN, "doclet", { tracker: "worklet", knowledge: "none", chat: "chatlet", code: "none" });
  expect(await screen.findByText("Nothing does this for your company")).toBeTruthy();
  expect(screen.queryByRole("link", { name: /Open Confluence/ })).toBeNull();
});

// UNTIL THE ENGINE HAS SAID, nothing is guessed: a node running no company
// says so, and no service wears In use or Not in use.
test("a node running no company says so and wears no state", async () => {
  mount(ADMIN, "worklet", null);
  expect(await screen.findByText("This node runs no company yet.")).toBeTruthy();
  // ITS NAME AND ITS MAKER, and nothing after them: Crewlet beside each service.
  for (const [name, says] of [
    ["Worklet", "WorkletCrewlet"],
    ["Doclet", "DocletCrewlet"],
    ["Chatlet", "ChatletCrewlet"],
    ["Gitlet", "GitletCrewlet"],
  ] as const) {
    expect((await line(new RegExp(name))).textContent, name).toBe(says);
  }
});

// CHATLET COUNTS NOTHING: the rooms it answers for are the reader's own seat's,
// so its page says what it gives and the way in, and asks no count.
test("Chatlet in use is In use, with its way in and nothing counted", async () => {
  const { asked } = mount({ ...MEMBER, grants: [...MEMBER.grants, "chatlet"] }, "chat");
  expect(await screen.findByRole("heading", { level: 2, name: "Chatlet" })).toBeTruthy();
  expect(screen.getByRole("link", { name: "Open Chatlet" }).getAttribute("href")).toBe(
    "#/chatlet/activity",
  );
  expect(screen.getByText("Rooms, threads and direct messages with your agents.")).toBeTruthy();
  expect((await line(/Chatlet/)).textContent).toContain("In use");
  expect(asked.filter((q) => q !== "viewer" && q !== "integrations")).toEqual([]);
});

// THE WAY IN IS OFFERED TO A HOLDER OF CHATLET, as Worklet's and Doclet's are
// to a holder of their apps. Mutation: offer Open Chatlet to every reader, and
// a reader without Chatlet is sent to a screen that refuses them.
test("a reader without Chatlet is told what opens it, and offered no way in", async () => {
  mount(MEMBER, "chat");
  expect(await screen.findByRole("heading", { level: 2, name: "Chatlet" })).toBeTruthy();
  expect(screen.queryByRole("link", { name: "Open Chatlet" })).toBeNull();
  expect(screen.getByText(/needs Chatlet access/)).toBeTruthy();
  expect(screen.getByText("Rooms, threads and direct messages with your agents.")).toBeTruthy();
});

// THE VENDOR BY NAME, from the viewer answer that names it to a holder of
// Chatlet, with the way to its line. Mutation: drop chat's `replacedBy` and the
// page says nothing does this for your company.
test("a company talking on a vendor's chat is told which, with the way to it", async () => {
  mount({ ...ADMIN, chat_vendors: ["slack"] }, "chat", { ...BUILT_IN, chat: "vendor" });
  expect(await screen.findByText("Slack does this for your company")).toBeTruthy();
  expect(screen.getByRole("link", { name: "Open Slack" }).getAttribute("href")).toBe(
    "#/company/integrations/slack",
  );
  expect(screen.getByText("Slack is not connected.")).toBeTruthy();
  expect((await line(/Chatlet/)).textContent).toContain("Not in use");
  expect(screen.queryByRole("link", { name: "Open Chatlet" })).toBeNull();
});

// A VENDOR NOBODY NAMED is still a vendor's chat, never a blank and never
// "nothing does this": the projection says the chat is a vendor's, and only
// a holder of Chatlet is told which.
test("a vendor chat nobody named is said in words, with no line to open", async () => {
  mount(MEMBER, "chat", { ...BUILT_IN, chat: "vendor" });
  expect(await screen.findByText("A vendor's chat does this for your company")).toBeTruthy();
  expect(screen.queryByRole("link", { name: /^Open / })).toBeNull();
});

test("a company running no chat says so, with nothing to open", async () => {
  mount(ADMIN, "chat", { ...BUILT_IN, chat: "none" });
  expect(await screen.findByText("Nothing does this for your company")).toBeTruthy();
  expect(screen.queryByRole("link", { name: "Open Chatlet" })).toBeNull();
});

// GITLET COUNTS NOTHING: which repositories a reader sees is their own grant's
// answer, so its page says what it gives and the way in, asks no count, and
// opens where the sidebar's Gitlet row does. Mutation: drop the service's
// `open` and the link goes.
test("Gitlet in use is In use, with its way in and nothing counted", async () => {
  const { asked } = mount({ ...MEMBER, grants: [...MEMBER.grants, "gitlet"] }, "gitlet");
  expect(await screen.findByRole("heading", { level: 2, name: "Gitlet" })).toBeTruthy();
  expect(screen.getByRole("link", { name: "Open Gitlet" }).getAttribute("href")).toBe("#/gitlet");
  expect(screen.getByText("Git repositories, their changes and checks, hosted here.")).toBeTruthy();
  expect(screen.getByRole("region", { name: "What Gitlet gives you" })).toBeTruthy();
  expect((await line(/Gitlet/)).textContent).toContain("In use");
  // GITLET'S SETTINGS ARE AN ADMIN'S, as the company document is.
  expect(screen.queryByRole("link", { name: "Gitlet settings" })).toBeNull();
  expect(asked.filter((q) => q !== "viewer" && q !== "integrations")).toEqual([]);
});

// THE WAY IN IS OFFERED TO A HOLDER OF GITLET, as Chatlet's is to a holder of
// Chatlet. Mutation: offer Open Gitlet to every reader, and a reader without
// Gitlet is sent to a screen that refuses them.
test("a reader without Gitlet is told what opens it, and offered no way in", async () => {
  mount(MEMBER, "gitlet");
  expect(await screen.findByRole("heading", { level: 2, name: "Gitlet" })).toBeTruthy();
  expect(screen.queryByRole("link", { name: "Open Gitlet" })).toBeNull();
  expect(screen.getByText(/needs Gitlet access/)).toBeTruthy();
});

// AN ADMIN IS SENT TO GITLET'S OWN SETTINGS, where the rest of `code` is read.
// Mutation: drop the admin's link and this goes red.
test("an Admin is offered Gitlet's settings beside its way in", async () => {
  mount({ ...ADMIN, grants: ["admin", "gitlet"] }, "gitlet");
  expect((await screen.findByRole("link", { name: "Gitlet settings" })).getAttribute("href")).toBe(
    "#/settings/gitlet",
  );
  expect(screen.getByRole("link", { name: "Open Gitlet" })).toBeTruthy();
});

// NOTHING IS IN GITLET'S PLACE: GitHub and GitLab are integrations a seat
// calls, never a host, so Gitlet off says Crewlet hosts none of the code
// rather than "nothing does this" for a company that keeps it on GitHub.
// Mutation: send Gitlet off through the replacement's sentences and the page
// says "Nothing does this for your company".
test("a company with Gitlet off is told Crewlet hosts none of its code", async () => {
  mount(ADMIN, "gitlet", { ...BUILT_IN, code: "none" });
  expect(await screen.findByText("Crewlet hosts none of your company's code")).toBeTruthy();
  expect(screen.queryByText("Nothing does this for your company")).toBeNull();
  expect(screen.getByRole("region", { name: "What Gitlet would give you" })).toBeTruthy();
  expect(screen.queryByRole("link", { name: "Open Gitlet" })).toBeNull();
  expect((await line(/Gitlet/)).textContent).toContain("Not in use");
});
