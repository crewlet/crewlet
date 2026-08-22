// The command palette — one way to reach anything.
//
// Until now it had no suite at all, which is how it came to be missing
// the Agents room: the room shipped, took a slot in the sidebar, and was
// the one place in the product ⌘K could not take you.
//
// The ranking is the part worth testing without a DOM, and `buildResults`
// is pure precisely so it can be. `chrome` is passed in rather than read
// off `document` for the same reason — a command whose label depends on
// the current setting must not be the thing that drags a DOM in.

import assert from "node:assert";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { test, run } from "./harness.mjs";

const base = new URL("../../../src/crewlet/static/dashboard/js/", import.meta.url);
const { buildResults, flatResults, commands, ROOMS } = await import(
  new URL("commandPalette.js", base)
);
// The sidebar's own list, read from the shell rather than copied here.
// Two hand-kept lists is exactly how the Agents room came to be in one
// and not the other.
const appSource = readFileSync(fileURLToPath(new URL("app.js", base)), "utf8");
const navNames = [...appSource.matchAll(/\{ name: "(\w+)", icon: "[\w-]+", label:/g)].map(
  (m) => m[1],
);

const ORG = {
  roles: [{ name: "CEO", handle: "ceo", kind: "agent" }],
  units: [
    {
      name: "Engineering",
      roles: [
        { name: "Engineer", handle: "eng", kind: "agent" },
        { name: "Dana", handle: "dana", kind: "human" },
      ],
    },
  ],
};

const state = (extra = {}) => ({ org: ORG, agents: [], ...extra });
const titles = (groups) => groups.map((g) => g.title);
const group = (groups, title) => groups.find((g) => g.title === title);

// ---- rooms ----------------------------------------------------------

test("every room in the sidebar is reachable from the palette", () => {
  // A room the palette cannot reach is a room that only exists if you
  // already know where it is in the nav — which is the opposite of what
  // a palette is for. The Agents room shipped that way: in the sidebar,
  // and the one place ⌘K could not take you.
  assert.ok(navNames.length >= 10, "the nav list could not be read from app.js");
  const routes = new Set(ROOMS.map((r) => (r.route === "/" ? "mission" : r.route.slice(1))));
  const missing = navNames.filter((name) => !routes.has(name));
  assert.deepEqual(missing, [], `not reachable from the palette: ${missing.join(", ")}`);
});

test("a prefix beats a word boundary beats a substring", () => {
  const hits = group(buildResults(state(), "ag"), "Rooms").items;
  assert.equal(hits[0].label, "Agents", "the prefix match did not rank first");
});

// ---- seats ----------------------------------------------------------

test("a seat is found by name or by handle", () => {
  assert.equal(group(buildResults(state(), "engineer"), "Seats").items[0].label, "Engineer");
  assert.equal(group(buildResults(state(), "eng"), "Seats").items[0].label, "Engineer");
});

test("a human seat is offered, and says it is one", () => {
  const hit = group(buildResults(state(), "dana"), "Seats").items[0];
  assert.equal(hit.hint, "human seat");
});

// ---- commands -------------------------------------------------------

// Looked up by `command`, never by position: the list grows, and a test
// that indexes it re-reads a different command than it asserts about.
const cmd = (opts, name) => commands(opts).find((c) => c.command === name);

test("a command states what it will DO, not what is currently set", () => {
  // The same rule an icon button's tooltip follows. A control labelled
  // with its current value asks the reader to work out the inverse.
  const dark = { theme: "dark", density: "comfortable" };
  assert.match(cmd(dark, "toggle-theme").label, /Switch to the light theme/);
  assert.match(cmd(dark, "toggle-density").label, /Switch to compact spacing/);

  const light = { theme: "light", density: "compact" };
  assert.match(cmd(light, "toggle-theme").label, /Switch to the dark theme/);
  assert.match(cmd(light, "toggle-density").label, /Switch to comfortable spacing/);
});

test("the stored token is offered as something to FORGET when one is held", () => {
  // The question this answers is "why does it never ask me for a token".
  // Because the browser already has one, kept in localStorage, sent on
  // every handshake and every query — and until now nothing said so and
  // nothing could drop it.
  assert.equal(cmd({ tokenHeld: true }, "set-token"), undefined);
  const forget = cmd({ tokenHeld: true }, "clear-token");
  assert.ok(forget, "no way to drop a held token");
  assert.match(forget.label, /Forget/);

  assert.equal(cmd({ tokenHeld: false }, "clear-token"), undefined);
  assert.ok(cmd({ tokenHeld: false }, "set-token"), "no way to set one either");
});

test("the token command is findable by the words for it", () => {
  for (const term of ["token", "forget"]) {
    const hits = group(buildResults(state(), term, { tokenHeld: true }), "Commands");
    assert.ok(hits, `nothing matched "${term}"`);
    assert.ok(hits.items.some((i) => i.command === "clear-token"), term);
  }
});

test("density is reachable by the words somebody would type for it", () => {
  // It lost its topbar button because an icon-only control next to the
  // theme toggle read as a mystery — twice reported as one. The palette
  // is where it lives now, so it has to be findable there.
  for (const term of ["compact", "spacing"]) {
    const hits = group(buildResults(state(), term), "Commands");
    assert.ok(hits, `nothing matched "${term}"`);
    assert.ok(
      hits.items.some((i) => i.command === "toggle-density"),
      `"${term}" did not reach the density command`,
    );
  }
});

test("commands are hidden on an empty query", () => {
  // The palette opens on navigation. A preference sitting in it every
  // time it opens is a preference offered constantly and wanted rarely.
  assert.ok(!titles(buildResults(state(), "")).includes("Commands"));
});

test("a command carries no route, and a room carries no command", () => {
  // The shell branches on which one is present, so a hit that had both
  // would navigate AND act.
  const groups = buildResults(state(), "switch");
  for (const hit of flatResults(groups)) {
    if (hit.kind === "command") assert.ok(!hit.route, "a command names a route");
    else assert.ok(!hit.command, "a place names a command");
  }
});

test("commands rank below rooms and seats", () => {
  // Somebody typing into the palette is navigating; a preference is the
  // rarer intent, so it must not sit above the thing they meant.
  const groups = buildResults(state(), "e");
  const order = titles(groups);
  if (order.includes("Commands")) {
    for (const above of ["Rooms", "Seats"]) {
      if (order.includes(above)) {
        assert.ok(
          order.indexOf(above) < order.indexOf("Commands"),
          `${above} ranked below Commands`,
        );
      }
    }
  }
});

// ---- ids ------------------------------------------------------------

test("a pasted id offers both readings, because the shape cannot tell them apart", () => {
  const id = "0f9c1b2a-3d4e-4f5a-8b6c-7d8e9f0a1b2c";
  const items = group(buildResults(state(), id), "Open by id").items;
  assert.deepEqual(
    items.map((i) => i.kind),
    ["event", "trace"],
  );
});

test("a word that is not an id offers no id row", () => {
  assert.ok(!titles(buildResults(state(), "engineer")).includes("Open by id"));
});

await run();
