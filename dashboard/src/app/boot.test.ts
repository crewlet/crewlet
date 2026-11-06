/**
 * What the shell paints before the app has loaded.
 *
 * On a slow connection every route was a blank page until every module had
 * arrived, which reads as a dead site. The boot screen stands in the root
 * element until React replaces it, and the engine's policy (`script-src 'self';
 * style-src 'self'`) refuses any inline script or style, so neither may carry it.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, expect, test } from "vitest";
import { STORAGE_KEYS } from "~/lib/storage.ts";

const read = (path: string) => readFileSync(join(process.cwd(), path), "utf8");
const shell = read("index.html").replace(/<!--[\s\S]*?-->/g, "");
const script = read("src/app/boot-theme.js");

afterEach(() => {
  localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
  document.documentElement.removeAttribute("data-palette");
});

// Mutation: empty the root element.
test("the root element holds a boot screen, named for a screen reader", () => {
  const root = /<div id="root">([\s\S]*)<\/div>\s*<\/body>/.exec(shell)?.[1] ?? "";
  expect(root).toContain('class="boot"');
  expect(root).toContain('role="status"');
  expect(root).toContain("Loading Crewlet");
});

// A STYLE OR A SCRIPT WRITTEN INTO THE SHELL IS REFUSED BY THE POLICY IT IS
// SERVED UNDER, silently, in production only.
test("the shell carries no inline script or style", () => {
  expect(shell).not.toMatch(/<style|\sstyle=/);
  for (const tag of shell.match(/<script\b[^>]*>/g) ?? []) expect(tag).toMatch(/\ssrc=/);
});

const run = () => new Function(script)();

// THE STORED CHOICE PAINTS BEFORE THE APP DOES, under the keys the app writes it
// to. Mutation: read another key, and a stored dark reader boots light.
test("the boot script paints the stored mode and theme, and leaves a system reader alone", () => {
  expect(script).toContain(`"${STORAGE_KEYS.theme}"`);
  expect(script).toContain(`"${STORAGE_KEYS.palette}"`);
  run();
  expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
  expect(document.documentElement.hasAttribute("data-palette")).toBe(false);
  localStorage.setItem(STORAGE_KEYS.theme, "system");
  run();
  expect(document.documentElement.hasAttribute("data-theme")).toBe(false);
  localStorage.setItem(STORAGE_KEYS.theme, "dark");
  localStorage.setItem(STORAGE_KEYS.palette, "ocean");
  run();
  expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  expect(document.documentElement.getAttribute("data-palette")).toBe("ocean");
});
