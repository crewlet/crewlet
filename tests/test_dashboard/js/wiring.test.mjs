// Every view gets what it asks the shell for.
//
// This suite exists because the Fleet view shipped dead. It declared
// `createFleetView({ store, api })` while app.js built its context as
// `{ store, navigate, query, setToken, refresh }` — no `api` — so every
// poll threw `Cannot read properties of undefined` before it reached the
// network, the rejection was unhandled and console-only, and the view
// held its loading skeleton for ever. It looked like a slow endpoint.
//
// Its own suite passed the whole time, because a test constructs the view
// directly and injects exactly what the view asks for. That is the right
// way to test a view's behaviour and it can never catch a dependency the
// SHELL fails to pass. Nothing mounted a view through the real context.
//
// So this checks the seam between them, statically: what app.js provides
// against what each view destructures. No DOM, no socket, no mounting —
// app.js boots on import, and a test that imported it would be testing
// the browser, not the wiring.

import assert from "node:assert";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { test, run } from "./harness.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const JS = join(HERE, "../../../src/crewlet/static/dashboard/js");

const appSource = readFileSync(join(JS, "app.js"), "utf8");

/** The keys app.js puts on the object every view factory receives. */
function shellProvides() {
  const block = /const ctx = \{([\s\S]*?)\n\};/.exec(appSource);
  assert.ok(block, "app.js no longer declares `const ctx = {...}`");
  const keys = new Set();
  for (const line of block[1].split("\n")) {
    // `store,` / `query: (what) => ...` / `refresh: () => ...`
    const m = /^\s*(\w+)\s*[,:]/.exec(line);
    if (m) keys.add(m[1]);
  }
  // Spread in at mount: `factory({ ...ctx, params: route.params })`.
  assert.match(
    appSource,
    /factory\(\{\s*\.\.\.ctx,\s*params:/,
    "app.js no longer spreads ctx and params into the factory",
  );
  keys.add("params");
  return keys;
}

/** What one view module asks for, by name. */
function viewRequires(source, file) {
  const m = /export function create\w+View\(\s*(\{[\s\S]*?\}|)\s*\)/.exec(source);
  assert.ok(m, `${file} has no create*View export`);
  const params = m[1];
  if (!params) return []; // destructures nothing at all
  return [...params.matchAll(/(\w+)\s*(?:=[^,}]+)?\s*(?:,|\})/g)].map(
    (hit) => hit[1],
  );
}

const VIEW_FILES = readdirSync(join(JS, "views")).filter((f) =>
  f.endsWith(".js"),
);

test("the shell still builds a context", () => {
  const provided = shellProvides();
  assert.ok(provided.size >= 3, `ctx looks empty: ${[...provided]}`);
  assert.ok(provided.has("query"), "ctx lost `query`, the only server seam");
});

test("every view's dependencies are provided by the shell", () => {
  const provided = shellProvides();
  const problems = [];
  for (const file of VIEW_FILES) {
    const source = readFileSync(join(JS, "views", file), "utf8");
    for (const name of viewRequires(source, file)) {
      if (!provided.has(name)) {
        problems.push(`${file} destructures \`${name}\`, which ctx does not carry`);
      }
    }
  }
  assert.deepEqual(problems, [], problems.join("\n"));
});

test("every module the shell imports exists on disk", () => {
  // ES modules fail as a graph, not as a file: one missing import in
  // app.js and NOTHING renders — no view, no nav, no error on the page,
  // just an empty shell and a 404 in a console nobody has open. A view
  // module deleted in a refactor, or renamed on one side of a rename,
  // takes the whole dashboard with it.
  const problems = [];
  const seen = new Set();
  const walk = (file) => {
    if (seen.has(file)) return;
    seen.add(file);
    let source;
    try {
      source = readFileSync(file, "utf8");
    } catch {
      return; // reported by whoever imported it
    }
    for (const hit of source.matchAll(/from\s+"(\.[^"]+)"/g)) {
      const target = join(dirname(file), hit[1]);
      if (!existsSync(target)) {
        problems.push(
          `${file.slice(JS.length + 1)} imports ${hit[1]}, which does not exist`,
        );
        continue;
      }
      walk(target);
    }
  };
  walk(join(JS, "app.js"));
  assert.deepEqual(problems, [], problems.join("\n"));
});

test("no module is orphaned", () => {
  // The other half: a view left behind by a refactor still passes every
  // test it has, still looks maintained, and is reachable from nothing.
  const reachable = new Set();
  const walk = (file) => {
    if (reachable.has(file)) return;
    reachable.add(file);
    let source;
    try {
      source = readFileSync(file, "utf8");
    } catch {
      return;
    }
    for (const hit of source.matchAll(/from\s+"(\.[^"]+)"/g)) {
      const target = join(dirname(file), hit[1]);
      if (existsSync(target)) walk(target);
    }
  };
  walk(join(JS, "app.js"));

  const all = [
    ...readdirSync(JS)
      .filter((f) => f.endsWith(".js"))
      .map((f) => join(JS, f)),
    ...VIEW_FILES.map((f) => join(JS, "views", f)),
  ];
  const orphans = all
    .filter((f) => !reachable.has(f))
    .map((f) => f.slice(JS.length + 1));
  assert.deepEqual(orphans, [], `unreachable from app.js: ${orphans.join(", ")}`);
});

test("every view in the router's table is imported", () => {
  // A typo'd key falls through to `VIEWS.dashboard`, so a whole screen
  // can quietly become a second copy of the overview.
  const table = /const VIEWS = \{([\s\S]*?)\n\};/.exec(appSource);
  assert.ok(table, "app.js no longer declares `const VIEWS = {...}`");
  const factories = [...table[1].matchAll(/:\s*(create\w+View)/g)].map(
    (m) => m[1],
  );
  assert.ok(factories.length, "the VIEWS table is empty");
  for (const factory of new Set(factories)) {
    assert.match(
      appSource,
      new RegExp(`import \\{[^}]*\\b${factory}\\b[^}]*\\} from`),
      `${factory} is in the VIEWS table but never imported`,
    );
  }
});

test("no view READS over its own transport", () => {
  // The split transport is what made the Fleet bug possible: a view with
  // its own HTTP client has a failure mode the shell knows nothing about.
  // Reads go through `query`, over the socket the pushes already arrive
  // on, so a view cannot be looking at a different server than the one
  // feeding its live state.
  //
  // Writes are the deliberate exception and stay REST: the socket has no
  // command frame, and a write wants the auth middleware's attribution
  // (`request.state.operator_id` becomes `created_by` on the revision).
  // So a `fetch` here has to name a method, and that method must not be
  // a read.
  const READ_METHODS = new Set(["GET", "HEAD", "OPTIONS"]);
  const problems = [];
  for (const file of VIEW_FILES) {
    const source = readFileSync(join(JS, "views", file), "utf8");
    if (/from "\.\.\/api\.js"/.test(source)) {
      problems.push(`${file} imports the REST client`);
    }
    for (const hit of source.matchAll(/\bfetch\s*\(/g)) {
      // The options object follows the URL; look far enough ahead to
      // clear a template literal and a headers block.
      const tail = source.slice(hit.index, hit.index + 400);
      const method = /method:\s*"(\w+)"/.exec(tail);
      if (!method) {
        problems.push(`${file} calls fetch() with no method — reads use query()`);
      } else if (READ_METHODS.has(method[1].toUpperCase())) {
        problems.push(
          `${file} calls fetch() with method ${method[1]} — reads use query()`,
        );
      }
    }
  }
  assert.deepEqual(problems, [], problems.join("\n"));
});

// ---------------------------------------------------------------------
// Stylesheets against the markup that ships
// ---------------------------------------------------------------------

// Classes that are built ENTIRELY by interpolation, so no literal prefix
// exists for the scan below to recognise. Each names the values that one
// site can produce, so the list stays checkable by reading that site.
const WHOLLY_INTERPOLATED = {
  // llm.js: `<span class="msg-role ${esc(role)}">` — the roles an LLM
  // conversation record can carry.
  "msg-role": ["system", "user", "assistant", "tool"],
};

test("no stylesheet rule targets a class nothing renders", () => {
  // Deleting a view deletes its markup and leaves its stylesheet behind,
  // where it is invisible: dead CSS breaks nothing, so it accumulates
  // until a later rule collides with it. Two rooms' worth of it survived
  // the redesign that cut them.
  //
  // The scan has to know the difference between a class that is GONE and
  // one that is BUILT — `cat-${category}`, `tone-${tone}`, `is-${stale}`
  // never appear as literals — so it harvests every prefix the JS
  // interpolates into and excuses anything starting with one. That
  // deliberately errs toward keeping: a false "dead" is a rule deleted
  // out from under a live element, which is a visual bug, and a false
  // "live" is only unswept CSS.
  const dash = join(JS, "..");
  const readAll = (dir, ext) => {
    const out = [];
    const walk = (d) => {
      for (const e of readdirSync(d, { withFileTypes: true })) {
        const full = join(d, e.name);
        if (e.isDirectory()) walk(full);
        else if (e.name.endsWith(ext)) out.push(readFileSync(full, "utf8"));
      }
    };
    walk(dir);
    return out;
  };

  const stripComments = (text) => text.replace(/\/\*[\s\S]*?\*\//g, "");
  const css = readAll(join(dash, "styles"), ".css").map(stripComments).join("\n");
  const js = readAll(JS, ".js")
    .map((t) => stripComments(t).replace(/\/\/[^\n]*/g, ""))
    .join("\n");
  const html = readFileSync(join(dash, "index.html"), "utf8");
  const markup = js + "\n" + html;

  // Two ways a class name gets built, and both have to be recognised:
  //   `class="ag-chip tone-${g.tone}"`  → interpolation, prefix `tone-`
  //   `"is-" + stale` / `"cat-" + key`  → concatenation, prefix `is-`
  // A harvested prefix must contain a `-`, so a one-letter fragment
  // cannot quietly excuse half the stylesheet.
  const prefixes = [
    ...[...markup.matchAll(/([a-zA-Z][\w-]*)\$\{/g)].map((m) => m[1]),
    ...[...markup.matchAll(/"([a-zA-Z][\w-]*-)"\s*\+/g)].map((m) => m[1]),
  ].filter((p) => p.includes("-"));
  for (const [base, values] of Object.entries(WHOLLY_INTERPOLATED)) {
    assert.ok(
      markup.includes(base),
      `${base} is in WHOLLY_INTERPOLATED but nothing renders it`,
    );
    prefixes.push(...values.map((v) => v));
  }

  const dead = [];
  for (const cls of new Set(stripComments(css).match(/\.([a-zA-Z][\w-]*)/g) || [])) {
    const name = cls.slice(1);
    if (markup.includes(name)) continue;
    if (prefixes.some((p) => name.startsWith(p) || name === p)) continue;
    dead.push(name);
  }
  assert.deepEqual(
    dead.sort(),
    [],
    `stylesheet rules with no markup behind them:\n  ${dead.join("\n  ")}`,
  );
});

await run();
