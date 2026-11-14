import { expect, test } from "vitest";
import { elapsedWords } from "./Waiting.tsx";

// THE COUNT READS AS A DURATION AT EVERY LENGTH a harness run has: seconds
// alone under a minute, minutes with two-figure seconds past it, and never a
// negative figure for a clock that stepped back.
test("a wait is counted in seconds, then in minutes and seconds", () => {
  expect(elapsedWords(0)).toBe("0 s");
  expect(elapsedWords(999)).toBe("0 s");
  expect(elapsedWords(17_400)).toBe("17 s");
  expect(elapsedWords(65_000)).toBe("1 min 05 s");
  expect(elapsedWords(120_000)).toBe("2 min 00 s");
  expect(elapsedWords(-5_000)).toBe("0 s");
});
