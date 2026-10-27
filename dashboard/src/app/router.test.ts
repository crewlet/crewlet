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
  FRAMELESS,
  RESERVED_SEGMENTS,
  WORKSPACES,
  grantsMissing,
  isSidebarRow,
  ownedBy,
  personalPath,
  sectionOf,
  sectionOpen,
  workspaceMissing,
  workspaceOf,
  workspaceOpen,
  workspaceRow,
  type WorkspaceRow,
} from "./nav.ts";
import { keyRow, sequenceWords } from "./keymap.ts";
import {
  avatarLabel,
  crumbsFor,
  projectArchivedKey,
  seatAvatarKey,
  seatKindKey,
  titleOf,
  turnHandleKey,
  turnSeatKey,
} from "./crumbs.ts";
import { CONTAINER_SHAPE, ITEM_KEY_SHAPE, KEY_SHAPE, resolve, resolves } from "./routes.ts";
import { ALL_PROJECTS, scopeAt } from "./scope.ts";
import { segmentsOf } from "~/test/routes.ts";
import { principlesEditHref } from "~/routes/settings/principles/edit/address.ts";

/** Every Settings section's own address but Spend's readings, which `nav gates by app` sets apart. */
function settingsPaths(): string[][] {
  return workspaceRow("settings")!
    .sections.filter((s) => s.group !== "Spend")
    .map((s) => s.path);
}

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
    expect(workspaceOf(["company", "org", "seats", "pm"])).toBe("org");
    // THE LONGEST PREFIX A ROW OWNS DECIDES: every Company row is under
    // `#/company`, and the bare address is Principles', the Company landing.
    expect(workspaceOf(["company"])).toBe("company");
    expect(workspaceOf(["company", "principles"])).toBe("company");
    expect(workspaceOf(["company", "schedules", "role", "ceo", "standup"])).toBe("schedules");
    expect(workspaceOf(["company", "integrations", "github"])).toBe("integrations");
    expect(workspaceOf(["settings", "spend", "tasks"])).toBe("settings");
    expect(workspaceOf(["live", "turns", "abc"])).toBe("live");
    expect(workspaceOf(["settings", "backups", "tracker"])).toBe("settings");
    expect(workspaceOf(["work", "ENG-42"])).toBe("work");
    // THE EMPTY FRAGMENT IS HOME, the landing screen.
    expect(workspaceOf([])).toBe("home");
  });

  // A ROUTE NOTHING OWNS RESOLVES TO NOTHING rather than to the first row: a
  // sidebar that marked a workspace for a path it does not hold would tell the
  // reader they are somewhere they are not — whatever the head is.
  test("an unowned route marks no workspace, whatever its head", () => {
    for (const head of ["nowhere", "agents", "spend", "activity", "cost", "admin", "pages"]) {
      expect(workspaceOf([head]), head).toBe("");
    }
  });

  test("every destination says what it answers", () => {
    // The hint is what the command palette shows. An entry with none, a lone
    // word or its own label again is an entry a reader has to click to
    // understand. A phrase is what counts, never a length: "Find tasks" says
    // what Work's search answers in ten characters.
    for (const d of DESTINATIONS) {
      expect(d.hint.trim().split(/\s+/).length, d.key).toBeGreaterThan(1);
      expect(d.hint.toLowerCase(), d.key).not.toBe(d.label.toLowerCase());
    }
  });

  // THE WORDS A READER LOOKS FOR A SCREEN BY are matched against what they
  // type, which the palette lowercases; a word in capitals would never match.
  // One a destination lists twice is noise, and one that IS another
  // destination's name would offer this screen beside the one so called.
  test("a destination's keywords are lowercase, its own, and nobody's name", () => {
    const labels = new Set(DESTINATIONS.map((d) => d.label.toLowerCase()));
    const seen = DESTINATIONS.filter((d) => (d.keywords ?? []).length > 0);
    expect(seen.map((d) => d.key)).toContain("account");
    for (const d of seen) {
      const words = d.keywords ?? [];
      expect(new Set(words).size, d.key).toBe(words.length);
      for (const word of words) {
        expect(word, d.key).toBe(word.toLowerCase().trim());
        expect(word.length, d.key).toBeGreaterThan(1);
        if (word !== d.label.toLowerCase())
          expect(labels.has(word), `${d.key}: ${word}`).toBe(false);
      }
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
      company: "c",
      brand: "e",
      org: "a",
      schedules: "d",
      integrations: "n",
      budgets: "b",
      work: "w",
      // `o`: Code's other letters are chords already (`c` the Principles',
      // `d` Schedules', `e` Brand's), and taking `c` would move the
      // Principles onto `p`, which is Switch project's.
      code: "o",
      knowledge: "k",
      artifacts: "f",
      live: "l",
      settings: "s",
      account: "u",
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
  // FIFTEEN WORKSPACES ARE SIDEBAR ROWS, in four runs, and the sidebar draws
  // them and nothing else from the table: a section that became a sidebar row
  // is a second column of navigation arriving one row at a time. The SIXTEENTH
  // is the reader's own Account, which has no row (the user block at the
  // sidebar's foot is the reader, and its menu links it) and is a workspace
  // all the same, so the palette and its chord reach it.
  test("the sidebar's rows are fifteen workspaces, in the story's order, and the menu holds the sixteenth", () => {
    const run = (place: string) => WORKSPACES.filter((w) => w.place === place).map((w) => w.key);
    expect(run("you")).toEqual(["home", "inbox", "me"]);
    expect(run("company")).toEqual([
      "company",
      "brand",
      "org",
      "schedules",
      "integrations",
      "budgets",
    ]);
    // CODE AFTER WORK: what the company does, its work and then its code.
    expect(run("workspace")).toEqual(["work", "code", "knowledge", "artifacts", "live"]);
    expect(run("foot")).toEqual(["settings"]);
    expect(run("menu")).toEqual(["account"]);
    // THE COMPANY'S FIRST ROW IS "Principles", the same name in the sidebar,
    // the trail and the palette, which finds it by what the page holds.
    expect(workspaceRow("company")?.label).toBe("Principles");
    const words = (key: string) => DESTINATIONS.find((d) => d.key === key)?.keywords ?? [];
    expect(words("company")).toEqual(
      expect.arrayContaining(["mission", "vision", "policies", "charter"]),
    );
    // WHAT THE PAGE NO LONGER HOLDS is found where it lives: the company's
    // name on the Org chart, whose company node renames it, and its clock in
    // the company document.
    expect(words("company")).not.toContain("company name");
    expect(words("company")).not.toContain("timezone");
    expect(words("org")).toContain("company name");
    expect(words("settings-config")).toContain("timezone");
    // ONE ROW FOR THE TWO PAGES IT REPLACED, and their addresses gone with them.
    expect(DESTINATIONS.some((d) => d.label === "Overview" || d.label === "Policies")).toBe(false);
  });

  // SETTINGS IS FOUR GROUPS, in this order, and lists only what lives in it.
  test("Settings' column is Runtime, Spend, Access and Engine", () => {
    const groups: Record<string, string[]> = {};
    for (const s of workspaceRow("settings")!.sections) (groups[s.group ?? ""] ??= []).push(s.key);
    expect(groups).toEqual({
      Runtime: ["models", "mcp-tools", "secrets", "limits"],
      Spend: ["spend", "spend-tasks"],
      Access: ["people", "service-accounts", "tokens", "sign-in", "audit"],
      Engine: ["nodes", "config", "backups", "storage"],
    });
    // IT LANDS ON ITS FIRST SECTION and holds every address under it.
    expect(workspaceRow("settings")?.path).toEqual(["settings", "models"]);
    expect(ownedBy(workspaceRow("settings")!)).toEqual(["settings"]);
  });

  // A SECTION IS A PATH INSIDE ITS WORKSPACE, never a query and never a row:
  // it starts with the prefix the workspace owns, it is in that workspace, it
  // resolves to a screen, and the section that path is in is that section. No
  // section leads out of its workspace: Settings lists only what lives in it.
  test("sections are paths, and never sidebar rows", () => {
    let sections = 0;
    for (const ws of WORKSPACES) {
      for (const s of ws.sections) {
        sections++;
        expect(s.path.join("/"), `${ws.key}/${s.key} carries a query`).not.toMatch(/[?=]/);
        expect(resolves(s.path), `#/${s.path.join("/")} does not resolve`).toBe(true);
        const owns = ownedBy(ws);
        expect(
          s.path.slice(0, owns.length),
          `${ws.key}/${s.key} is not under its workspace`,
        ).toEqual(owns);
        expect(workspaceOf(s.path), `${ws.key}/${s.key} is in another workspace`).toBe(ws.key);
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

  // A LOCK GOES WHERE THE CLOSED THING IS, by ONE rule: any one of a
  // section's permissions opens it (Admin counting every app), a section
  // naming none is the overview everybody reads, and a workspace is closed
  // only when nothing in it is open — needing its own permission beside its
  // section's, never instead of it.
  test("one rule opens a section, and a workspace only when nothing in it is", () => {
    const settings = workspaceRow("settings")!;
    const people = settings.sections.find((s) => s.key === "people")!;
    expect(sectionOpen(people, ["admin"])).toBe(true);
    expect(sectionOpen(people, ["tracker", "knowledge"]), "every app is not Admin").toBe(false);
    expect(grantsMissing(people, ["admin"])).toEqual([]);
    expect(grantsMissing(people, [])).toEqual(["admin"]);
    // SPEND'S READINGS NEED NOTHING, so Settings is never closed as a whole:
    // its locks are drawn per section, in its column.
    const spend = settings.sections.find((s) => s.key === "spend")!;
    expect(sectionOpen(spend, [])).toBe(true);
    expect(workspaceOpen(settings, [])).toBe(true);
    expect(workspaceMissing(settings, [])).toEqual([]);
    expect(workspaceOpen(workspaceRow("account")!, [])).toBe(true);
    // A WORKSPACE'S OWN PERMISSION IS NEEDED BESIDE ITS SECTION'S.
    const guarded: WorkspaceRow = { ...workspaceRow("live")!, grants: ["tracker"] };
    expect(workspaceOpen(guarded, [])).toBe(false);
    expect(workspaceMissing(guarded, [])).toEqual(["tracker"]);
    expect(workspaceOpen(guarded, ["tracker"])).toBe(true);
    // THE READER'S OWN ADDRESSES, which the frame draws for anybody signed in.
    expect(personalPath(["account"])).toBe(true);
    expect(personalPath(["settings", "models"])).toBe(false);
    expect(personalPath(["company"])).toBe(false);
    expect(personalPath(["work"])).toBe(false);
  });

  // EVERY PLACE IS THE OVERVIEW'S, ONE APP'S, OR ADMIN'S — and the table says
  // which, so the sidebar, the palette and the frame's lock read one answer.
  // The tracker's screens open with Tracker (or Admin, which includes it) and
  // nothing else; the knowledge base's with Knowledge; the company-wide
  // transcripts and every Settings page but You and General with Admin; and
  // the overview — Home, the agents' status, what is running, spend — with
  // nothing at all. Mutation: drop a row's permission, or give a transcript
  // an app's, and the matching line here goes red.
  //
  // (The tracker's screens: Inbox, My work, Work and Settings › Expensive
  // tasks. Admin's: the company-wide transcripts, Company › Integrations, and
  // every Settings page but Spend's readings.)
  test("nav gates by app", () => {
    const at = (...path: string[]) => sectionOf(path) ?? workspaceRow(workspaceOf(path))!;
    const opens = (path: string[], held: string[]) => {
      const row = workspaceRow(workspaceOf(path))!;
      const section = sectionOf(path);
      return workspaceOpen(row, held) && (!section || sectionOpen(section, held));
    };
    const TRACKER_ONLY = [
      ["inbox"],
      ["me"],
      ["work"],
      ["work", "tasks"],
      ["settings", "spend", "tasks"],
    ];
    for (const path of TRACKER_ONLY) {
      expect(opens(path, ["tracker"]), path.join("/")).toBe(true);
      expect(opens(path, ["admin"]), path.join("/")).toBe(true);
      expect(opens(path, ["knowledge"]), path.join("/")).toBe(false);
      expect(opens(path, []), path.join("/")).toBe(false);
    }
    for (const path of [["knowledge"], ["knowledge", "skills"]]) {
      expect(opens(path, ["knowledge"]), path.join("/")).toBe(true);
      expect(opens(path, ["admin"]), path.join("/")).toBe(true);
      expect(opens(path, ["tracker"]), path.join("/")).toBe(false);
    }
    const ADMIN_ONLY = [
      ["live", "turns"],
      ["live", "a2a"],
      ["live", "events"],
      ["knowledge", "diaries"],
      ...settingsPaths(),
    ];
    expect(ADMIN_ONLY.length).toBeGreaterThan(10);
    for (const path of ADMIN_ONLY) {
      expect(opens(path, ["admin"]), path.join("/")).toBe(true);
      expect(opens(path, ["tracker", "knowledge"]), path.join("/")).toBe(false);
    }
    const OVERVIEW = [
      [],
      ["company"],
      ["company", "principles"],
      ["company", "org"],
      ["company", "org", "roster"],
      ["company", "schedules"],
      // THE SERVICES BUILT INTO CREWLET are the overview every reader holds;
      // the apps' state and connecting stay an Admin's, on the page itself.
      ["company", "integrations"],
      ["company", "budgets"],
      // NO APP GATES CODE (ADR-0033): every person reads every repository,
      // and the engine holds each answer to the reader's own grant.
      ["code", "repos"],
      ["code", "pushes"],
      ["live"],
      ["live", "runs"],
      ["settings", "spend"],
      ["account"],
    ];
    for (const path of OVERVIEW) {
      expect(opens(path, []), path.join("/") || "home").toBe(true);
      expect(at(...path).grants ?? [], path.join("/") || "home").toEqual([]);
    }
  });

  // ALREADY IN THE SIDEBAR is a fact about the sidebar: every row's landing,
  // the empty fragment being Home's — and not the reader's Account, which is
  // one segment long and has no row.
  test("a sidebar row is a workspace's landing drawn as a row", () => {
    expect(isSidebarRow([])).toBe(true);
    for (const row of WORKSPACES.filter((w) => w.place !== "menu")) {
      expect(isSidebarRow(row.path), row.key).toBe(true);
    }
    expect(isSidebarRow(["account"])).toBe(false);
    expect(isSidebarRow(["work", "tasks"])).toBe(false);
  });

  // SETTINGS LISTS ONLY WHAT LIVES IN IT, and so does every renderer: a row
  // leading out of its workspace was a second way to a place that is already
  // a row, drawn with words of its own that drifted from the place's.
  test("no section leads out of its workspace", () => {
    for (const ws of WORKSPACES) {
      for (const s of ws.sections) {
        expect(workspaceOf(s.path), `${ws.key}/${s.key}`).toBe(ws.key);
      }
    }
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
    // A FRAMELESS HEAD TOO: `#/login` is read before any workspace is, so a
    // head the engine could mint would take the page that key names.
    for (const segment of [...RESERVED_SEGMENTS, ...FRAMELESS]) {
      expect(KEY_SHAPE.test(segment), `${segment} is a project-key shape`).toBe(false);
      expect(ITEM_KEY_SHAPE.test(segment), `${segment} is an item-key shape`).toBe(false);
      expect(CONTAINER_SHAPE.test(segment), `${segment} is a container-key shape`).toBe(false);
      expect(segment).toBe(segment.toLowerCase());
    }
  });
});

describe("the resolver", () => {
  const uuid = "5f0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f";

  // THE SIGN-IN SCREENS take exactly the tail their address has. An
  // invitation and a reset link are the exceptions: each screen answers a link
  // that is not whole (an empty one) rather than Not Found, since its holder
  // pasted it.
  test("a sign-in screen is its exact address, and an invitation or a reset its one link", () => {
    expect(resolve(["login"])).toMatchObject({ screen: "login", workspace: "" });
    expect(resolve(["login", "x"]).resolved).toBe(false);
    expect(resolve(["enrol"])).toMatchObject({ screen: "enrol", workspace: "" });
    expect(resolve(["enrol", "x"]).resolved).toBe(false);
    expect(resolve(["invite", `${uuid}.secret`])).toMatchObject({
      screen: "invite",
      link: `${uuid}.secret`,
    });
    expect(resolve(["invite", "a", "b"])).toMatchObject({ screen: "invite", link: "" });
    expect(resolve(["reset", `${uuid}.secret`])).toMatchObject({
      screen: "reset",
      link: `${uuid}.secret`,
      workspace: "",
    });
    expect(resolve(["reset", "a", "b"])).toMatchObject({ screen: "reset", link: "" });
  });

  // THE ACCOUNT IS IN THE FRAME, unlike the sign-in screens: it is a signed-in
  // reader's page, under the frame's own reads, and takes no tail.
  test("the account is its exact address, inside the frame", () => {
    expect(resolve(["account"])).toMatchObject({ screen: "account", workspace: "account" });
    expect(resolve(["account", "tokens"]).resolved).toBe(false);
  });

  // THE GALLERY HAS THREE SECTIONS, and an earlier gallery's five addresses
  // each land on the nearest: its own artifacts on Yours, what the company or
  // an agent shared among All. The gallery rewrites the address in place.
  test("the artifacts gallery's sections, and the earlier sections' addresses", () => {
    expect(resolve(["artifacts"])).toMatchObject({ screen: "artifacts", section: "all" });
    expect(resolve(["artifacts", "yours"])).toMatchObject({
      screen: "artifacts",
      section: "yours",
    });
    expect(resolve(["artifacts", "shared"])).toMatchObject({
      screen: "artifacts",
      section: "shared",
    });
    expect(resolve(["artifacts", "mine"])).toMatchObject({ screen: "artifacts", section: "yours" });
    expect(resolve(["artifacts", "company"])).toMatchObject({
      screen: "artifacts",
      section: "all",
    });
    expect(resolve(["artifacts", "agents"])).toMatchObject({ screen: "artifacts", section: "all" });
    expect(resolve(["artifacts", "for-you"]).resolved).toBe(false);
    expect(resolve(["artifacts", "constructor"]).resolved).toBe(false);
  });

  // EVERY KIND IS MADE ON ITS OWN PAGE, and the bare `new` (an earlier link's,
  // with `?kind=`) resolves so the frame can rewrite it to one.
  test("a new artifact's address names its kind", () => {
    expect(resolve(["artifacts", "new", "doc"])).toMatchObject({
      screen: "new-artifact",
      kind: "doc",
    });
    expect(resolve(["artifacts", "new", "html"])).toMatchObject({
      screen: "new-artifact",
      kind: "html",
    });
    expect(resolve(["artifacts", "new"])).toMatchObject({ screen: "new-artifact", kind: null });
    expect(resolve(["artifacts", "new", "deck"]).resolved).toBe(false);
    expect(resolve(["artifacts", "new", "doc", "x"]).resolved).toBe(false);
  });

  // A FOLDER BY THE ID THE ENGINE MINTED, never by its path or a word, so a
  // rename or a move breaks no link; and its page is in the Folders section.
  test("the Folders section and a folder's page, by id", () => {
    expect(resolve(["artifacts", "folders"])).toMatchObject({ screen: "artifact-folders" });
    expect(resolve(["artifacts", "folders", uuid])).toMatchObject({
      screen: "artifact-folder",
      id: uuid,
    });
    expect(resolve(["artifacts", "folders", "Marketing"]).resolved).toBe(false);
    expect(resolve(["artifacts", "folders", uuid, "x"]).resolved).toBe(false);
    expect(RESERVED_SEGMENTS).toContain("folders");
    expect(sectionOf(["artifacts", "folders", uuid])?.key).toBe("folders");
    expect(crumbsFor(["artifacts", "folders"]).map((c) => c.label)).toEqual([
      "Artifacts",
      "Folders",
    ]);
  });

  test("a key is a project, a key with a number or a uuid an item, and nothing else is", () => {
    expect(resolve(["work", "ENG"])).toMatchObject({ screen: "project", key: "ENG" });
    expect(resolve(["work", "ENG-42"])).toMatchObject({ screen: "item", id: "ENG-42" });
    expect(resolve(["work", uuid])).toMatchObject({ screen: "item", id: uuid });
    // A WORD IS NOT AN ITEM. Every one of these was an item id once, so each
    // drew the tracker's refusal under a trail rather than Not Found — the
    // sections the plan gives other workspaces, and any other word.
    for (const word of ["projects", "people", "skills", "models", "diaries", "eng-42", "t1"]) {
      expect(resolve(["work", word]).resolved, word).toBe(false);
    }
  });

  test("a page has ONE address, its id — the old container/title form is gone", () => {
    expect(resolve(["knowledge", "pages", uuid])).toMatchObject({ screen: "page", id: uuid });
    expect(resolve(["knowledge", "ENG"])).toMatchObject({ screen: "container", key: "ENG" });
    expect(resolve(["knowledge", "ENG", "Deploy runbook"]).resolved).toBe(false);
    expect(resolve(["pages", uuid]).resolved).toBe(false);
  });

  // A HEAD NO ROUTE OWNS IS NOT FOUND, and there is no redirect table: a
  // redirect whose path becomes a live route sends every reader of it
  // somewhere else, permanently.
  test("an address under a head no route owns is Not Found", () => {
    for (const path of [
      ["agents"],
      ["agents", "roster"],
      ["spend", "budgets"],
      ["company", "people", "pm"],
      ["settings"],
      ["settings", "general"],
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
    expect(bare.resolved ? "" : (bare.hint ?? "")).toBe("Open a trace from a turn, run or event");
    expect(resolve(["live", "traces", "abc"])).toMatchObject({ screen: "trace", id: "abc" });
  });

  // THE DISCRIMINANT IS NOT `kind`, because an integration's own parameter is:
  // spread beside a `kind` discriminant, `#/company/integrations/github`
  // resolved to an integration called "screen".
  test("an integration's kind survives the resolver's own discriminant", () => {
    expect(resolve(["company", "integrations", "github"])).toMatchObject({
      screen: "integrations",
      kind: "github",
    });
  });

  // A TOOL NAME IS A THIRD PARTY'S STRING, so the two shapes under `tools/`
  // are told apart by the tail's LENGTH: a tool literally called `servers`
  // keeps its page.
  test("a tool named `servers` keeps its page, and the origin filter keeps its", () => {
    expect(resolve(["settings", "mcp-tools", "servers"])).toMatchObject({
      screen: "mcp-tools",
      tool: "servers",
    });
    expect(resolve(["settings", "mcp-tools", "servers", "github"])).toMatchObject({
      screen: "mcp-tools",
      server: "github",
    });
  });

  // HANDLES LIVE ONLY UNDER `seats/`, domains only under `backups/`: a handle
  // or a domain equal to a reserved word is still that seat or that domain.
  test("a handle or a domain equal to a reserved word keeps its page", () => {
    expect(resolve(["company", "org", "seats", "roster"])).toMatchObject({
      screen: "seat",
      handle: "roster",
    });
    expect(resolve(["settings", "backups", "nodes"])).toMatchObject({
      screen: "backups",
      domain: "nodes",
    });
    expect(resolve(["company", "org", "seats"]).resolved).toBe(false);
  });

  // CODE'S SECTIONS ARE ITS ADDRESSES and take no tail: the repository a
  // history is narrowed to is `repo=`, never a segment, and the bare `#/code`
  // names no section — the row lands on Repositories, as Settings' lands on
  // Models & keys. Mutation: resolve the bare head to Repositories and its
  // trail stops saying where the reader is.
  test("Code's sections are its two lists, and nothing else under it is a page", () => {
    expect(resolve(["code", "repos"])).toMatchObject({ screen: "code-repos", workspace: "code" });
    expect(resolve(["code", "pushes"])).toMatchObject({ screen: "code-pushes", workspace: "code" });
    const bare = resolve(["code"]);
    expect(bare.resolved).toBe(false);
    expect(bare.resolved ? "" : (bare.hint ?? "")).toBe("Code opens on Repositories");
    for (const path of [
      ["code", "repos", "web"],
      ["code", "pushes", "web"],
      ["code", "changes"],
      ["code", "web"],
    ]) {
      expect(resolve(path).resolved, `#/${path.join("/")}`).toBe(false);
      expect(workspaceOf(path), `#/${path.join("/")}`).toBe("code");
    }
    expect(workspaceRow("code")?.path).toEqual(["code", "repos"]);
    expect(ownedBy(workspaceRow("code")!)).toEqual(["code"]);
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
    // `new` is the composer, which carries where it files in the query.
    expect(resolve(["knowledge", "new"])).toMatchObject({ screen: "new-page" });
    expect(resolve(["knowledge", "new", "ENG"]).resolved).toBe(false);
    for (const word of ["diary", "eng", "Eng"]) {
      expect(resolve(["knowledge", word]).resolved, word).toBe(false);
    }
  });

  test("a closed set refuses the obvious typo rather than drawing the list", () => {
    expect(resolve(["settings", "spend", "task"]).resolved).toBe(false);
    expect(resolve(["company", "budgets", "x"]).resolved).toBe(false);
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
      ["company", "org", "seats", "ada"],
      ["company", "org", "teams", "Platform"],
      ["knowledge", "pages", "0f0f0f0f-1111-4222-8333-444455556666"],
      ["knowledge", "new"],
      ["artifacts", "new", "sheet"],
      ["live", "turns", "t-1"],
      ["settings", "config", "revisions", "r-1"],
      ["settings", "backups", "tracker"],
      // THE TWO-SEGMENT FORMS, which are live addresses and were the ones
      // with the blank crumb: the list of revisions, and the page of a tool
      // that happens to be called `servers`.
      ["settings", "config", "revisions"],
      ["settings", "mcp-tools", "servers"],
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

  test("an artifact is named by its title, and never by its uuid", () => {
    const id = "0192f00e-0000-7000-8000-00000000a001";
    expect(crumbsFor(["artifacts", id]).map((c) => [c.label, c.mono ?? false])).toEqual([
      ["Artifacts", false],
      ["Artifact", false],
    ]);
    expect(crumbsFor(["artifacts", id], { [id]: "Q4 roadmap" }).at(-1)?.label).toBe("Q4 roadmap");
    expect(crumbsFor(["artifacts", "new", "doc"]).map((c) => c.label)).toEqual([
      "Artifacts",
      "New doc",
    ]);
    expect(crumbsFor(["artifacts", "new", "slides"]).at(-1)?.label).toBe("New slide deck");
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

  // PRINCIPLES IN EDIT IS STILL PRINCIPLES: the parts open are what the page
  // holds, never another page, so the address the org chart's company node
  // sends a person to resolves to the same screen, the same sidebar row and
  // the same trail as the page read. And the builder's address from before
  // the principles moved is still the builder, opening nothing of its own.
  // Mutation: give `edit=` a screen of its own in the resolver and the
  // trail names something else.
  test("Principles with parts in edit is the same screen, row and trail as Principles", () => {
    for (const address of [
      principlesEditHref(),
      principlesEditHref("vision"),
      principlesEditHref("policies"),
      "#/company/principles?edit=mission,policies",
    ]) {
      const { path } = parseHash(address);
      expect(resolve(path)).toEqual(resolve(["company", "principles"]));
      expect(workspaceOf(path)).toBe("company");
      expect(crumbsFor(path).map((c) => c.label)).toEqual(["Company", "Principles"]);
    }
    for (const old of ["charter", "mission", "vision", "policies"]) {
      const route = resolve(parseHash(`#/company/org/edit?edit=${old}`).path);
      expect(route.resolved && route.screen).toBe("org-edit");
    }
  });

  // A SECTION FOLLOWS ITS WORKSPACE, unless it IS the landing page: the chart
  // is "Org chart", the roster "Org chart › Roster". A Company row's own page
  // is its row alone.
  test("a section is named after its workspace, and the landing section is not", () => {
    expect(crumbsFor(["company", "org"]).map((c) => c.label)).toEqual(["Org chart"]);
    expect(crumbsFor(["company", "org", "roster"]).map((c) => c.label)).toEqual([
      "Org chart",
      "Roster",
    ]);
    // THE COMPANY'S LANDING IS ITS GROUP, THEN THE PAGE: the row reads
    // Principles under the Company heading, and the trail says both, at the
    // bare address and at the page's own.
    for (const path of [["company"], ["company", "principles"]]) {
      expect(crumbsFor(path).map((c) => c.label)).toEqual(["Company", "Principles"]);
      expect(crumbsFor(path)[0]?.path).toBeUndefined();
      expect(titleOf(crumbsFor(path))).toBe("Principles");
    }
    expect(crumbsFor(["company", "integrations"]).map((c) => c.label)).toEqual(["Integrations"]);
    // THE BRAND READS AS ITS SIBLING DOES: the group under the landing's
    // glyph, not a link, then the page.
    const brand = crumbsFor(["company", "brand"]);
    expect(brand.map((c) => c.label)).toEqual(["Company", "Brand"]);
    expect(brand[0]?.path).toBeUndefined();
    expect(brand[0]?.icon).toBe(crumbsFor(["company"])[0]?.icon);
    expect(titleOf(brand)).toBe("Brand");
    expect(crumbsFor(["settings", "mcp-tools"]).map((c) => c.label)).toEqual([
      "Settings",
      "MCP Tools",
    ]);
  });

  // A LANDING IS THE SECTION AT THE ROW'S OWN ADDRESS, never one merely as
  // deep: Code lands on Repositories, and Pushes, as deep, is named. Judged by
  // depth, the trail and the tab read "Code" over a repository's pushes.
  test("Code's landing is the workspace alone, and its other section is named", () => {
    expect(crumbsFor(["code", "repos"]).map((c) => c.label)).toEqual(["Code"]);
    const pushes = crumbsFor(["code", "pushes"]);
    expect(pushes.map((c) => c.label)).toEqual(["Code", "Pushes"]);
    expect(pushes[0]?.path).toEqual(["code", "repos"]);
    expect(titleOf(pushes)).toBe("Pushes");
    expect(crumbsFor(["code"]).map((c) => c.label)).toEqual(["Code", "Not found"]);
  });

  // EXCEPT SETTINGS', whose landing is one of the column's sections: "Settings"
  // alone over the models said nothing the column's selected row did not.
  test("Settings' landing is named Models & keys in the trail", () => {
    const crumbs = crumbsFor(["settings", "models"]);
    expect(crumbs.map((c) => c.label)).toEqual(["Settings", "Models & keys"]);
    expect(titleOf(crumbs)).toBe("Models & keys");
  });

  test("a path the table does not have is named Not Found, under its workspace if any", () => {
    expect(crumbsFor(["live", "traces"]).map((c) => c.label)).toEqual(["Live", "Not found"]);
    expect(crumbsFor(["company", "people"]).map((c) => c.label)).toEqual(["Company", "Not found"]);
    expect(crumbsFor(["agents"]).map((c) => c.label)).toEqual(["Not found"]);
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
    const raw = crumbsFor(["company", "org", "seats", "agent-cto"]);
    expect(raw[raw.length - 1]?.label).toBe("agent-cto");
    expect(raw[raw.length - 1]?.mono, "a slug drawn as prose reads as a name").toBe(true);
    const named = crumbsFor(["company", "org", "seats", "agent-cto"], {
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
    const raw = crumbsFor(["company", "org", "teams", "platform-eng"]);
    expect(raw[raw.length - 1]?.label).toBe("platform-eng");
    expect(raw[raw.length - 1]?.mono, "a key drawn as prose reads as a name").toBe(true);
    const named = crumbsFor(["company", "org", "teams", "platform-eng"], {
      "platform-eng": "Platform",
    });
    expect(named[named.length - 1]?.label).toBe("Platform");
    expect(named[named.length - 1]?.mono, "a name is not an identifier").toBeFalsy();
  });

  // A SEAT'S BADGE IN THE TRAIL IS THE ONE ITS PAGE DRAWS: an agent wears
  // the character and colour its page published, on its own crumb and on the
  // seat crumb of each of its turns, and a crumb with nothing published keeps
  // the kit's defaults rather than a character the crumb made up.
  test("an agent's crumb wears the character and colour its page published", () => {
    const avatar = { character: "foxlet", color: "rose" } as const;
    const labels = {
      "agent-cto": "CTO",
      [seatKindKey("agent-cto")]: "agent",
      [seatAvatarKey("agent-cto")]: avatarLabel(avatar),
    };
    const seat = crumbsFor(["company", "org", "seats", "agent-cto"], labels);
    expect(seat[seat.length - 1]?.seat).toEqual({ name: "CTO", kind: "agent", avatar });

    const bare = crumbsFor(["company", "org", "seats", "agent-cto"], {
      "agent-cto": "CTO",
      [seatKindKey("agent-cto")]: "agent",
    });
    expect(bare[bare.length - 1]?.seat?.avatar).toBeUndefined();

    const turn = crumbsFor(["live", "turns", "t-1"], {
      [turnSeatKey("t-1")]: "CTO",
      [turnHandleKey("t-1")]: "agent-cto",
      [seatAvatarKey("agent-cto")]: avatarLabel(avatar),
    });
    expect(turn.find((c) => c.seat)?.seat).toEqual({ name: "CTO", kind: "agent", avatar });
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
      ["settings", "mcp-tools", "servers", "github"],
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

  // ON WORK'S SCOPE PAGES THE LENS COMES FIRST, so a project's board and its
  // history open in two tabs read apart.
  test("on Work's scope pages the tab title reads the lens, then the scope", () => {
    const title = (path: string[], query = "", labels = {}) =>
      titleOf(crumbsFor(path, labels), scopeAt(path, new URLSearchParams(query)));
    expect(title(["work"])).toBe("Projects · All projects");
    expect(title(["work", "tasks"])).toBe("Tasks · All projects");
    expect(title(["work", "history"])).toBe("History · All projects");
    expect(title(["work", "ENG"], "", { ENG: "Core platform" })).toBe("Tasks · Core platform");
    expect(title(["work", "ENG"], "lens=about", { ENG: "Core platform" })).toBe(
      "About · Core platform",
    );
    // A task and a saved view are not scopes: their own names title them.
    expect(title(["work", "views"])).toBe("Saved views");
  });
});

// EVERY FIXED DESTINATION NAMES ITSELF IN THE TRAIL, from the one table — a
// section that fell through to an object branch read as "Work / Item /
// search", a trail naming a page that does not exist.
//
// A COLUMN'S LANDING NAMES ITS SECTION: Settings draws Models & keys as a row of
// its column like any other, so its page is "Settings › Models & keys" while
// the palette's destination is still the workspace.
test("every fixed destination names itself rather than reading as a key", () => {
  for (const dest of DESTINATIONS) {
    const crumbs = crumbsFor(dest.path);
    const last = crumbs[crumbs.length - 1];
    const row = workspaceRow(dest.workspace)!;
    // A SCOPE'S LENS ENDS IN ITS SCOPE: Work's trail names All projects, the
    // switcher's button, and the lens is what the tab title leads with.
    const lens =
      row.renderer === "lenses"
        ? row.sections.find((s) => s.tab !== false && samePath(s.path, dest.path))
        : undefined;
    if (lens) {
      expect(last?.label, `#/${dest.path.join("/")}`).toBe(ALL_PROJECTS);
      expect(last?.switch, `#/${dest.path.join("/")}`).toBe("project");
      expect(titleOf(crumbs, scopeAt(dest.path, new URLSearchParams()))).toBe(
        `${lens.label} · ${ALL_PROJECTS}`,
      );
      continue;
    }
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
      ["settings", "mcp-tools", "servers", "github"],
      ["Settings", "MCP Tools", "Servers"],
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
      ...WORKSPACES.flatMap((w) => w.sections.map((s) => s.path)),
      ...DESTINATIONS.map((d) => d.path),
      // THE SCREENS OUTSIDE THE FRAME, which no workspace declares: a page a
      // person is sent to before anything else opens is one somebody
      // starting from the document has to be able to find.
      // An invitation and a reset link are written down as the link each carries.
      ...FRAMELESS.map((head) =>
        head === "invite" || head === "reset" ? [head, "{id}.{secret}"] : [head],
      ),
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

// WORK OPENS ON ITS PROJECTS, its task list is the lens beside them, and the
// trail names the scope those lenses are of, as the button that switches it.
describe("Work's scope", () => {
  test("#/work is Projects, #/work/tasks the company's tasks, and #/work/projects is retired", () => {
    expect(resolve(["work"])).toMatchObject({ screen: "work-projects" });
    expect(resolve(["work", "tasks"])).toMatchObject({ screen: "work-tasks" });
    expect(resolve(["work", "projects"]).resolved).toBe(false);
    expect(RESERVED_SEGMENTS).toContain("tasks");
    expect(RESERVED_SEGMENTS).not.toContain("projects");
  });

  test("Work draws its sections as lenses, and saved views and search are addresses, not lenses", () => {
    const work = workspaceRow("work")!;
    expect(work.renderer).toBe("lenses");
    expect(work.sections.map((s) => [s.key, s.label, s.tab !== false])).toEqual([
      ["projects", "Projects", true],
      ["tasks", "Tasks", true],
      ["history", "History", true],
      ["views", "Saved views", false],
      ["search", "Search", false],
    ]);
  });

  test("Work's trail ends in its scope, and Work is plain words on its landing", () => {
    expect(crumbsFor(["work"])).toEqual([
      { label: "Work", icon: "square-kanban" },
      { label: "All projects", switch: "project" },
    ]);
    expect(crumbsFor(["work", "history"])).toEqual([
      { label: "Work", path: ["work"], icon: "square-kanban" },
      { label: "All projects", switch: "project" },
    ]);
    expect(crumbsFor(["work", "ENG"], { ENG: "Core platform" }).at(-1)).toEqual({
      label: "Core platform",
      tag: "ENG",
      switch: "project",
    });
    // A TASK AND A SAVED VIEW ARE NOT SCOPES: their trails switch nothing.
    expect(crumbsFor(["work", "ENG-42"]).some((c) => c.switch)).toBe(false);
    expect(crumbsFor(["work", "views"]).some((c) => c.switch)).toBe(false);
  });

  test("an archived project says so in its crumb, once its page has said", () => {
    expect(
      crumbsFor(["work", "OLD"], {
        OLD: "Legacy importer",
        [projectArchivedKey("OLD")]: "archived",
      }).at(-1),
    ).toMatchObject({ label: "Legacy importer", tag: "OLD", archived: true });
    expect(crumbsFor(["work", "OLD"], { OLD: "Legacy importer" }).at(-1)?.archived).toBeUndefined();
  });

  test("G then P switches project, from anywhere", () => {
    const row = keyRow("project");
    expect(row.scope).toBe("frame");
    expect(row.presses).toEqual([{ after: "g", key: "p" }]);
    expect(row.does).toBe("Switch project");
    expect(sequenceWords("project")).toBe("G then P");
  });
});
