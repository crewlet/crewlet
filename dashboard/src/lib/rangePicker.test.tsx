/**
 * A screen's range, as the kit's `TimeRangePicker` is handed it.
 *
 * The control, its dialog, its width and its focus are uilet's and asserted
 * there. What is decided HERE is what a window means on this engine: which
 * segments a screen offers and in which order, that Today is the company's
 * day, that a custom window is read on the reader's zone or as whole company
 * days, and that a window holding nothing is refused with its reason. Each
 * case below drives the kit's real control through [rangePickerProps].
 *
 * THE REFUSAL MATTERS. A window is HALF-OPEN, so one that ends where it
 * begins holds nothing, and "nothing" is a perfectly good query the engine
 * answers with an empty screen, which reads as a quiet company rather than as
 * a window nobody meant. Apply is disabled and the reason is said.
 */

import { afterEach, expect, test, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { LayerHost, TimeRangePicker } from "@crewlethq/ui";
import { rangePickerProps } from "./rangePicker.ts";
import type { TimeRange, Window } from "./range.ts";
import { afterMicrotasks } from "~/test/settle.ts";

/** The kit's control, as every screen draws it. */
function Picker({ range }: { range: TimeRange }) {
  return <TimeRangePicker {...rangePickerProps(range)} />;
}

afterEach(cleanup);

const FROM = "2031-04-16T09:00:00Z";
const TO = "2031-04-16T17:00:00Z";

/**
 * A range the way a screen's own hook hands one over.
 *
 * TYPED, NOT CAST. The first draft of this file built the fixture behind an
 * `as unknown as TimeRange` and gave `since`/`until` epoch numbers — which
 * `tsKey` reads as unparseable and answers 0 for, so both boxes opened on 1
 * January 1970 and every assertion below was about a dialog no reader will
 * ever see. The compiler knew; the cast was what stopped it saying so.
 */
function picker(set: (next: Window) => void = vi.fn()): TimeRange {
  return {
    window: "1d",
    since: FROM,
    until: TO,
    previous: { since: "2031-04-16T01:00:00Z", until: FROM },
    bucket: "hour",
    offer: { ranges: ["1d", "7d"], custom: true, fallback: "1d", buckets: ["hour"] },
    set,
  };
}

/** Open the custom window, which is a segment on the strip rather than a button. */
function open(range: TimeRange = picker()) {
  const out = render(
    <LayerHost>
      <Picker range={range} />
    </LayerHost>,
  );
  fireEvent.click(screen.getByTitle(/Name two instants|^Apr/));
  return out;
}

test("a window that ends where it begins is refused, and says why", () => {
  const set = vi.fn();
  open(picker(set));
  const to = screen.getByLabelText("To") as HTMLInputElement;
  const from = screen.getByLabelText("From") as HTMLInputElement;
  fireEvent.change(to, { target: { value: from.value } });

  expect(screen.getByText("The end must be after the start.")).toBeTruthy();
  const apply = screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement;
  expect(apply.disabled).toBe(true);
  fireEvent.click(apply);
  expect(set).not.toHaveBeenCalled();
});

// ENTER IN EITHER BOX APPLIES. A browser submits a form of several date
// fields on Enter only by pressing the form's submit button (its implicit
// submission), so Apply has to BE that button: as a plain button beside the
// form's `onSubmit`, Enter in "From" or "To" did nothing at all.
test("Apply is the form's submit button, so Enter in a box applies", () => {
  open();
  const apply = screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement;
  const from = screen.getByLabelText("From") as HTMLInputElement;
  expect(apply.type).toBe("submit");
  expect(apply.form).toBeTruthy();
  expect(apply.form).toBe(from.form);
});

// DISMISSED, FOCUS COMES BACK TO A TAB STOP. The dialog returns focus to the
// Custom option that opened it, which is not the checked one — and it has to
// be the group's stop while the reader stands on it, or Tab and the arrows
// resume from an option they are not on.
test("a dismissed custom window leaves the reader on a tab stop", async () => {
  render(
    <LayerHost>
      <Picker range={picker()} />
    </LayerHost>,
  );
  const custom = screen.getByRole("radio", { name: "Custom" });
  // A PRESS FOCUSES what it presses, which `fireEvent.click` does not.
  act(() => custom.focus());
  fireEvent.click(custom);
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  // INSIDE `act`, so the close's render is flushed rather than left to race the tick.
  await afterMicrotasks();
  expect(document.activeElement).toBe(custom);
  expect(custom.getAttribute("tabindex")).toBe("0");
  const stops = screen.getAllByRole("radio").filter((r) => r.getAttribute("tabindex") === "0");
  expect(stops).toEqual([custom]);
});

test("a window that holds something is applied", () => {
  const set = vi.fn();
  open(picker(set));
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  expect(set).toHaveBeenCalledOnce();
  const picked = set.mock.calls[0]?.[0] as { from: number; to: number };
  expect(picked.to).toBeGreaterThan(picked.from);
});

// TODAY IS A WINDOW OF ITS OWN where a screen offers it, first on the strip,
// and pressing it names the company's day rather than twenty-four hours.
test("today leads the strip where it is offered, and sets the company's day", () => {
  const set = vi.fn();
  const range = picker(set);
  range.offer = { ...range.offer, today: true, zone: "UTC" };
  render(
    <LayerHost>
      <Picker range={range} />
    </LayerHost>,
  );
  const radios = screen.getAllByRole("radio");
  expect(radios[0]!.textContent).toBe("Today");
  fireEvent.click(radios[0]!);
  const chosen = set.mock.calls[0]![0] as Window;
  expect(typeof chosen === "object" && chosen.today).toBe(true);
});

test("a screen that does not offer today does not draw it", () => {
  render(
    <LayerHost>
      <Picker range={picker()} />
    </LayerHost>,
  );
  expect(screen.queryByText("Today")).toBeNull();
});

// A DAY-GRAINED SCREEN TAKES TWO COMPANY DATES. Spend's question is company
// days; a picker that took minutes would offer a precision the answer cannot
// have. The boxes are dates on the company's clock, both counted, and what is
// set is the interval from the first one's midnight to the one after the last.
test("a screen of company days picks two dates on the company's clock", () => {
  const set = vi.fn();
  const range = picker(set);
  range.offer = { ...range.offer, customDays: true, zone: "Asia/Tokyo" };
  render(
    <LayerHost>
      <Picker range={range} />
    </LayerHost>,
  );
  fireEvent.click(screen.getByTitle("Pick two dates"));
  const first = screen.getByLabelText("First day") as HTMLInputElement;
  const last = screen.getByLabelText("Last day") as HTMLInputElement;
  expect(first.type).toBe("date");
  // Prefilled from the window on screen, on TOKYO's calendar: 09:00Z–17:00Z
  // on the 16th is 18:00 on the 16th to 02:00 on the 17th there.
  expect(first.value).toBe("2031-04-16");
  expect(last.value).toBe("2031-04-17");
  fireEvent.change(first, { target: { value: "2031-04-01" } });
  fireEvent.change(last, { target: { value: "2031-04-08" } });
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  expect(set).toHaveBeenCalledWith({
    from: Date.parse("2031-03-31T15:00:00Z"),
    to: Date.parse("2031-04-08T15:00:00Z"),
  });
});
