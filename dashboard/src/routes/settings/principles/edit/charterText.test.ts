/**
 * The editor judges charter text as the engine does: the same trim, the same
 * count, the same bounds and the same line breaks.
 *
 * The vectors are the engine's own file (`internal/config/testdata/
 * charter_text.json`), which `internal/config`'s
 * `TestTheCharterCountsCharactersAsTheDashboardDoes` and
 * `TestTheCharterBreaksALineWhereTheDashboardDoes` hold the engine's admission
 * rules to. A draft the editor calls fine and the engine refuses is a Save
 * that fails for a reason the field never showed; one the editor refuses and
 * the engine would take is a charter nobody can write. Either is a failing
 * test here or there, rather than a person's afternoon.
 */

import { expect, test } from "vitest";
import { engineFile } from "~/test/engineFiles.ts";
import {
  CHARTER_LIMITS,
  lengthOf,
  problemsOf,
  trimLikeEngine,
  type CharterPart,
} from "./charter.ts";

interface Length {
  name: string;
  field: "mission" | "vision" | "policy";
  prefix?: string;
  unit: string;
  repeat: number;
  suffix?: string;
  chars: number;
  utf16: number;
  bytes: number;
  graphemes: number;
  admitted: boolean;
}

interface CharterText {
  lengths: Length[];
  breaks: { name: string; char: string }[];
}

const text = engineFile<CharterText>("internal/config/testdata/charter_text.json");

const valueOf = (v: Length) => (v.prefix ?? "") + v.unit.repeat(v.repeat) + (v.suffix ?? "");

const graphemes = (s: string) =>
  [...new Intl.Segmenter(undefined, { granularity: "grapheme" }).segment(s)].length;

/** What the editor says is wrong with `value` in `field`, said at the field or the item. */
function refusal(field: Length["field"], value: string): string | undefined {
  if (field === "policy") return problemsOf("policies", [value]).items?.[0];
  return problemsOf(field satisfies CharterPart, value).field;
}

test("the shared vectors are there to be read", () => {
  expect(text.lengths.length).toBeGreaterThan(0);
  expect(text.breaks.length).toBeGreaterThan(0);
});

// EVERY VECTOR, COUNTED AND JUDGED AS THE ENGINE JUDGES IT. Mutation: count
// with `.length` in `lengthOf` and every emoji vector fails; count bytes and
// every accent and CJK vector does; trim with `String.prototype.trim` and the
// byte order mark and next-line vectors do.
test.each(text.lengths.map((v) => [v.name, v] as const))("%s", (_name, v) => {
  const value = valueOf(v);
  const stored = trimLikeEngine(value);
  // THE VECTOR SAYS WHAT IT CLAIMS: the counts the engine's test checks in Go,
  // and the grapheme count, which only a browser can take.
  expect(new TextEncoder().encode(stored).length).toBe(v.bytes);
  expect(stored.length).toBe(v.utf16);
  expect(graphemes(stored)).toBe(v.graphemes);
  expect(lengthOf(value)).toBe(v.chars);
  const said = refusal(v.field, value);
  if (v.admitted) expect(said).toBeUndefined();
  else expect(said).toMatch(/characters/);
});

// A VECTOR SET THAT COULD NOT TELL THE COUNTS APART PROVES NOTHING. The Go
// test holds bytes and UTF-16 units to this; the grapheme half is here,
// where a grapheme count is one call away.
test("the vectors part every count a client might reach for from the engine's", () => {
  const bound = {
    mission: CHARTER_LIMITS.mission,
    vision: CHARTER_LIMITS.vision,
    policy: CHARTER_LIMITS.policy,
  } as const;
  const parts = (count: (v: Length) => number) =>
    text.lengths.some((v) => count(v) <= bound[v.field] !== v.admitted);
  expect(parts((v) => v.utf16)).toBe(true);
  expect(parts((v) => v.bytes)).toBe(true);
  expect(parts((v) => v.graphemes)).toBe(true);
});

// ONE LINE FOR THE MISSION AND EACH POLICY, in every character the engine
// reads as ending one; the vision is paragraphs. Mutation: drop `\v` from the
// editor's line-break rule and the vertical tab fails.
test.each(text.breaks.map((b) => [b.name, b.char] as const))(
  "%s breaks the mission and a policy, and not the vision",
  (_name, char) => {
    const value = `Split costs${char}fairly.`;
    expect(refusal("mission", value)).toMatch(/one line/);
    expect(refusal("policy", value)).toMatch(/one line/);
    expect(refusal("vision", value)).toBeUndefined();
  },
);
