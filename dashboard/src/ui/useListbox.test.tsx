/**
 * The listbox keys, once, for every field that offers a list.
 *
 * The secret completion's own behaviour is asserted in `Field.test.tsx`;
 * this is the contract both it and the multi-select rely on, including the
 * one that matters most inside a modal: Escape closes the list and nothing
 * around it.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useId, useState } from "react";
import { afterEach, expect, test, vi } from "vitest";
import { Dialog } from "./Dialog.tsx";
import { useListbox } from "./useListbox.ts";

afterEach(cleanup);

function Picker({
  options,
  tabCommits,
  onCommit,
}: {
  options: string[];
  tabCommits?: boolean;
  onCommit: (value: string) => void;
}) {
  const id = useId();
  const [open, setOpen] = useState(true);
  const listbox = useListbox({
    id,
    open,
    count: options.length,
    tabCommits,
    onCommit: (i) => onCommit(options[i]!),
    onClose: () => setOpen(false),
  });
  return (
    <>
      <input
        aria-label="query"
        role="combobox"
        aria-expanded={open}
        aria-controls={listbox.listId}
        aria-activedescendant={open ? listbox.optionId(listbox.active) : undefined}
        onKeyDown={listbox.onKeyDown}
      />
      {open && (
        <ul role="listbox" id={listbox.listId} aria-label="options">
          {options.map((o, i) => (
            <li
              key={o}
              id={listbox.optionId(i)}
              role="option"
              aria-selected={i === listbox.active}
              {...listbox.optionHandlers(i)}
            >
              {o}
            </li>
          ))}
        </ul>
      )}
    </>
  );
}

function highlighted(): string | null {
  const input = screen.getByLabelText("query");
  const id = input.getAttribute("aria-activedescendant");
  return id ? (document.getElementById(id)?.textContent ?? null) : null;
}

test("the arrows wrap at both ends", () => {
  render(<Picker options={["a", "b", "c"]} onCommit={() => {}} />);
  const input = screen.getByLabelText("query");
  expect(highlighted()).toBe("a");
  fireEvent.keyDown(input, { key: "ArrowUp" });
  expect(highlighted()).toBe("c");
  fireEvent.keyDown(input, { key: "ArrowDown" });
  expect(highlighted()).toBe("a");
});

test("the highlight is clamped, not reset, when the list shrinks", () => {
  const { rerender } = render(<Picker options={["a", "b", "c"]} onCommit={() => {}} />);
  const input = screen.getByLabelText("query");
  fireEvent.keyDown(input, { key: "ArrowUp" });
  expect(highlighted()).toBe("c");
  rerender(<Picker options={["a", "b"]} onCommit={() => {}} />);
  expect(highlighted()).toBe("b");
});

test("Enter takes the highlighted option without submitting the form around it", () => {
  const onCommit = vi.fn();
  render(<Picker options={["a", "b"]} onCommit={onCommit} />);
  const input = screen.getByLabelText("query");
  fireEvent.keyDown(input, { key: "ArrowDown" });
  // Prevented: that is what stops a browser's implicit form submission, which
  // jsdom does not perform and so cannot be observed directly.
  expect(fireEvent.keyDown(input, { key: "Enter" })).toBe(false);
  expect(onCommit).toHaveBeenCalledWith("b");
});

test("Tab takes the option only where the list is a completion, and never backwards", () => {
  const onCommit = vi.fn();
  const { rerender } = render(<Picker options={["a"]} onCommit={onCommit} />);
  expect(fireEvent.keyDown(screen.getByLabelText("query"), { key: "Tab" })).toBe(true);
  expect(onCommit).not.toHaveBeenCalled();

  rerender(<Picker options={["a"]} tabCommits onCommit={onCommit} />);
  fireEvent.keyDown(screen.getByLabelText("query"), { key: "Tab", shiftKey: true });
  expect(onCommit).not.toHaveBeenCalled();
  fireEvent.keyDown(screen.getByLabelText("query"), { key: "Tab" });
  expect(onCommit).toHaveBeenCalledWith("a");
});

test("Escape closes the list and leaves the dialog around it open", () => {
  const closed = vi.fn();
  render(
    <Dialog title="Edit seat" onClose={closed}>
      <Picker options={["a"]} onCommit={() => {}} />
    </Dialog>,
  );
  const input = screen.getByLabelText("query");
  input.focus();
  fireEvent.keyDown(input, { key: "Escape" });
  expect(screen.queryByRole("listbox")).toBeNull();
  expect(closed).not.toHaveBeenCalled();

  fireEvent.keyDown(input, { key: "Escape" });
  expect(closed).toHaveBeenCalledTimes(1);
});

test("a press on an option takes it on mousedown, before the field can blur", () => {
  const onCommit = vi.fn();
  render(<Picker options={["a", "b"]} onCommit={onCommit} />);
  const option = screen.getByRole("option", { name: "b" });
  expect(fireEvent.mouseDown(option)).toBe(false);
  expect(onCommit).toHaveBeenCalledWith("b");
  fireEvent.mouseEnter(screen.getByRole("option", { name: "a" }));
  expect(highlighted()).toBe("a");
});
