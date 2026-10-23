/**
 * The engine's sentences as the builder draws them: sentence case where they
 * open on a word, a backticked key as code, and every word the engine's own.
 */

import { render } from "@testing-library/react";
import { expect, test } from "vitest";
import { engineSentence } from "./EngineSentence.tsx";

const drawn = (text: string) => render(<p>{engineSentence(text)}</p>).container.firstElementChild!;

// THE ADVISORY ABOUT A COMPANY WITH NO PEOPLE opened lower case and printed
// its backticks beside the sentence-case callouts around it.
test("a sentence opening on a word is raised, and a backticked key is code", () => {
  const p = drawn("this company has no `kind: human` seats, so nobody can be invited");
  expect(p.textContent).toBe("This company has no kind: human seats, so nobody can be invited");
  expect(p.querySelector("code")?.textContent).toBe("kind: human");
});

test("a message opening on a field path keeps the key's case", () => {
  expect(drawn("units[0].roles[0].goal: missing").textContent).toBe(
    "units[0].roles[0].goal: missing",
  );
});

test("an unmatched backtick is a character, not code that never ends", () => {
  const p = drawn("a value with one ` mark");
  expect(p.textContent).toBe("A value with one ` mark");
  expect(p.querySelector("code")).toBeNull();
});
