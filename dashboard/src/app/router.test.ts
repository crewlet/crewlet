/**
 * The router's rules, which are invisible in a URL — and the NAVIGATION
 * GRAMMAR and the ROUTE TABLE, which are invisible everywhere.
 *
 * Both of the router's rules shipped wrong once and neither shows up in an
 * address bar: Back from a screen's fourth lens left the screen entirely, and
 * Back to a list somebody had scrolled halfway down landed at the top.
 *
 * The grammar is the half that decays: a sidebar row is a workspace or a kept
 * object, a section is a PATH inside a workspace, an object tab is a `tab=` on
 * an object's own page. Nothing is two of those, and it is lost one pull
 * request at a time. The route table is the half nothing compiles: the design
 * doc's `### The routes` is held against `routes.ts`'s resolver in both
 * directions.
 */

// @vitest-environment node

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";
import { buildHash, parseHash, samePath } from "./router.tsx";
import {
  DESTINATIONS,
  RESERVED_SEGMENTS,
  WORKSPACES,
  sectionOf,
  workspaceOf,
  workspaceRow,
} from "./nav.ts";
import { crumbsFor, titleOf } from "./crumbs.ts";
import { CONTAINER_SHAPE, ITEM_KEY_SHAPE, KEY_SHAPE, resolve, resolves } from "./routes.ts";
import { segmentsOf } from "~/test/routes.ts";

describe("parsing", () => {
  test("a bare hash is the overview", () => {
    expect(parseHash("#/").path).toEqual([]);
    expect(parseHash("").path).toEqual([]);
  });

  test("path segments are decoded", () => {
    const route = parseHash("#/seats/product%20manager?tab=model");
    expect(route.path).toEqual(["seats", "product manager"]);
    expect(route.query.get("tab")).toBe("model");
  });

  test("a hand-edited URL with a stray %% lands on a screen rather than throwing", () => {
    // Routing must not be the thing that fails: a bad escape should reach a
    // screen that says so, not a blank page.
    expect(() => parseHash("#/seats/100%")).not.toThrow();
    expect(parseHash("#/seats/100%").path).toEqual(["seats", "100%"]);
  });

  test("building and parsing round-trip", () => {
    const hash = buildHash(["seats", "a b"], { tab: "cost" });
    expect(parseHash(hash).path).toEqual(["seats", "a b"]);
    expect(parseHash(hash).query.get("tab")).toBe("cost");
  });

  test("an empty query value is omitted rather than written as a bare key", () => {
    // A filter cleared to "" means "no filter", and `?actor=` in a URL is a
    // filter for the empty actor.
    expect(buildHash(["live"], { actor: "" })).toBe("#/live");
  });
});

describe("navigation identity", () => {
  // A DETAIL KEEPS THE READER'S PLACE IN THE SIDEBAR. Otherwise opening one
  // event loses the mark on the workspace you came from, which reads as
  // having navigated somewhere unrelated.
  test("a detail resolves to the workspace that holds it", () => {
    expect(workspaceOf(["agents", "seats", "pm"])).toBe("agents");
    expect(workspaceOf(["live", "turns", "abc"])).toBe("live");
    expect(workspaceOf(["settings", "backups", "tracker"])).toBe("settings");
    expect(workspaceOf(["work", "ENG-42"])).toBe("work");
    // THE EMPTY FRAGMENT IS HOME, the landing screen.
    expect(workspaceOf([])).toBe("home");
  });

  // A ROUTE NOTHING OWNS RESOLVES TO NOTHING rather than to the first row: a
  // sidebar that marked a workspace for a path it does not hold would tell the
  // reader they are somewhere they are not. The retired heads are exactly that.
  test("an unowned route marks no workspace, the retired heads included", () => {
    for (const head of ["nowhere", "company", "activity", "cost", "admin", "pages"]) {
      expect(workspaceOf([head]), head).toBe("");
    }
  });

  test("every destination says what it answers", () => {
    // The hint is what the command palette shows. An entry with none is an
    // entry a reader has to click to understand.
    for (const d of DESTINATIONS) {
      expect(d.hint.length, d.key).toBeGreaterThan(10);
    }
  });

  test("every workspace has its own jump chord, and they are the documented ones", () => {
    // `g` then a letter. Two rows sharing one makes the second unreachable
    // by keyboard, silently.
    const chords = WORKSPACES.map((r) => r.chord);
    expect(new Set(chords).size).toBe(chords.length);
    expect(Object.fromEntries(WORKSPACES.map((r) => [r.key, r.chord]))).toEqual({
      home: "h",
      inbox: "i",
      me: "m",
      work: "w",
      agents: "a",
      live: "l",
      knowledge: "k",
      spend: "t",
      settings: "s",
    });
  });
});

describe("am I already here", () => {
  // For the links a component draws without knowing where it is rendered. A
  // phase card carries "event →" to its own event: a way out on the turn and
  // on the seat, and on that event's own page a link back to itself — the
  // reader clicks, the URL does not change, nothing moves, and the only thing
  // they learn is that the control was a lie.

  test("the same page is the same page", () => {
    expect(samePath(parseHash("#/live/events/abc").path, ["live", "events", "abc"])).toBe(true);
  });

  test("a different id is a different page", () => {
    expect(samePath(parseHash("#/live/events/abc").path, ["live", "events", "def"])).toBe(false);
  });

  test("a prefix is not a match", () => {
    // `#/live/events` lists events; `#/live/events/abc` is one of them. A link from the
    // list to a row must not be suppressed as a self-link.
    expect(samePath(parseHash("#/live/events").path, ["live", "events", "abc"])).toBe(false);
    expect(samePath(parseHash("#/live/events/abc").path, ["live", "events"])).toBe(false);
  });

  test("a query string does not make it a different page", () => {
    // A filter or a tab the reader happens to have on the URL is still the
    // same page, and a self-link is still a loop.
    expect(
      samePath(parseHash("#/live/events/abc?category=system").path, ["live", "events", "abc"]),
    ).toBe(true);
  });

  test("segments are compared as segments, never as a joined string", () => {
    // A join makes `["events", "a/b"]` and `["events", "a", "b"]` equal, and
    // an id is not a path.
    expect(samePath(["events", "a/b"], ["events", "a", "b"])).toBe(false);
  });
});

describe("the navigation grammar", () => {
  // NINE WORKSPACES, and the sidebar draws them and nothing else from the
  // table: a section that became a sidebar row is a second column of
  // navigation arriving one row at a time.
  test("the sidebar's rows are the nine workspaces, in the story's order", () => {
    expect(WORKSPACES.map((w) => w.key)).toEqual([
      "home",
      "inbox",
      "me",
      "work",
      "agents",
      "live",
      "knowledge",
      "spend",
      "settings",
    ]);
  });

  // A SECTION IS A PATH INSIDE ITS WORKSPACE, never a query and never a row:
  // it starts with the workspace's own segment, it resolves to a screen, and
  // the section that path is in is that section. A cross-link is the one row a
  // renderer draws whose address is another workspace's, and it says so.
  test("sections are paths, and never sidebar rows", () => {
    let sections = 0;
    for (const ws of WORKSPACES) {
      for (const s of ws.sections) {
        sections++;
        expect(s.path.join("/"), `${ws.key}/${s.key} carries a query`).not.toMatch(/[?=]/);
        expect(resolves(s.path), `#/${s.path.join("/")} does not resolve`).toBe(true);
        if (s.elsewhere) {
          expect(s.path[0], `${s.key} is a cross-link into its own workspace`).not.toBe(ws.key);
          continue;
        }
        expect(s.path[0], `${ws.key}/${s.key} is not under its workspace`).toBe(ws.key);
        expect(sectionOf(s.path)?.key, `#/${s.path.join("/")}`).toBe(s.key);
        expect(
          WORKSPACES.some((w) => w.key !== ws.key && w.path.join("/") === s.path.join("/")),
          `${s.key} is also a sidebar row`,
        ).toBe(false);
      }
    }
    // NOT VACUOUS: the table has sections to hold.
    expect(sections).toBeGreaterThan(25);
  });

  // EVERY DESTINATION IS ONE ADDRESS, resolved to the workspace it claims. Two
  // rows leading to one address are two names for one place, which is how a
  // reader comes to believe the second of them is broken.
  test("a destination's path resolves to a screen in the workspace it declares", () => {
    const addresses = new Set(DESTINATIONS.map((d) => d.path.join("/")));
    expect(addresses.size).toBe(DESTINATIONS.length);
    for (const d of DESTINATIONS) {
      const where = resolve(d.path);
      expect(where.resolved, `#/${d.path.join("/")}`).toBe(true);
      expect(where.workspace, d.key).toBe(d.workspace);
    }
  });

  // A RESERVED SEGMENT CANNOT COLLIDE WITH A KEY THE ENGINE MINTS. Project and
  // container keys are uppercase, item keys are `KEY-n`, everything else is a
  // uuid — and every reserved segment is lowercase. Without this,
  // `#/work/views` is a list of saved views until somebody creates a project
  // called VIEWS, and then it is a project.
  test("no reserved segment is the shape of a minted key", () => {
    for (const segment of RESERVED_SEGMENTS) {
      expect(KEY_SHAPE.test(segment), `${segment} is a project-key shape`).toBe(false);
      expect(ITEM_KEY_SHAPE.test(segment), `${segment} is an item-key shape`).toBe(false);
      expect(CONTAINER_SHAPE.test(segment), `${segment} is a container-key shape`).toBe(false);
      expect(segment).toBe(segment.toLowerCase());
    }
  });
});

describe("the resolver", () => {
  const uuid = "5f0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f";

  test("a key is a project, a key with a number or a uuid an item, and nothing else is", () => {
    expect(resolve(["work", "ENG"])).toMatchObject({ screen: "project", key: "ENG" });
    expect(resolve(["work", "ENG-42"])).toMatchObject({ screen: "item", id: "ENG-42" });
    expect(resolve(["work", uuid])).toMatchObject({ screen: "item", id: uuid });
    // A WORD IS NOT AN ITEM. Every one of these was an item id once, so each
    // drew the tracker's refusal under a trail rather than Not Found — the
    // sections the plan gives other workspaces, and any other word.
    for (const word of ["tasks", "people", "skills", "models", "diaries", "eng-42", "t1"]) {
      expect(resolve(["work", word]).resolved, word).toBe(false);
    }
  });

  test("a page has ONE address, its id — the old container/title form is gone", () => {
    expect(resolve(["knowledge", "pages", uuid])).toMatchObject({ screen: "page", id: uuid });
    expect(resolve(["knowledge", "ENG"])).toMatchObject({ screen: "container", key: "ENG" });
    expect(resolve(["knowledge", "ENG", "Deploy runbook"]).resolved).toBe(false);
    expect(resolve(["pages", uuid]).resolved).toBe(false);
  });

  // THE RETIRED HEADS ARE NOT FOUND, and there is no redirect table: a
  // redirect whose old path becomes a live route sends every reader of it
  // somewhere else, permanently.
  test("an address from before the one-sidebar rebuild is Not Found", () => {
    for (const path of [
      ["company"],
      ["company", "people", "pm"],
      ["activity", "turns"],
      ["cost", "budgets"],
      ["admin", "fleet"],
    ]) {
      expect(resolve(path).resolved, `#/${path.join("/")}`).toBe(false);
    }
  });

  // A TRACE HAS NO LIST, and the bare address says so rather than drawing the
  // turns list under a trail that names traces.
  test("a bare traces address is Not Found, and says a trace is opened from a turn", () => {
    const bare = resolve(["live", "traces"]);
    expect(bare.resolved).toBe(false);
    expect(bare.resolved ? "" : (bare.hint ?? "")).toMatch(/a trace has no list/);
    expect(resolve(["live", "traces", "abc"])).toMatchObject({ screen: "trace", id: "abc" });
  });

  // THE DISCRIMINANT IS NOT `kind`, because an integration's own parameter is:
  // spread beside a `kind` discriminant, `#/settings/integrations/github`
  // resolved to an integration called "screen".
  test("an integration's kind survives the resolver's own discriminant", () => {
    expect(resolve(["settings", "integrations", "github"])).toMatchObject({
      screen: "integrations",
      kind: "github",
    });
  });

  // A TOOL NAME IS A THIRD PARTY'S STRING, so the two shapes under `tools/`
  // are told apart by the tail's LENGTH: a tool literally called `servers`
  // keeps its page.
  test("a tool named `servers` keeps its page, and the origin filter keeps its", () => {
    expect(resolve(["settings", "tools", "servers"])).toMatchObject({
      screen: "tools",
      tool: "servers",
    });
    expect(resolve(["settings", "tools", "servers", "github"])).toMatchObject({
      screen: "tools",
      server: "github",
    });
  });

  // HANDLES LIVE ONLY UNDER `seats/`, domains only under `backups/`: a handle
  // or a domain equal to a reserved word is still that seat or that domain.
  test("a handle or a domain equal to a reserved word keeps its page", () => {
    expect(resolve(["agents", "seats", "roster"])).toMatchObject({
      screen: "seat",
      handle: "roster",
    });
    expect(resolve(["settings", "backups", "nodes"])).toMatchObject({
      screen: "backups",
      domain: "nodes",
    });
    expect(resolve(["agents", "seats"]).resolved).toBe(false);
  });

  // A RESERVED WORD THAT IS NOT A WORK SECTION IS NOTHING HERE. Read as an item
  // id it would ask the tracker for an item called `turns`.
  test("a reserved word under a workspace that does not have it is Not Found", () => {
    expect(resolve(["work", "turns"]).resolved).toBe(false);
    expect(resolve(["knowledge", "budgets"]).resolved).toBe(false);
  });

  // A CONTAINER IS WHAT THE ENGINE KEEPS, and it keeps every key uppercased —
  // so a lowercase segment under Knowledge is a section or nothing, never a
  // container. Without the shape, `#/knowledge/skills` (a section the plan
  // gives Knowledge) drew a container screen asking the engine for `skills`.
  test("a container is an uppercase key, and a lowercase word is never one", () => {
    expect(resolve(["knowledge", "ENG"])).toMatchObject({ screen: "container", key: "ENG" });
    expect(resolve(["knowledge", "2026-OFFSITE"])).toMatchObject({
      screen: "container",
      key: "2026-OFFSITE",
    });
    // `skills` is the section it was reserved for, and never a container.
    expect(resolve(["knowledge", "skills"])).toMatchObject({ screen: "skills" });
    // `diaries` is the other, and a handle under it is one agent's diary —
    // a handle lives there as it does under `seats/`, so a reserved word is one.
    expect(resolve(["knowledge", "diaries"])).toMatchObject({ screen: "diaries" });
    expect(resolve(["knowledge", "diaries", "swe"])).toMatchObject({
      screen: "diary",
      handle: "swe",
    });
    expect(resolve(["knowledge", "diaries", "skills"])).toMatchObject({
      screen: "diary",
      handle: "skills",
    });
    expect(resolve(["knowledge", "diaries", "swe", "x"]).resolved).toBe(false);
    for (const word of ["diary", "eng", "Eng"]) {
      expect(resolve(["knowledge", word]).resolved, word).toBe(false);
    }
  });

  test("a closed set refuses the obvious typo rather than drawing the list", () => {
    expect(resolve(["spend", "budget"]).resolved).toBe(false);
    expect(resolve(["settings", "config", "revision"]).resolved).toBe(false);
    expect(resolve(["settings", "audit", "x"]).resolved).toBe(false);
  });
});

describe("the breadcrumb", () => {
  // THE LAST CRUMB IS THE OBJECT and carries no link. A trail whose final
  // segment links to the page you are already on teaches a reader that the
  // control does nothing.
  test("the last crumb is never a link, and never blank", () => {
    for (const path of [
      [],
      ["inbox"],
      ["work"],
      ["work", "ENG"],
      ["work", "ENG-42"],
      ["agents", "seats", "ada"],
      ["agents", "teams", "Platform"],
      ["knowledge", "pages", "0f0f0f0f-1111-4222-8333-444455556666"],
      ["live", "turns", "t-1"],
      ["settings", "config", "revisions", "r-1"],
      ["settings", "backups", "tracker"],
      // THE TWO-SEGMENT FORMS, which are live addresses and were the ones
      // with the blank crumb: the list of revisions, and the page of a tool
      // that happens to be called `servers`.
      ["settings", "config", "revisions"],
      ["settings", "tools", "servers"],
      ["company", "people"],
    ]) {
      const crumbs = crumbsFor(path);
      expect(crumbs.length, `#/${path.join("/")} has no crumbs`).toBeGreaterThan(0);
      expect(crumbs[crumbs.length - 1]?.path, `#/${path.join("/")}`).toBeUndefined();
      expect(
        String(crumbs[crumbs.length - 1]?.label ?? "").trim(),
        `#/${path.join("/")} ends in an empty crumb`,
      ).not.toBe("");
    }
  });

  // AND EVERY OTHER CRUMB IS ONE, or the trail is a label rather than an
  // address: a reader two levels into a project has to be able to step back
  // out through the trail.
  test("every crumb but the last carries a path, and the first is the workspace with its glyph", () => {
    const crumbs = crumbsFor(["work", "ENG-42"]);
    expect(crumbs.map((c) => c.label)).toEqual(["Work", "ENG", "ENG-42"]);
    for (const crumb of crumbs.slice(0, -1)) {
      expect(crumb.path, `${crumb.label} is not a link`).toBeTruthy();
    }
    expect(crumbs[0]?.icon).toBe(workspaceRow("work")?.icon);
  });

  // A SECTION FOLLOWS ITS WORKSPACE, unless it IS the landing page: the chart
  // is "Agents", the roster "Agents › Roster".
  test("a section is named after its workspace, and the landing section is not", () => {
    expect(crumbsFor(["agents"]).map((c) => c.label)).toEqual(["Agents"]);
    expect(crumbsFor(["agents", "roster"]).map((c) => c.label)).toEqual(["Agents", "Roster"]);
    expect(crumbsFor(["settings", "integrations"]).map((c) => c.label)).toEqual([
      "Settings",
      "Integrations",
    ]);
  });

  // EXCEPT SETTINGS', whose landing is one of the column's sections: the
  // charter's page said only "Settings", and nothing but the column's selected
  // row said it was General.
  test("Settings' landing is named General in the trail", () => {
    const crumbs = crumbsFor(["settings"]);
    expect(crumbs.map((c) => c.label)).toEqual(["Settings", "General"]);
    expect(titleOf(crumbs)).toBe("General");
  });

  test("a path the table does not have is named Not Found, under its workspace if any", () => {
    expect(crumbsFor(["live", "traces"]).map((c) => c.label)).toEqual(["Live", "Not found"]);
    expect(crumbsFor(["company"]).map((c) => c.label)).toEqual(["Not found"]);
  });

  // A LABEL A SCREEN RESOLVED WINS over the raw segment, and the raw segment
  // is what shows until it does — never a spinner in the chrome.
  test("a screen's own labels replace the segments", () => {
    const raw = crumbsFor(["work", "ENG"]);
    expect(raw[raw.length - 1]?.label).toBe("ENG");
    const named = crumbsFor(["work", "ENG"], { ENG: "Platform" });
    expect(named[named.length - 1]?.label).toBe("Platform");
    // A NAMED PROJECT CARRIES ITS KEY AS A CHIP; the raw key carries none, or
    // it would say the key twice.
    expect(named[named.length - 1]?.tag).toBe("ENG");
    expect(raw[raw.length - 1]?.tag).toBeUndefined();
  });

  // A SEAT IS NAMED BY ITS NAME AND ADDRESSED BY A SLUG, so an unresolved
  // handle is drawn in the mono face and a resolved name is not.
  test("a seat's crumb is its name, and an unnamed handle is an identifier", () => {
    const raw = crumbsFor(["agents", "seats", "agent-cto"]);
    expect(raw[raw.length - 1]?.label).toBe("agent-cto");
    expect(raw[raw.length - 1]?.mono, "a slug drawn as prose reads as a name").toBe(true);
    const named = crumbsFor(["agents", "seats", "agent-cto"], {
      "agent-cto": "Chief Technology Officer",
    });
    expect(named[named.length - 1]?.label).toBe("Chief Technology Officer");
    expect(named[named.length - 1]?.mono, "a name is not an identifier").toBeFalsy();
  });

  // A UNIT'S SEGMENT IS ITS KEY, so it follows the seat's rule rather than
  // being drawn as prose: `platform-eng` is an address until the screen
  // publishes the name it resolved it to. It was the unit's NAME once, which
  // two units may share — keyed on it, two teams shared one page.
  test("a unit's crumb is its name, and an unnamed key is an identifier", () => {
    const raw = crumbsFor(["agents", "teams", "platform-eng"]);
    expect(raw[raw.length - 1]?.label).toBe("platform-eng");
    expect(raw[raw.length - 1]?.mono, "a key drawn as prose reads as a name").toBe(true);
    const named = crumbsFor(["agents", "teams", "platform-eng"], {
      "platform-eng": "Platform",
    });
    expect(named[named.length - 1]?.label).toBe("Platform");
    expect(named[named.length - 1]?.mono, "a name is not an identifier").toBeFalsy();
  });

  // A SEGMENT THAT IS ITS OBJECT'S NAME IS DRAWN AS ONE. A node, a secret, a
  // server and a backup domain are each titled on their page by this same
  // word in the body face; the trail fell back to the mono face meant for a
  // key still awaiting its name, and no screen ever supplies one — so the
  // crumb printed `harness-0` in one face over a title in another.
  test("a node, a secret, a server, a domain and a model are named in the trail as their pages title them", () => {
    for (const path of [
      ["settings", "nodes", "harness-0"],
      ["settings", "secrets", "GITHUB_TOKEN"],
      ["settings", "tools", "servers", "github"],
      ["settings", "backups", "tracker"],
      ["settings", "models", "claude-opus"],
    ]) {
      const crumbs = crumbsFor(path);
      const last = crumbs[crumbs.length - 1];
      expect(last?.label, path.join("/")).toBe(path[path.length - 1]);
      expect(last?.mono, `${path.join("/")} is its own name, not a key`).toBeFalsy();
    }
  });

  // THE TAB TITLE COMES FROM THE SAME TRAIL, so a reader with four tabs open
  // can tell them apart.
  test("the tab title is the last crumb", () => {
    expect(titleOf(crumbsFor(["work", "ENG-42"], { "ENG-42": "Fix the applier" }))).toBe(
      "Fix the applier",
    );
    expect(titleOf(crumbsFor(["live", "turns"]))).toBe("Turns");
  });
});

// EVERY FIXED DESTINATION NAMES ITSELF IN THE TRAIL, from the one table — a
// section that fell through to an object branch read as "Work / Item /
// search", a trail naming a page that does not exist.
//
// A COLUMN'S LANDING NAMES ITS SECTION: Settings draws General as a row of its
// column like any other, so its page is "Settings › General" while the
// palette's destination is still the workspace.
test("every fixed destination names itself rather than reading as a key", () => {
  for (const dest of DESTINATIONS) {
    const crumbs = crumbsFor(dest.path);
    const last = crumbs[crumbs.length - 1];
    const row = workspaceRow(dest.workspace)!;
    const landing =
      row.renderer === "column" && samePath(dest.path, row.path)
        ? row.sections.find((s) => samePath(s.path, row.path))
        : undefined;
    expect(last?.label, `#/${dest.path.join("/")}`).toBe(landing?.label ?? dest.label);
    expect(last?.path, `#/${dest.path.join("/")} links to itself`).toBeUndefined();
  }
});

/**
 * HOW DEEP THE FIXED PART OF A TRAIL GOES, which is a fact about `crumbs.ts`
 * the STYLESHEET is written against: `.crumbs` floors the trail and scrolls
 * past it, and the note on that rule is about the deepest fixed ancestry. The
 * width is not assertable here — jsdom has no fonts — but the shape is.
 */
test("three fixed ancestors is the deepest trail the route table produces", () => {
  const three: [string[], string[]][] = [
    [
      ["settings", "config", "revisions", "01JCFG"],
      ["Settings", "Configuration", "Revisions"],
    ],
    [
      ["settings", "tools", "servers", "github"],
      ["Settings", "Tools & MCP", "Servers"],
    ],
  ];
  for (const [path, ancestry] of three) {
    expect(
      crumbsFor(path)
        .slice(0, -1)
        .map((c) => c.label),
      `#/${path.join("/")}`,
    ).toEqual(ancestry);
  }
  // AND NOTHING IS DEEPER, walked from the tables rather than listed — with
  // each reserved segment under every destination, because those are what the
  // deep branches are made of.
  const under: string[][] = [["x"], ["x", "y"]];
  for (const seg of RESERVED_SEGMENTS) under.push([seg], [seg, "x"], [seg, "x", "y"]);
  for (const dest of DESTINATIONS) {
    for (const tail of under) {
      const path = [...dest.path, ...tail];
      expect(
        crumbsFor(path).length,
        `#/${path.join("/")} is deeper than the trail's floor was measured against`,
      ).toBeLessThanOrEqual(4);
    }
  }
});

/**
 * THE DESIGN DOC'S ROUTE TABLE AND THE RESOLVER SAY THE SAME THING.
 *
 * `docs/reference/dashboard-design.md` carries the product's information
 * architecture as a table of addresses, and it is the half of the IA nothing
 * compiles: a route the resolver draws and the table leaves out is invisible
 * to every reader who starts from the document, and a row in the table whose
 * address nothing resolves is a promise the product does not keep. Both have
 * happened: `#/activity/traces` was dispatched, screened and breadcrumbed
 * while the word "traces" appeared nowhere in the document, and `audit` sat
 * reserved while no route took it.
 *
 * So each documented address — its placeholders filled with a value of the
 * shape they name — must RESOLVE to a screen, and every workspace, section and
 * destination the code declares must be written down.
 */
describe("the information architecture", () => {
  const DESIGN_DOC = fileURLToPath(
    new URL("../../../docs/reference/dashboard-design.md", import.meta.url),
  );
  const HEADING = "### The routes";

  /** The lines of the route table's own section, and no other section's. */
  function region(): string[] {
    const lines = readFileSync(DESIGN_DOC, "utf8").split("\n");
    const start = lines.findIndex((line) => line.trim() === HEADING);
    // A GUARD THAT CANNOT FIND ITS SUBJECT IS A GUARD THAT PASSES.
    expect(
      start,
      `“${HEADING}” is not in dashboard-design.md — this guard is asserting nothing`,
    ).toBeGreaterThan(-1);
    const rest = lines.slice(start + 1);
    const end = rest.findIndex((line) => /^#{2,3} /.test(line));
    expect(end, `no heading closes “${HEADING}” — this guard is asserting nothing`).toBeGreaterThan(
      -1,
    );
    return rest.slice(0, end);
  }

  /** Every address written in the table's ROUTE column, in document order. */
  function documented(): string[] {
    const out: string[] = [];
    for (const line of region()) {
      if (!line.startsWith("|")) continue;
      // THE FIRST CELL ONLY: the Page column's prose names addresses too,
      // including the ones it says are NOT routes.
      for (const [token] of (line.split("|")[1] ?? "").matchAll(/`#\/[^`]*`/g)) {
        out.push(token.slice(1, -1));
      }
    }
    // A FLOOR, so a table that lost its rows cannot pass by holding none.
    expect(out.length, "the route table holds too few routes to be the table").toBeGreaterThan(30);
    return out;
  }

  // DOC → CODE. Every address the document promises resolves to a screen.
  test("every route in the design doc resolves to a screen", () => {
    for (const route of documented()) {
      const where = resolve(segmentsOf(route));
      expect(where.resolved, `${route} resolves to Not Found`).toBe(true);
    }
  });

  // CODE → DOC. Every workspace, every section and every destination is
  // written down at its own address.
  test("every workspace, section and destination is written down", () => {
    const routes = new Set(documented().map((r) => r.replace(/^#\/$/, "#/")));
    const paths = [
      ...WORKSPACES.map((w) => w.path),
      ...WORKSPACES.flatMap((w) => w.sections.filter((s) => !s.elsewhere).map((s) => s.path)),
      ...DESTINATIONS.map((d) => d.path),
    ];
    for (const path of paths) {
      expect(
        routes.has(`#/${path.join("/")}`),
        `#/${path.join("/")} is not in the route table`,
      ).toBe(true);
    }
  });

  // AND A RESERVATION NOTHING ADDRESSES IS ROUTE SPACE HELD FOR A SCREEN THAT
  // DOES NOT EXIST — which is how `audit` sat reserved with no route.
  test("every reserved segment is a segment of some documented route", () => {
    const written = new Set(documented().flatMap((r) => r.replace(/^#\//, "").split("/")));
    for (const segment of RESERVED_SEGMENTS) {
      expect(written.has(segment), `“${segment}” is reserved but no documented route uses it`).toBe(
        true,
      );
    }
  });
});
