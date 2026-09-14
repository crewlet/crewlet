/**
 * The keyboard contract of a group of choices, and the role each group claims.
 *
 * Both were missing, and they failed in opposite directions. `Segmented` and
 * `Tabs` rendered a row of ordinary buttons under `role="tablist"`: the ARIA
 * promised one tab stop with arrow keys inside it, and the DOM delivered N tab
 * stops with no arrow keys at all — so a keyboard reader got neither the
 * behaviour the role implies nor the behaviour plain buttons would have given
 * them. And `Segmented`'s role was wrong on top of that: a tab controls a
 * panel it labels, and not one call site does that.
 *
 * WHAT THE ARROWS DO is the third question, and the one these cases are most
 * careful about. They move focus and commit nothing, because seven of the nine
 * call sites drive a `useParam` — five of those push a history entry, and every
 * one re-runs the screen's query. There is no case here that presses Enter:
 * jsdom does not perform a focused button's default activation, so such a case
 * would be asserting the test runner's manners rather than this component's.
 * What IS asserted is both halves of the chain that ARE ours — the group hands
 * Enter and Space to the browser untouched, and every option is a real
 * `<button>` whose click commits the option focus is standing on.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, expect, test, vi } from "vitest";
import { Segmented, Tabs } from "./primitives.tsx";

afterEach(cleanup);

type Lens = "chart" | "directory" | "charter";

const LENSES: { value: Lens; label: string }[] = [
  { value: "chart", label: "Chart" },
  { value: "directory", label: "Directory" },
  { value: "charter", label: "Charter" },
];

/** The control as a screen uses it: it owns the value and re-renders. */
function LiveSegmented({
  onChange,
  activate,
}: {
  onChange?: (v: Lens) => void;
  activate?: "manual" | "automatic";
}) {
  const [value, setValue] = useState<Lens>("chart");
  return (
    <Segmented<Lens>
      ariaLabel="Org view"
      activate={activate}
      value={value}
      onChange={(v) => {
        setValue(v);
        onChange?.(v);
      }}
      options={LENSES}
    />
  );
}

const tabstops = () => screen.getAllByRole("radio").map((b) => b.getAttribute("tabindex"));
const checked = () => screen.getAllByRole("radio").map((b) => b.getAttribute("aria-checked"));

// A SEGMENTED CONTROL IS A RADIO GROUP, NOT A TAB LIST.
//
// All eight call sites pick a theme, a density, a grouping, a scope, a window
// or a lens; none of them controls a `tabpanel`, and several sit in a screen
// head with the content they affect hundreds of pixels below. `role="tab"`
// promises a panel relationship that does not exist.
test("a segmented control announces as a radio group", () => {
  render(<LiveSegmented />);
  expect(screen.getByRole("radiogroup", { name: "Org view" })).toBeTruthy();
  expect(screen.getAllByRole("radio")).toHaveLength(3);
  // The CONTROL: no tab role survives anywhere, so this cannot pass on a
  // component that simply added radios beside the tabs it already had.
  expect(screen.queryAllByRole("tab")).toHaveLength(0);
  expect(screen.queryByRole("tablist")).toBeNull();
});

// ONE TAB STOP FOR THE GROUP. Every option being its own stop is what a row of
// plain buttons gives you, and the shell alone renders two of these groups —
// six stops in front of the page content, on every screen.
test("only one option is in the page's tab order", () => {
  render(<LiveSegmented />);
  expect(tabstops()).toEqual(["0", "-1", "-1"]);
});

// ARROWS MOVE FOCUS AND COMMIT NOTHING.
//
// This is the whole of manual activation, and it is the default because of
// what these groups are wired to: seven of the nine sites drive a `useParam`,
// five of those push a history entry, and every one re-runs the screen's
// query with no cache behind it. Selection-following-focus turns one reader
// arrowing across a five-option group into four uncached queries and four
// history entries they have to press Back through — and the reader most
// likely to arrow across every option to hear what is there is the one using
// a screen reader.
test("an arrow moves focus without committing anything", () => {
  const moved = vi.fn();
  render(<LiveSegmented onChange={moved} />);
  const group = screen.getByRole("radiogroup");

  fireEvent.keyDown(group, { key: "ArrowRight" });

  const [chart, directory] = screen.getAllByRole("radio");
  expect(document.activeElement).toBe(directory);
  // NOTHING was chosen: no call, and the checked option has not moved.
  expect(moved).not.toHaveBeenCalled();
  expect(chart!.getAttribute("aria-checked")).toBe("true");
  expect(directory!.getAttribute("aria-checked")).toBe("false");
});

// THE TAB STOP IS UNDER THE READER'S FEET, not on the checked option. Once
// arrows stop selecting, those are two different options for as long as
// somebody is still deciding — and leaving the stop behind on the checked one
// means tabbing out and back drops the reader somewhere they did not leave.
test("the tab stop follows focus rather than the selection", () => {
  render(<LiveSegmented />);
  fireEvent.keyDown(screen.getByRole("radiogroup"), { key: "ArrowRight" });
  expect(tabstops()).toEqual(["-1", "0", "-1"]);
  // …while the selection has stayed where it was, which is what makes the two
  // columns distinguishable at all.
  expect(checked()).toEqual(["true", "false", "false"]);
});

// WRAPS, at both ends: a reader holding the key gets the whole group rather
// than stopping at an end they cannot see.
test("arrow focus wraps at both ends", () => {
  render(<LiveSegmented />);
  const group = screen.getByRole("radiogroup");
  const [chart, , charter] = screen.getAllByRole("radio");

  fireEvent.keyDown(group, { key: "ArrowLeft" });
  expect(document.activeElement).toBe(charter);
  fireEvent.keyDown(group, { key: "ArrowRight" });
  expect(document.activeElement).toBe(chart);
});

// BOTH AXES. These render as a horizontal row today, but a group that wraps to
// two lines is the same control, and Down is what a reader presses on it.
test("the vertical arrows move focus too", () => {
  render(<LiveSegmented />);
  const group = screen.getByRole("radiogroup");
  const [chart, directory] = screen.getAllByRole("radio");

  fireEvent.keyDown(group, { key: "ArrowDown" });
  expect(document.activeElement).toBe(directory);
  fireEvent.keyDown(group, { key: "ArrowUp" });
  expect(document.activeElement).toBe(chart);
});

test("Home and End jump focus to the ends", () => {
  render(<LiveSegmented />);
  const group = screen.getByRole("radiogroup");
  const [chart, , charter] = screen.getAllByRole("radio");

  fireEvent.keyDown(group, { key: "End" });
  expect(document.activeElement).toBe(charter);
  fireEvent.keyDown(group, { key: "Home" });
  expect(document.activeElement).toBe(chart);
});

// THE OTHER HALF OF MANUAL ACTIVATION: the option focus is standing on is the
// one that commits. A browser turns Enter and Space on a focused button into
// exactly this click, so this is the gesture under those keys — and it is the
// assertion that would catch focus and the click handler disagreeing about
// which option a reader arrowed to.
test("activating the focused option is what commits it", () => {
  const moved = vi.fn();
  render(<LiveSegmented onChange={moved} />);
  const group = screen.getByRole("radiogroup");

  fireEvent.keyDown(group, { key: "End" });
  fireEvent.click(document.activeElement!);

  expect(moved).toHaveBeenCalledTimes(1);
  expect(moved).toHaveBeenCalledWith("charter");
  expect(checked()).toEqual(["false", "false", "true"]);
  // And the two columns agree again, because the reader has stopped deciding.
  expect(tabstops()).toEqual(["-1", "-1", "0"]);
});

// …WHICH ONLY WORKS BECAUSE EVERY OPTION IS A REAL BUTTON. A `div` with a
// click handler takes neither key, and the group's own handler deliberately
// does not step in for it — so the element type is load-bearing here rather
// than a styling detail.
test("every option is a button the browser will activate", () => {
  render(<LiveSegmented />);
  for (const option of screen.getAllByRole("radio")) {
    expect(option.tagName).toBe("BUTTON");
    // `type` too: inside a form, a button without one submits it.
    expect(option.getAttribute("type")).toBe("button");
  }
});

// A KEY THE GROUP DOES NOT HANDLE IS THE BROWSER'S. Tab above all: swallowing
// it is how a group becomes a keyboard trap, and this handler is on the
// container every key inside it bubbles to. Enter and Space are on this list
// for a second reason now — they are how a reader COMMITS, and the browser
// only turns them into a click if this handler leaves them alone.
test("keys it does not own reach the browser", () => {
  render(<LiveSegmented />);
  const group = screen.getByRole("radiogroup");
  for (const key of ["Tab", "Enter", " ", "a", "PageDown"]) {
    const event = new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true });
    group.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(false);
  }
  // …and one it does own IS taken, or the assertion above passes on a
  // component with no handler at all.
  const own = new KeyboardEvent("keydown", {
    key: "ArrowRight",
    bubbles: true,
    cancelable: true,
  });
  group.dispatchEvent(own);
  expect(own.defaultPrevented).toBe(true);
});

// AN OUTSIDE CHANGE RETIRES WHATEVER THE ARROWS WERE POINTING AT — a click on
// another control, the browser's Back button, a URL somebody pasted. Without
// this the group keeps its tab stop on an option nothing selected and the
// reader's next Tab into the group lands on a stale choice.
test("a change from elsewhere takes the tab stop back", () => {
  function Externally() {
    const [value, setValue] = useState<Lens>("chart");
    return (
      <>
        <Segmented<Lens> ariaLabel="Org view" value={value} onChange={setValue} options={LENSES} />
        <button type="button" onClick={() => setValue("charter")}>
          Elsewhere
        </button>
      </>
    );
  }
  render(<Externally />);

  fireEvent.keyDown(screen.getByRole("radiogroup"), { key: "ArrowRight" });
  expect(tabstops()).toEqual(["-1", "0", "-1"]);

  fireEvent.click(screen.getByRole("button", { name: "Elsewhere" }));
  expect(tabstops()).toEqual(["-1", "-1", "0"]);
  expect(checked()).toEqual(["false", "false", "true"]);
});

// AN OPTION THAT LEAVES THE GROUP TAKES THE TAB STOP WITH IT unless the stop
// falls back. `options` is a prop, so a caller may narrow it while a reader is
// standing on one of them — and a stop nothing carries is a group that has
// dropped out of the tab order altogether, reachable by no key at all.
test("an option disappearing does not strand the tab stop", () => {
  function Shrinking() {
    const [value] = useState<Lens>("chart");
    const [all, setAll] = useState(true);
    return (
      <>
        <Segmented<Lens>
          ariaLabel="Org view"
          value={value}
          onChange={() => {}}
          options={all ? LENSES : LENSES.slice(0, 2)}
        />
        <button type="button" onClick={() => setAll(false)}>
          Narrow
        </button>
      </>
    );
  }
  render(<Shrinking />);

  fireEvent.keyDown(screen.getByRole("radiogroup"), { key: "End" });
  expect(tabstops()).toEqual(["-1", "-1", "0"]);

  fireEvent.click(screen.getByRole("button", { name: "Narrow" }));
  // Back on the selected option, because that is the only one left that any
  // part of the group still agrees about.
  expect(tabstops()).toEqual(["0", "-1"]);
});

// A VALUE THE GROUP DOES NOT OFFER STILL LEAVES ONE TAB STOP.
//
// `value` comes off the URL at seven of the nine call sites, and `useParam`
// hands back whatever the query string says — so `?lens=bogus` (a link from an
// older build, a typo, a renamed option) reaches this component as a value no
// option carries. The stop fell back to `value` in that case, which is not in
// the group either, so EVERY option rendered tabIndex=-1 and the whole control
// left the page's tab order: unreachable by keyboard, and worse than the plain
// buttons this replaced, which were each a stop of their own.
test("a value outside the group still leaves it reachable", () => {
  render(
    <Segmented<Lens>
      ariaLabel="Org view"
      value={"bogus" as Lens}
      onChange={() => {}}
      options={LENSES}
    />,
  );
  // Exactly one stop, on the first option — nothing is checked, so nothing
  // else has a claim to it.
  expect(tabstops()).toEqual(["0", "-1", "-1"]);
  expect(checked()).toEqual(["false", "false", "false"]);

  // And the arrows still work from there rather than stepping from nowhere.
  fireEvent.keyDown(screen.getByRole("radiogroup"), { key: "ArrowRight" });
  expect(document.activeElement).toBe(screen.getAllByRole("radio")[1]);
});

// THE HELD STOP LIVES IN STATE, NOT A REF.
//
// A ref write during render survives a render attempt React DISCARDS, while
// the state update queued beside it does not — so the pair that tracks "which
// value have we observed" and "where is the stop" would disagree permanently,
// and the guard that resyncs them would never fire again. Keeping both in one
// state object means a discarded attempt reverts them together.
//
// The observable consequence is what this asserts: repeated outside changes
// keep taking the stop back, every time rather than only the first.
test("every outside change takes the tab stop back, not just the first", () => {
  function Externally() {
    const [value, setValue] = useState<Lens>("chart");
    return (
      <>
        <Segmented<Lens> ariaLabel="Org view" value={value} onChange={setValue} options={LENSES} />
        {LENSES.map((l) => (
          <button key={l.value} type="button" onClick={() => setValue(l.value)}>
            go {l.label}
          </button>
        ))}
      </>
    );
  }
  render(<Externally />);
  const group = screen.getByRole("radiogroup");

  for (const [label, want] of [
    ["go Charter", ["-1", "-1", "0"]],
    ["go Directory", ["-1", "0", "-1"]],
    ["go Chart", ["0", "-1", "-1"]],
  ] as const) {
    // Arrow away first, so the stop has somewhere to be taken back FROM.
    fireEvent.keyDown(group, { key: "ArrowRight" });
    fireEvent.click(screen.getByRole("button", { name: label }));
    expect(tabstops()).toEqual(want);
  }
});

// AUTOMATIC IS STILL THERE, for the two groups that earn it: the shell's theme
// and density write `localStorage` and a `data-` attribute, so arrowing across
// them costs a repaint and nothing a reader has to undo. Selection following
// focus is the better control when that is true.
test("an automatic group commits as focus moves", () => {
  const moved = vi.fn();
  render(<LiveSegmented activate="automatic" onChange={moved} />);
  const group = screen.getByRole("radiogroup");

  fireEvent.keyDown(group, { key: "ArrowRight" });
  expect(moved).toHaveBeenCalledWith("directory");
  expect(checked()).toEqual(["false", "true", "false"]);
  // One call, not two: the focus move and the commit are one gesture.
  expect(moved).toHaveBeenCalledTimes(1);

  fireEvent.keyDown(group, { key: "End" });
  expect(moved).toHaveBeenLastCalledWith("charter");
  expect(document.activeElement).toBe(screen.getAllByRole("radio")[2]);
});

type Tab = "overview" | "model" | "cost";

function LiveTabs() {
  const [value, setValue] = useState<Tab>("overview");
  return (
    <Tabs<Tab>
      ariaLabel="Seat sections"
      value={value}
      onChange={setValue}
      options={[
        { value: "overview", label: "Overview" },
        { value: "model", label: "Model activity" },
        { value: "cost", label: "Cost" },
      ]}
    />
  );
}

// TABS KEEP THE TAB ROLE — they are the one control here that genuinely is a
// tab list, with its panels directly beneath it — and they get manual
// activation for the reason the pattern gives for it by name: a tab whose
// panel is not displayed without noticeable latency should not be selected by
// focus alone. Seat's strip is `useParam(…, "section")`, so each of its tabs
// is a query and a history entry.
test("a tab list keeps its role and selects only on activation", () => {
  render(<LiveTabs />);
  const list = screen.getByRole("tablist", { name: "Seat sections" });
  const stops = () => screen.getAllByRole("tab").map((b) => b.getAttribute("tabindex"));
  expect(stops()).toEqual(["0", "-1", "-1"]);

  fireEvent.keyDown(list, { key: "ArrowRight" });
  const [overview, model] = screen.getAllByRole("tab");
  expect(document.activeElement).toBe(model);
  expect(stops()).toEqual(["-1", "0", "-1"]);
  // FOCUSED, NOT SELECTED: the panel below has not been swapped out from
  // under a reader who was only looking.
  expect(model!.getAttribute("aria-selected")).toBe("false");
  expect(overview!.getAttribute("aria-selected")).toBe("true");

  fireEvent.click(document.activeElement!);
  expect(model!.getAttribute("aria-selected")).toBe("true");
});
