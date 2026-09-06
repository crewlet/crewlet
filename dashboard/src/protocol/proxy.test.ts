/**
 * Every engine path the dashboard fetches is proxied by the dev server.
 *
 * THIS FAILS SILENTLY AND LOOKS LIKE THE ENGINE'S OWN ANSWER, which is why it
 * is asserted rather than remembered. A prefix Vite has no proxy entry for is
 * served by Vite itself, and Vite answers 404 for a path it has no file
 * for. So the screen sees a refusal it cannot tell from the engine refusing:
 * the Integrations page rendered "not configured" for a company that had
 * configured things, and the degraded-mode snapshot poll (the one read that
 * exists for when everything else is already down) returned null forever.
 *
 * Sources are read as TEXT rather than imported, because the point is what
 * the config FILE lists next to what the modules actually call — a fetch
 * whose path is assembled at runtime is exactly the one nothing else checks.
 */

import { describe, expect, it } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";

// Vitest runs with the dashboard package as its root, and under jsdom
// `import.meta.url` is an http URL rather than a file one.
const root = process.cwd();
const src = join(root, "src");

/** Every .ts/.tsx under src that is not itself a test. */
function sources(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) {
      out.push(...sources(path));
      continue;
    }
    if (!/\.tsx?$/.test(entry.name) || entry.name.includes(".test.")) continue;
    out.push(path);
  }
  return out;
}

/**
 * The first path segment of every absolute path the dashboard fetches.
 *
 * Three shapes, because all three are in the tree: a `rest.*` call, a bare
 * `fetch(location.origin + "/…")`, and a module constant the socket dials.
 */
function fetched(): Set<string> {
  const found = new Set<string>();
  const patterns = [
    /rest\.(?:get|post|put|patch|del)\(\s*[`"'](\/[a-z][a-z0-9/_-]*)/g,
    /fetch\(\s*(?:location\.origin\s*\+\s*)?[`"'](\/[a-z][a-z0-9/_-]*)/g,
    /=\s*["'](\/(?:ws\/)?[a-z][a-z0-9/_-]*)["']\s*;/g,
  ];
  for (const file of sources(src)) {
    const text = readFileSync(file, "utf8");
    for (const pattern of patterns) {
      for (const match of text.matchAll(pattern)) {
        const path = match[1]!;
        // The socket's own path is two segments and is proxied as both, so
        // keep it whole; everything else joins on its first segment.
        found.add(path.startsWith("/ws/") ? "/ws/stream" : "/" + path.split("/")[1]);
      }
    }
  }
  return found;
}

/** The prefixes vite.config.ts declares, as written. */
function proxied(): Set<string> {
  const config = readFileSync(join(root, "vite.config.ts"), "utf8");
  const block = config.slice(config.indexOf("proxy: {"));
  return new Set([...block.matchAll(/["'](\/[a-z][a-z0-9/_-]*)["']\s*:\s*\{/g)].map((m) => m[1]!));
}

describe("the dev server proxy", () => {
  it("forwards every engine path the dashboard fetches", () => {
    const missing = [...fetched()].filter((path) => !proxied().has(path)).sort();
    expect(missing).toEqual([]);
  });

  it("finds the paths it is meant to be checking", () => {
    // Without this the test above passes on an empty set, which is exactly
    // how a scanner that stops matching reports everything as fine.
    const paths = fetched();
    expect(paths.has("/setup")).toBe(true);
    expect(paths.has("/secrets")).toBe(true);
    expect(paths.has("/stream")).toBe(true);
    expect(paths.has("/ws/stream")).toBe(true);
  });
});
