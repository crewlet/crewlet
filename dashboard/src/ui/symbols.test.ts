/**
 * The drawings this build holds are the drawings upstream drew.
 *
 * `src/ui/symbols/` vendors four Material Symbols `@crewlethq/icons` has not,
 * and `glyph.tsx` carries their path data inline so a page draws them without
 * a fetch, a sprite or a bundler plugin. That inlining is the hazard: a path is
 * a 200-character string nobody reads, so an edit to one — a merge resolved the
 * wrong way, a "tidy" that dropped a segment — changes what the product draws
 * and looks like nothing in review.
 *
 * Two checks, because each catches what the other cannot. The CHECKSUMS catch
 * an edit to a vendored file, which is what `SHA256SUMS` is for and what the
 * upstream package verifies with `shasum -c`. The PATH COMPARISON catches an
 * edit to `glyph.tsx`, where the checksums have nothing to say: the file on
 * disk would still be upstream's and the product would still draw something
 * else.
 *
 * THE FILES ARE REACHED THROUGH VITE rather than through `fs` and a path built
 * from `import.meta.url`, which is the idiom the rest of this directory could
 * use and this one cannot: a glob is resolved by the bundler at transform time,
 * so it answers the same under `vitest`, under `vite build` and from whatever
 * directory either was started in.
 */

import { createHash } from "node:crypto";
import { expect, test } from "vitest";
import { LOCAL_DRAWINGS } from "./glyph.tsx";

const SVGS = import.meta.glob("./symbols/*/*.svg", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;
import SUMS from "./symbols/SHA256SUMS?raw";

/** `./symbols/24/star.svg` as `24/star.svg`, which is how SHA256SUMS spells it. */
function key(path: string): string {
  return path.replace("./symbols/", "");
}

/** The one shape `@crewlethq/icons`' own build accepts, asserted on ours too. */
const SVG =
  /^<svg xmlns="http:\/\/www\.w3\.org\/2000\/svg" height="(?:20|24)" viewBox="0 -960 960 960" width="(?:20|24)"><path d="([^"]+)"\/><\/svg>$/;

test("the glob reaches the vendored drawings at all", () => {
  // A GLOB THAT MATCHES NOTHING passes every assertion below it, which is the
  // one way this suite could go quiet without anybody moving a file.
  expect(Object.keys(SVGS).length).toBeGreaterThanOrEqual(8);
  expect(SUMS.trim().length).toBeGreaterThan(0);
});

test("every vendored drawing matches its recorded checksum", () => {
  const recorded = new Map(
    SUMS.split("\n")
      .filter(Boolean)
      .map((line) => {
        const [sum, path] = line.trim().split(/\s+/);
        return [path, sum] as const;
      }),
  );
  // BOTH DIRECTIONS. A checksum file that simply stopped listing a drawing
  // would verify clean while covering nothing, which is the failure a count
  // cannot see either.
  expect([...recorded.keys()].sort()).toEqual(Object.keys(SVGS).map(key).sort());
  for (const [path, svg] of Object.entries(SVGS)) {
    const sum = createHash("sha256").update(svg).digest("hex");
    expect(`${key(path)}: ${sum}`).toBe(`${key(path)}: ${recorded.get(key(path))}`);
  }
});

test("every vendored drawing is one path on the 960 grid", () => {
  for (const [path, svg] of Object.entries(SVGS)) {
    expect(`${key(path)}: ${SVG.test(svg.trim())}`).toBe(`${key(path)}: true`);
  }
});

test("the path data this build renders is the vendored file's own", () => {
  // Every vendored file is drawn, and nothing is drawn that is not vendored: a
  // name in one and not the other is a mark that renders blank, or a drawing
  // nothing verifies.
  const vendored = Object.keys(SVGS)
    .map(key)
    .map((p) => p.replace(/\.svg$/, ""))
    .sort();
  const drawn = Object.entries(LOCAL_DRAWINGS)
    .flatMap(([name, sizes]) => Object.keys(sizes).map((px) => `${px}/${name}`))
    .sort();
  expect(drawn).toEqual(vendored);

  for (const [path, svg] of Object.entries(SVGS)) {
    const [px = "", file = ""] = key(path).split("/");
    const name = file.replace(/\.svg$/, "") as keyof typeof LOCAL_DRAWINGS;
    expect(`${key(path)}: ${LOCAL_DRAWINGS[name][Number(px) as 20 | 24]}`).toBe(
      `${key(path)}: ${SVG.exec(svg.trim())?.[1]}`,
    );
  }
});
