import { expect, test } from "vitest";
import {
  ARGUMENTS_NOT_KNOWN,
  argumentsInPlace,
  HELD_ARGUMENTS,
  SEALED_ARGUMENTS,
  UNKNOWN_ARGUMENTS,
  unknownArguments,
} from "./withheld.ts";

/**
 * WHAT IS SAID IN PLACE OF A CALL'S ARGUMENTS, wherever a reader meets the call.
 *
 * A sealed call says which of its two it is, a call whose arguments are not
 * known says why, and a reason this build has no words for (a newer engine's)
 * is still not known. Each is a call made WITH arguments, so none is ever the
 * empty string, which a reader takes as "draw the arguments as they stand",
 * `{}` or "(none)", the tool called with none. And a call whose record holds
 * what it was made with is drawn as it stands.
 *
 * Mutation: return "" for a reason this build does not know, let an unknown
 * reason outrank a seal, or say anything for a plain call, and this goes red.
 */
test("a call's arguments are said where no record holds them, and only there", () => {
  expect(argumentsInPlace({ approved: true })).toBe(SEALED_ARGUMENTS);
  expect(argumentsInPlace({ held: true, argsUnknown: "unkept" })).toBe(HELD_ARGUMENTS);
  expect(argumentsInPlace({ argsUnknown: "unread" })).toBe(UNKNOWN_ARGUMENTS.unread);
  expect(argumentsInPlace({ argsUnknown: "unkept" })).toBe(UNKNOWN_ARGUMENTS.unkept);
  expect(argumentsInPlace({ argsUnknown: "truncated" })).toBe(ARGUMENTS_NOT_KNOWN);
  expect(argumentsInPlace({})).toBe("");
  expect(argumentsInPlace({ argsUnknown: "" })).toBe("");
});

test("every reason's words say the arguments are not known, never none", () => {
  for (const words of [...Object.values(UNKNOWN_ARGUMENTS), unknownArguments("later")]) {
    expect(words).toMatch(/^\(not known/);
    expect(words).not.toMatch(/none|\{\}/);
  }
  expect(unknownArguments("unread")).not.toBe(unknownArguments("unkept"));
});
