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

import { useState, type CSSProperties } from "react";
import { RANGES, RANGE_LABEL, isRange, windowLabel } from "~/lib/range.ts";
import type { Range, TimeRange, Window } from "~/lib/range.ts";
import { fromWall, toWall, tsKey } from "~/lib/format.ts";
import { Button, Callout, FormField, Input, Modal } from "@crewlethq/ui";
import { ScheduleGlyph } from "@crewlethq/icons/glyphs";
// OURS, AND DELIBERATELY. `SegmentedControl` welds keyboard ACTIVATION to its
// `semantics` — the arrows commit the option they land on — and this strip
// drives a `useParam` that re-runs the screen's series query. Arrowing across
// the offered windows under that control is a query per keypress. Ours
// activates on Enter or Space and says so. See the report.
import { Segmented } from "~/ui/primitives.tsx";

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
    <Modal
      open
      title="Custom window"
      icon={<ScheduleGlyph size="md" />}
      // 420 IS NOT A STEP AND DOES NOT WANT TO BE. `sm` is 480 and `md` is
      // 560; two date boxes read in one glance are neither, and there is
      // nothing else in the product at this width to make a step out of.
      // `--crewlet-modal-width` is what the package publishes for exactly
      // this case, and the cast is only because React types no custom
      // property.
      style={{ "--crewlet-modal-width": "420px" } as CSSProperties}
      // WHAT `.dialog-body col gap-3` USED TO BE, as a prop rather than as
      // two classes on a body this file no longer owns.
      stackBody
      onClose={onClose}
      onSubmit={() => {
        if (!problem && at !== null && till !== null) onPick({ from: at, to: till });
      }}
      footer={
        <>
          {/* TERTIARY, NOT THE DEFAULT. uilet's Button defaults to `primary`
              — ours defaulted to the quiet recipe — so a footer that named no
              variant would put two primary buttons side by side and say
              nothing about which one commits. */}
          <Button variant="tertiary" onClick={onClose}>
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
      {/* ON uilet's [FormField]. The zone sentence belongs to BOTH boxes and
            is said once, under the first: it is what "From" and "To" are
            measured in, not a note about one of them. The component hands each
            control the id its own label points at, so a second field added
            here cannot quietly inherit the first one's. */}
      <FormField label="From" helper="In your own time zone, as every time on this screen is.">
        {(field) => (
          <Input
            id={field.id}
            type="datetime-local"
            width="full"
            value={start}
            aria-describedby={field.describedBy}
            onChange={(e) => setStart(e.target.value)}
          />
        )}
      </FormField>
      <FormField label="To">
        {(field) => (
          <Input
            id={field.id}
            type="datetime-local"
            width="full"
            value={end}
            aria-describedby={field.describedBy}
            onChange={(e) => setEnd(e.target.value)}
          />
        )}
      </FormField>
      {problem && (
        // `role="alert"` rather than `live`, which is what this has always
        // carried and what uilet's own doc reserves for the one that must
        // interrupt: it is the sentence standing between the reader and a
        // disabled Apply, so it has to reach them where they are typing
        // rather than wait for them to look.
        <Callout variant="danger" role="alert">
          {problem}
        </Callout>
      )}
    </Modal>
  );
}
