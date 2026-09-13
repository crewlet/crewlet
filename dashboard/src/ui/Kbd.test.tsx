/**
 * A shortcut hint names the key the reader actually presses, and is read as
 * words rather than as glyph names.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { Kbd, keyGlyph } from "./Kbd.tsx";

afterEach(cleanup);

test("Mod is Command on Apple platforms and Control everywhere else", () => {
  expect(keyGlyph("Mod", true)).toEqual({ glyph: "⌘", spoken: "Command" });
  expect(keyGlyph("Mod", false)).toEqual({ glyph: "Ctrl", spoken: "Control" });
  expect(keyGlyph("Alt", true).spoken).toBe("Option");
  expect(keyGlyph("Backspace", true)).toEqual({ glyph: "⌫", spoken: "Delete" });
});

test("a letter is printed in capitals, and an unknown key as itself", () => {
  expect(keyGlyph("z", false)).toEqual({ glyph: "Z", spoken: "Z" });
  expect(keyGlyph("F10", false)).toEqual({ glyph: "F10", spoken: "F10" });
});

test("the glyphs are hidden and one sentence is read instead", () => {
  const { container } = render(<Kbd keys={["Mod", "Shift", "z"]} apple />);
  const drawn = container.querySelector("[aria-hidden='true']")!;
  expect([...drawn.querySelectorAll("kbd")].map((k) => k.textContent)).toEqual(["⌘", "⇧", "Z"]);
  expect(container.querySelector(".sr-only")?.textContent).toBe("Command plus Shift plus Z");
});
