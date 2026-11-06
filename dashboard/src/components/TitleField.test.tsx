import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { TitleField } from "./TitleField.tsx";

afterEach(cleanup);

const mount = (onEnter = vi.fn(), onKeyDown?: () => void) => {
  render(
    <TitleField
      aria-label="Title"
      value="A title"
      onChange={() => {}}
      {...{ onEnter, onKeyDown }}
    />,
  );
  return { onEnter, field: screen.getByRole("textbox", { name: "Title" }) };
};

// A ONE-LINE BOX SHOWED THE MIDDLE OF A LONG TITLE, so nobody could review it.
test("a title wraps to be read whole, and still holds one line", () => {
  const { field } = mount();
  expect(field.tagName).toBe("TEXTAREA");
  expect(field.getAttribute("rows")).toBe("1");
  expect(field.getAttribute("aria-multiline")).toBe("false");
  expect(field.className).toContain("is-auto-resize");
});

// A TEXTAREA HAS NO IMPLICIT SUBMIT, so a plain Enter would have done nothing.
test("a plain Enter is handed on and writes no line", () => {
  const { onEnter, field } = mount();
  expect(fireEvent.keyDown(field, { key: "Enter" })).toBe(false);
  expect(onEnter).toHaveBeenCalledTimes(1);
});

test("Shift with Enter writes no line and presses nothing", () => {
  const { onEnter, field } = mount();
  expect(fireEvent.keyDown(field, { key: "Enter", shiftKey: true })).toBe(false);
  expect(onEnter).not.toHaveBeenCalled();
});

test("an input method's Enter is its own", () => {
  const { onEnter, field } = mount();
  fireEvent.keyDown(field, { key: "Enter", isComposing: true });
  expect(onEnter).not.toHaveBeenCalled();
});

test("a press the caller already answered is not handed on", () => {
  const onKeyDown = vi.fn((event: { preventDefault: () => void }) => event.preventDefault());
  const onEnter = vi.fn();
  render(
    <TitleField
      aria-label="Title"
      value=""
      onChange={() => {}}
      onEnter={onEnter}
      onKeyDown={onKeyDown}
    />,
  );
  fireEvent.keyDown(screen.getByRole("textbox", { name: "Title" }), { key: "Enter" });
  expect(onEnter).not.toHaveBeenCalled();
});
