// @vitest-environment node
/**
 * Every fact a screen draws is drawn in an ink a reader can read.
 *
 * `--text-faint` is DECORATION ONLY — `src/styles/uilet.css` says so at the
 * token, and it is the rung below the one the palette is measured for as TEXT.
 * Ninety-eight spellings of `.faint` put WORDS in it: "not set", "none",
 * "nobody", "no summary recorded", a seat's goal, a node's remedy, a
 * schedule's next fire, an integration's notes, and the ten spellings of
 * `t-caption faint`, which overrode `.t-caption`'s own muted step DOWN to the
 * decoration one for nothing.
 *
 * There is no third text utility for words now: a fact takes `.muted`, which
 * is the step it has always been measured for, and `t-caption` already draws
 * it.
 *
 * WHAT IS EXEMPT AND WHY. A mark beside words the reader already has is
 * decoration, because the label carries the meaning — the tree guide in a
 * trace and the fallback arrow between two model keys are both that. Each
 * exemption is NAMED with its reason rather than counted: a count goes green
 * when one site is fixed and another appears, which is the shape
 * `internal/skipgate` exists in this repository to have removed.
 *
 * READ AS TEXT, and the screens deliberately are not imported: this runs in
 * the node environment, and importing them would pull a React module graph
 * into it to answer a question about a string. The same argument
 * `lib/phaseOrder.test.ts` makes for the same shape.
 */

import { describe, expect, test } from "vitest";

import { modules } from "../test/source.ts";

/** The directories whose sources a reader reads facts out of, relative to `src/`. */
const SCANNED = ["routes", "components"];

/**
 * The sites where `faint` is a MARK rather than a word, with the reason.
 *
 * Keyed on what the element draws rather than on a line number, so moving a
 * screen does not fail the gate and deleting the mark does not silently widen
 * it: an entry that matches nothing is reported below.
 */
const DECORATION: { draws: string; why: string }[] = [
  {
    draws: "└ ",
    why: "the tree guide down a trace's rows: the row's own words carry the depth",
  },
  {
    draws: 'title="falls back to"',
    why: "the arrow between two model keys, which the keys either side of it name",
  },
];

interface Site {
  file: string;
  line: number;
  text: string;
}

function sources(): { file: string; text: string }[] {
  return (
    modules()
      .filter(({ path }) => SCANNED.some((dir) => path.startsWith(`${dir}/`)))
      // The org builder arrived from another branch with its own sheet and its
      // own gate (`routes/org/builder/builderStyles.test.ts`); it is not this
      // scan's subject.
      .filter(({ path }) => !path.split("/").slice(0, -1).includes("builder"))
      .map(({ path, text }) => ({ file: path, text }))
  );
}

/** Every `className` in the shipped screens that names the decoration step. */
function faintSites(): Site[] {
  const found: Site[] = [];
  for (const { file, text } of sources()) {
    text.split("\n").forEach((line, i) => {
      if (/className=(["`])[^"`]*\bfaint\b/.test(line)) {
        found.push({ file, line: i + 1, text: line.trim() });
      }
    });
  }
  return found;
}

const exempt = (site: Site, all: Site[]): { draws: string; why: string } | undefined =>
  DECORATION.find((d) => {
    const at = all.indexOf(site);
    const next = all[at]?.text ?? "";
    return site.text.includes(d.draws) || next.includes(d.draws);
  });

describe("the ink a fact is drawn in", () => {
  test("no screen writes words in the decoration step", () => {
    const sites = faintSites();
    const undeclared = sites
      .filter((s) => !exempt(s, sites))
      .map((s) => `${s.file}:${s.line} — ${s.text}`);
    expect(undeclared).toEqual([]);
  });

  // TWO-SIDED, for the reason `internal/skipgate` gives: an exemption that
  // stopped firing is one nobody can tell from a rule that is still working.
  test("and every decoration named here is still drawn", () => {
    const sites = faintSites();
    const stale = DECORATION.filter((d) => !sites.some((s) => s.text.includes(d.draws))).map(
      (d) => `${d.draws} — ${d.why}`,
    );
    expect(stale).toEqual([]);
  });

  // THE CONTROL, and it is the half that keeps the two above honest: a scan
  // whose pattern stopped matching anything passes every list for ever. This
  // is the pattern reading a line it must catch.
  test("the scan still recognises what it polices", () => {
    const line = '  <span className="t-caption faint">not set</span>';
    expect(/className=(["`])[^"`]*\bfaint\b/.test(line)).toBe(true);
    expect(/className=(["`])[^"`]*\bfaint\b/.test('  <span className="muted">not set</span>')).toBe(
      false,
    );
    // And it read real files rather than an empty directory.
    expect(sources().length).toBeGreaterThan(20);
  });
});
