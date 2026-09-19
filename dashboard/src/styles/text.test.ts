// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

/**
 * How this product cuts text, and the two rules that are not the same rule.
 *
 * `.truncate` cuts at a PIXEL — `white-space: nowrap` plus an ellipsis — and it
 * is right for a value that IS one line: a name, a handle, a key, a grid cell
 * whose row height is the line. `.clamp` cuts at a LINE, and prose takes it.
 *
 * Reaching for the first with prose is what shipped: a seat's goal came out of a
 * 300px card as "Build and run the ingestio…" in a ~160px slot with the rest of
 * the card empty underneath, and a turn card's trigger line as "Message from
 * founder: a task created: Write the …" with room beneath that too.
 *
 * ASSERTED IN THE SHEET because there is nowhere else. jsdom computes no
 * layout, so every suite that renders one of these panels stays green whether
 * the cell wraps or ellipses; the DOM half — which rule an element carries — is
 * beside the component that carries it.
 */

const STYLES = fileURLToPath(new URL(".", import.meta.url));

function sheet(name: string): string {
  return readFileSync(join(STYLES, name), "utf8");
}

/** The body of the first rule whose selector is exactly `selector`. */
function block(css: string, selector: string): string {
  const at = new RegExp(
    `(^|\\})\\s*${selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\s*\\{`,
    "m",
  );
  const m = at.exec(css);
  expect(m, `${selector} is not declared at all`).not.toBeNull();
  const from = m!.index + m![0].length;
  const to = css.indexOf("}", from);
  expect(to, `${selector} has no closing brace`).toBeGreaterThan(-1);
  return css.slice(from, to);
}

/** Every stylesheet in this directory, as text with its comments blanked. */
function everySheet(): { name: string; css: string }[] {
  return readdirSync(STYLES)
    .filter((f) => f.endsWith(".css"))
    .map((name) => ({ name, css: sheet(name).replace(/\/\*[\s\S]*?\*\//g, "") }));
}

describe("how this product cuts text", () => {
  // A CLAMP IS ALL OF ITS DECLARATIONS, because any subset of them does
  // nothing at all — silently, with a valid stylesheet and no warning.
  test("a clamp is every declaration it needs, not a subset of them", () => {
    const clamp = block(sheet("base.css"), ".clamp");
    expect(clamp).toMatch(/display:\s*-webkit-box/);
    expect(clamp).toMatch(/-webkit-box-orient:\s*vertical/);
    expect(clamp).toMatch(/-webkit-line-clamp:\s*var\(--clamp-lines/);
    expect(clamp).toMatch(/line-clamp:\s*var\(--clamp-lines/);
    expect(clamp).toMatch(/overflow:\s*hidden/);
    // A FLEX ITEM THAT CANNOT SHRINK NEVER WRAPS: `min-width: auto` is the
    // default, and it is why `.truncate` carries this too.
    expect(clamp).toMatch(/min-width:\s*0/);
    // NOWRAP IS THE WHOLE BUG. A clamp needs lines to clamp; with
    // `white-space: nowrap` there is only ever one, and the rule renders as
    // `.truncate` minus the ellipsis — strictly worse than what it replaced.
    expect(clamp).not.toMatch(/white-space:\s*nowrap/);
  });

  // THE COUNT BELONGS TO THE BOX, not to a class per number, and it must not
  // fire by ancestry: a card that says three would otherwise clamp every nested
  // caption inside it to three as well.
  test("the line count is a property of the element, and does not inherit", () => {
    const declared = block(sheet("base.css"), "@property --clamp-lines");
    expect(declared).toMatch(/inherits:\s*false/);
    expect(declared).toMatch(/initial-value:\s*2/);
    expect(block(sheet("screens.css"), ".work-card-title")).toMatch(/--clamp-lines:\s*3/);
  });

  // ONE IMPLEMENTATION. `.work-card-title` hand-rolled the same declarations
  // and had already drifted from this one — it never carried `min-width: 0`, so
  // a long unbroken title could refuse to shrink inside its flex parent.
  test("the clamp is written once, so a second copy cannot drift from it", () => {
    const owners = everySheet()
      .flatMap(({ name, css }) =>
        [...css.matchAll(/([^{}]+)\{([^{}]*)\}/g)]
          .filter((m) => /line-clamp/.test(m[2]!))
          .map((m) => `${name}: ${m[1]!.trim()}`),
      )
      .filter((where) => !where.includes("@property"));
    expect(owners, "the clamp is `.clamp` in base.css").toEqual(["base.css: .clamp"]);
  });

  // AND THE TWO RULES ARE NEVER WORN TOGETHER: their `white-space` disagrees,
  // so an element carrying both gets one line with no ellipsis, which is
  // strictly worse than either.
  test("nothing carries both .truncate and .clamp", () => {
    const src = fileURLToPath(new URL("..", import.meta.url));
    const walk = (dir: string): string[] =>
      readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
        e.isDirectory()
          ? walk(join(dir, e.name))
          : /\.tsx?$/.test(e.name) && !e.name.includes(".test.")
            ? [join(dir, e.name)]
            : [],
      );
    const both = walk(src).flatMap((file) =>
      readFileSync(file, "utf8")
        .split("\n")
        .map((line, i) => ({ line, at: `${file.slice(src.length)}:${i + 1}` }))
        .filter(({ line }) => /className=(["`])[^"`]*\btruncate\b[^"`]*\bclamp\b/.test(line))
        .map(({ at, line }) => `${at} — ${line.trim()}`),
    );
    expect(both, "a pixel cut and a line cut are different rules").toEqual([]);
  });
});
