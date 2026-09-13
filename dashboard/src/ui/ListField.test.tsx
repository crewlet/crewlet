/**
 * An ordered list edited in place: Enter adds, Alt+Arrow and the buttons
 * move, focus follows the item, and every change is said out loud.
 *
 * Each case is one a keyboard or screen-reader user meets on the first list
 * they edit: Enter that saved the whole editor mid-sentence, a move that left
 * focus on a button now describing a different item, a removal that dropped
 * focus to the page, a reorder nobody heard.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, expect, test } from "vitest";
import { ListField } from "./ListField.tsx";

afterEach(cleanup);

function Goals({
  initial,
  multiline,
  copies,
}: {
  initial: string[];
  multiline?: boolean;
  /** An owner that stores a copy of what it is given, as a reducer does. */
  copies?: boolean;
}) {
  const [value, setValue] = useState(initial);
  return (
    <form onSubmit={() => {}}>
      <ListField
        label="Goals"
        itemName="goal"
        value={value}
        onChange={(next) => setValue(copies ? [...next] : next)}
        multiline={multiline}
        placeholder="Add a goal"
      />
      <pre data-testid="value">{JSON.stringify(value)}</pre>
    </form>
  );
}

const current = () => JSON.parse(screen.getByTestId("value").textContent ?? "[]") as string[];
const announced = () => screen.getByRole("status").textContent;

test("items are labelled controls in a group named by the list, edited in place", () => {
  render(<Goals initial={["Ship the beta", "Hire two engineers"]} />);
  expect(screen.getByRole("group", { name: "Goals" })).toBeDefined();
  const second = screen.getByLabelText("Goal 2 of 2") as HTMLInputElement;
  fireEvent.change(second, { target: { value: "Hire three engineers" } });
  expect(current()).toEqual(["Ship the beta", "Hire three engineers"]);
});

test("Enter in the new-item box adds a trimmed item, keeps focus there, and never submits", () => {
  render(<Goals initial={["Ship the beta"]} />);
  const box = screen.getByLabelText("New goal") as HTMLInputElement;
  box.focus();
  fireEvent.change(box, { target: { value: "  Open a second region  " } });
  expect(fireEvent.keyDown(box, { key: "Enter" })).toBe(false);
  expect(current()).toEqual(["Ship the beta", "Open a second region"]);
  expect(box.value).toBe("");
  expect(document.activeElement).toBe(box);
  expect(announced()).toBe("Added goal 2 of 2");

  // Blank adds nothing, and is still no submit.
  expect(fireEvent.keyDown(box, { key: "Enter" })).toBe(false);
  expect(current()).toHaveLength(2);
});

test("the Enter that accepts an input method's word neither adds nor moves on", () => {
  render(<Goals initial={["Ship the beta", "Hire two engineers"]} />);
  const box = screen.getByLabelText("New goal") as HTMLInputElement;
  box.focus();
  fireEvent.change(box, { target: { value: "ベータ版" } });
  expect(fireEvent.keyDown(box, { key: "Enter", isComposing: true })).toBe(true);
  expect(fireEvent.keyDown(box, { key: "Enter", keyCode: 229 })).toBe(true);
  expect(current()).toEqual(["Ship the beta", "Hire two engineers"]);
  expect(box.value).toBe("ベータ版");

  const first = screen.getByLabelText("Goal 1 of 2");
  first.focus();
  expect(fireEvent.keyDown(first, { key: "Enter", isComposing: true })).toBe(true);
  expect(fireEvent.keyDown(first, { key: "ArrowDown", altKey: true, isComposing: true })).toBe(
    true,
  );
  expect(current()).toEqual(["Ship the beta", "Hire two engineers"]);
  expect(document.activeElement).toBe(first);

  fireEvent.keyDown(box, { key: "Enter" });
  expect(current()).toEqual(["Ship the beta", "Hire two engineers", "ベータ版"]);
});

test("Shift+Enter is a newline in a multiline list, and Enter still adds", () => {
  render(<Goals initial={[]} multiline />);
  const box = screen.getByLabelText("New goal") as HTMLTextAreaElement;
  expect(box.tagName).toBe("TEXTAREA");
  expect(fireEvent.keyDown(box, { key: "Enter", shiftKey: true })).toBe(true);
  fireEvent.change(box, { target: { value: "First line\nSecond line" } });
  fireEvent.keyDown(box, { key: "Enter" });
  expect(current()).toEqual(["First line\nSecond line"]);
});

test("Enter in an item moves on to the next item, then to the new-item box", () => {
  render(<Goals initial={["a", "b"]} />);
  const first = screen.getByLabelText("Goal 1 of 2");
  first.focus();
  expect(fireEvent.keyDown(first, { key: "Enter" })).toBe(false);
  expect(document.activeElement).toBe(screen.getByLabelText("Goal 2 of 2"));
  fireEvent.keyDown(document.activeElement!, { key: "Enter" });
  expect(document.activeElement).toBe(screen.getByLabelText("New goal"));
});

test("Alt+Arrow moves the focused item, and focus and the element move with it", () => {
  // An owner that copies the array is the harder case: the list cannot tell
  // its own change from somebody else's by reference.
  render(<Goals initial={["a", "b", "c"]} copies />);
  const a = screen.getByLabelText("Goal 1 of 3") as HTMLInputElement;
  a.focus();
  fireEvent.keyDown(a, { key: "ArrowDown", altKey: true });
  expect(current()).toEqual(["b", "a", "c"]);
  // The same element, now second, still holding focus.
  expect(screen.getByLabelText("Goal 2 of 3")).toBe(a);
  expect(document.activeElement).toBe(a);
  expect(announced()).toBe("Moved goal to position 2 of 3");

  fireEvent.keyDown(a, { key: "ArrowUp", altKey: true });
  fireEvent.keyDown(a, { key: "ArrowUp", altKey: true });
  expect(current()).toEqual(["a", "b", "c"]);
});

test("a Move button keeps focus on the moved item's button, switching sides at an end", () => {
  render(<Goals initial={["a", "b", "c"]} />);
  expect(
    (screen.getByRole("button", { name: "Move goal 1 of 3 up" }) as HTMLButtonElement).disabled,
  ).toBe(true);
  expect(
    (screen.getByRole("button", { name: "Move goal 3 of 3 down" }) as HTMLButtonElement).disabled,
  ).toBe(true);

  fireEvent.click(screen.getByRole("button", { name: "Move goal 2 of 3 down" }));
  expect(current()).toEqual(["a", "c", "b"]);
  // "b" reached the bottom, so its Down is disabled and focus takes its Up.
  expect(document.activeElement).toBe(screen.getByRole("button", { name: "Move goal 3 of 3 up" }));

  fireEvent.click(document.activeElement as HTMLButtonElement);
  expect(current()).toEqual(["a", "b", "c"]);
  expect(document.activeElement).toBe(screen.getByRole("button", { name: "Move goal 2 of 3 up" }));

  // And the other way: "b" reaches the top, so focus takes its Down.
  fireEvent.click(document.activeElement as HTMLButtonElement);
  expect(current()).toEqual(["b", "a", "c"]);
  expect(document.activeElement).toBe(
    screen.getByRole("button", { name: "Move goal 1 of 3 down" }),
  );
});

test("removing an item focuses the one that took its place, else the one before, else the new box", () => {
  render(<Goals initial={["a", "b"]} />);
  fireEvent.click(screen.getByRole("button", { name: "Remove goal 1 of 2" }));
  expect(current()).toEqual(["b"]);
  expect(document.activeElement).toBe(screen.getByLabelText("Goal 1 of 1"));
  expect(announced()).toBe("Removed goal 1 of 2");

  fireEvent.click(screen.getByRole("button", { name: "Remove goal 1 of 1" }));
  expect(current()).toEqual([]);
  expect(document.activeElement).toBe(screen.getByLabelText("New goal"));
});

test("two identical sentences are two items", () => {
  render(<Goals initial={["same", "same"]} />);
  fireEvent.click(screen.getByRole("button", { name: "Remove goal 2 of 2" }));
  expect(current()).toEqual(["same"]);
});
