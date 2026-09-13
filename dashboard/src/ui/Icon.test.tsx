/**
 * Every glyph is a path the renderer can draw, and none of them speaks.
 *
 * A glyph that is not path data fails silently: an SVG with a malformed `d`
 * draws nothing, and the button it sits in becomes an empty square with a
 * label only a screen reader hears. The icon itself is decoration everywhere,
 * because its meaning is always carried by a label or a title beside it.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { Icon, ICON_NAMES } from "./Icon.tsx";

afterEach(cleanup);

// The SVG path grammar's own alphabet: commands, numbers, separators.
const PATH_DATA = /^[MmLlHhVvCcSsQqTtAaZz0-9.,\s-]+$/;

test("every glyph is non-empty path data that starts with a move", () => {
  const broken = ICON_NAMES.filter((name) => {
    const { container } = render(<Icon name={name} />);
    const d = container.querySelector("path")?.getAttribute("d") ?? "";
    cleanup();
    return !PATH_DATA.test(d) || !/^[Mm]/.test(d);
  });
  expect(broken).toEqual([]);
});

test("an icon is hidden from assistive technology and never takes focus", () => {
  const { container } = render(<Icon name="trash" />);
  const svg = container.querySelector("svg")!;
  expect(svg.getAttribute("aria-hidden")).toBe("true");
  expect(svg.getAttribute("focusable")).toBe("false");
  expect(svg.getAttribute("fill")).toBe("none");
});
