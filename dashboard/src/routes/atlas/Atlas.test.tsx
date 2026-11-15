/**
 * Company › Atlas: what the screens ask the engine for and what they say.
 * The map's drawing is measured in a real browser (`Atlas.layout.test.tsx`);
 * these suites hold the words, the band, the filters, the one selection and
 * search the map and the catalogue share, the sheet and an entry's page.
 */

import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, test } from "vitest";
import {
  disputedServer,
  entryAnswer,
  fact,
  generatedCompany,
  grownCompany,
  mapAnswer,
  sparseCompany,
  summary,
  traceAnswer,
  type AtlasFixture,
} from "~/test/atlas.ts";
import { mountAtlas } from "~/test/atlasMount.tsx";
import type { AtlasEntryAnswer } from "~/contract/atlas.ts";
import { Atlas } from "./Atlas.tsx";
import { AtlasPeek } from "./AtlasPeek.tsx";
import { EntryPage } from "./EntryPage.tsx";
import { pathSentence, serviceColumns } from "./AtlasMap.tsx";
import { bandsOf } from "./Catalogue.tsx";
import { queryParams } from "./filters.ts";
import { Signals } from "./entry.tsx";
import { cardOf, lineOf } from "./graph.tsx";
import { neighbourhood, relatedOf, touching } from "./Neighbourhood.tsx";
import { coverageLine, coverageNotes } from "./parts.tsx";
import { factLabel } from "./words.ts";

afterEach(() => {
  cleanup();
  location.hash = "";
});

/** Mount `children` over a socket that answers the atlas questions for `fixture`, recording each ask. */
function mount(
  hash: string,
  children: React.ReactNode,
  fixture: AtlasFixture,
  entry: (id: string) => AtlasEntryAnswer | undefined = () => undefined,
) {
  return mountAtlas(hash, children, fixture, { entry });
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

describe("a fact of each host", () => {
  // A MIRRORED REPOSITORY HAS A VISIBILITY ON EACH HOST, and each row says
  // whose it is: one "Visibility" row with two values would read as a
  // disagreement, which it is not.
  test("is named by its host", () => {
    const of = (key: string) => ({
      fact: "visibility" as const,
      of: { kind: "repository" as const, key },
    });
    expect(factLabel(of("github.com/acme/api"))).toBe("Visibility on GitHub");
    expect(factLabel(of("gitlet/api"))).toBe("Visibility in Gitlet");
    expect(factLabel(of("gitlab.com/acme/api"))).toBe("Visibility on GitLab");
    expect(factLabel(of("git.acme.example/acme/api"))).toBe("Visibility on git.acme.example");
    expect(factLabel({ fact: "default_branch" })).toBe("Default branch");
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

  // A SERVICE ON NO CALL CHAIN NAMES NO COLUMN, so the map flows it across the
  // lane as it flows every lane. Mutation: give every service its depth again
  // (0 for one nothing calls) and every lone service stands in column 0.
  test("a service that calls no service and that none calls names no column", () => {
    const company = grownCompany();
    const lone = summary("service", "lone-svc");
    const columns = serviceColumns(mapAnswer({ ...company, nodes: [...company.nodes, lone] }));
    expect(columns.has(lone.id)).toBe(false);
    expect(columns.has(company.checkout.id)).toBe(true);
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

  // ONE ENTRY, ONE COUNT OF EACH THING: the sentence over the diagram counts
  // its clusters, and the lists on the sheet and the page list the same
  // groups. Mutation: list every outgoing relation under "It relies on" again
  // (around it included) and the lists stop matching the sentence.
  test("the lists of what an entry touches are the groups its diagram draws and its sentence counts", () => {
    const company = grownCompany();
    for (const entry of [company.ordersDb, company.web1, company.checkout]) {
      const down = traceAnswer(company, entry, "down", 1);
      const up = traceAnswer(company, entry, "up", 1);
      const counted = touching(neighbourhood(entry, down, up, 0).nodes);
      const listed = relatedOf(entry.id, down, up);
      expect(
        {
          usedBy: listed.reliedOnBy.length,
          reliesOn: listed.reliesOn.length,
          around: listed.around.length,
        },
        entry.name,
      ).toEqual(counted);
    }
    const db = relatedOf(
      company.ordersDb.id,
      traceAnswer(company, company.ordersDb, "down", 1),
      traceAnswer(company, company.ordersDb, "up", 1),
    );
    // What stands around is never something it relies on.
    expect(db.reliesOn.map((one) => one.entry.name)).not.toContain("orders-db cpu high");
    expect(db.around.map((one) => one.entry.name)).toContain("orders-db cpu high");
    expect(db.reliedOnBy.find((one) => one.entry.name === "orders-api")?.relation).toMatch(/ it$/);
  });

  // EVERY HOP IS SAID: the dock counts the path the map lights, which is a
  // different question from one step out, so it says which it answers.
  test("the dock's counts name every hop, in the verb one step out uses", () => {
    expect(pathSentence(2, 13)).toBe("Over every hop, 2 entries rely on it and it relies on 13");
    expect(pathSentence(1, 0)).toBe(
      "Over every hop, 1 entry relies on it and it relies on nothing",
    );
  });

  // NONE IS SAID AS NOTHING in the dock as on the entry's own page, never
  // "0 entries rely on it".
  test("the dock says nothing where nothing relies on the entry", () => {
    expect(pathSentence(0, 9)).toBe("Over every hop, nothing relies on it and it relies on 9");
    expect(pathSentence(0, 0)).toBe(
      "Over every hop, nothing relies on it and it relies on nothing",
    );
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
      show: "",
    } as const;
    expect(queryParams(none)).toEqual({});
    expect(queryParams({ ...none, show: "retired" })).toEqual({ show: "retired" });
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

  // A RETIRED ENTRY IS FOUND AGAIN where the Retire dialog says it is: every
  // read leaves the retired out, and the engine lists them alone when asked.
  // Mutation: drop the switch and a retired entry is reachable only by its
  // old address.
  test("Show retired entries only asks the engine for the retired, and says so as a chip", async () => {
    const { asked } = mount("#/company/atlas", <Atlas />, grownCompany());
    const control = await screen.findByRole("button", { name: "Filter" });
    await act(async () => {
      fireEvent.click(control);
    });
    const toggle = await screen.findByRole("switch", { name: "Show retired entries only" });
    expect((toggle as HTMLInputElement).checked).toBe(false);
    expect(asked.every((a) => !("show" in a.params))).toBe(true);
    await act(async () => {
      fireEvent.click(toggle);
    });
    expect(location.hash).toContain("show=retired");
    for (const what of ["atlas_map", "atlas_entries"]) {
      expect(
        asked.some((a) => a.what === what && a.params.show === "retired"),
        what,
      ).toBe(true);
    }
    const chips = screen.getByRole("group", { name: "Filters in force" });
    expect(chips.textContent).toContain("Retired entries only");
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

  // A VALUE THIS BUILD DOES NOT KNOW IS LEFT OUT, never a crash and never a
  // question the engine refuses: a stale bookmark, a newer build's value or a
  // typo. Mutation: cast the address values again, and the chips index the
  // word tables with "bogus" and take the screen down.
  test("an unknown value in the address is left out, said once, and removed on request", async () => {
    const { asked } = mount(
      "#/company/atlas?lane=bogus&kind=nope&vendor=nope&health=nope&confidence=nope",
      <Atlas />,
      grownCompany(),
    );
    const note = await screen.findByText("Part of this address is not applied");
    const callout = note.closest(".crewlet-callout") as HTMLElement;
    expect(callout.textContent).toContain(
      "It asks for the kind “nope”, the lane “bogus”, the vendor “nope”, the confidence “nope” and the health “nope”",
    );
    expect(screen.queryByText(/could not be drawn/)).toBeNull();
    // No chip for what is left out, and nothing of it sent to the engine.
    expect(screen.queryByRole("group", { name: "Filters in force" })).toBeNull();
    const sent = asked.filter((a) => a.what === "atlas_map" || a.what === "atlas_entries");
    expect(sent.length).toBeGreaterThan(0);
    for (const one of sent) {
      for (const key of ["lane", "kind", "vendor", "health", "confidence"]) {
        expect(one.params).not.toHaveProperty(key);
      }
    }
    await act(async () => {
      fireEvent.click(within(callout).getByRole("button", { name: "Remove it" }));
    });
    for (const key of ["lane=", "kind=", "vendor=", "health=", "confidence="]) {
      expect(location.hash).not.toContain(key);
    }
    expect(screen.queryByText("Part of this address is not applied")).toBeNull();
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
    const show = await screen.findByRole("button", {
      name: /^orders-db(?! cpu).*: show it on the map$/,
    });
    expect(show.textContent).toContain("A standing alarm is raised");
    // THE WHOLE SENTENCE IS REACHABLE where two lines do not hold it: in the
    // button's own name (and the kit's tooltip), never a native title.
    const reason = show.querySelector(".clamp")!;
    expect(reason.hasAttribute("title")).toBe(false);
    expect(show.textContent).toContain(reason.textContent ?? "");
    expect(await screen.findByText("Recorded this week")).toBeTruthy();
    expect(screen.getByText("Where it comes from")).toBeTruthy();
  });

  // MUTATION: draw the count as plain words again and the entries past the
  // two cards are nowhere a reader can go.
  test("how many need attention is a link per health to the catalogue narrowed to it", async () => {
    const company = grownCompany();
    mount("#/company/atlas?q=orders", <Atlas />, company);
    const wrong = company.nodes.filter((node) => node.health !== "quiet");
    expect(wrong.length).toBe(5);
    await screen.findByText("Needs attention now");
    let counted = 0;
    for (const [health, label] of [
      ["alerting", "Alerting"],
      ["needs_look", "Needs a look"],
    ] as const) {
      const n = wrong.filter((node) => node.health === health).length;
      if (n === 0) continue;
      // NAMED BY WHAT IT SHOWS FIRST (WCAG 2.5.3): "Alerting 4" is what a
      // person speaking to the page says. Mutation: name it by its count first.
      const link = await screen.findByRole("link", {
        name: `${label} ${n}: show them in the catalogue`,
      });
      // The health alone: the band is about the company, so the search is not carried.
      expect(link.getAttribute("href")).toBe(`#/company/atlas?health=${health}`);
      counted += n;
    }
    expect(counted).toBe(5);
    expect(screen.queryByText(/in all$/)).toBeNull();
  });

  // MUTATION: read "the list has not answered" as an empty list and the tile
  // says nothing needs attention under a count that says five do.
  test("the tile never says nothing needs attention while its list is still being read", async () => {
    const company = grownCompany();
    mountAtlas("#/company/atlas", <Atlas />, company, {
      answers: { atlas_entries: new Promise(() => {}) },
    });
    await screen.findByRole("link", { name: /^Alerting \d+: show them in the catalogue$/ });
    expect(screen.getByText("Reading what needs attention")).toBeTruthy();
    expect(screen.queryByText("Nothing needs attention.")).toBeNull();
  });

  test("a company with nothing wrong is told so, once the list has answered", async () => {
    const company = grownCompany();
    const calm = { ...company, nodes: company.nodes.map((node) => ({ ...node, health: "quiet" })) };
    mount("#/company/atlas", <Atlas />, calm as typeof company);
    expect(await screen.findByText("Nothing needs attention.")).toBeTruthy();
    expect(screen.queryByRole("link", { name: /show them in the catalogue/ })).toBeNull();
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
    const show = await screen.findByRole("button", {
      name: /^orders-db(?! cpu).*: show it on the map$/,
    });
    await act(async () => {
      fireEvent.click(show);
    });
    expect(location.hash).toContain(`sel=${company.ordersDb.id}`);
  });

  // THE WHOLE REASON IS REACHABLE: the card is named by its own words, the
  // reason whole among them, never kept in a native title a keyboard or a
  // finger cannot reach. Mutation: put the reason back in a title.
  test("a Needs attention card says its whole reason in its name, with no native title", async () => {
    const company = grownCompany();
    mount("#/company/atlas", <Atlas />, company);
    const show = await screen.findByRole("button", {
      name: /^orders-db(?! cpu).*: show it on the map$/,
    });
    const reason = company.ordersDb.reason ?? "";
    expect(reason).not.toBe("");
    expect(show.textContent).toContain(reason);
    expect(show.hasAttribute("title")).toBe(false);
    expect(show.querySelector("[title]")).toBeNull();
  });
});

describe("an entry", () => {
  test("its peek lists what relies on it, what it relies on and what stands around it", async () => {
    const company = grownCompany();
    mount(
      `#/company/atlas?peek=atlas:${company.web1.id}`,
      <AtlasPeek id={company.web1.id} />,
      company,
      (id) => (id === company.web1.id ? disputedServer(company) : undefined),
    );
    expect(await screen.findByText("web-1")).toBeTruthy();
    const listed = relatedOf(
      company.web1.id,
      traceAnswer(company, company.web1, "down", 1),
      traceAnswer(company, company.web1, "up", 1),
    );
    expect(listed.reliedOnBy.length).toBeGreaterThan(0);
    expect(await screen.findByText(`Relies on it (${listed.reliedOnBy.length})`)).toBeTruthy();
    expect(screen.getByText(`It relies on (${listed.reliesOn.length})`)).toBeTruthy();
    expect(screen.getByText(`Around it (${listed.around.length})`)).toBeTruthy();
    expect(screen.getByText("What it touches")).toBeTruthy();
    expect(screen.getByText("Show it on the map")).toBeTruthy();
  });

  // AN EMPTY SIGNALS LIST SAYS WHAT IS EMPTY, never "open", the word the Work
  // tab counts tasks in: "Nothing open on this entry" stood beside "2 open
  // tasks name this entry". Mutation: put the old sentence back.
  test("an entry with no signals says nothing is wrong with it, never that nothing is open", () => {
    const { entry } = entryAnswer(grownCompany().checkout);
    render(<Signals entry={{ ...entry, signals: [], review: false }} now={0} />);
    expect(
      screen.getByText("Nothing is wrong with it: no source reports a signal on it."),
    ).toBeTruthy();
    expect(screen.queryByText(/open/i)).toBeNull();
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

  // WHAT A REPOSITORY'S FILES DECLARE IS LIKELY, and named for the repository
  // whose files said it. Mutation: drop the code case from sourceName and the
  // by-line reads the bare repository name.
  test("a fact a repository's files declared names the repository, as likely", async () => {
    const company = grownCompany();
    const declared = entryAnswer(company.ordersDb, {
      facts: [
        fact(
          "engine",
          { type: "word", text: "postgres" },
          { source: { kind: "code", name: "shop" }, confidence: "inferred", word: "likely" },
        ),
      ],
      entry: {
        state: "discovered",
        confidence: "inferred",
        word: "likely",
        sources: [{ kind: "code", name: "shop" }],
      },
    });
    const answer = (id: string) => (id === company.ordersDb.id ? declared : undefined);
    mount(
      `#/company/atlas/${company.ordersDb.id}`,
      <EntryPage id={company.ordersDb.id} />,
      company,
      answer,
    );
    expect((await screen.findAllByText(/set by the files of shop/)).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/likely/i).length).toBeGreaterThan(0);
    cleanup();
    mount(
      `#/company/atlas/${company.ordersDb.id}?tab=sources`,
      <EntryPage id={company.ordersDb.id} />,
      company,
      answer,
    );
    expect(await screen.findByText("The files of shop")).toBeTruthy();
    expect(screen.getByText("A repository's deploy and dependency files")).toBeTruthy();
  });
});
