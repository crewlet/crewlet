/**
 * The keyboard contract of a group of choices, and the role each group claims.
 *
 * Both were missing from the controls this dashboard drew for itself, and they
 * failed in opposite directions. A row of ordinary buttons rendered under
 * `role="tablist"` promised one tab stop with arrow keys inside it and
 * delivered N tab stops with no arrow keys at all, so a keyboard reader got
 * neither the behaviour the role implies nor the behaviour plain buttons would
 * have given them. And the role was wrong on top of that: a tab controls a
 * panel it labels, and a theme, a density, a scope or a lens controls nothing.
 *
 * THE DESIGN SYSTEM OWNS BOTH CONTROLS NOW, and it makes the distinction in
 * the prop: [SegmentedControl] with `semantics="radio"` is a SETTING, whose
 * arrows select as they move because a setting costs nothing to change; with
 * `semantics="tabs"`, and [Tabs] itself, are SECTIONS, whose arrows move focus
 * and leave the choosing to Enter or Space, because a section here is a query
 * and a history entry. `uilet.test.tsx` holds that distinction, along with the
 * roles, the single tab stop and the panel relationship.
 *
 * WHAT IS HELD HERE is the half of the contract that survives a reader doing
 * something the happy path does not: a key the group must hand back to the
 * browser, a value that arrived from a URL and matches no option, and an
 * option list that changes underneath somebody who is standing on one. Each of
 * those is a way for a group to fall out of the tab order altogether, which is
 * worse than the plain buttons it replaced, and none of them is visible in a
 * screenshot or reachable from a screen's own suite.
 *
 * WHAT THE ARROWS DO NOT DO is the rule underneath all of it: they commit
 * nothing on a section row. No case here presses Enter, because jsdom does not
 * perform a focused button's default activation, so such a case would assert
 * the test runner's manners rather than this control's. What IS asserted is
 * both halves of the chain that are real: the group hands Enter and Space to
 * the browser untouched, and every option is a real `<button>` whose click
 * commits the option focus is standing on.
 *
 * TWO THINGS THE CONTROL THIS REPLACES DID ARE NOT ASSERTED HERE, because the
 * design system does not do them and a suite may not hold a package to a
 * promise it never made. They are written down rather than dropped quietly,
 * since each is a real affordance a reader has lost and each is one small
 * change to `@crewlethq/ui` away:
 *
 *  1. **No note saying how to choose.** A row whose arrows move focus breaks a
 *     radio group's learned contract, and the control this replaces carried a
 *     hidden sentence naming Enter and Space so the silence did not read as a
 *     control ignoring the reader. [SegmentedControl] renders no such node in
 *     either semantics, so there is nothing to assert and asserting its
 *     absence would be a vacuous negative.
 *  2. **The panel is not a tab stop.** [TabPanel] sets no `tabIndex`, so Tab
 *     from the selected tab leaves the widget and lands on whatever follows it
 *     in the document: choosing a section moves a keyboard reader further from
 *     the section they chose. It takes no prop that could carry one either, so
 *     no call site can make up the difference.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, expect, test, vi } from "vitest";
import { SegmentedControl, TabPanel, Tabs, tabId } from "@crewlethq/ui";

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
  semantics = "tabs",
}: {
  onChange?: ((v: Lens) => void) | undefined;
  semantics?: "radio" | "tabs" | undefined;
}) {
  const [value, setValue] = useState<Lens>("chart");
  const commit = (v: Lens) => {
    setValue(v);
    onChange?.(v);
  };
  return semantics === "radio" ? (
    <SegmentedControl<Lens>
      label="Org view"
      semantics="radio"
      value={value}
      onValueChange={commit}
      options={LENSES}
    />
  ) : (
    <SegmentedControl<Lens>
      label="Org view"
      semantics="tabs"
      panelId="org-lens"
      value={value}
      onValueChange={commit}
      options={LENSES}
    />
  );
}

/** Where the page's tab order enters the group, option by option. */
const tabstops = (role: "radio" | "tab" = "tab") =>
  screen.getAllByRole(role).map((b) => b.getAttribute("tabindex"));

/** Which option each part of the group considers chosen. */
const chosen = (role: "radio" | "tab" = "tab") =>
  screen
    .getAllByRole(role)
    .map((b) => b.getAttribute(role === "radio" ? "aria-checked" : "aria-selected"));

/** Move the keyboard onto an option, then press a key on it.
 *
 *  The group reads `document.activeElement` to decide where it is stepping
 *  from, so a key fired at an element nothing has focused is a key it
 *  correctly ignores. */
function press(option: HTMLElement, key: string): void {
  option.focus();
  fireEvent.keyDown(option, { key });
}

// EVERY OPTION IS A REAL BUTTON, which is what makes Enter and Space work at
// all. A `div` with a click handler takes neither key, and the group's own
// handler deliberately does not step in for it, so the element type is
// load-bearing here rather than a styling detail.
test("every option is a button the browser will activate", () => {
  render(<LiveSegmented />);
  for (const option of screen.getAllByRole("tab")) {
    expect(option.tagName).toBe("BUTTON");
    // `type` too: inside a form, a button without one submits it.
    expect(option.getAttribute("type")).toBe("button");
  }
});

// A KEY THE GROUP DOES NOT HANDLE IS THE BROWSER'S. Tab above all: swallowing
// it is how a group becomes a keyboard trap. Enter and Space are on this list
// for a second reason: they are how a reader COMMITS on a section row, and the
// browser only turns them into a click if the handler leaves them alone.
test("keys it does not own reach the browser", () => {
  render(<LiveSegmented />);
  const [chart] = screen.getAllByRole("tab");
  chart!.focus();
  for (const key of ["Tab", "Enter", " ", "a", "PageDown"]) {
    const event = new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true });
    chart!.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(false);
  }
  // …and one it does own IS taken, or the assertion above passes on a
  // control with no handler at all.
  const own = new KeyboardEvent("keydown", { key: "ArrowRight", bubbles: true, cancelable: true });
  chart!.dispatchEvent(own);
  expect(own.defaultPrevented).toBe(true);
});

// AND THE CLICK UNDER THOSE KEYS IS WHAT COMMITS. This is the gesture a
// browser turns Enter and Space on a focused button into, so it is the
// assertion that would catch focus and the click handler disagreeing about
// which option a reader arrowed to.
test("activating the focused option is what commits it", () => {
  const moved = vi.fn();
  render(<LiveSegmented onChange={moved} />);
  const tabs = screen.getAllByRole("tab");

  press(tabs[0]!, "End");
  expect(document.activeElement).toBe(tabs[2]);
  // NOTHING chosen by the arrow itself: that is the whole of manual activation.
  expect(moved).not.toHaveBeenCalled();

  fireEvent.click(document.activeElement!);
  expect(moved).toHaveBeenCalledTimes(1);
  expect(moved).toHaveBeenCalledWith("charter");
  expect(chosen()).toEqual(["false", "false", "true"]);
});

// HOME AND END ARE THE ENDS OF THE ROW, which a reader holding an arrow key
// should not have to walk to. End is what the case above steps with; Home is
// the return trip, and it is the one that used to be missing.
test("Home and End jump focus to the ends", () => {
  render(<LiveSegmented />);
  const tabs = screen.getAllByRole("tab");

  press(tabs[0]!, "End");
  expect(document.activeElement).toBe(tabs[2]);
  press(tabs[2]!, "Home");
  expect(document.activeElement).toBe(tabs[0]);
});

// THE TAB STOP IS ON THE CHOSEN OPTION, and it is DERIVED from the value
// rather than held in the control. That is what makes the three cases below
// true at once: tabbing out of a row and back lands on the section the reader
// chose, an outside change takes the stop with it, and there is no remembered
// index left over to strand.
test("the tab stop is the chosen option, and an outside change takes it along", () => {
  function Externally() {
    const [value, setValue] = useState<Lens>("chart");
    return (
      <>
        <SegmentedControl<Lens>
          label="Org view"
          semantics="tabs"
          panelId="org-lens"
          value={value}
          onValueChange={setValue}
          options={LENSES}
        />
        {LENSES.map((lens) => (
          <button key={lens.value} type="button" onClick={() => setValue(lens.value)}>
            go {lens.label}
          </button>
        ))}
      </>
    );
  }
  render(<Externally />);
  expect(tabstops()).toEqual(["0", "-1", "-1"]);

  // A click on another control, the browser's Back button, a URL somebody
  // pasted: each of them arrives here as a new `value` and nothing else.
  for (const [label, want] of [
    ["go Charter", ["-1", "-1", "0"]],
    ["go Directory", ["-1", "0", "-1"]],
    ["go Chart", ["0", "-1", "-1"]],
  ] as const) {
    // Arrow away first, so the stop has somewhere to be taken back FROM.
    press(screen.getAllByRole("tab")[0]!, "ArrowRight");
    fireEvent.click(screen.getByRole("button", { name: label }));
    expect(tabstops()).toEqual(want);
  }
});

// A VALUE THE GROUP DOES NOT OFFER STILL LEAVES ONE TAB STOP.
//
// `value` comes off the URL at most of these call sites, and `useParam` hands
// back whatever the query string says, so `?lens=bogus` (a link from an older
// build, a typo, a renamed option) reaches the control as a value no option
// carries. A stop that fell back to `value` in that case would be on no option
// at all: every one would render `tabIndex=-1` and the whole control would
// leave the page's tab order, unreachable by keyboard and worse than the plain
// buttons this replaced, which were each a stop of their own.
test("a value outside the group still leaves it reachable", () => {
  render(
    <SegmentedControl<Lens>
      label="Org view"
      semantics="tabs"
      panelId="org-lens"
      value={"bogus" as Lens}
      onValueChange={() => {}}
      options={LENSES}
    />,
  );
  // Exactly one stop, on the first option, since nothing is chosen and
  // nothing else has a claim to it.
  expect(tabstops()).toEqual(["0", "-1", "-1"]);
  expect(chosen()).toEqual(["false", "false", "false"]);

  // And the arrows still work from there rather than stepping from nowhere.
  press(screen.getAllByRole("tab")[0]!, "ArrowRight");
  expect(document.activeElement).toBe(screen.getAllByRole("tab")[1]);
});

// THE OPTION THAT LEAVES IS THE CHOSEN ONE, and it must not take the tab stop
// with it. `options` is a prop, so a caller may narrow it while a reader is
// standing in the row, and a `value` that named an option a render ago now
// names none: a stop derived from it alone would be on no option at all, every
// one would render `tabIndex=-1`, and the group would drop out of the page's
// tab order entirely, reachable by no key.
//
// THE CHOSEN OPTION RATHER THAN A SPARE ONE, because the stop here is DERIVED
// from `value` on every render: narrowing away an option nobody had chosen
// leaves the stop exactly where it already was, so the case would pass on a
// control with no fallback at all and assert nothing.
test("an option disappearing does not strand the tab stop", () => {
  function Shrinking() {
    const [all, setAll] = useState(true);
    return (
      <>
        <SegmentedControl<Lens>
          label="Org view"
          semantics="tabs"
          panelId="org-lens"
          value="charter"
          onValueChange={() => {}}
          options={all ? LENSES : LENSES.slice(0, 2)}
        />
        <button type="button" onClick={() => setAll(false)}>
          Narrow
        </button>
      </>
    );
  }
  render(<Shrinking />);
  expect(tabstops()).toEqual(["-1", "-1", "0"]);

  press(screen.getAllByRole("tab")[2]!, "Home");
  expect(document.activeElement).toBe(screen.getAllByRole("tab")[0]);

  fireEvent.click(screen.getByRole("button", { name: "Narrow" }));
  // Exactly one stop, on the first option, since nothing is chosen any more
  // and nothing else has a claim to it. The arrows still step from there.
  expect(tabstops()).toEqual(["0", "-1"]);
  expect(chosen()).toEqual(["false", "false"]);
  press(screen.getAllByRole("tab")[0]!, "ArrowRight");
  expect(document.activeElement).toBe(screen.getAllByRole("tab")[1]);
});

// A DISABLED OPTION IS NOT A DESTINATION. It is still drawn, because a choice
// a reader cannot make is information, but the arrows step over it and the tab
// stop never lands on it: focusing a disabled button is a keypress that looks
// like nothing happened.
test("the arrows step over an option nobody can choose", () => {
  render(
    <SegmentedControl<Lens>
      label="Org view"
      semantics="tabs"
      panelId="org-lens"
      value="chart"
      onValueChange={() => {}}
      options={[LENSES[0]!, { ...LENSES[1]!, disabled: true }, LENSES[2]!]}
    />,
  );
  const tabs = screen.getAllByRole("tab");

  press(tabs[0]!, "ArrowRight");
  expect(document.activeElement).toBe(tabs[2]);
  press(tabs[2]!, "ArrowLeft");
  expect(document.activeElement).toBe(tabs[0]);
});

// A SETTING IS THE OTHER CONTROL, and the arrows on it DO choose: a theme, a
// density or a grouping is not in the URL and costs nothing to change on every
// keypress, so selection follows focus there. Its options are radios rather
// than tabs, and the group claims no panel.
test("a setting commits as focus moves, and claims no panel", () => {
  const moved = vi.fn();
  render(<LiveSegmented semantics="radio" onChange={moved} />);
  const radios = screen.getAllByRole("radio");
  expect(screen.getByRole("radiogroup", { name: "Org view" })).toBeTruthy();
  expect(screen.queryAllByRole("tab")).toHaveLength(0);

  press(radios[0]!, "ArrowRight");
  expect(document.activeElement).toBe(radios[1]);
  // One call, not two: the focus move and the commit are one gesture.
  expect(moved).toHaveBeenCalledTimes(1);
  expect(moved).toHaveBeenCalledWith("directory");
  expect(chosen("radio")).toEqual(["false", "true", "false"]);
  expect(tabstops("radio")).toEqual(["-1", "0", "-1"]);

  // BOTH AXES on a setting: these draw as a horizontal row today, but a group
  // that wraps to two lines is the same control and Down is what a reader
  // presses on it.
  press(screen.getAllByRole("radio")[1]!, "ArrowDown");
  expect(moved).toHaveBeenLastCalledWith("charter");
  press(screen.getAllByRole("radio")[2]!, "ArrowUp");
  expect(moved).toHaveBeenLastCalledWith("directory");
});

// WRAPS, at both ends: a reader holding the key gets the whole group rather
// than stopping at an end they cannot see.
test("arrow focus wraps at both ends", () => {
  render(<LiveSegmented />);
  const tabs = screen.getAllByRole("tab");

  press(tabs[0]!, "ArrowLeft");
  expect(document.activeElement).toBe(tabs[2]);
  press(tabs[2]!, "ArrowRight");
  expect(document.activeElement).toBe(tabs[0]);
});

type Section = "overview" | "model" | "cost";

const SECTIONS: { value: Section; label: string }[] = [
  { value: "overview", label: "Overview" },
  { value: "model", label: "Model activity" },
  { value: "cost", label: "Cost" },
];

// A TAB LIST WITHOUT A PANEL IS A ROW OF BUTTONS WEARING THE ROLE.
//
// The role used to be declared and the relationship left out: nothing carried
// `role="tabpanel"`, nothing was referenced by `aria-controls`, and the
// switched content was an ordinary run of siblings after the strip. A screen
// reader could find the tabs and had no way to reach what the selected one
// opened, so choosing a tab moved the reader further from the content they had
// chosen. The panel is a component now, and naming it is what wires the two
// together: a row given no panel promises no panel.
test("a tab row promises a panel only when it has one", () => {
  const { rerender } = render(
    <Tabs ariaLabel="Seat sections" value="overview" onValueChange={() => {}} items={SECTIONS} />,
  );
  expect(screen.queryByRole("tabpanel")).toBeNull();
  for (const tab of screen.getAllByRole("tab")) {
    expect(tab.hasAttribute("aria-controls")).toBe(false);
  }

  rerender(
    <>
      <Tabs
        ariaLabel="Seat sections"
        value="overview"
        onValueChange={() => {}}
        items={SECTIONS}
        panelId="seat-sections"
      />
      <TabPanel id="seat-sections" value="overview">
        <p>the overview content</p>
      </TabPanel>
    </>,
  );
  const panel = screen.getByRole("tabpanel");
  const [overview] = screen.getAllByRole("tab");
  expect(overview!.getAttribute("aria-controls")).toBe(panel.getAttribute("id"));
  expect(panel.getAttribute("aria-labelledby")).toBe(overview!.getAttribute("id"));
  // Named by the tab that opened it, which is how a reader arriving in the
  // panel knows where they are.
  expect(screen.getByRole("tabpanel", { name: "Overview" })).toBe(panel);
});

// TWO STRIPS ON ONE PAGE MUST NOT SHARE IDS. A duplicated id makes
// `aria-controls` ambiguous and drops a reader into the wrong panel, and the
// id is composed from the panel's own name for exactly that reason.
test("two tab rows over two panels mint their own ids", () => {
  const strip = (id: string, label: string) => (
    <>
      <Tabs
        ariaLabel={label}
        value="overview"
        onValueChange={() => {}}
        items={[SECTIONS[0]!]}
        panelId={id}
      />
      <TabPanel id={id} value="overview">
        <p>{label} content</p>
      </TabPanel>
    </>
  );
  render(
    <>
      {strip("first", "First")}
      {strip("second", "Second")}
    </>,
  );
  const [a, b] = screen.getAllByRole("tabpanel");
  expect(a!.getAttribute("aria-labelledby")).toBe(tabId("first", "overview"));
  expect(b!.getAttribute("aria-labelledby")).toBe(tabId("second", "overview"));
  expect(a!.getAttribute("aria-labelledby")).not.toBe(b!.getAttribute("aria-labelledby"));
});
