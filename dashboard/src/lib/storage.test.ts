// @vitest-environment node
/**
 * Every key this dashboard keeps in a browser is declared in ONE table, and a
 * key nothing reads any more is removed rather than left behind.
 *
 * Browser storage outlives every build, so a key is a promise about a
 * reader's profile that deleting the module does not retract: the app rail's
 * `crewlet.rail.collapsed` outlived the rail. Both halves are about the SOURCE
 * rather than a browser, so they are read off it.
 */

import { describe, expect, test, vi } from "vitest";

import { RETIRED_STORAGE_KEYS, STORAGE_KEYS, forgetRetiredKeys } from "./storage.ts";
import { type Node, lineOf, modules, parse, stringValue, walk } from "~/test/source.ts";

/** A string that has the shape of one of this product's storage keys. */
const KEY_SHAPE = /^crewlet[._][a-z0-9_.]+$/;
/** …and the shapes that are file names instead, which share the prefix. */
const FILE = /\.(ya?ml|json)$/;

/** Every constant string the module spells, with its line. */
function literals(
  text: string,
  lang: Parameters<typeof parse>[1],
): { value: string; line: number }[] {
  const line = lineOf(text);
  const out: { value: string; line: number }[] = [];
  walk(parse(text, lang), (node: Node) => {
    const value = stringValue(node);
    if (value !== null) out.push({ value, line: line(node.start) });
  });
  return out;
}

describe("the registry", () => {
  test("no module outside it spells a storage key", () => {
    const stray: string[] = [];
    for (const mod of modules()) {
      if (mod.path === "lib/storage.ts" || mod.lang === "dts") continue;
      for (const { value, line } of literals(mod.text, mod.lang)) {
        if (KEY_SHAPE.test(value) && !FILE.test(value))
          stray.push(`${mod.path}:${line} — ${value}`);
      }
    }
    expect(stray, "declare the key in lib/storage.ts and import it by name").toEqual([]);
  });

  test("the rule can tell a key from a file name", () => {
    const found = literals(
      'const a = "crewlet_theme"; const b = "crewlet.peek.width"; const c = "crewlet.yaml";',
      "ts",
    )
      .map((l) => l.value)
      .filter((v) => KEY_SHAPE.test(v) && !FILE.test(v));
    expect(found).toEqual(["crewlet_theme", "crewlet.peek.width"]);
  });

  // TWO-SIDED: a key the table declares and nothing reads is a key nothing
  // cleans up either — it belongs in the retired list instead.
  test("every declared key is read by something", () => {
    const text = modules()
      .filter((m) => m.path !== "lib/storage.ts")
      .map((m) => m.text)
      .join("\n");
    const unread = Object.keys(STORAGE_KEYS).filter(
      (name) => !new RegExp(`STORAGE_KEYS\\.${name}\\b`).test(text),
    );
    expect(unread, "retire these: move them to RETIRED_STORAGE_KEYS").toEqual([]);
  });

  test("a retired key is not also a live one, and the two lists share no value", () => {
    const live = new Set<string>(Object.values(STORAGE_KEYS));
    expect(RETIRED_STORAGE_KEYS.filter((k) => live.has(k))).toEqual([]);
    expect(RETIRED_STORAGE_KEYS).toContain("crewlet.rail.collapsed");
    expect(new Set(Object.values(STORAGE_KEYS)).size).toBe(Object.values(STORAGE_KEYS).length);
  });
});

describe("a retired key", () => {
  test("is removed at boot, and nothing live is touched", () => {
    const removed: string[] = [];
    forgetRetiredKeys(() => ({ removeItem: (k: string) => removed.push(k) }));
    expect(removed).toEqual([...RETIRED_STORAGE_KEYS]);
  });

  test("a storage that refuses does not stop the boot", () => {
    expect(() =>
      forgetRetiredKeys(() => {
        throw new DOMException("denied", "SecurityError");
      }),
    ).not.toThrow();
    const refusing = {
      removeItem: vi.fn(() => {
        throw new Error("quota");
      }),
    };
    expect(() => forgetRetiredKeys(() => refusing)).not.toThrow();
  });

  test("the entry point runs the sweep", () => {
    const main = modules().find((m) => m.path === "main.tsx")!;
    expect(main.text).toMatch(/forgetRetiredKeys\(\s*\(\)\s*=>\s*localStorage\s*\)/);
  });
});
