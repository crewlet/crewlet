/**
 * The component library.
 *
 * Each of these is a recipe from `styles/components.css` given a typed props
 * surface. The reason they are components rather than class names a screen
 * remembers to spell: the previous dashboard hand-wrote 97 template strings
 * carrying `data-k` keys and `data-action` names, and its shell carried a
 * branch for one screen's rows because that screen forgot to handle its own
 * clicks. A typed prop cannot be forgotten.
 */

import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent,
  type MouseEvent,
  type ReactNode,
} from "react";
import { Icon, type IconName } from "./Icon.tsx";

export type Tone = "neutral" | "positive" | "caution" | "critical" | "info" | "accent";

export function cx(...parts: (string | false | null | undefined)[]): string {
  return parts.filter(Boolean).join(" ");
}

// ---------------------------------------------------------------------------
// Surface
// ---------------------------------------------------------------------------

export function Panel({
  title,
  subtitle,
  icon,
  count,
  actions,
  children,
  padding = "normal",
  className,
  style,
}: {
  title?: ReactNode;
  subtitle?: ReactNode;
  icon?: IconName;
  count?: number | null;
  actions?: ReactNode;
  children?: ReactNode;
  padding?: "normal" | "tight" | "none";
  className?: string;
  style?: CSSProperties;
}) {
  return (
    <section className={cx("panel-flush", className)} style={style}>
      {(title || actions) && (
        <header className="panel-head">
          <div className="panel-title truncate">
            {icon && <Icon name={icon} size="sm" style={{ color: "var(--text-muted)" }} />}
            <span className="truncate">{title}</span>
            {count != null && <span className="count-chip">{count}</span>}
          </div>
          {subtitle && <span className="panel-sub truncate">{subtitle}</span>}
          <span className="spacer" />
          {actions}
        </header>
      )}
      <div
        className={cx("panel-body", padding === "tight" && "tight", padding === "none" && "none")}
      >
        {children}
      </div>
    </section>
  );
}

// ---------------------------------------------------------------------------
// Controls
// ---------------------------------------------------------------------------

export function Button({
  children,
  icon,
  variant = "default",
  size = "md",
  onClick,
  disabled,
  title,
  type = "button",
  block,
  active,
}: {
  children?: ReactNode;
  icon?: IconName;
  variant?: "default" | "primary" | "ghost" | "danger";
  size?: "md" | "sm";
  /**
   * The event is passed through, so a button inside an element with a
   * default action of its own can stop it. A Copy button in a `<summary>`
   * toggled the disclosure on its way through, giving an operator who only
   * wanted the text the whole block they were copying instead.
   */
  onClick?: (event: MouseEvent<HTMLButtonElement>) => void;
  disabled?: boolean;
  title?: string;
  type?: "button" | "submit";
  block?: boolean;
  active?: boolean;
}) {
  return (
    <button
      type={type}
      className={cx(
        "btn",
        variant !== "default" && variant,
        size === "sm" && "sm",
        !children && "icon",
        block && "block",
      )}
      onClick={onClick}
      disabled={disabled}
      title={title}
      aria-label={!children ? title : undefined}
      aria-pressed={active}
    >
      {icon && <Icon name={icon} size={size === "sm" ? "xs" : "sm"} />}
      {children}
    </button>
  );
}

export function Badge({
  children,
  tone = "neutral",
  icon,
  dot,
  outline,
  mono,
  title,
  onClick,
  pressed,
}: {
  children: ReactNode;
  tone?: Tone;
  icon?: IconName;
  dot?: boolean;
  outline?: boolean;
  mono?: boolean;
  title?: string;
  /** Makes the badge a real button — see the note below. */
  onClick?: () => void;
  /** For a badge that toggles a filter, whether that filter is on. */
  pressed?: boolean;
}) {
  const className = cx(
    "badge",
    tone !== "neutral" && tone,
    outline && "outline",
    mono && "mono",
    onClick && "actionable",
    pressed && "pressed",
  );
  const inner = (
    <>
      {dot && <i className={cx("dot", tone !== "neutral" && tone)} />}
      {icon && <Icon name={icon} size="xs" />}
      {children}
    </>
  );
  // A BUTTON when it acts, a span when it does not. A count that filters the
  // list has to be reachable by keyboard and announce its pressed state; a
  // span with a click handler is neither, and looks identical to the inert
  // badges beside it.
  if (onClick) {
    return (
      <button
        type="button"
        className={className}
        title={title}
        onClick={onClick}
        aria-pressed={!!pressed}
      >
        {inner}
      </button>
    );
  }
  return (
    <span className={className} title={title}>
      {inner}
    </span>
  );
}

/**
 * A phase mark.
 *
 * Phase is the one categorical identity the product spends colour on outside a
 * chart, because it is what a reader follows across the Model, Seat, Activity
 * and Trace screens. The three hues are measured to stay separable under
 * protan and deutan vision, and the word is always present beside the colour.
 */
export function PhaseTag({ phase, children }: { phase: string; children?: ReactNode }) {
  const key = (phase || "").toLowerCase();
  return (
    <span className="phase-tag" data-phase={key}>
      {children ?? (key || "—")}
    </span>
  );
}

/**
 * One tab stop for a whole group, and the arrow keys that move within it.
 *
 * BOTH `Segmented` and `Tabs` need it, and it is the half each of them was
 * missing. A radio group and a tab list share one keyboard contract — the
 * group is ONE stop in the page's tab order, arrows move within it, Home and
 * End jump to the ends — and a group of plain buttons has the opposite
 * behaviour: every option is its own tab stop, and arrow keys do nothing. On
 * the density control in the shell that is three extra stops before a
 * keyboard reader reaches the page content, on every screen.
 *
 * ARROWS MOVE FOCUS AND COMMIT NOTHING, which is what the pattern calls
 * MANUAL activation, and it is the default here because of what these groups
 * are wired to. Seven of the nine sites drive a `useParam`: five push a
 * history entry and every one of them re-runs the screen's query, which
 * `socket.query` mints fresh with no cache, no dedupe and no coalescing. So
 * under selection-follows-focus, arrowing from the first option of Seat's tab
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
 * A RADIO GROUP, not a tab list. It used to declare `role="tablist"` with
 * `role="tab"` children, and not one of its call sites is a tab widget: they
 * pick a theme, a density, a grouping, a scope, a window and a kind. A tab
 * controls a `tabpanel` it is adjacent to and labels; these narrow, regroup or
 * re-scope what is already on the screen, and several of them sit in a screen
 * head with the content they affect hundreds of pixels below. Announcing them
 * as tabs promises a reader a panel relationship that does not exist, which is
 * a mis-role rather than a missing handler — and it would not have been fixed
 * by adding the keyboard behaviour alone.
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
  options: { value: T; label: ReactNode; icon?: IconName; title?: string }[];
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
  return (
    <div
      ref={box}
      className={cx("segmented", size === "sm" && "sm")}
      role="radiogroup"
      aria-label={ariaLabel}
      onKeyDown={onKeyDown}
    >
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
          {o.icon && <Icon name={o.icon} size="xs" />}
          {o.label}
        </button>
      ))}
    </div>
  );
}

/**
 * A tab list, and the one control here that genuinely is one: its options sit
 * directly above the panel each of them shows.
 */
export function Tabs<T extends string>({
  value,
  options,
  onChange,
  ariaLabel,
  activate = "manual",
}: {
  value: T;
  options: { value: T; label: ReactNode; icon?: IconName; count?: number | null }[];
  onChange: (value: T) => void;
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
  return (
    <div ref={box} className="tabs" role="tablist" aria-label={ariaLabel} onKeyDown={onKeyDown}>
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          data-roving
          role="tab"
          aria-selected={o.value === value}
          tabIndex={o.value === stop ? 0 : -1}
          onClick={() => onChange(o.value)}
        >
          {o.icon && <Icon name={o.icon} size="sm" />}
          {o.label}
          {o.count != null && <span className="count-chip">{o.count}</span>}
        </button>
      ))}
    </div>
  );
}

export function Chip({
  children,
  on,
  onClick,
  count,
  title,
}: {
  children: ReactNode;
  on?: boolean;
  onClick?: () => void;
  count?: number | null;
  title?: string;
}) {
  return (
    <button className="chip" aria-pressed={!!on} onClick={onClick} title={title}>
      {children}
      {count != null && <span className="chip-count">{count}</span>}
    </button>
  );
}

export function SearchInput({
  value,
  onChange,
  placeholder,
  ariaLabel,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  ariaLabel: string;
}) {
  return (
    <div className="search-input">
      <Icon name="search" size="sm" />
      <input
        className="input"
        type="search"
        value={value}
        placeholder={placeholder}
        aria-label={ariaLabel}
        onChange={(e) => onChange(e.target.value)}
      />
    </div>
  );
}

/**
 * A picker over a known set of values.
 *
 * Exists because the alternatives were both wrong for a bounded set. A free
 * text box that filters on EXACT equality — which is what a role filter does,
 * on the server and in memory alike — silently returns nothing the moment
 * somebody types a prefix, while looking exactly like a search that found no
 * matches. And a row of chips is one control per option, so it only works
 * while the set is small and quietly disappears when it is not.
 */
export function Select({
  value,
  onChange,
  options,
  ariaLabel,
  anyLabel = "Any",
}: {
  value: string;
  onChange: (v: string) => void;
  options: string[];
  ariaLabel: string;
  anyLabel?: string;
}) {
  return (
    <div className={cx("picker", value && "on")}>
      <select
        className="input"
        value={value}
        aria-label={ariaLabel}
        onChange={(e) => onChange(e.target.value)}
      >
        <option value="">{anyLabel}</option>
        {options.map((o) => (
          <option key={o} value={o}>
            {o}
          </option>
        ))}
      </select>
      <Icon name="chevronDown" size="xs" />
    </div>
  );
}

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

/**
 * A seat's monogram. Deliberately neutral — see the note in components.css.
 */
export function Avatar({
  name,
  size = "md",
  human,
}: {
  name: string;
  size?: "sm" | "md" | "lg";
  human?: boolean;
}) {
  const initials =
    (name || "?")
      .split(/[\s\-_.]+/)
      .filter(Boolean)
      .slice(0, 2)
      .map((w) => w[0])
      .join("") || "?";
  return (
    <span
      className={cx("avatar", size !== "md" && size, human && "human")}
      title={name}
      aria-hidden="true"
    >
      {initials}
    </span>
  );
}

// ---------------------------------------------------------------------------
// Measure
// ---------------------------------------------------------------------------

/**
 * How full something is, drawn as a bar.
 *
 * Two things about the ARIA here, both of which it got wrong.
 *
 * A `role="meter"` with no accessible name announces as "meter, 120000" and
 * nothing else — a number with no subject, on a screen that has several. The
 * visible legend is not the name: `label` is a ReactNode, several call sites
 * pass a percentage sentence rather than a noun, and two sites pass none at
 * all because the meter sits in a table cell whose column heading does the
 * naming for a sighted reader. So the name is a required prop of its own,
 * exactly as it is on [Segmented], [Tabs] and Select.
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
}: {
  used: number;
  max: number;
  label?: ReactNode;
  /** What this meter measures, as a bare noun phrase — "Company budget", not
      "94% used". The accessible name; see the note above. */
  ariaLabel: string;
  right?: ReactNode;
  tone?: "accent" | "positive" | "caution" | "critical" | "neutral";
}) {
  const scaled = max > 0;
  // CLAMPED AT BOTH ENDS, like `aria-valuenow` below. Only the top used to
  // be, so a negative reading rendered `width: -5%` — which CSSOM drops,
  // leaving a bar that silently keeps its previous width rather than reading
  // empty. The doc above promises clamping; this is the half that was not.
  const pct = scaled ? Math.max(0, Math.min(100, (used / max) * 100)) : 0;
  // The tone is DERIVED from the fill unless the caller overrides it, so a bar
  // that is nearly full says so without every call site remembering to.
  const auto = pct >= 100 ? "critical" : pct >= 75 ? "caution" : "accent";
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

export function StatRow({ cols, children }: { cols?: number; children: ReactNode }) {
  return (
    <div className="stat-row" style={{ "--stat-cols": cols ?? 4 } as CSSProperties}>
      {children}
    </div>
  );
}

export function Stat({
  label,
  value,
  unit,
  sub,
  icon,
  tone,
}: {
  label: ReactNode;
  value: ReactNode;
  unit?: ReactNode;
  sub?: ReactNode;
  icon?: IconName;
  /**
   * The STATE this number is in, when it has one — the ink of the value moves
   * to that tone's step.
   *
   * Deliberately absent from most stats. Colour carries state and never
   * identity, and a token count or an elapsed time is not in a state: tinting
   * every tile would spend the four status hues on decoration and leave the
   * one tile that means something indistinguishable from its neighbours. Use
   * it where the value IS an outcome — a turn's decision, a probe's verdict —
   * and nowhere else.
   */
  tone?: Exclude<Tone, "neutral" | "accent">;
}) {
  return (
    <div className="stat">
      <div className="stat-label">
        {icon && <Icon name={icon} size="xs" />}
        {label}
      </div>
      <div className={cx("stat-value truncate", tone && `tone-${tone}`)}>
        {value}
        {unit && <span className="unit">{unit}</span>}
      </div>
      <div className="stat-sub truncate">{sub}</div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// States
// ---------------------------------------------------------------------------

/**
 * An empty state that says WHY it is empty and what would fill it.
 *
 * `hint` is not optional politeness. "No events" on a company that has never
 * run and "no events" on a node with no event store are the same sentence and
 * completely different problems, and the reader cannot tell them apart from
 * the list.
 */
export function Empty({
  icon = "inbox",
  title,
  hint,
  action,
  inline,
}: {
  icon?: IconName;
  title: ReactNode;
  hint?: ReactNode;
  action?: ReactNode;
  inline?: boolean;
}) {
  return (
    <div className={cx("empty", inline && "inline")}>
      <Icon name={icon} size="xl" />
      <div className="empty-title">{title}</div>
      {hint && <div className="empty-sub">{hint}</div>}
      {action}
    </div>
  );
}

export function Banner({
  tone = "neutral",
  icon,
  children,
  action,
}: {
  tone?: "neutral" | "info" | "caution" | "critical";
  icon?: IconName;
  children: ReactNode;
  action?: ReactNode;
}) {
  const fallback: IconName = tone === "critical" ? "alert" : tone === "caution" ? "alert" : "info";
  return (
    <div className={cx("banner", tone)} role={tone === "critical" ? "alert" : undefined}>
      <Icon name={icon ?? fallback} size="sm" />
      <span style={{ flex: 1, minWidth: 0 }}>{children}</span>
      {action}
    </div>
  );
}

export function Skeleton({ rows = 3, height = 14 }: { rows?: number; height?: number }) {
  return (
    <div className="col" aria-busy="true" aria-live="polite">
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="skeleton" style={{ height, width: `${100 - (i % 3) * 12}%` }} />
      ))}
      <span className="sr-only">Loading</span>
    </div>
  );
}

export function KeyValue({ items }: { items: [ReactNode, ReactNode][] }) {
  return (
    <dl className="kv">
      {items.map(([k, v], i) => (
        <div key={i} style={{ display: "contents" }}>
          <dt>{k}</dt>
          <dd>{v}</dd>
        </div>
      ))}
    </dl>
  );
}

/**
 * A block of preformatted text, optionally one that OWNS select-all.
 *
 * `selectable` makes the block focusable and gives ⌘A / Ctrl+A a local
 * meaning while it has focus: select this block, not the document. That is
 * the one keyboard gesture a reader brings to a screen whose point is a
 * single record, and the browser's own answer to it — the nav, the stat row,
 * every phase card and the record — is never what they wanted.
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
   * What this block is.
   *
   * REQUIRED ON EVERY BLOCK, not only on a selectable one, and that is the
   * defect this replaced. `.code` is `overflow: auto` with a `max-height`, so
   * ANY block taller than 460px is a scroll container — and in Chrome and
   * Safari a scroll container is only reachable by keyboard if something
   * makes it focusable. The `selectable` ones were, because ⌘A needed it; the
   * others were not, and they are the tall ones: a phase card's verbatim
   * system prompt is tens of kilobytes, and a keyboard reader could not
   * scroll it at all. Taking focus without a name is the other half of that
   * trade — a tab stop a screen reader announces as nothing — so the two
   * arrive together or neither does.
   */
  label: string;
}

/**
 * A labelled section the reader opens.
 *
 * Lifted out of PhaseCard when a second screen needed one. The alternative was
 * a bare `<details>`, and a bare `<details>` is not the same control: it draws
 * the platform's own marker instead of the chevron every other expander here
 * uses, it takes none of the hover, inset or type of `.disclosure-head`, and
 * it cannot carry a count chip or a status mark. Two expanders that behave the
 * same and look different is the specific thing this component library exists
 * to stop.
 */
export function Disclosure({
  label,
  count,
  children,
  defaultOpen,
  mono,
  tone,
  mark,
  actions,
}: {
  label: ReactNode;
  count?: ReactNode;
  children: ReactNode;
  defaultOpen?: boolean;
  mono?: boolean;
  tone?: "reasoning";
  /** A status mark, rendered as its OWN item in the head's row. */
  mark?: ReactNode;
  /** Controls that belong to the section, kept OUT of the toggle. Rendered
      beside the head rather than inside it: a button nested in a button is
      not a thing, and clicking a copy control must not also collapse the
      thing it copied. */
  actions?: ReactNode;
}) {
  const [open, setOpen] = useState(!!defaultOpen);
  return (
    <div className={cx("disclosure", tone && `tone-${tone}`)}>
      <div className="disclosure-bar">
        <button className="disclosure-head" onClick={() => setOpen((v) => !v)} aria-expanded={open}>
          <Icon name={open ? "chevronDown" : "chevronRight"} size="xs" />
          {mark}
          <span className={cx("truncate", mono && "mono")}>{label}</span>
          {count != null && <span className="count-chip">{count}</span>}
        </button>
        {actions}
      </div>
      {open && <div className="disclosure-body">{children}</div>}
    </div>
  );
}

export function Code({ children, plain, selectable, label }: CodeProps) {
  const box = useRef<HTMLPreElement>(null);
  // WHETHER THIS BLOCK ACTUALLY SCROLLS, measured rather than assumed.
  //
  // A tab stop on every code block would put one in front of each of a phase
  // card's tool arguments — dozens on a long round — and most of them are
  // three lines that never overflow. So the stop is given to the blocks that
  // need it, which is a fact about the rendered box and not about any prop:
  // the same content overflows or does not depending on the viewport. Firefox
  // does this natively and Chrome and Safari do not, hence measuring.
  const [scrolls, setScrolls] = useState(false);
  useEffect(() => {
    const node = box.current;
    if (!node) return;
    // BOTH AXES: `plain` sets `white-space: pre`, so a wide line scrolls
    // sideways in a box that is not tall enough to scroll at all.
    const measure = () =>
      setScrolls(node.scrollHeight > node.clientHeight || node.scrollWidth > node.clientWidth);
    measure();
    // The box is resized by the window, by a Disclosure opening above it and
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
  icon: IconName;
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

  return (
    <span className="row gap-1">
      <Button
        size={size}
        variant={variant}
        icon={state === "done" ? "check" : state === "failed" ? "alert" : icon}
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
      icon="copy"
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
      icon="download"
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
  document.body.appendChild(field);
  try {
    field.select();
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    field.remove();
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
  return stem.slice(0, MAX_FILENAME - ext.length) + ext;
}
