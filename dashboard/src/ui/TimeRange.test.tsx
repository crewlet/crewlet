/**
 * The custom window: the two claims this file still decides for itself.
 *
 * Everything the frame does — Escape, the veil, focus in and back, the three
 * bands — belongs to uilet's `Modal` and is asserted there. Two things do not,
 * and both were left uncovered when the dialog this drew in was our own:
 *
 * THE WIDTH. `sm` is 480 and `md` is 560, and two date boxes read in one
 * glance are neither. 420 travels as a component variable the package
 * publishes rather than as a step, which is a thing that compiles either way
 * and can only be seen.
 *
 * THE REFUSAL. A window is HALF-OPEN, so one that ends where it begins holds
 * nothing — and "nothing" is a perfectly good query the engine answers with an
 * empty screen, which reads as a quiet company rather than as a window nobody
 * meant. Apply is disabled and the reason is said.
 */

import { afterEach, expect, test, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { LayerHost } from "@crewlethq/ui";
import { TimeRangePicker } from "./TimeRange.tsx";
import type { TimeRange, Window } from "~/lib/range.ts";

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
      <TimeRangePicker range={range} />
    </LayerHost>,
  );
  fireEvent.click(screen.getByTitle(/Name two instants|^Apr/));
  return out;
}

test("the width a two-field window needs reaches the frame", () => {
  open();
  const frame = document.querySelector(".crewlet-modal") as HTMLElement | null;
  expect(frame).toBeTruthy();
  // The VARIABLE rather than a computed width: jsdom lays nothing out, and
  // what is being claimed is that the value reaches the element the package
  // reads it on — not what a browser then does with it.
  expect(frame?.style.getPropertyValue("--crewlet-modal-width")).toBe("420px");
});

test("a window that ends where it begins is refused, and says why", () => {
  const set = vi.fn();
  open(picker(set));
  const to = screen.getByLabelText("To") as HTMLInputElement;
  const from = screen.getByLabelText("From") as HTMLInputElement;
  fireEvent.change(to, { target: { value: from.value } });

  expect(screen.getByText(/half-open/)).toBeTruthy();
  const apply = screen.getByRole("button", { name: "Apply" }) as HTMLButtonElement;
  expect(apply.disabled).toBe(true);
  fireEvent.click(apply);
  expect(set).not.toHaveBeenCalled();
});

test("a window that holds something is applied", () => {
  const set = vi.fn();
  open(picker(set));
  fireEvent.click(screen.getByRole("button", { name: "Apply" }));
  expect(set).toHaveBeenCalledOnce();
  const picked = set.mock.calls[0]?.[0] as { from: number; to: number };
  expect(picked.to).toBeGreaterThan(picked.from);
});
