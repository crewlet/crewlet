// @vitest-environment node
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

/**
 * AN ANCHOR IN A SENTENCE SAYS SO, because nothing else can tell.
 *
 * `base.css` resets every anchor to no underline, which is right for the
 * chrome this product is mostly made of — a breadcrumb, a row that happens to
 * be an anchor, a caption-sized navigation — because each of those is a THING
 * on the page rather than a word in a line. It is wrong the moment an anchor
 * IS a word in a line: the sentence around it is `--text`, the link is
 * `--accent-ink`, and colour alone is what WCAG 1.4.1 refuses. Those anchors
 * take `.prose-link`, which puts the underline back and keeps it.
 *
 * # Why a scan, and why it keys on the CLASS ATTRIBUTE
 *
 * The rule was first written as prose in `base.css` — "the two containers
 * below are the only registers in this tree that are genuinely a phrase" — and
 * nothing checked it. It was wrong when it was written: SEVEN anchors sat in
 * running sentences outside those containers, in five different files, and
 * every one of them had lost its only non-colour cue. A claim about which
 * registers exist is exactly the kind that decays, because the next sentence
 * with a link in it is written in a sixth file by somebody who never read the
 * comment.
 *
 * No scan can decide whether an anchor is inside a sentence — that is a
 * question about the prose around it. What a scan CAN do is refuse the one
 * shape where the question was never asked: an `<a>` with no `className` at
 * all. Every anchor in this tree is either chrome, and carries the class that
 * draws it, or prose, and carries `.prose-link`. One with neither is one
 * nobody classified, which is how all seven arrived.
 *
 * TWO-SIDED, like every gate in this directory: an entry in [ALLOWED] whose
 * anchor is gone fails too, so the list cannot outlive what it excuses.
 */

const SRC = fileURLToPath(new URL("..", import.meta.url));

/** An anchor this scan does not ask about, and why it does not have to. */
interface Allowed {
  /** `file:line`, so a moved anchor is re-read rather than silently excused. */
  where: string;
  why: string;
}

const ALLOWED: Allowed[] = [
  {
    where: "routes/work/Work.tsx",
    why:
      "The two `N more →` links at the foot of a column and of a group. Each is " +
      "a standalone call to action on its own line, not a word in a sentence, and " +
      "`.work-col-foot a` draws it — so the reset is right and there is no prose " +
      "for an underline to separate it from.",
  },
  {
    where: "routes/knowledge/Pages.tsx",
    why:
      "The ancestor trail and the children list under 'Where it sits'. Both are " +
      "LISTS of links — a path and a set of siblings — rather than links inside a " +
      "sentence, so the reset is right for them: what separates them from the text " +
      "around them is that there is no text around them. They carry no class " +
      "because the containers draw them, which is the one case the forward " +
      "question has a good answer without one.",
  },
];

const SOURCE = /\.tsx$/;

function files(directory: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(directory)) {
    const path = join(directory, entry);
    if (statSync(path).isDirectory()) out.push(...files(path));
    else if (SOURCE.test(entry) && !entry.includes(".test.")) out.push(path);
  }
  return out;
}

/**
 * Every `<a` in the tree that opens without a `className`.
 *
 * Read to the tag's own `>` rather than line by line, because a formatted
 * anchor spreads its attributes over five lines and a per-line scan reports
 * the opening of every one of them.
 *
 * COMMENTS ARE BLANKED FIRST, KEPT LINE FOR LINE. This file's own subject is
 * anchors, so the prose explaining them writes `<a download>` and `<a>` in
 * passing — and a scan that reads its own explanations reports the
 * explanation as the offence. (It did, on the first run.) Block comments go
 * whole, which covers the JSX `{/* … *\/}` form; line comments only where
 * `//` opens the line, because a `//` mid-line is far more likely to be the
 * middle of an `https://` inside an `href` than a comment, and cutting the
 * rest of THAT line would truncate the tag being judged.
 */
export function unclassified(source: string): number[] {
  const text = source
    .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, " "))
    .replace(/^[ \t]*\/\/[^\n]*/gm, "");
  const lines: number[] = [];
  for (const match of text.matchAll(/<a\s[^>]*>/g)) {
    if (/className=/.test(match[0])) continue;
    lines.push(text.slice(0, match.index).split("\n").length);
  }
  return lines;
}

describe("an anchor in a sentence", () => {
  const scanned = files(SRC).map((path) => ({
    where: path.slice(SRC.length),
    text: readFileSync(path, "utf8"),
  }));

  test("carries a class, so somebody decided which kind of link it is", () => {
    const bare = scanned
      .filter((file) => !ALLOWED.some((entry) => entry.where === file.where))
      .flatMap((file) => unclassified(file.text).map((line) => `${file.where}:${line}`));
    expect(
      bare,
      "give it `.prose-link` if it sits in a sentence, or the class that draws it if it is chrome",
    ).toEqual([]);
  });

  test("and an excuse cannot outlive the anchor it excuses", () => {
    const stale = ALLOWED.filter((entry) => {
      const file = scanned.find((f) => f.where === entry.where);
      return !file || unclassified(file.text).length === 0;
    }).map((entry) => entry.where);
    expect(stale, "these files no longer hold a classless anchor — drop the entry").toEqual([]);
  });

  test("it can tell — a bare anchor is caught, a classed one is not", () => {
    // The mutation this file exists to catch, run against the function the
    // tests above call. Both arms: the multi-line form is the one a formatter
    // produces, and the one a per-line scan gets wrong.
    expect(unclassified('<a href="#/x">y</a>')).toEqual([1]);
    expect(unclassified('<a className="t-link" href="#/x">y</a>')).toEqual([]);
    expect(unclassified('\n<a\n  className="prose-link"\n  href="#/x"\n>\n  y\n</a>')).toEqual([]);
    expect(unclassified('\n\n<a\n  href="#/x"\n>\n  y\n</a>')).toEqual([3]);
    // And the trap this scan fell into on its first run: prose ABOUT anchors.
    expect(unclassified("  // see `<a download>` for why\n")).toEqual([]);
    expect(unclassified("/* an `<a href>` in a block comment */")).toEqual([]);
    expect(unclassified('{/* a JSX comment naming <a href="#/x"> */}')).toEqual([]);
    // A `//` inside an href is not a comment, and the anchor is still judged.
    expect(unclassified('<a href="https://x.example">y</a>')).toEqual([1]);
  });

  test("the scan reads the tree at all, so it cannot pass on nothing", () => {
    expect(scanned.length).toBeGreaterThan(50);
    expect(scanned.some((file) => /className="prose-link"/.test(file.text))).toBe(true);
  });
});
