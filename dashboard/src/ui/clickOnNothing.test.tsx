import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { cleanup, fireEvent, render } from "@testing-library/react";

import { useClickOnNothing } from "./clickOnNothing.ts";

// A MODAL IS THE ONE THING THE PAGE ITSELF CANNOT SHOW, so the layer stack's
// answer is stood in for: everything else here is the real DOM.
let modalOpen = false;
vi.mock("@crewlethq/ui", async (original) => ({
  ...(await original<typeof import("@crewlethq/ui")>()),
  isModalLayerOpen: () => modalOpen,
}));

beforeEach(() => {
  modalOpen = false;
});

afterEach(() => {
  cleanup();
  window.getSelection()?.removeAllRanges();
});

function Page({ onNothing }: { onNothing: (() => void) | null }) {
  useClickOnNothing(onNothing, ".held");
  return (
    <div>
      <p>Words on the page</p>
      <button type="button">A control</button>
      <div role="treeitem" aria-selected="false">
        <span>A node</span>
      </div>
      <div role="row" aria-level={1}>
        <span>A row</span>
      </div>
      <div style={{ cursor: "pointer" }}>
        <span>A row that opens a peek</span>
      </div>
      <div role="menu">
        <span>Inside a menu</span>
      </div>
      <div role="dialog">
        <span>Inside a dialog</span>
      </div>
      <aside className="held">
        <span>Inside what is held</span>
      </aside>
    </div>
  );
}

function mount(onNothing: (() => void) | null) {
  return render(<Page onNothing={onNothing} />);
}

// A CLICK ON NOTHING PUTS DOWN WHAT IS HELD, and only a click on nothing.
test("a click on the page lets go, and a click on anything that acts or selects does not", () => {
  const onNothing = vi.fn();
  const { getByText } = mount(onNothing);
  for (const something of [
    "A control",
    "A node",
    "A row",
    "A row that opens a peek",
    "Inside a menu",
    "Inside a dialog",
    "Inside what is held",
  ]) {
    fireEvent.click(getByText(something));
    expect(onNothing, something).not.toHaveBeenCalled();
  }
  fireEvent.click(getByText("Words on the page"));
  expect(onNothing).toHaveBeenCalledTimes(1);
});

// NOTHING HELD, NOTHING HEARD.
test("with nothing held, no click is listened to", () => {
  const onNothing = vi.fn();
  const { getByText, rerender } = mount(null);
  fireEvent.click(getByText("Words on the page"));
  rerender(<Page onNothing={null} />);
  fireEvent.click(getByText("Words on the page"));
  expect(onNothing).not.toHaveBeenCalled();
});

// THREE CLICKS THAT ARE NOT A READER LETTING GO: a secondary press, a click
// something else already answered, and one under a modal.
test("a secondary press, an answered click and a click under a modal all keep hold", () => {
  const onNothing = vi.fn();
  const { getByText } = mount(onNothing);
  const words = getByText("Words on the page");
  fireEvent.click(words, { button: 1 });
  const answered = new MouseEvent("click", { bubbles: true, cancelable: true });
  answered.preventDefault();
  words.dispatchEvent(answered);
  modalOpen = true;
  fireEvent.click(words);
  expect(onNothing).not.toHaveBeenCalled();
});

// A CLICK THAT ENDS A TEXT SELECTION is the reader taking words.
test("a click that leaves words selected keeps hold", () => {
  const onNothing = vi.fn();
  const { getByText } = mount(onNothing);
  const words = getByText("Words on the page");
  const range = document.createRange();
  range.selectNodeContents(words);
  window.getSelection()?.addRange(range);
  fireEvent.click(words);
  expect(onNothing).not.toHaveBeenCalled();
});
