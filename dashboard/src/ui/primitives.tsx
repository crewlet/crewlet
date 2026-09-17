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
 *  3. FIVE CONTROLS THE PACKAGE CANNOT YET DRAW, each for a measured reason
 *     written at its own definition: `Segmented` (its `SegmentedControl` has
 *     no way to stop the arrows committing), `Meter` (no unknown ceiling, no
 *     direction), `Code` (no focus without select-all), `CopyButton` (its
 *     `useClipboard` welds the write to a reset policy, and one half of that
 *     policy is a timer nothing can clear) and `DownloadButton` (no peer at
 *     all). The CHROME of the last two is the package's even where their
 *     contract is not.
 *
 * Each of (3) names, at its definition, exactly what `@crewlethq/ui` would
 * have to grow for this file to lose it. That list is the point of keeping
 * them here rather than quietly reimplementing the package beside it.
 */

import {
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import { CheckGlyph, ContentCopyGlyph, ErrorGlyph, SaveGlyph } from "@crewlethq/icons/glyphs";
import { Button, Tag, cx, type TagVariant, type Tone as UiletTone } from "@crewlethq/ui";
import { Mark, type MarkName } from "./glyph.tsx";

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
 * Which uilet variant draws a phase.
 *
 * Phase is the one categorical identity this product spends colour on outside
 * a chart, because it is what a reader follows across the Model, Seat,
 * Activity and Trace screens — and `TagVariant` carries that vocabulary
 * already, on the design system's own hues (`--color-phase-*`), measured to
 * stay separable under protan and deutan vision.
 *
 * THE PREFIX IS THE POINT: uilet spells them `phase-onboarding`,
 * `phase-execute` and `phase-review` so a phase and a state can never be
 * confused for one another inside one union, and a bare `execute` is a type
 * error rather than a pill that quietly renders neutral.
 */
const PHASE_VARIANT: Record<string, TagVariant> = {
  onboarding: "phase-onboarding",
  execute: "phase-execute",
  review: "phase-review",
};

/**
 * A phase mark.
 *
 * The engine emits three phases beyond the three uilet draws — `subagent`,
 * `auxiliary` and `judge` — and there is no hue for them, which is the right
 * answer rather than a gap: they take the neutral pill, exactly as this drew
 * them before the port, and the word beside the colour is what a reader
 * actually reads. An unknown phase off the wire lands there too, which is the
 * behaviour a rolling upgrade needs.
 */
export function PhaseTag({ phase }: { phase: string }) {
  const key = (phase || "").toLowerCase();
  return <Tag variant={PHASE_VARIANT[key] ?? "neutral"}>{key || "—"}</Tag>;
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
  // link from an older build, a typo, or a renamed option. With neither in
  // `values`, every option rendered `tabIndex={-1}` and the group left the
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

  return { box, onKeyDown, stop };
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
  options: { value: T; label: ReactNode; icon?: MarkName; title?: string }[];
  onChange: (value: T) => void;
  size?: "sm";
  ariaLabel: string;
  /** See [useRovingGroup]. Defaults to manual; `automatic` needs a reason. */
  activate?: "manual" | "automatic";
}) {
  const { box, onKeyDown, stop } = useRovingGroup(
    options.map((o) => o.value),
    value,
    onChange,
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
          onClick={() => onChange(o.value)}
        >
          {o.icon && <Mark name={o.icon} size="xs" />}
          {o.label}
        </button>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Measure
// ---------------------------------------------------------------------------

/**
 * A bar: how full something is, and WHICH WAY FULL MEANS.
 *
 * # Why this is not `@crewlethq/ui`'s `Meter`
 *
 * TWO GAPS, and each of them makes a bar say something untrue on a screen this
 * dashboard ships today.
 *
 *  1. THERE IS NO WAY TO SAY "NO CEILING". `Meter` draws `role="meter"`
 *     unconditionally with `aria-valuemax={max}`, and `aria-valuemax` defaults
 *     to 100 when it is absent or NOT GREATER than the minimum — so a company
 *     with no token budget, which reaches this component as `max={0}`,
 *     announces "0 out of 100": a confident claim that nothing has been spent,
 *     where the truth is that nobody has said what the limit is. What it would
 *     need is an `unbounded` prop, or the rule below — a non-positive `max`
 *     draws the bar as decoration and leaves the legend to carry what IS
 *     known. Its `aria-valuenow` is unclamped too, which is the second half of
 *     the ARIA note further down.
 *
 *  2. `meterTone` HAS THE "SPENT" POLARITY WELDED IN — `>= 100` is `danger`,
 *     `>= 75` is `warning` — so a bar measuring PROGRESS reads a finished goal
 *     as a crisis and a goal three quarters of the way there as a warning.
 *     `routes/work/Goals.tsx` measures progress. A caller can override with
 *     `tone`, but an override computed per call site from the same ratio is
 *     the rule spelled at every site rather than once, which is precisely what
 *     `fullMeans` exists to replace. What it would need is a
 *     `fullMeans`/`polarity` prop feeding `meterTone`, or a second exported
 *     ramp for the achieved direction.
 *
 * # The direction is the caller's to state, because it is not derivable
 *
 * A bar at 100% is two opposite pieces of news depending on what it measures.
 * A budget at 100% is refused charges; a goal at 100% is the goal reached.
 * The tone was derived from the fill alone — 75% caution, 100% critical —
 * which is exactly right for a budget and exactly backwards for progress: a
 * goal three-quarters of the way there rendered as a WARNING, and one fully
 * achieved would have rendered as a CRISIS.
 *
 * So `fullMeans` is REQUIRED rather than defaulted. A default would be the
 * wrong answer half the time, silently, and the one call site that had
 * noticed was passing `tone="accent"` to opt out of the rule rather than
 * fixing it — which is the shape a wrong default always leaves behind.
 *
 *   - `spent`    — a budget, a capacity, a quota. Full is bad, and the bar
 *                  warns before it gets there.
 *   - `achieved` — progress towards something wanted. Full is GOOD and says
 *                  so; nothing below it is a fault the bar can diagnose, so
 *                  everything short of done is simply the accent.
 *
 * `tone` still overrides both, for the cases a caller knows something the
 * ratio does not — a budget already refusing charges is critical at any fill.
 *
 * # And two things about the ARIA
 *
 * A `role="meter"` with no accessible name announces as "meter, 120000" and
 * nothing else — a number with no subject, on a screen that has several. The
 * visible legend is not the name: `label` is a ReactNode, several call sites
 * pass a percentage sentence rather than a noun, and two sites pass none at
 * all because the meter sits in a table cell whose column heading does the
 * naming for a sighted reader. So the name is a required prop of its own,
 * exactly as it is on [Segmented].
 *
 * And `aria-valuenow` may not exceed `aria-valuemax`. The fill is clamped —
 * a bar cannot be 130% long — but the value was not, so a budget LOWERED
 * under a counter that has already spent past it (which is the whole reason
 * an operator opens this screen) published an out-of-range value that a
 * screen reader is entitled to render as anything at all. The clamp goes on
 * the value and the true figures go in `aria-valuetext`, so the overage is
 * reported rather than hidden.
 */
export function Meter({
  used,
  max,
  label,
  ariaLabel,
  right,
  tone,
  fullMeans,
}: {
  used: number;
  max: number;
  label?: ReactNode;
  /** What this meter measures, as a bare noun phrase — "Company budget", not
      "94% used". The accessible name; see the note above. */
  ariaLabel: string;
  right?: ReactNode;
  tone?: "accent" | "positive" | "caution" | "critical" | "neutral";
  /** Which way full means; see the note above. Required, never defaulted. */
  fullMeans: "spent" | "achieved";
}) {
  const scaled = max > 0;
  // CLAMPED AT BOTH ENDS, like `aria-valuenow` below. Only the top used to
  // be, so a negative reading rendered `width: -5%` — which CSSOM drops,
  // leaving a bar that silently keeps its previous width rather than reading
  // empty. The doc above promises clamping; this is the half that was not.
  const pct = scaled ? Math.max(0, Math.min(100, (used / max) * 100)) : 0;
  // The tone is DERIVED from the fill AND from what full means, unless the
  // caller overrides it — so a bar that is nearly full warns where warning is
  // the right news, and celebrates where it is not.
  const auto =
    fullMeans === "achieved"
      ? pct >= 100
        ? "positive"
        : "accent"
      : pct >= 100
        ? "critical"
        : pct >= 75
          ? "caution"
          : "accent";
  return (
    <div className="meter">
      {(label || right) && (
        <div className="meter-legend">
          <span className="truncate">{label}</span>
          <span className="t-num">{right}</span>
        </div>
      )}
      <div
        className="meter-track"
        // NO SCALE, NO METER. `aria-valuemax` defaults to 100 when it is
        // absent or not greater than the minimum, so a meter with an
        // unknown ceiling would announce "0 out of 100" — a confident claim
        // that nothing has been spent, where the truth is that nobody has
        // said what the limit is. Drawn as decoration instead; the legend
        // beside it carries whatever is actually known.
        role={scaled ? "meter" : undefined}
        aria-label={scaled ? ariaLabel : undefined}
        aria-valuenow={scaled ? Math.max(0, Math.min(used, max)) : undefined}
        aria-valuemin={scaled ? 0 : undefined}
        aria-valuemax={scaled ? max : undefined}
        // THE TRUE FIGURES, past the clamp. A meter reading "100%" when the
        // counter is at 130% of a budget somebody just lowered is the one
        // state where the exact numbers are the whole message.
        aria-valuetext={scaled ? `${used} of ${max}` : undefined}
      >
        <div className="meter-fill" data-tone={tone ?? auto} style={{ width: `${pct}%` }} />
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Record
// ---------------------------------------------------------------------------

/**
 * A block of preformatted text, optionally one that OWNS select-all.
 *
 * # Why this is not `@crewlethq/ui`'s `CodeBlock`
 *
 * `CodeBlock` is the better block wherever a block is selectable, and every
 * screen whose blocks are has already moved to it. What it cannot express is
 * the OTHER case: `selectable` is one boolean carrying three things —
 * `tabIndex={0}`, the `role="region"` with its required `label`, and the
 * Command/Control+A capture — and its type deliberately welds the label to the
 * flag, so a block cannot be focusable without also taking the key.
 *
 * That trade is wrong for a tool call's arguments, and measurably so. `.code`
 * is `overflow: auto` under a `max-height`, so ANY block past the cap is a
 * scroll container, and in Chrome and Safari a scroll container is only
 * reachable by keyboard if something makes it focusable — while a tab stop on
 * EVERY block would put one in front of each of a phase card's dozens of tool
 * arguments, most of them three lines that never overflow. So the stop is
 * given to the blocks that need it, which is a fact about the RENDERED BOX and
 * not about any prop: the same content overflows or does not depending on the
 * viewport.
 *
 * What `CodeBlock` would need is either a `focusWhenScrollable` prop that
 * measures its own box, or `selectable` split from label-and-focus so a block
 * can be a named region without owning ⌘A.
 *
 * # What `selectable` means here
 *
 * It makes the block focusable and gives ⌘A / Ctrl+A a local meaning while it
 * has focus: select this block, not the document. That is the one keyboard
 * gesture a reader brings to a screen whose point is a single record, and the
 * browser's own answer to it — the nav, the stat row, every phase card and the
 * record — is never what they wanted.
 *
 * INTERCEPTED ON THE ELEMENT, not on the document. A document-level handler
 * would have to guess which block the reader meant, and it would take the
 * gesture away from the rest of the page for as long as this screen is
 * mounted. Focus is the reader saying which one; everywhere else ⌘A keeps
 * meaning what it has always meant.
 */
interface CodeProps {
  children: ReactNode;
  plain?: boolean;
  /** Take ⌘A / Ctrl+A while focused. */
  selectable?: boolean;
  /**
   * What this block is: "The turn record, as JSON".
   *
   * REQUIRED ON EVERY BLOCK, not only on a selectable one, because any block
   * may turn out to scroll and a block that scrolls takes focus — see the
   * note above. Taking focus without a name is a tab stop a screen reader
   * announces as nothing, so the two arrive together or neither does.
   */
  label: string;
}

export function Code({ children, plain, selectable, label }: CodeProps) {
  const box = useRef<HTMLPreElement>(null);
  // WHETHER THIS BLOCK ACTUALLY SCROLLS, measured rather than assumed — the
  // whole of why this is not `CodeBlock`, argued at the top. MEASURED because
  // Firefox makes a scroll container focusable on its own and Chrome and
  // Safari do not, so there is nothing to read the answer off.
  const [scrolls, setScrolls] = useState(false);
  useEffect(() => {
    const node = box.current;
    if (!node) return;
    // BOTH AXES: `plain` sets `white-space: pre`, so a wide line scrolls
    // sideways in a box that is not tall enough to scroll at all.
    const measure = () =>
      setScrolls(node.scrollHeight > node.clientHeight || node.scrollWidth > node.clientWidth);
    measure();
    // The box is resized by the window, by a disclosure opening above it and
    // by its own content arriving on a streamed frame, and none of those is
    // a render of THIS component. ResizeObserver is the only one of the
    // three it can see.
    if (typeof ResizeObserver === "undefined") return;
    const watch = new ResizeObserver(measure);
    watch.observe(node);
    return () => watch.disconnect();
  }, [children, plain]);

  // Focusable when it owns ⌘A, and when it is a scroll container a reader
  // would otherwise be unable to reach.
  const focusable = Boolean(selectable) || scrolls;

  const onKeyDown = (e: KeyboardEvent<HTMLPreElement>) => {
    // THE PHYSICAL KEY FIRST. Browsers resolve select-all from the key's
    // position, not from the character a layout maps it to, so matching only
    // `e.key` misses on every non-Latin layout — where `"ф"` or `"α"` comes
    // back, this handler declines, and the document-wide select-all it exists
    // to replace happens instead. `e.key` stays as the fallback for anything
    // that reports no `code`.
    const isA = e.code === "KeyA" || (!e.code && e.key.toLowerCase() === "a");
    // Shift and Alt make DIFFERENT chords, several of which the browser owns
    // (Ctrl+Shift+A is Chrome's tab search). Swallowing them would take a
    // shortcut away and put nothing in its place.
    if (!isA || e.altKey || e.shiftKey || !(e.metaKey || e.ctrlKey)) return;
    const node = box.current;
    const selection = window.getSelection?.();
    // No Selection API — the browser's own select-all is then strictly
    // better than nothing, so this hands the key back rather than
    // swallowing it.
    if (!node || !selection) return;
    e.preventDefault();
    selection.removeAllRanges();
    const range = document.createRange();
    range.selectNodeContents(node);
    selection.addRange(range);
  };

  return (
    <pre
      ref={box}
      className={cx("code", plain && "plain", selectable && "selectable")}
      tabIndex={focusable ? 0 : undefined}
      role={focusable ? "region" : undefined}
      aria-label={focusable ? label : undefined}
      onKeyDown={selectable ? onKeyDown : undefined}
    >
      {children}
    </pre>
  );
}

// ---------------------------------------------------------------------------
// Hand-off
// ---------------------------------------------------------------------------

/**
 * How long a confirmation holds before the control offers its action again.
 *
 * Long enough to be read at a glance, short enough that a reader who wants it
 * twice is not waiting on it. A REFUSAL is not on this clock — see the click
 * handler below.
 */
const CONFIRMED_HOLD_MS = 2000;

/**
 * The half a Copy and a Download button are the same: do a thing that either
 * lands or does not, say which, and settle back so the label is never a stale
 * claim.
 *
 * Extracted rather than written twice. The two controls sit SIDE BY SIDE in
 * the same header, so any difference between them — how long the
 * confirmation holds, whether a refusal is announced at all, whether the
 * status text leaks into the accessible name — is a visible inconsistency
 * rather than a private detail, and a second copy is exactly how one of them
 * acquires it. The engine has the same lesson written down twice, in
 * `internal/textcut` and `internal/api/httpjson`.
 *
 * THE CHROME IS UILET'S AND THE CONTRACT IS OURS. What is drawn is
 * `@crewlethq/ui`'s `Button`, wearing the two outcome glyphs the package's own
 * copy control wears, so a copy button here and a `Copyable` elsewhere on the
 * page are the same mark. What this adds is the two behaviours uilet's
 * `CopyButton` does not have and a download has no peer for at all: a refusal
 * that HOLDS, and a live region keyed on the attempt, so an identical outcome
 * twice running is still a change something announces.
 *
 * `run` returns whether it landed. It may be async — the Clipboard API is —
 * and it must not throw: a refusal is a `false`, because the caller is the
 * only frame that knows what a refusal MEANS to say about.
 */
function FeedbackButton({
  run,
  icon,
  label,
  doneLabel,
  failedLabel,
  doneSaid,
  failedSaid,
  title,
  variant,
  size = "sm",
}: {
  run: () => boolean | Promise<boolean>;
  /** The glyph at rest. A COMPONENT, which is how every uilet control takes
      one, and what lets the outcome glyphs below stand in its place. */
  icon: ReactNode;
  /** What the control offers, at rest. */
  label: string;
  /** The same control once it worked — short, because it is a button. */
  doneLabel: string;
  /** …and once it did not. */
  failedLabel: string;
  /** What a screen reader is told on success. Specific, because the icon
   *  swap is the only signal a sighted reader gets and a screen reader gets
   *  none of it. */
  doneSaid: string;
  /** What a screen reader is told on failure, and the button's `title` while
   *  it is in that state: the reason replaces the offer, since the offer is
   *  the thing that just did not happen. */
  failedSaid: string;
  title?: string;
  variant?: "default" | "ghost";
  size?: "md" | "sm";
}) {
  // THE ATTEMPT TRAVELS WITH THE STATE, because an identical outcome twice
  // running is not a DOM change and a live region announces changes only. A
  // second refusal left a reader who cannot see the button with silence — and
  // a refusal now holds rather than settling back, so there is no reset to
  // make the third one audible either. Keyed on the count, the status node is
  // REPLACED rather than re-rendered, which is a change.
  const [{ state, attempt }, setOutcome] = useState<{
    state: "idle" | "done" | "failed";
    attempt: number;
  }>({ state: "idle", attempt: 0 });
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const live = useRef(true);

  // The timeout outlives the component otherwise, and a screen left during
  // the seconds after a click sets state on something unmounted. Set on the
  // way IN as well as cleared on the way out, because StrictMode mounts,
  // unmounts and mounts again, and a flag only ever cleared stays cleared.
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
      clearTimeout(timer.current);
    };
  }, []);

  const onClick = useCallback(() => {
    void Promise.resolve(run()).then((ok) => {
      // The answer can arrive after the screen is gone: a clipboard write
      // waits on a permission prompt the reader may never answer. The
      // cleanup above has already run by then, so arming a timer here would
      // arm the one thing nothing clears.
      if (!live.current) return;
      setOutcome((prior) => ({ state: ok ? "done" : "failed", attempt: prior.attempt + 1 }));
      clearTimeout(timer.current);
      // A CONFIRMATION SETTLES BACK. A REFUSAL DOES NOT.
      //
      // Both used to, on one constant, and that put the failure back into the
      // state this control exists to leave: three seconds after a download
      // the reader is looking at the browser's shelf and not at the button,
      // and a button that has reverted to offering its action is
      // indistinguishable from one that was never pressed. So a refusal holds
      // until the next click — which is the gesture a reader who wants to
      // retry makes anyway — and only the confirmation is on a clock.
      if (ok) {
        timer.current = setTimeout(
          () => setOutcome((prior) => ({ ...prior, state: "idle" })),
          CONFIRMED_HOLD_MS,
        );
      }
    });
  }, [run]);

  const said = state === "done" ? doneSaid : state === "failed" ? failedSaid : "";
  // The glyph step this button's own type size asks for. The mark is the only
  // signal the swap gives a sighted reader, so the outcomes are drawn at the
  // step the resting glyph was.
  const glyph = size === "sm" ? "xs" : "sm";

  return (
    <span className="row gap-1">
      <Button
        // OUR TWO WORDS, SAID IN UILET'S. `ghost` is the transparent one and
        // `default` the bordered one this product has always drawn; the
        // package spells them `tertiary` and `secondary`, and its own default
        // is `primary`, which is neither. Translated in one place rather than
        // at the three call sites, for the reason [uiletTone] gives above.
        size={size === "sm" ? "small" : "medium"}
        variant={variant === "ghost" ? "tertiary" : "secondary"}
        leadingIcon={
          state === "done" ? (
            <CheckGlyph size={glyph} />
          ) : state === "failed" ? (
            <ErrorGlyph size={glyph} />
          ) : (
            icon
          )
        }
        onClick={onClick}
        title={state === "failed" ? failedSaid : title}
      >
        {state === "done" ? doneLabel : state === "failed" ? failedLabel : label}
      </Button>
      {/* Announced, not just drawn: the icon swap is the only signal a
          sighted reader gets, and a screen reader gets none of it.

          A SIBLING of the button, never a child. A button's accessible name
          is computed from its contents, so inside it this named the control
          "Copied copied to the clipboard" — the status text becoming part of
          what the button claims to be. */}
      <span className="sr-only" role="status">
        <span key={attempt}>{said}</span>
      </span>
    </span>
  );
}

/**
 * The text, or a THUNK that produces it.
 *
 * The thunk is not a convenience: on a live turn the thing worth copying is
 * assembled from every phase and every streamed frame, and `agents` is pushed
 * twice per tool round — so a string prop means a full `JSON.stringify` of
 * the whole turn on every push, for a button nobody has clicked. Resolved on
 * click, it costs nothing until it is asked for.
 *
 * The same union `useClipboard`'s `copy` takes, so the copy path hands it
 * straight through and only the download path has to resolve it.
 */
type Text = string | (() => string);

function resolve(text: Text): string {
  return typeof text === "function" ? text() : text;
}

/**
 * Put text on the clipboard, and SAY WHETHER IT LANDED.
 *
 * Two things a bare `navigator.clipboard.writeText(x)` gets wrong, and both
 * were live in this dashboard:
 *
 *  1. **It is not always there.** The Clipboard API is gated on a secure
 *     context. `http://localhost:8000` qualifies, but the same engine
 *     reached at `http://10.0.0.4:8000` — which is how anyone reads the
 *     dashboard of a node that is not their laptop — does not, and
 *     `navigator.clipboard` is then undefined. `?.` made that failure
 *     silent: the button clicked, nothing was copied, nothing said so. The
 *     textarea fallback is deprecated and is also the only thing that works
 *     there, so it stays until the dashboard is only ever served over TLS.
 *  2. **A copy with no feedback is indistinguishable from a dead button.**
 *     The clipboard is invisible; the only way a reader learns it worked is
 *     if the control says so.
 *
 * # WHY THE WRITE IS STILL OURS, and what would retire it
 *
 * `@crewlethq/ui` publishes [useClipboard], which is the same two rules and
 * should be the only copy of them in the tree. It cannot be used here yet, and
 * the reason is not style: the hook does not expose the WRITE, only a write
 * welded to a reset policy, and that policy breaks two things this control is
 * measured on.
 *
 *  - A REFUSAL SETTLES BACK. `copy` arms `setState("idle")` for `resetMs`
 *    after BOTH outcomes, so a refused copy reverts to offering its action —
 *    which is indistinguishable from a button that was never pressed, and is
 *    the exact state [FeedbackButton] exists to leave. That half is
 *    survivable: the boolean `copy` returns is the same answer without the
 *    reset, and ignoring `state` recovers it.
 *  - THE TIMER IS ARMED AFTER THE AWAIT, WITH NO LIVENESS GUARD. The hook's
 *    only cleanup is an unmount effect that clears whatever timer exists AT
 *    that moment; a clipboard write held behind a permission prompt settles
 *    later, and the `setTimeout` it then arms is the one timer nothing can
 *    clear. That half is not survivable from outside the hook, and it is
 *    asserted here ("an answer that arrives after the screen is gone arms
 *    nothing") — measured, not theorised: the suite goes red on it.
 *
 * So what uilet would need is either a guard of its own (an unmount ref
 * checked after the await, exactly as below) or an exported `writeClipboard`,
 * so a caller that owns its own outcome can take the write without the
 * policy. With either, [copyToClipboard] and [execCommandCopy] below delete
 * and this becomes `const { copy } = useClipboard()`.
 */
export function CopyButton({
  text,
  label = "Copy",
  title,
  variant,
  size = "sm",
}: {
  text: Text;
  label?: string;
  title?: string;
  variant?: "default" | "ghost";
  size?: "md" | "sm";
}) {
  const run = useCallback(() => copyToClipboard(resolve(text)), [text]);
  return (
    <FeedbackButton
      run={run}
      icon={<ContentCopyGlyph size={size === "sm" ? "xs" : "sm"} />}
      label={label}
      doneLabel="Copied"
      failedLabel="Copy failed"
      doneSaid="copied to the clipboard"
      failedSaid="the browser refused the clipboard"
      title={title}
      variant={variant}
      size={size}
    />
  );
}

/**
 * Hand the same text to the reader as a FILE, and say whether it landed.
 *
 * The sibling of [CopyButton], over the same bytes, because the two things an
 * operator does with a turn are different: one is pasted into a message, the
 * other is attached to a bug report or kept beside the incident. A clipboard
 * also holds exactly one thing, so copying two turns to compare them is not a
 * gesture that exists.
 *
 * NO PEER AT ALL in `@crewlethq/ui`: there is a `CopyButton` and a `Copyable`,
 * and nothing that saves. What the package would need is the mirror of
 * `useClipboard` — a `useDownload` owning the two checks below, the anchor and
 * the revoke — after which this control is the package's `Button` and nothing
 * else.
 *
 * Its failure modes are not the clipboard's, and one of them is worse than a
 * dead button:
 *
 *  1. **No `URL.createObjectURL`** — nothing to point a saveable link at, and
 *     nothing to fall back to. A `data:` URL is the usual answer and it is
 *     the wrong one here: several engines cap it around two megabytes and a
 *     self-iterating turn's JSON goes past that, so the fallback would work
 *     on the small turns nobody needs it for and fail silently on the large
 *     ones.
 *  2. **No `download` attribute** — the click then NAVIGATES to the JSON
 *     instead of saving it, which looks enough like something happening that
 *     nobody checks. Refusing is the honest answer; both are checked up front
 *     rather than assumed.
 */
export function DownloadButton({
  text,
  filename,
  mime = "application/json;charset=utf-8",
  label = "Download",
  title,
  variant,
  size = "sm",
}: {
  text: Text;
  /** The name to offer it under. Sanitised here — see [safeFilename]. */
  filename: string;
  mime?: string;
  label?: string;
  title?: string;
  variant?: "default" | "ghost";
  size?: "md" | "sm";
}) {
  const name = safeFilename(filename);
  const run = useCallback(() => saveTextFile(resolve(text), name, mime), [text, name, mime]);
  return (
    <FeedbackButton
      run={run}
      icon={<SaveGlyph size={size === "sm" ? "xs" : "sm"} />}
      label={label}
      // WHAT WAS OBSERVED, which is a HAND-OFF. There is no completion event
      // on an `<a download>`: `saveTextFile` returns true because the click
      // did not throw, and Chrome's automatic-multiple-download gate can stop
      // it silently after that. "Saved" claimed a file on a disk nothing here
      // can see. The NAME is the half that is true and the half worth saying,
      // since a reader who cannot see the download shelf has nothing else to
      // tell them what to go and open.
      doneLabel="Downloading"
      failedLabel="Download failed"
      doneSaid={`download started — ${name}`}
      failedSaid="the browser refused the download"
      title={title}
      variant={variant}
      size={size}
    />
  );
}

async function copyToClipboard(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    // A denied permission and a non-secure origin both land here, and the
    // fallback below is the answer to both.
  }
  return execCommandCopy(text);
}

/** The pre-Clipboard-API copy: a selected off-screen textarea. */
function execCommandCopy(text: string): boolean {
  if (typeof document.execCommand !== "function") return false;
  const field = document.createElement("textarea");
  field.value = text;
  // Off-screen rather than hidden: a `display: none` field cannot be
  // selected, and selecting it is the whole mechanism. `readOnly` stops a
  // mobile keyboard appearing for the frame it exists.
  field.setAttribute("readonly", "");
  field.style.position = "fixed";
  field.style.top = "-1000px";
  field.style.opacity = "0";
  // WHERE FOCUS WAS, because `select()` takes it and `remove()` drops it on
  // the document body: after a fallback copy the reader's next Tab started
  // from the top of the page rather than from the button they had just
  // pressed, on exactly the plain-http origin this branch exists for. Read
  // before the field is in the document and restored after it is gone —
  // `isConnected` because the click may have unmounted whatever held it.
  const held = document.activeElement;
  document.body.appendChild(field);
  try {
    field.select();
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    field.remove();
    if (held instanceof HTMLElement && held.isConnected) held.focus();
  }
}

/**
 * Save `text` to the reader's machine as `filename`, and report whether the
 * browser took it.
 *
 * The blob URL is revoked on the NEXT macrotask rather than here: the click
 * only QUEUES the download, and freeing the entry inside the same task races
 * the fetch that is about to read it. Not revoking at all is the other
 * failure — the blob is a second copy of the whole turn, held for the life
 * of the tab, and a reader comparing turns clicks this several times.
 */
function saveTextFile(text: string, filename: string, mime: string): boolean {
  if (typeof URL.createObjectURL !== "function" || !("download" in HTMLAnchorElement.prototype)) {
    return false;
  }
  let url = "";
  try {
    url = URL.createObjectURL(new Blob([text], { type: mime }));
    const link = document.createElement("a");
    link.href = url;
    link.download = filename;
    // In the document for the one synchronous call: not every engine
    // dispatches an activation behaviour on a node that is in no document,
    // and there is no state to leave behind either way.
    document.body.appendChild(link);
    try {
      link.click();
    } finally {
      link.remove();
    }
    return true;
  } catch {
    return false;
  } finally {
    if (url) setTimeout(() => URL.revokeObjectURL(url), 0);
  }
}

/** Longer than any extension in use, so a `.` deep inside a name is not read
 *  as one when a long name has to be cut. */
const MAX_EXTENSION = 12;
/**
 * Every filesystem a reader of this dashboard is on allows at least 255
 * BYTES, and the browser appends its own " (1)" to deduplicate against what
 * is already in the folder; eCryptfs stops at 143. 120 clears all three with
 * room to spare.
 */
const MAX_FILENAME = 120;

/**
 * A filename the reader will actually get, out of whatever the caller had.
 *
 * The name is composed from data — a turn id comes off the URL — and the
 * `download` attribute is only a SUGGESTION: the browser sanitises it its own
 * way, stripping path separators and whatever else each engine dislikes.
 * Deciding it here means every caller gets one predictable answer rather than
 * three, and a name that arrived as `../etc/passwd` or with a newline in it
 * never reaches that guess.
 */
function safeFilename(name: string): string {
  // UNICODE-AWARE, and that is not a nicety. `\w` is ASCII-only, so an
  // ASCII-only class collapsed a whole non-Latin stem to a single `-` and the
  // leading-strip below then took that hyphen AND the extension's own
  // separator with it: `日本語.json` came out as a file called `json`, and
  // `résumé.json` as `r-sum-.json`. What has to be excluded here is the
  // separators and the invisibles — `/`, `\`, `:`, the fullwidth solidus, an
  // RTL override — and not one of those is a letter, a mark or a number in
  // any script.
  const cleaned = name.replace(/[^\p{L}\p{M}\p{N}_.-]+/gu, "-").replace(/-{2,}/g, "-");
  // An extension is a dot with SOMETHING after it and not much: a `.` deep
  // inside a long name is part of the name, and a trailing one is not an
  // extension at all — it is also illegal on Windows.
  const dot = cleaned.lastIndexOf(".");
  const tail = cleaned.length - dot;
  const ext = dot >= 0 && tail >= 2 && tail <= MAX_EXTENSION ? cleaned.slice(dot) : "";
  // THE STEM IS WHAT GETS STRIPPED AND WHAT GETS CUT, never the extension: a
  // name that loses its `.json` opens in the wrong application on every
  // desktop there is. Leading dots are what make `..` a traversal and `.turn`
  // a hidden file, and they can only ever be in the stem.
  const stem = (ext ? cleaned.slice(0, dot) : cleaned).replace(/^[-.]+/, "");
  if (!stem) return `download${ext}`;
  return cutToBytes(stem, MAX_FILENAME - bytes(ext)) + ext;
}

const encoder = new TextEncoder();

/** How many BYTES a string takes on a filesystem, which is what limits it. */
function bytes(s: string): number {
  return encoder.encode(s).length;
}

/**
 * Cut to a byte budget, on whole characters.
 *
 * `MAX_FILENAME` is a BYTE limit — every filesystem in the comment above
 * counts bytes — and `slice` counts UTF-16 code units, which are the same
 * thing only for ASCII. The sanitizer above deliberately keeps letters in
 * every script (a name of `日本語.json` must not come out as `json`), so the
 * gap is not hypothetical: 120 units of Japanese is 360 bytes, past ext4's 255
 * and well past eCryptfs's 143 — and what the browser does with a name over
 * the limit is its own business, which may include losing the extension.
 *
 * Iterated with `for…of`, which walks CODE POINTS rather than units: a cut
 * that lands between the halves of a surrogate pair leaves a lone surrogate,
 * which is not valid UTF-8 and reaches the disk as a replacement character.
 */
function cutToBytes(s: string, max: number): string {
  if (max <= 0) return "";
  if (bytes(s) <= max) return s;
  let out = "";
  let used = 0;
  for (const ch of s) {
    const n = bytes(ch);
    if (used + n > max) break;
    out += ch;
    used += n;
  }
  return out;
}
