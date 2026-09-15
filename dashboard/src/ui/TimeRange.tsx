/**
 * The one control for `window=`.
 *
 * Every screen with a range had its own: Spend's picker in whole days, the
 * live strip's `STRIP_MINUTES` constant, and the event log's implicit
 * "whatever happens to be loaded". So three screens showing the same hour
 * disagreed about how long an hour was, and a link from one to another
 * carried no range at all.
 *
 * THE OFFERED SET IS THE SCREEN'S, not the whole vocabulary — a spend chart
 * has nothing useful to say about fifteen minutes and a client-buffered strip
 * has nothing to say about ninety days — and it is the SAME declaration the
 * screen's `useTimeRange` reads, carried on the range itself. Two lists would
 * be two answers to "which windows does this screen have", and the one the URL
 * is checked against is not the one a reader can see.
 *
 * The control renders what it is given in the VOCABULARY'S own order, so two
 * screens offering overlapping sets still mean the same thing by `1d` and
 * still read left to right the same way.
 */

import { useState } from "react";
import { RANGES, RANGE_LABEL, isRange, windowLabel } from "~/lib/range.ts";
import type { Range, TimeRange, Window } from "~/lib/range.ts";
import { fromWall, toWall, tsKey } from "~/lib/format.ts";
import { Dialog } from "~/ui/Dialog.tsx";
import { Button, Segmented } from "~/ui/primitives.tsx";

/** The custom option's value in the segmented group — not a [Range]. */
const CUSTOM = "custom";

export function TimeRangePicker({
  range,
  ariaLabel = "Time range",
}: {
  range: TimeRange;
  ariaLabel?: string;
}) {
  const [editing, setEditing] = useState(false);
  const { window, offer, since, until, set } = range;

  const options: { value: Range | typeof CUSTOM; label: string; title: string }[] = RANGES.filter(
    (r) => offer.ranges.includes(r),
  ).map((r) => ({ value: r, label: r, title: RANGE_LABEL[r] }));
  if (offer.custom) {
    options.push({
      value: CUSTOM,
      label: "Custom",
      // THE CHOSEN INTERVAL IS THE TOOLTIP, not the label. Two full
      // timestamps do not fit in a segment, and a segment whose width
      // changes with its value reflows the control beside it every time a
      // reader picks a different window.
      title: isRange(window) ? "Name two instants of your own" : windowLabel(window),
    });
  }

  return (
    <>
      <Segmented<Range | typeof CUSTOM>
        ariaLabel={ariaLabel}
        value={isRange(window) ? window : CUSTOM}
        onChange={(next) => (next === CUSTOM ? setEditing(true) : set(next))}
        options={options}
      />
      {editing && (
        <CustomWindow
          from={tsKey(since)}
          to={tsKey(until)}
          onClose={() => setEditing(false)}
          onPick={(next) => {
            set(next);
            setEditing(false);
          }}
        />
      )}
    </>
  );
}

/**
 * Two instants, typed.
 *
 * PREFILLED FROM THE WINDOW ON SCREEN rather than from an empty form or from
 * "today": a reader opening this has a window in front of them and almost
 * always wants to move one of its edges. Starting blank makes them retype the
 * edge they were keeping.
 *
 * The inputs are wall clock in the VIEWER'S chosen zone, which is what
 * [fromWall] exists for — a `datetime-local` is always the browser's, and this
 * dashboard renders in whichever zone the reader picked.
 */
function CustomWindow({
  from,
  to,
  onClose,
  onPick,
}: {
  from: number;
  to: number;
  onClose: () => void;
  onPick: (next: Window) => void;
}) {
  const [start, setStart] = useState(() => toWall(from));
  const [end, setEnd] = useState(() => toWall(to));

  const at = fromWall(start);
  const till = fromWall(end);
  // THE THREE FAILURES ARE THREE SENTENCES. "Invalid" over a form with two
  // fields tells a reader to check both of them.
  const problem =
    at === null
      ? "The start is not a date and time."
      : till === null
        ? "The end is not a date and time."
        : till <= at
          ? "The end has to be after the start — a window is half-open, so one that ends where it begins holds nothing."
          : "";

  return (
    <Dialog
      title="Custom window"
      icon="clock"
      width={420}
      onClose={onClose}
      onSubmit={() => {
        if (!problem && at !== null && till !== null) onPick({ from: at, to: till });
      }}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={problem !== ""}
            onClick={() => {
              if (!problem && at !== null && till !== null) onPick({ from: at, to: till });
            }}
          >
            Apply
          </Button>
        </>
      }
    >
      <div className="col gap-3">
        <div className="field">
          <label htmlFor="window-from">From</label>
          <input
            id="window-from"
            className="input"
            type="datetime-local"
            value={start}
            onChange={(e) => setStart(e.target.value)}
          />
          <span className="hint">In your own time zone, as every time on this screen is.</span>
        </div>
        <div className="field">
          <label htmlFor="window-to">To</label>
          <input
            id="window-to"
            className="input"
            type="datetime-local"
            value={end}
            onChange={(e) => setEnd(e.target.value)}
          />
        </div>
        {problem && (
          <div className="banner critical" role="alert">
            <span>{problem}</span>
          </div>
        )}
      </div>
    </Dialog>
  );
}
