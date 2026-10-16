/**
 * What is left of the component library once the dashboard draws
 * `@crewlethq/ui`.
 *
 * Everything with a peer in the design system is gone — the badge, the panel,
 * the empty state, the stat row, the skeleton, the select, the tabs, the
 * avatar, the banner, the disclosure, the search input, the button, the chip
 * and `cx` itself all come from the package now, and a second recipe for a
 * mark the package already draws is exactly the shape this file was written to
 * stop being.
 *
 * WHAT SURVIVES IS OF THREE KINDS, and the distinction is the useful part of
 * this file:
 *
 *  1. A VOCABULARY the package does not have and should not: `Tone`, the six
 *     words the engine's own payloads are spelled in, and the single
 *     translation of them into the package's spelling.
 *  2. A TABLE, not a recipe: `PhaseTag` draws the package's `Tag` and adds
 *     the one thing the package cannot know — which of the SIX phase strings
 *     the engine emits map onto the three hues it publishes, and that the
 *     other three take neutral.
 *  3. ONE CONTROL THE PACKAGE CANNOT YET DRAW, for a measured reason written
 *     at its definition: `Segmented` (its `SegmentedControl` keeps the stop
 *     the arrows last pointed at, which is stale when the value is changed
 *     from OUTSIDE the row).
 *
 * (3) names, at its definition, exactly what `@crewlethq/ui` would have to
 * grow for this file to lose it. That is the point of keeping it here rather
 * than quietly reimplementing the package beside it.
 *
 * THE OTHERS LEFT when the package grew what they were waiting for, and the
 * note is here because the list above is only honest if things come off it:
 * `Code` went to `CodeBlock`'s `focusWhenScrollable`, the clipboard write to
 * the package's `writeClipboard`, and `CopyButton`, `DownloadButton` and
 * `downloadText` to the package's own, whose refusal holds until the next
 * press and whose every attempt is announced.
 */

import {
  useId,
  useRef,
  useState,
  type FocusEvent,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import { cx, EmptyValue, Tag, type Tone as UiletTone } from "@crewlethq/ui";
import type { GlyphName } from "@crewlethq/icons/glyphs";
import { Mark } from "./glyph.tsx";

// ---------------------------------------------------------------------------
// Vocabulary
// ---------------------------------------------------------------------------

/**
 * The six words this product says about state.
 *
 * Kept although `@crewlethq/ui` has its own six, because they are the words
 * the ENGINE uses: an event's severity, a seat's health, a reconcile finding
 * and a tool's status all reach a screen spelled this way, and a dashboard
 * carrying the package's spelling instead would be translating at the point
 * where the data ARRIVES rather than at the point where it is drawn.
 */
export type Tone = "neutral" | "positive" | "caution" | "critical" | "info" | "accent";

/**
 * Our tone vocabulary, said in uilet's.
 *
 * The two sets name the same six ideas and agree on what they are FOR — uilet's
 * own tone doc carries this product's rule verbatim ("a tone says what a thing
 * IS, never who it is") — but they spell three of them differently: our
 * `positive`, `caution` and `critical` are their `success`, `warning` and
 * `danger`, and our `accent` is their `brand`.
 *
 * ONE TRANSLATION, not one per call site. The engine side of this repository
 * has paid for the alternative three times (`textcut`, `whsec`, `jsprovision`):
 * a rule spelled at each site is a rule whose copies come to disagree, and the
 * disagreement here is silent — a wrong `variant` string renders the neutral
 * pill rather than failing, so a caution reads as "nothing in particular" and
 * nothing in the build says so.
 *
 * It lives BESIDE `Tone` because that is the only place both spellings are in
 * view: a reader adding a seventh word to the union sees, in the next
 * paragraph, that it needs a uilet word too — and the `Record` below fails the
 * build if they do not give it one.
 */
const SPELLING: Record<Tone, UiletTone> = {
  neutral: "neutral",
  positive: "success",
  caution: "warning",
  critical: "danger",
  info: "info",
  accent: "brand",
};

/** The uilet variant a tone of ours is drawn as. */
export function uiletTone(tone: Tone): UiletTone {
  return SPELLING[tone];
}

// ---------------------------------------------------------------------------
// Marks
// ---------------------------------------------------------------------------

/**
 * A phase mark: the neutral pill with the phase's word.
 *
 * A PHASE IS A CATEGORY, AND A CATEGORY HAS NO COLOUR. The design system
 * removed its phase family in 0.5.0 for the reason this product gives for every
 * other identity: beside a state badge the state is the one coloured thing on
 * the row, and the phase is the word that says where the work is. Inside a
 * FIGURE a phase is a series (`lib/spend.ts`' `phaseColor`); on a row it is a
 * word, and every phase — the engine's seven, and one a newer peer sends — is
 * drawn the same way, which is the behaviour a rolling upgrade needs.
 */
export function PhaseTag({ phase }: { phase: string }) {
  const key = (phase || "").toLowerCase();
  return <Tag variant="neutral">{key || <EmptyValue label="No phase on this record" />}</Tag>;
}

// ---------------------------------------------------------------------------
// Choice
// ---------------------------------------------------------------------------

/**
 * One tab stop for a whole group, and the arrow keys that move within it.
 *
 * A radio group's keyboard contract is that the group is ONE stop in the
 * page's tab order, arrows move within it, and Home and End jump to the ends.
 * A group of plain buttons has the opposite behaviour: every option is its own
 * tab stop, and arrow keys do nothing. On the density control in the shell
 * that is three extra stops before a keyboard reader reaches the page content,
 * on every screen.
 *
 * ARROWS MOVE FOCUS AND COMMIT NOTHING, which is what the pattern calls
 * MANUAL activation, and it is the default here because of what these groups
 * are wired to. Seven of the nine sites drive a `useParam`: five push a
 * history entry and every one of them re-runs the screen's query, which
 * `socket.query` mints fresh with no cache, no dedupe and no coalescing. So
 * under selection-follows-focus, arrowing from the first option of a lens
 * strip to the last is four queries nobody asked for and four history entries
 * a reader then has to press Back through — and the reader most likely to
 * arrow through every option to hear what is there is the one using a screen
 * reader. That is exactly the trade the pattern names: selection follows
 * focus only while the result is displayed without noticeable latency and is
 * not costly to undo, and neither clause holds for a query behind a push.
 *
 * `automatic` is for a group whose options cost nothing — the shell's theme
 * and density, which write `localStorage` and a `data-` attribute — where
 * selection following focus is the better control and there is nothing to
 * undo.
 *
 * ENTER AND SPACE ARE NOT HANDLED HERE, and their absence is the design
 * rather than the gap it looks like: every option is a real `<button>`, so
 * the browser's own activation fires the click this already listens for. A
 * second handler would commit the same option twice on one keypress.
 */
function useRovingGroup<T extends string>(
  values: readonly T[],
  value: T,
  onChange: (value: T) => void,
  activate: "manual" | "automatic",
) {
  const box = useRef<HTMLDivElement>(null);

  // WHERE THE GROUP'S ONE TAB STOP IS, which stops being the same question as
  // which option is selected the moment arrows stop selecting: under manual
  // activation a reader stands on an option they have not chosen yet, and the
  // tab stop has to be under their feet or tabbing out and back drops them
  // somewhere else and loses the place they were holding.
  //
  // BOTH HALVES IN ONE STATE, the stop and the `value` it was seeded against.
  // The second used to be a ref, and a ref is the one thing that must not
  // hold it: React may DISCARD a render attempt and replay it, and a ref
  // write survives that discard while the state update queued beside it does
  // not. The pair then disagrees permanently — `seen` already says it has
  // observed the new value, so the guard below never fires again, and the tab
  // stop stays on an option nothing selected until `value` changes a second
  // time. Kept together, a discarded attempt reverts both and the replay
  // re-runs the guard.
  const [held, setHeld] = useState<{ stop: T; seen: T }>({ stop: value, seen: value });

  // An outside change to `value` retires whatever the arrows were pointing
  // at — a click elsewhere, the browser's Back button, a pasted URL. Adjusted
  // during render rather than from an effect, because an effect commits one
  // render first, and that render is the one a reader tabs into. Setting a
  // component's own state during its own render is what React documents for
  // this, and it is not the impurity a ref write is.
  if (held.seen !== value) setHeld({ stop: value, seen: value });

  // WHERE THE STOP LANDS WHEN THE OBVIOUS ANSWER IS NOT IN THE GROUP. Both
  // fallbacks are load-bearing and the second was missing: `held` leaves the
  // set when a caller narrows `options`, and `value` is outside it whenever a
  // URL carries a parameter this build does not know — `?lens=bogus` is a
  // typo, a hand-written link, or an option a newer build offers. With neither
  // in `values`, every option rendered `tabIndex={-1}` and the group left the
  // page's tab order altogether: unreachable by keyboard, which is worse than
  // the plain buttons this replaced, since those were each a stop of their
  // own. The first option is the honest landing place — nothing is checked,
  // so nothing else has a claim.
  const stop = values.includes(held.stop) ? held.stop : values.includes(value) ? value : values[0];

  const step = (from: number, by: number) => {
    // WRAPS, which is the pattern's own rule for both roles: a reader holding
    // the arrow key gets the whole group rather than stopping at an end they
    // cannot see.
    const next = values[(from + by + values.length) % values.length];
    if (next === undefined) return;
    // `seen` is untouched: it means "the last `value` this group observed",
    // and arrowing does not observe a new one.
    setHeld((h) => ({ ...h, stop: next }));
    if (activate === "automatic" && next !== value) onChange(next);
    // Focus is MOVED rather than requested, because `tabIndex` decides where
    // a LATER Tab lands and says nothing about where the browser's focus is
    // now. Read from the DOM rather than a ref array: the buttons are this
    // hook's own `[data-roving]` children and there is exactly one list of
    // them, so a ref array would be a second copy to keep in step.
    const buttons = box.current?.querySelectorAll<HTMLElement>("[data-roving]");
    buttons?.[values.indexOf(next)]?.focus();
  };

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    // `stop` is undefined only for a group with no options at all, where
    // `step` returns before it uses this.
    const at = stop === undefined ? -1 : values.indexOf(stop);
    // BOTH AXES. These render as a horizontal row today, but a group that
    // wraps to two lines is the same control and a reader pressing Down on
    // it is asking for the next option either way.
    switch (e.key) {
      case "ArrowRight":
      case "ArrowDown":
        step(at, 1);
        break;
      case "ArrowLeft":
      case "ArrowUp":
        step(at, -1);
        break;
      case "Home":
        step(at, -at);
        break;
      case "End":
        step(at, values.length - 1 - at);
        break;
      default:
        // Everything else is the browser's — Tab above all, which is how a
        // reader LEAVES the group, and Enter and Space, which are how the
        // focused button commits.
        return;
    }
    // Only for a key this handled: the arrows scroll the page otherwise, and
    // swallowing a key without acting on it takes a gesture away and puts
    // nothing in its place.
    e.preventDefault();
  };

  // THE STOP FOLLOWS FOCUS, HOWEVER FOCUS ARRIVED — a click, or a dialog
  // handing focus back to the option that opened it. Only the arrows used to
  // move it, so an option that opens something rather than committing (the
  // time picker's Custom) took focus on its click, kept `tabIndex={-1}`, and
  // got focus back when its dialog was dismissed: the reader stood on an
  // option that was neither checked nor the group's tab stop, and the next
  // Tab or arrow started from somewhere else. Where focus is, the reader is.
  const onFocus = (e: FocusEvent<HTMLDivElement>) => {
    const buttons = box.current?.querySelectorAll<HTMLElement>("[data-roving]");
    const at = buttons ? Array.prototype.indexOf.call(buttons, e.target) : -1;
    const next = values[at];
    if (next !== undefined && next !== held.stop) setHeld((h) => ({ ...h, stop: next }));
  };

  return { box, onKeyDown, onFocus, stop };
}

/**
 * One of N choices, drawn as a joined row.
 *
 * # Why this is not `@crewlethq/ui`'s `SegmentedControl`
 *
 * WHAT IS MISSING THERE IS AN `activate` PROP. `SegmentedControl` derives
 * activation from its `semantics` and offers no way to separate the two:
 * `radio` hands `useRoving` a select-on-move callback, so the arrows COMMIT,
 * and `tabs` moves focus only but claims `role="tablist"` and demands a
 * `panelId` for the `tabpanel` it controls.
 *
 * Neither is this control. Not one of the nine call sites is a tab widget —
 * they pick a theme, a density, a grouping, a scope, a window, a kind and
 * three lenses, and several sit in a screen head with the content they affect
 * hundreds of pixels below — so `tabs` would promise a reader a panel
 * relationship that does not exist. And `radio` is exactly what this IS, with
 * the one behaviour it cannot have: Config's lens pushes a history entry and
 * re-runs an uncached query per option, so arrow-to-commit is a query per
 * keystroke.
 *
 * So the gap is one prop — `activate: "manual" | "automatic"` on the `radio`
 * semantics, which is `useRoving`'s second argument already and is simply not
 * reachable from outside. With it, this component and its hook both delete and
 * every call site becomes `<SegmentedControl semantics="radio"
 * activate="manual" …>`. The second half of that gap is the tab stop:
 * `SegmentedControl` keeps it on the SELECTED option, and under manual
 * activation the stop has to travel with FOCUS instead, or tabbing out and
 * back drops the reader somewhere they did not leave.
 *
 * # What it is
 *
 * A RADIO GROUP, not a tab list. It used to declare `role="tablist"` with
 * `role="tab"` children, and announcing these as tabs is a mis-role rather
 * than a missing handler — it would not have been fixed by adding the
 * keyboard behaviour alone.
 */
export function Segmented<T extends string>({
  value,
  options,
  onChange,
  size,
  ariaLabel,
  activate = "manual",
}: {
  value: T;
  /** `icon` is a NAME rather than a node, so the size step below is decided
      once here rather than at each of the nine call sites — the shape
      `Dialog`, `ObjectHeader` and `cells` already take a mark in. */
  /**
   * `count` is how many things the option selects, where the caller has the
   * engine's own number for it. It is drawn in the same `count-chip` the
   * tab strip's count uses, so one figure does not read two ways on one
   * screen — and it is INSIDE the button, so a radio announces "Archived 2"
   * rather than leaving the number as unattached text beside a control.
   *
   * A NUMBER RATHER THAN A NODE, deliberately: a count is a count, and a
   * prop taking a node here is how one call site starts drawing a word in
   * the chip. `0` is a value and draws — "Archived 0" is exactly what a
   * reader needs to know before they click it — so an option with nothing
   * to say passes no count at all.
   *
   * `capped` says the number is a FLOOR — the page it was counted on stopped
   * with more behind it — and it is drawn `50+`, never as an exact figure.
   * A flag rather than a string for the same reason the count is a number.
   */
  options: {
    value: T;
    label: ReactNode;
    count?: number;
    capped?: boolean;
    icon?: GlyphName;
    title?: string;
    /**
     * The option cannot be chosen here, and `describedBy` names the element
     * that says why. It STAYS IN THE ARROWS' PATH and focusable
     * (`aria-disabled`, not `disabled`): a reader has to be able to land on a
     * mode to hear why it is unavailable, and a disabled button is skipped
     * silently. Pressing it does nothing.
     */
    disabled?: boolean;
    describedBy?: string;
  }[];
  onChange: (value: T) => void;
  size?: "sm";
  ariaLabel: string;
  /** See [useRovingGroup]. Defaults to manual; `automatic` needs a reason. */
  activate?: "manual" | "automatic";
}) {
  // A DISABLED OPTION IS NEVER COMMITTED, by a press or by automatic
  // activation alike — the one gate both paths go through.
  const choose = (next: T) => {
    if (options.find((o) => o.value === next)?.disabled) return;
    onChange(next);
  };
  const { box, onKeyDown, onFocus, stop } = useRovingGroup(
    options.map((o) => o.value),
    value,
    choose,
    activate,
  );
  // THE ONE THING MANUAL ACTIVATION OWES A READER: saying so.
  //
  // A radio group's learned contract is that the arrows CHOOSE — native radios
  // do, and the authoring practices describe no manual variant of the pattern.
  // The deviation is deliberate and measured (see [useRovingGroup]), and every
  // announcement along the way is honest: a reader arrowing onto an option
  // hears it is not checked, which is true. What they were never told is which
  // key would check it, so a reader who pressed Right, heard "not checked" and
  // moved on took the group's silence for a control that ignored them.
  //
  // A description rather than a different role: the alternatives that would
  // make the arrows conform — a toolbar of pressed buttons, a plain group with
  // `aria-current` — each drop either the mutual exclusivity that says these
  // are one choice or the "2 of 3" that says how many there are. Losing a true
  // semantic to gain a convention is the wrong trade when a sentence closes
  // the gap. Announced on entry to the group, and only where the arrows do not
  // already choose.
  const hintID = useId();
  const manual = activate !== "automatic";
  return (
    <div
      ref={box}
      className={cx("segmented", size === "sm" && "sm")}
      role="radiogroup"
      aria-label={ariaLabel}
      aria-describedby={manual ? hintID : undefined}
      onKeyDown={onKeyDown}
      onFocus={onFocus}
    >
      {manual && (
        <span id={hintID} className="sr-only">
          Arrow keys move between options; press Enter or Space to choose one.
        </span>
      )}
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          data-roving
          role="radio"
          aria-checked={o.value === value}
          // The GROUP is one tab stop. Without this every option is its own,
          // and the shell's two controls alone put six of them in front of
          // the page on every screen. It sits on the option the arrows are
          // HOLDING rather than the one that is checked, because under manual
          // activation those are different options for as long as a reader is
          // still deciding.
          tabIndex={o.value === stop ? 0 : -1}
          title={o.title}
          aria-disabled={o.disabled || undefined}
          aria-describedby={o.describedBy}
          onClick={() => choose(o.value)}
        >
          {o.icon && <Mark name={o.icon} size="xs" />}
          {o.label}
          {o.count != null && (
            <span className="count-chip t-num">{`${o.count.toLocaleString()}${o.capped ? "+" : ""}`}</span>
          )}
        </button>
      ))}
    </div>
  );
}
