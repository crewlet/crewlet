/**
 * The edit bar on a phone: the tools fold into the page bar's More, and the
 * commit pair stays inline, so Done is never the control past the edge.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { EditBar, editMenu } from "./EditBar.tsx";

afterEach(cleanup);

const noop = () => {};
const props = {
  changed: true,
  steps: 3,
  canRedo: false,
  onUndo: noop,
  onRedo: noop,
  onAdd: noop,
  onCancel: noop,
  onDone: noop,
};

/* Mutation: drop the `.page-action-folds` wrapper, or put Done inside it. */
test("folds Undo, Redo and Add widgets on a phone, and never Cancel or Done", () => {
  render(<EditBar {...props} />);
  const folded = (name: string) =>
    screen.getByRole("button", { name }).closest(".page-action-folds") !== null;
  expect(folded("Undo")).toBe(true);
  expect(folded("Redo")).toBe(true);
  expect(folded("Add widgets")).toBe(true);
  expect(folded("Cancel")).toBe(false);
  expect(folded("Done")).toBe(false);
});

test("the state keeps its words as its name and shows the count on a phone", () => {
  const { container } = render(<EditBar {...props} />);
  expect(container.querySelector(".page-wide-words")!.textContent).toBe("3 unsaved changes");
  expect(container.querySelector(".page-phone-only")!.textContent).toBe("3");
});

test("More offers the folded tools with the reasons the inline ones carry", () => {
  const entries = editMenu({ ...props, steps: 0 });
  expect(entries.map((e) => [e.label, e.disabled ?? false, e.description])).toEqual([
    ["Undo", true, "Nothing to undo."],
    ["Redo", true, "Nothing to redo."],
    ["Add widgets", false, undefined],
  ]);
});
