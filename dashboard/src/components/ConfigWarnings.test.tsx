/**
 * The engine's sentences as a notice draws them: sentence case where they
 * open on a word, and a backticked key as code.
 */

import { render } from "@testing-library/react";
import { expect, test } from "vitest";
import { engineWords } from "./ConfigWarnings.tsx";

const drawn = (text: string) => render(<p>{engineWords(text)}</p>).container.firstElementChild!;

test("a sentence opening on a word is raised, and a backticked key is code", () => {
  const p = drawn("this company has no `kind: human` seats, so nobody can be invited");
  expect(p.textContent).toBe("This company has no kind: human seats, so nobody can be invited");
  expect(p.querySelector("code")?.textContent).toBe("kind: human");
});

test("a message opening on a field path keeps the key's case", () => {
  expect(drawn("units[0].roles[0].goal: missing").textContent).toBe("units[0].roles[0].goal: missing");
});
