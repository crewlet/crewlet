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
import { readFileSync, readdirSync } from "node:fs";
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

await run();
