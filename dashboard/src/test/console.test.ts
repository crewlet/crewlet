/**
 * The console gate judges what a case says — and only a gate that can fail is
 * one. These cases drive its recorder directly, because a case that let the
 * real console fail would fail this suite, and each ends on a verdict of its
 * own so nothing it recorded reaches the gate's judgement of the case. The
 * wiring is held by the last three: one says something on purpose and
 * declares it, and two see that `setup.ts` judged between them.
 */

import { describe, expect, test } from "vitest";
import { consoleText, expectConsole, record, verdict, verdictsGiven } from "./console.ts";

describe("the console gate", () => {
  test("fails a case that said something on error or warn and declared nothing", () => {
    expect(record("error", ["Each child in a list should have a unique key prop."], "a case")).toBe(
      false,
    );
    expect(record("warn", ["announce() was called with no <Announcer> mounted"])).toBe(false);
    const problems = verdict();
    expect(problems).toHaveLength(2);
    expect(problems[0]).toContain('during "a case"');
    expect(problems[0]).toContain("unique key");
    expect(problems[1]).toContain("between cases");
  });

  test("takes a declared line as the case's own, on its level only", () => {
    expectConsole("error", /could not be loaded/);
    expect(record("error", [new Error("The Spend screens could not be loaded")])).toBe(true);
    // THE SAME WORDS ON THE OTHER STREAM are not what was declared.
    expect(record("warn", ["The Spend screens could not be loaded"])).toBe(false);
    expect(verdict()).toHaveLength(1);
  });

  test("fails a declaration nothing met, so none outlives what it excused", () => {
    expectConsole("error", /model is an object/);
    const problems = verdict();
    expect(problems).toEqual([
      "console.error was expected to say /model is an object/, and did not",
    ]);
  });

  test("starts every case afresh", () => {
    record("error", ["left over"]);
    expectConsole("warn", /never said/);
    verdict();
    expect(verdict()).toEqual([]);
  });

  test("reads an error as its name and message, and anything else as JSON", () => {
    expect(consoleText([new TypeError("boom"), "and", { a: 1 }])).toBe(
      'TypeError: boom and {"a":1}',
    );
  });

  // REACT WRITES EVERY REPORT AS A FORMAT STRING, so a quote of the string
  // alone said "%o / %s" and nothing of what broke.
  test("fills a format string in the way the console prints it", () => {
    expect(
      consoleText([
        "%o\n\n%s\n\n%s",
        new Error("model is an object"),
        "The above error occurred in the <Throws> component.",
        "React will try to recreate this component tree.",
        "and one left over",
      ]),
    ).toBe(
      "Error: model is an object\n\nThe above error occurred in the <Throws> component.\n\n" +
        "React will try to recreate this component tree. and one left over",
    );
    expect(consoleText(["%cstyled%c, 100%% and %s", "color: red", "", "unfilled %d"])).toBe(
      "styled, 100% and unfilled %d",
    );
    record("error", ["%o\n\n%s", new Error("model is an object"), "The above error occurred."]);
    expect(verdict()).toEqual([
      "console.error said between cases, and nothing declared it: " +
        "Error: model is an object / The above error occurred.",
    ]);
  });

  // THE WIRING: this case writes to the real console, so it passes only
  // because the declaration covers it — drop the declaration and the gate in
  // `setup.ts` fails it after it ends.
  test("is wired in front of the real console", () => {
    expectConsole("error", /the gate's own case/);
    console.error("the gate's own case says this on purpose");
  });

  // AND JUDGES AFTER EVERY CASE: exactly one verdict is given between the end
  // of one case and the start of the next, which is `setup.ts`'s. With none
  // the gate would record what every case said and hold none to it.
  let atEnd = -1;
  test("is judged after a case (the case before)", () => {
    atEnd = verdictsGiven();
  });
  test("is judged after a case (the case after)", () => {
    expect(verdictsGiven()).toBe(atEnd + 1);
  });
});
