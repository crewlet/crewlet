/**
 * Company › Atlas: what the screens ask the engine for and what they say.
 * The map's drawing is measured in a real browser (`Atlas.layout.test.tsx`);
 * these suites hold the words, the band, the filters, the one selection and
 * search the map and the catalogue share, the sheet and an entry's page.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import { ToastProvider } from "@crewlethq/ui";
import { Router } from "~/app/router.tsx";
import { SessionReading } from "~/lib/frameSession.ts";
import { ClientContext } from "~/lib/store-hooks.ts";
import { ViewerProvider } from "~/lib/viewer.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import { InertWebSocket } from "~/test/client.tsx";
import {
  activityAnswer,
  disputedServer,
  entriesAnswer,
  entryAnswer,
  facetsAnswer,
  generatedCompany,
  grownCompany,
  logAnswer,
  mapAnswer,
  sparseCompany,
  summary,
  traceAnswer,
  type AtlasFixture,
} from "~/test/atlas.ts";
import type { AtlasEntryAnswer } from "~/contract/atlas.ts";
import { Atlas } from "./Atlas.tsx";
import { AtlasPeek } from "./AtlasPeek.tsx";
import { EntryPage } from "./EntryPage.tsx";
import { serviceColumns } from "./AtlasMap.tsx";
import { bandsOf } from "./Catalogue.tsx";
import { queryParams } from "./filters.ts";
import { cardOf, lineOf } from "./graph.tsx";
import { neighbourhood } from "./Neighbourhood.tsx";
import { coverageLine, coverageNotes } from "./parts.tsx";

afterEach(() => {
  cleanup();
  location.hash = "";
});

type Asked = { what: string; params: Record<string, unknown> };

/** Mount `children` over a socket that answers the atlas questions for `fixture`, recording each ask. */
function mount(
  hash: string,
  children: React.ReactNode,
  fixture: AtlasFixture,
  entry: (id: string) => AtlasEntryAnswer | undefined = () => undefined,
) {
  Object.defineProperty(globalThis, "WebSocket", { writable: true, value: InertWebSocket });
  location.hash = hash;
  const asked: Asked[] = [];
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string, params?: unknown) => Promise<unknown> }).query = (
    what,
    raw,
  ) => {
    const params = (typeof raw === "string" ? JSON.parse(raw) : (raw ?? {})) as Record<
      string,
      unknown
    >;
    asked.push({ what, params });
    const id = String(params.id ?? "");
    const from = fixture.nodes.find((node) => node.id === id);
    switch (what) {
      case "atlas_facets":
        return Promise.resolve(facetsAnswer(fixture));
      case "atlas_map": {
        // A SEARCH LEAVES THE ENTRIES WHOSE NAME HOLDS IT, as the engine's does.
        const q = String(params.q ?? "").toLowerCase();
        return Promise.resolve(
          mapAnswer(
            q === ""
              ? fixture
              : {
                  ...fixture,
                  nodes: fixture.nodes.filter((node) => node.name.toLowerCase().includes(q)),
                },
          ),
        );
      }
      case "atlas_entries":
        return Promise.resolve(
          entriesAnswer(
            fixture,
            Number(params.limit ?? 50),
            Number(params.cursor ?? 0),
            String(params.sort ?? ""),
          ),
        );
      case "atlas_activity":
        return Promise.resolve(activityAnswer(fixture));
      case "atlas_log":
        return Promise.resolve(logAnswer(fixture, id));
      case "atlas_entry": {
        const answer = entry(id) ?? (from ? entryAnswer(from) : undefined);
        return answer ? Promise.resolve(answer) : new Promise(() => {});
      }
      case "atlas_trace":
        return from
          ? Promise.resolve(
              traceAnswer(
                fixture,
                from,
                params.direction === "down" ? "down" : "up",
                Number(params.depth ?? 6),
              ),
            )
          : new Promise(() => {});
      default:
        return new Promise(() => {});
    }
  };
  const view = render(
    <ToastProvider>
      <ClientContext.Provider value={{ store, socket }}>
        <SessionReading>
          <ViewerProvider>
            <Router>{children}</Router>
          </ViewerProvider>
        </SessionReading>
      </ClientContext.Provider>
    </ToastProvider>,
  );
  return { view, asked };
}

describe("the coverage line", () => {
  // AN ATLAS IS NEVER COMPLETE, and the line never says it is.
  test("names the vendors, most entries first, four before the rest", () => {
    expect(coverageLine(3, {})).toBe("3 entries");
    expect(coverageLine(1, { github: 1 })).toBe("1 entry, from GitHub");
    expect(coverageLine(12, { github: 4, cloudflare: 6, other: 2 })).toBe(
      "12 entries, from Cloudflare and GitHub",
    );
    const many = coverageLine(48, {
      github: 9,
      cloudflare: 8,
      aws: 7,
      datadog: 6,
      anthropic: 5,
      openai: 4,
    });
    expect(many).toBe("48 entries, from GitHub, Cloudflare, AWS, Datadog and 2 more");
    expect(many).not.toMatch(/complete/i);
  });

  test("says what a read could not read in full, and what it left out", () => {
    expect(
      coverageNotes({
        coverage: [
          { source: "settings", complete: true },
          {
            source: "gitlet",
            complete: false,
            reason: "you do not use Gitlet, so no repository was read",
          },
        ],
        withheld: 2,
      }),
    ).toEqual([
      "Gitlet was not read in full: you do not use Gitlet, so no repository was read",
      "2 values were left out for looking like a credential",
    ]);
  });
});

describe("the map's cards and lines", () => {
  test("a card carries its health as a notch with its word, and an unconfirmed entry a dashed edge", () => {
    const alerting = cardOf(
      summary("datastore", "orders-db", { health: "alerting", vendor: "aws" }),
      Date.parse("2026-11-11T10:00:00Z"),
    );
    expect(alerting.attention).toEqual({ tone: "danger", label: "Alerting" });
    expect(alerting.kind).toBe("Database");
    expect(alerting.mark).toBeTruthy();
    const quiet = cardOf(summary("host", "web-1", { word: "unconfirmed" }), 0);
    expect(quiet.attention).toBeUndefined();
    expect(quiet.unconfirmed).toBe(true);
    const stale = cardOf(
      summary("host", "web-9", { state: "stale", last_seen: "2026-11-02T10:00:00Z" }),
      Date.parse("2026-11-11T10:00:00Z"),
    );
    expect(stale.caption).toMatch(/^Last seen /);
  });

  // EVERY RELATION IS DRAWN IN THE FAMILY THE ENGINE NAMES: a call at rest,
  // what a thing runs on as a trunk, the rest only for the entry stood on.
  // Mutation: draw every line as a call and the map is a net of accounts.
  test("a line takes the engine's family, so what stands around is not drawn at rest", () => {
    const company = grownCompany();
    const families = new Map(
      mapAnswer(company).edges.map((edge) => [edge.relation, lineOf(edge).family]),
    );
    expect(families.get("depends_on")).toBe("flow");
    expect(families.get("runs_on")).toBe("support");
    expect(families.get("belongs_to")).toBe("context");
    expect(families.get("monitored_by")).toBe("context");
  });

  test("a service stands one column right of the deepest service that calls it", () => {
    const company = grownCompany();
    const columns = serviceColumns(mapAnswer(company));
    const orders = company.nodes.find((node) => node.name === "orders-api")!;
    expect(columns.get(company.checkout.id)).toBe(0);
    expect(columns.get(orders.id)).toBe(1);
  });

  // AN ENTRY AMONG ITS NEIGHBOURS: each stands in the cluster that says how it
  // relates, read from one step along every relation in both directions.
  test("a neighbourhood stands each neighbour in the cluster of its relation", () => {
    const company = grownCompany();
    const db = company.ordersDb;
    const near = neighbourhood(
      db,
      traceAnswer(company, db, "down", 1),
      traceAnswer(company, db, "up", 1),
      0,
    );
    const lane = (name: string) => near.nodes.find((node) => node.name === name)?.lane;
    expect(lane("orders-db")).toBe("self");
    expect(lane("orders-api")).toBe("used");
    expect(lane("db-1")).toBe("runs");
    expect(lane("orders-db cpu high")).toBe("around");
    expect(near.edges).toHaveLength(near.nodes.length - 1);
  });
});

describe("the filters", () => {
  // THE UNCONFIRMED ENTRIES ARE ASKED FOR ONLY WHEN SHOWN, never filtered out
  // of an answer that counted them.
  test("become the engine's parameters, unconfirmed only when asked for", () => {
    const none = {
      q: "",
      kind: "",
      lane: "",
      vendor: "",
      confidence: "",
      health: "",
      unconfirmed: false,
    } as const;
    expect(queryParams(none)).toEqual({});
    expect(queryParams({ ...none, kind: "host", unconfirmed: true })).toEqual({
      kind: "host",
      unconfirmed: true,
    });
  });

  // MUTATION: make the switch set nothing, or send `unconfirmed` by default,
  // and the asks below go red.
  test("Show unconfirmed is off by default and asks the engine for them once on", async () => {
    const company = grownCompany();
    const { asked } = mount("#/company/atlas", <Atlas />, company);
    const control = await screen.findByRole("button", { name: "Filter" });
    await act(async () => {
      fireEvent.click(control);
    });
    const toggle = await screen.findByRole("switch", { name: /Show unconfirmed \(1\)/ });
    expect((toggle as HTMLInputElement).checked).toBe(false);
    expect(
      asked.filter((a) => a.what === "atlas_map").every((a) => !("unconfirmed" in a.params)),
    ).toBe(true);
    await act(async () => {
      fireEvent.click(toggle);
    });
    expect(location.hash).toContain("unconfirmed=true");
    expect(asked.some((a) => a.what === "atlas_map" && a.params.unconfirmed === true)).toBe(true);
    expect(asked.some((a) => a.what === "atlas_facets" && a.params.unconfirmed === true)).toBe(
      true,
    );
  });

  // THE TOOLBAR ROW IS THE SEARCH AND ONE FILTER CONTROL: no menu stands on
  // the row, the control counts what is in force, and each filter in force is
  // a chip removed on its own. Mutation: draw the menus on the row again.
  test("the row holds the search and one Filter control, with the filters in force as chips", async () => {
    mount("#/company/atlas?kind=host&vendor=aws", <Atlas />, grownCompany());
    const control = await screen.findByRole("button", { name: /^Filter/ });
    expect(control.textContent).toContain("2");
    expect(screen.queryByRole("combobox", { name: "Kind" })).toBeNull();
    const chips = screen.getByRole("group", { name: "Filters in force" });
    expect(chips.textContent).toContain("Kind is Server");
    expect(chips.textContent).toContain("Vendor is AWS");
    await act(async () => {
      fireEvent.click(
        within(chips).getByRole("button", { name: /Remove the Kind is Server filter/ }),
      );
    });
    expect(location.hash).not.toContain("kind=");
    expect(location.hash).toContain("vendor=aws");
    await act(async () => {
      fireEvent.click(within(chips).getByRole("button", { name: "Clear all" }));
    });
    expect(location.hash).not.toContain("vendor=");
  });

  test("no chip row is drawn while no filter is set, and a search is not a chip", async () => {
    mount("#/company/atlas?q=orders", <Atlas />, grownCompany());
    await screen.findByRole("button", { name: "Filter" });
    expect(screen.queryByRole("group", { name: "Filters in force" })).toBeNull();
  });

  test("the coverage line says how many entries and where from, and no word of complete", async () => {
    mount("#/company/atlas", <Atlas />, sparseCompany());
    const line = await screen.findByText("5 entries, from GitHub and Anthropic");
    expect(line).toBeTruthy();
    expect(document.body.textContent).not.toMatch(/complete/i);
  });

  test("the Filter control opens the menus, each offering the values with entries", async () => {
    mount("#/company/atlas?kind=host", <Atlas />, grownCompany());
    const control = await screen.findByRole("button", { name: /^Filter/ });
    await act(async () => {
      fireEvent.click(control);
    });
    const kind = await screen.findByRole("combobox", { name: "Kind" });
    expect(kind.textContent).toContain("Server");
    expect(screen.getByRole("combobox", { name: "Vendor" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Clear all filters" })).toBeTruthy();
  });
});

describe("the page", () => {
  // ONE ANSWER, TWO SURFACES: the map asks for every relation and never for
  // the search, which pushes cards back instead of removing them; the
  // catalogue asks for the same filters in the health order.
  test("the map asks for every relation, and the catalogue for the health order", async () => {
    const { asked } = mount("#/company/atlas", <Atlas />, grownCompany());
    await screen.findByRole("table", { name: /Catalogue/ });
    const map = asked.find((a) => a.what === "atlas_map");
    expect(map?.params).toEqual({ relations: "all" });
    expect(
      asked.some(
        (a) => a.what === "atlas_entries" && a.params.sort === "health" && !a.params.limit,
      ),
    ).toBe(true);
    expect(asked.some((a) => a.what === "atlas_activity")).toBe(true);
  });

  // MUTATION: send `q` with the drawn map's question and the map loses the
  // entries a search leaves out, where it should only push them back.
  test("a search narrows the catalogue and marks the map, which keeps every entry", async () => {
    const { asked } = mount("#/company/atlas?q=orders", <Atlas />, grownCompany());
    await screen.findByRole("table", { name: /Catalogue/ });
    const maps = asked.filter((a) => a.what === "atlas_map");
    expect(maps.some((a) => a.params.relations === "all" && !("q" in a.params))).toBe(true);
    expect(maps.some((a) => a.params.q === "orders")).toBe(true);
    expect(asked.some((a) => a.what === "atlas_entries" && a.params.q === "orders")).toBe(true);
  });

  test("the band counts the entries, names what needs attention with its reason, and the week", async () => {
    const company = grownCompany();
    mount("#/company/atlas", <Atlas />, company);
    expect(await screen.findByText("The company runs on")).toBeTruthy();
    expect(screen.getAllByText(`${company.nodes.length} entries`).length).toBeGreaterThan(0);
    expect(await screen.findByText("5 in all")).toBeTruthy();
    const show = await screen.findByTitle("Show orders-db on the map");
    expect(show.textContent).toContain("A standing alarm is raised");
    expect(await screen.findByText("Recorded this week")).toBeTruthy();
    expect(screen.getByText("Where it comes from")).toBeTruthy();
  });

  test("an empty atlas says what fills it, and draws no band of zeros", async () => {
    mount("#/company/atlas", <Atlas />, { nodes: [], edges: [] });
    expect(await screen.findByText(/^Nothing in the atlas yet · It fills from/)).toBeTruthy();
    expect(screen.queryByText("The company runs on")).toBeNull();
  });
});

describe("the catalogue", () => {
  // THE ROWS GROUP BY HEALTH, THE MOST URGENT FIRST, each heading counting
  // all of its group. Mutation: order the groups as the rows arrive.
  test("groups the rows by health, the most urgent first", () => {
    const company = grownCompany();
    const bands = bandsOf(company.nodes, { alerting: 2, needs_look: 3, quiet: 30 });
    expect(bands.map((band) => band.key)).toEqual(["alerting", "needs_look", "quiet"]);
    expect(bands[0]?.total).toBe(2);
    expect(bands[0]?.rows.every((row) => row.health === "alerting")).toBe(true);
    expect(bandsOf([summary("host", "web-1")], undefined).map((band) => band.key)).toEqual([
      "quiet",
    ]);
  });

  test("lists every entry under its health and says when it is all of it", async () => {
    const company = grownCompany();
    mount("#/company/atlas", <Atlas />, { ...company, nodes: company.nodes.slice(0, 12) });
    const grid = await screen.findByRole("table", { name: /Catalogue/ });
    expect(within(grid).getByText("acme-prod")).toBeTruthy();
    expect(
      within(grid).getByText("Needs a look", { selector: "[role=rowheader] *, th *" }),
    ).toBeTruthy();
    expect(await screen.findByText("That is all of it: 12 entries")).toBeTruthy();
  });

  test("pages a large atlas in the health order", async () => {
    const { asked } = mount("#/company/atlas", <Atlas />, generatedCompany(120));
    expect(await screen.findByText("50 entries of 120 loaded")).toBeTruthy();
    expect(asked.find((a) => a.what === "atlas_entries" && !a.params.limit)?.params.sort).toBe(
      "health",
    );
  });
});

describe("the selection", () => {
  // ONE SELECTION FOR THE MAP AND THE CATALOGUE, in the address; the map's
  // side of it (the lit path and the dock) is held in `Atlas.layout.test.tsx`.
  // Mutation: keep it in the map's own state and the row below is never marked.
  test("an entry the address selects is marked in the catalogue", async () => {
    const company = grownCompany();
    mount(`#/company/atlas?sel=${company.ordersDb.id}`, <Atlas />, company);
    const grid = await screen.findByRole("table", { name: /Catalogue/ });
    const row = within(grid).getByText("orders-db").closest(".crewlet-data-grid__row");
    expect(row?.className).toContain("crewlet-data-grid__row--selected");
  });

  // A ROW OPENS ITS SHEET, and the sheet's entry is the selection, so closing
  // the sheet leaves the entry lit.
  test("pressing a row opens its sheet and selects it", async () => {
    const company = grownCompany();
    mount("#/company/atlas", <Atlas />, company);
    const grid = await screen.findByRole("table", { name: /Catalogue/ });
    const row = within(grid).getByText("orders-db").closest(".crewlet-data-grid__row")!;
    await act(async () => {
      fireEvent.click(row.querySelector("a") ?? row);
    });
    const address = decodeURIComponent(location.hash);
    expect(address).toContain(`peek=atlas:${company.ordersDb.id}`);
    expect(address).toContain(`sel=${company.ordersDb.id}`);
  });

  test("the band's pointer selects an entry on the map", async () => {
    const company = grownCompany();
    mount("#/company/atlas", <Atlas />, company);
    const show = await screen.findByTitle("Show orders-db on the map");
    await act(async () => {
      fireEvent.click(show);
    });
    expect(location.hash).toContain(`sel=${company.ordersDb.id}`);
  });
});

describe("an entry", () => {
  test("its peek names what it depends on and what depends on it", async () => {
    const company = grownCompany();
    mount(
      `#/company/atlas?peek=atlas:${company.web1.id}`,
      <AtlasPeek id={company.web1.id} />,
      company,
      (id) => (id === company.web1.id ? disputedServer(company) : undefined),
    );
    expect(await screen.findByText("web-1")).toBeTruthy();
    expect(await screen.findByText("Used by (3)")).toBeTruthy();
    expect(screen.getByText("What it touches")).toBeTruthy();
    expect(screen.getByText("Show it on the map")).toBeTruthy();
  });

  // THE TIMELINE IS THE ATLAS'S OWN LOG, asked for this entry alone.
  test("its sheet reads its recent changes from the atlas log", async () => {
    const company = grownCompany();
    const { asked } = mount(
      `#/company/atlas?peek=atlas:${company.ordersDb.id}`,
      <AtlasPeek id={company.ordersDb.id} />,
      company,
    );
    expect(await screen.findByText("Recent changes")).toBeTruthy();
    expect((await screen.findAllByText("Seen by its source")).length).toBeGreaterThan(0);
    expect(asked.find((a) => a.what === "atlas_log")?.params).toEqual({ id: company.ordersDb.id });
  });

  // A DISAGREEMENT IS SHOWN, NEVER SETTLED: both values, who said each, and
  // that the entry waits for a person. Mutation: drop the others and it goes red.
  test("its Sources tab shows a disagreement with both values and leaves it to a person", async () => {
    const company = grownCompany();
    mount(
      `#/company/atlas/${company.web1.id}?tab=sources`,
      <EntryPage id={company.web1.id} />,
      company,
      (id) => (id === company.web1.id ? disputedServer(company) : undefined),
    );
    const section = await screen.findByRole("region", { name: "Where the sources disagree" });
    expect(within(section).getByText("10.0.4.12")).toBeTruthy();
    expect(within(section).getByText("10.0.4.19")).toBeTruthy();
    expect(within(section).getByText(/waits for a person to decide/)).toBeTruthy();
    expect(within(section).getAllByText("Internal").length).toBeGreaterThan(0);
  });

  test("its Overview gives each fact its source and confidence word, an internal address marked", async () => {
    const company = grownCompany();
    mount(
      `#/company/atlas/${company.web1.id}`,
      <EntryPage id={company.web1.id} />,
      company,
      (id) => (id === company.web1.id ? disputedServer(company) : undefined),
    );
    expect(await screen.findByText("10.0.4.12")).toBeTruthy();
    expect(screen.getAllByText("Internal").length).toBeGreaterThan(0);
    expect(screen.getAllByText(/seen by AWS/).length).toBeGreaterThan(0);
    expect(screen.queryByRole("button", { name: /export|print|share/i })).toBeNull();
  });
});
