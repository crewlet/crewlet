/**
 * Company › Atlas: what the screens ask the engine for and what they say.
 * The map's drawing is measured in a real browser (`Atlas.layout.test.tsx`);
 * these suites hold the words, the filters, the list, the peek and an
 * entry's page.
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
  disputedServer,
  entriesAnswer,
  entryAnswer,
  facetsAnswer,
  generatedCompany,
  grownCompany,
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
import { cardOf, edgesOf } from "./AtlasMap.tsx";
import { queryParams } from "./filters.ts";
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
      case "atlas_map":
        return Promise.resolve(mapAnswer(fixture));
      case "atlas_entries":
        return Promise.resolve(entriesAnswer(fixture));
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

describe("the map's cards and edges", () => {
  test("a card carries its health as a dot with its word, and an unconfirmed entry a dashed edge", () => {
    const alerting = cardOf(
      summary("datastore", "orders-db", { health: "alerting" }),
      Date.parse("2026-11-11T10:00:00Z"),
    );
    expect(alerting.attention).toEqual({ tone: "danger", label: "Alerting" });
    expect(alerting.kind).toBe("Database");
    const quiet = cardOf(summary("host", "web-1", { word: "unconfirmed" }), 0);
    expect(quiet.attention).toBeUndefined();
    expect(quiet.unconfirmed).toBe(true);
    const stale = cardOf(
      summary("host", "web-9", { state: "stale", last_seen: "2026-11-02T10:00:00Z" }),
      Date.parse("2026-11-11T10:00:00Z"),
    );
    expect(stale.caption).toMatch(/^Last seen /);
  });

  // A SELECTED ENTRY LIGHTS ITS OTHER RELATIONS, never drawn at rest, and only
  // between cards the map draws. Mutation: drop `atRest: false` and they draw at rest.
  test("a selection adds its relations not drawn at rest, between drawn cards only", () => {
    const company = grownCompany();
    const answer = mapAnswer(company);
    const atRest = edgesOf(answer, null);
    expect(atRest.every((edge) => edge.atRest !== false)).toBe(true);
    const selected = disputedServer(company);
    const lit = edgesOf(answer, selected);
    const added = lit.filter((edge) => edge.atRest === false);
    expect(added.map((edge) => edge.label)).toEqual(["belongs to"]);
    const gone = edgesOf(mapAnswer(company, { closed: ["platforms/aws"] }), selected);
    expect(gone.some((edge) => edge.atRest === false)).toBe(false);
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

  test("the coverage line says how many entries and where from, and no word of complete", async () => {
    mount("#/company/atlas", <Atlas />, sparseCompany());
    const line = await screen.findByText("5 entries, from GitHub and Anthropic");
    expect(line).toBeTruthy();
    expect(document.body.textContent).not.toMatch(/complete/i);
  });

  test("a kind menu offers the kinds with entries, each with its count", async () => {
    mount("#/company/atlas?kind=host", <Atlas />, grownCompany());
    const kind = await screen.findByRole("combobox", { name: "Kind" });
    expect(kind.textContent).toContain("Server");
  });
});

describe("the list", () => {
  test("is one press away, a row per entry, and says when it is all of it", async () => {
    const company = grownCompany();
    mount("#/company/atlas?shape=list", <Atlas />, {
      ...company,
      nodes: company.nodes.slice(0, 8),
    });
    const grid = await screen.findByRole("table", { name: "Atlas entries" });
    expect(within(grid).getByText("acme-prod")).toBeTruthy();
    expect(await screen.findByText("That is all of it: 8 entries")).toBeTruthy();
  });

  test("pages a large atlas, and asks in the order chosen", async () => {
    const { asked } = mount(
      "#/company/atlas?shape=list&order=name",
      <Atlas />,
      generatedCompany(120),
    );
    expect(await screen.findByText("50 entries of 120 loaded")).toBeTruthy();
    expect(asked.find((a) => a.what === "atlas_entries")?.params.sort).toBe("name");
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
    expect(screen.getByText("Show what depends on it on the map")).toBeTruthy();
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
    expect(screen.getByText(/seen by AWS/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /export|print|share/i })).toBeNull();
  });
});
