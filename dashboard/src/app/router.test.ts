/**
 * The router's rules, which are invisible in a URL — and the NAVIGATION
 * GRAMMAR, which is invisible everywhere.
 *
 * Both of the router's rules shipped wrong once and neither shows up in an
 * address bar: Back from a screen's fourth lens left the screen entirely, and
 * Back to a list somebody had scrolled halfway down landed at the top.
 *
 * The grammar is the other half, and it is the one that decays: rail row is a
 * workspace, sidebar row is a destination with its OWN PATH, tab is a query on
 * the path you are already on. Nothing is two of those. It is lost one pull
 * request at a time — "just one more row under a project" — and the loss is
 * invisible until the product has three levels of navigation and no rule for
 * which one a thing belongs in.
 */

// @vitest-environment node

import { describe, expect, test } from "vitest";
import { buildHash, parseHash, samePath } from "./router.tsx";
import { DESTINATIONS, RAIL, RESERVED_SEGMENTS, railRow, workspaceOf } from "./nav.ts";
import { crumbsFor, titleOf } from "./workspaces/crumbs.ts";

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
    expect(buildHash(["activity"], { actor: "" })).toBe("#/activity");
  });
});

describe("navigation identity", () => {
  // A DETAIL KEEPS THE READER'S PLACE IN THE RAIL. Otherwise opening one
  // event loses the mark on the workspace you came from, which reads as
  // having navigated somewhere unrelated.
  test("a detail resolves to the workspace that holds it", () => {
    expect(workspaceOf(["company", "people", "pm"])).toBe("company");
    expect(workspaceOf(["activity", "turns", "abc"])).toBe("activity");
    expect(workspaceOf(["activity", "events", "abc"])).toBe("activity");
    expect(workspaceOf(["work", "ENG-42"])).toBe("work");
    // GOALS IS WORK'S, although its first segment is its own: the tier above
    // projects belongs with the projects, and a rail row that unmarked itself
    // whenever somebody opened a goal would be the proof it does not.
    expect(workspaceOf(["goals"])).toBe("work");
    expect(workspaceOf([])).toBe("inbox");
  });

  // A ROUTE NOTHING OWNS RESOLVES TO NOTHING rather than to the first row: a
  // rail that marked a workspace for a path it does not hold would tell the
  // reader they are somewhere they are not.
  test("an unowned route marks no workspace", () => {
    expect(workspaceOf(["nowhere"])).toBe("");
  });

  test("every destination says what it answers", () => {
    // The hint is what the command palette shows. An entry with none is an
    // entry a reader has to click to understand.
    for (const d of DESTINATIONS) {
      expect(d.hint.length, d.key).toBeGreaterThan(10);
    }
  });

  test("every rail row has its own jump chord", () => {
    // `g` then a letter. Two rows sharing one makes the second unreachable
    // by keyboard, silently.
    const chords = RAIL.map((r) => r.chord);
    expect(new Set(chords).size).toBe(chords.length);
  });
});

describe("am I already here", () => {
  // For the links a component draws without knowing where it is rendered. A
  // phase card carries "event →" to its own event: a way out on the turn and
  // on the seat, and on that event's own page a link back to itself — the
  // reader clicks, the URL does not change, nothing moves, and the only thing
  // they learn is that the control was a lie.

  test("the same page is the same page", () => {
    expect(samePath(parseHash("#/events/abc").path, ["events", "abc"])).toBe(true);
  });

  test("a different id is a different page", () => {
    expect(samePath(parseHash("#/events/abc").path, ["events", "def"])).toBe(false);
  });

  test("a prefix is not a match", () => {
    // `#/events` lists events; `#/events/abc` is one of them. A link from the
    // list to a row must not be suppressed as a self-link.
    expect(samePath(parseHash("#/events").path, ["events", "abc"])).toBe(false);
    expect(samePath(parseHash("#/events/abc").path, ["events"])).toBe(false);
  });

  test("a query string does not make it a different page", () => {
    // A filter or a tab the reader happens to have on the URL is still the
    // same page, and a self-link is still a loop.
    expect(samePath(parseHash("#/events/abc?category=system").path, ["events", "abc"])).toBe(true);
  });

  test("segments are compared as segments, never as a joined string", () => {
    // A join makes `["events", "a/b"]` and `["events", "a", "b"]` equal, and
    // an id is not a path.
    expect(samePath(["events", "a/b"], ["events", "a", "b"])).toBe(false);
  });
});

describe("the navigation grammar", () => {
  // A SIDEBAR ROW IS A DESTINATION WITH ITS OWN PATH. A row that differed
  // from the page you are on only by a query key is a TAB wearing a sidebar
  // row's clothes, and the moment one exists the two levels stop meaning
  // anything: the reader cannot tell what is a place from what is a filter.
  test("every fixed destination differs from its workspace by its PATH", () => {
    for (const d of DESTINATIONS) {
      const row = RAIL.find((r) => r.key === d.workspace);
      expect(row, `${d.key} names workspace ${d.workspace}, which is not a rail row`).toBeTruthy();
      // A DESTINATION IS A PATH, never a query on one: the grammar's whole
      // point, and the thing a "just one more row" pull request breaks first.
      expect(d.path.length, `${d.key} has no path of its own`).toBeGreaterThan(0);
    }
    // AND NO TWO DESTINATIONS SHARE ONE. Two rows leading to one address are
    // two names for one place, which is how a reader comes to believe the
    // second of them is broken.
    const addresses = new Set(DESTINATIONS.map((d) => d.path.join("/")));
    expect(addresses.size).toBe(DESTINATIONS.length);
  });

  // EVERY DESTINATION BELONGS TO THE WORKSPACE IT CLAIMS, derived the same way
  // the rail marks the current row. A destination whose path routes to a
  // different workspace marks the wrong rail row the moment it is opened.
  test("a destination's path resolves to the workspace it declares", () => {
    for (const d of DESTINATIONS) {
      expect(workspaceOf(d.path), `${d.key}`).toBe(d.workspace);
    }
  });

  // ONE ROW PER WORKSPACE, and every workspace owns its own first segments.
  // Two rail rows claiming one segment is a route whose workspace depends on
  // which row was declared first.
  test("no two workspaces own the same first segment", () => {
    const owner = new Map<string, string>();
    for (const row of RAIL) {
      for (const segment of row.owns) {
        expect(owner.has(segment), `${segment} is owned by two rail rows`).toBe(false);
        owner.set(segment, row.key);
      }
    }
  });

  // A RESERVED SEGMENT CANNOT COLLIDE WITH A KEY THE ENGINE MINTS. Project
  // and container keys are uppercase, item keys are `KEY-n`, everything else
  // is a uuid — and every reserved segment is lowercase. Without this,
  // `#/work/views` is a list of saved views until somebody creates a project
  // called VIEWS, and then it is a project.
  test("no reserved segment is the shape of a minted key", () => {
    for (const segment of RESERVED_SEGMENTS) {
      expect(/^[A-Z][A-Z0-9_]*$/.test(segment), `${segment} is a project-key shape`).toBe(false);
      expect(/-\d+$/.test(segment), `${segment} is an item-key shape`).toBe(false);
      expect(segment).toBe(segment.toLowerCase());
    }
  });
});

describe("the breadcrumb", () => {
  // THE LAST CRUMB IS THE OBJECT and carries no link. A trail whose final
  // segment links to the page you are already on teaches a reader that the
  // control does nothing.
  test("the last crumb is never a link", () => {
    for (const path of [
      ["inbox"],
      ["work"],
      ["work", "ENG"],
      ["work", "ENG", "sprints", "3"],
      ["work", "ENG-42"],
      ["company", "people", "ada"],
      ["company", "units", "platform"],
      ["knowledge", "ENG", "Deploy runbook"],
      ["activity", "turns", "t-1"],
      ["admin", "config", "revisions", "r-1"],
      ["admin", "fleet", "domains", "tracker"],
      // THE TWO-SEGMENT FORMS, which are live addresses and were the ones
      // with the blank crumb: `#/admin/config/revisions` routes, and
      // `#/admin/tools/servers` is the page of a tool called `servers`.
      ["admin", "config", "revisions"],
      ["admin", "tools", "servers"],
    ]) {
      const crumbs = crumbsFor(path);
      expect(crumbs.length, `#/${path.join("/")} has no crumbs`).toBeGreaterThan(0);
      expect(crumbs[crumbs.length - 1]?.path, `#/${path.join("/")}`).toBeUndefined();
      // AND IT IS NOT BLANK. The trail is what `Shell` titles the browser tab
      // from, so an empty final crumb showed as " · Crewlet" — a tab a reader
      // with four of them open cannot tell from any other.
      expect(
        String(crumbs[crumbs.length - 1]?.label ?? "").trim(),
        `#/${path.join("/")} ends in an empty crumb`,
      ).not.toBe("");
    }
  });

  // AND EVERY OTHER CRUMB IS ONE, or the trail is a label rather than an
  // address: a reader two levels into a project has to be able to step back
  // out through the trail.
  test("every crumb but the last carries a path", () => {
    const crumbs = crumbsFor(["work", "ENG", "sprints", "3"]);
    expect(crumbs.length).toBe(4);
    for (const crumb of crumbs.slice(0, -1)) {
      expect(crumb.path, `${crumb.label} is not a link`).toBeTruthy();
    }
  });

  // A LABEL A SCREEN RESOLVED WINS over the raw segment, and the raw segment
  // is what shows until it does — never a spinner in the chrome.
  test("a screen's own labels replace the segments", () => {
    const raw = crumbsFor(["work", "ENG"]);
    expect(raw[raw.length - 1]?.label).toBe("ENG");
    const named = crumbsFor(["work", "ENG"], { ENG: "Platform" });
    expect(named[named.length - 1]?.label).toBe("Platform");
  });

  // THE TAB TITLE COMES FROM THE SAME TRAIL, so a reader with four tabs open
  // can tell them apart. It said "Crewlet" on every screen.
  test("the tab title is the last crumb", () => {
    expect(titleOf(crumbsFor(["work", "ENG-42"], { "ENG-42": "Fix the applier" }))).toBe(
      "Fix the applier",
    );
  });
});

// EVERY FIXED DESTINATION NAMES ITSELF IN THE TRAIL, from the one table.
//
// The work workspace's trail reads the segment after `work/` as a KEY — a
// project is uppercase, an item is `KEY-n` — so a lowercase reserved segment
// matched neither and fell through to the item branch. `#/work/search` read
// "Work / Item / search": a trail naming a page that does not exist, on the
// screen whose whole job is finding the page that does. The previous shape
// spelled each reserved segment out by hand, which is what left `search` out
// on the day it was added, so this walks the DESTINATIONS table instead.
test("every fixed destination names itself rather than reading as a key", () => {
  for (const dest of DESTINATIONS) {
    // EXCEPT A WORKSPACE'S OWN LANDING PAGE, whose trail is the workspace
    // name: the sidebar calls it "All work" because it sits among that
    // workspace's other rows, and the trail calls it "Work" because there is
    // nothing above it to disambiguate from. Two labels for one address, and
    // both are right in their own chrome.
    if (samePath(dest.path, railRow(dest.workspace)?.path ?? [])) continue;
    const crumbs = crumbsFor(dest.path);
    const last = crumbs[crumbs.length - 1];
    expect(last?.label, `#/${dest.path.join("/")}`).toBe(dest.label);
    expect(last?.path, `#/${dest.path.join("/")} links to itself`).toBeUndefined();
  }
});

// A WORKSPACE THAT OWNS TWO FIRST SEGMENTS still says which one you are on.
//
// Work owns `work` and `goals`. The trail read "Work" for both, so the goals
// list and the company's board were indistinguishable in the page bar and in
// the browser tab — which is exactly what a breadcrumb exists to prevent.
test("a second owned segment names itself in the trail", () => {
  const goals = crumbsFor(["goals"]);
  expect(goals.map((c) => c.label)).toEqual(["Work", "Goals"]);
  expect(goals[0]?.path).toEqual(["work"]);
  expect(goals[1]?.path).toBeUndefined();
  // And the workspace's OWN landing page is still a single crumb.
  expect(crumbsFor(["work"]).map((c) => c.label)).toEqual(["Work"]);
});
