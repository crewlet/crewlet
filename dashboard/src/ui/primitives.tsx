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

import { cx } from "@crewlethq/ui";
import {
  type AriaAttributes,
  type CSSProperties,
  type ComponentType,
  type KeyboardEvent,
  type MouseEvent,
  type ReactNode,
  type Ref,
  useCallback,
  useEffect,
  useRef,
  useState,
} from "react";
import {
  type GlyphProps,
  CheckGlyph,
  ChevronRightGlyph,
  ContentCopyGlyph,
  ErrorGlyph,
  InboxGlyph,
  InfoGlyph,
  KeyboardArrowDownGlyph,
  SearchGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";

export type Tone = "neutral" | "positive" | "caution" | "critical" | "info" | "accent";

// ---------------------------------------------------------------------------
// Surface
// ---------------------------------------------------------------------------

export function Panel({
  title,
  subtitle,
  icon: Glyph,
  count,
  actions,
  children,
  padding = "normal",
  className,
  style,
}: {
  title?: ReactNode;
  subtitle?: ReactNode;
  icon?: ComponentType<GlyphProps>;
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
            {Glyph && <Glyph size="sm" style={{ color: "var(--color-text-tertiary)" }} />}
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

/**
 * What a Button hands to its `<button>` beyond its own props.
 *
 * A menu trigger needs `aria-haspopup`, `aria-expanded`, a ref for returning
 * focus and a `tabIndex` of -1 when the keyboard reaches its actions another
 * way; a list's Move button needs a ref and a name longer than its tooltip.
 * Without these, each of them hand-wrote `.btn` markup beside the primitive,
 * and a class list spelled in two places is how the two drift apart.
 *
 * DELIBERATELY NOT `className` OR `style`: the variants are the vocabulary,
 * and a caller that can restyle the recipe is a caller that will. Nor
 * `aria-pressed`, whose one spelling is `active`.
 */
type ButtonPassthrough = Omit<AriaAttributes, "aria-pressed"> & {
  ref?: Ref<HTMLButtonElement>;
  id?: string;
  tabIndex?: number;
  onKeyDown?: (event: KeyboardEvent<HTMLButtonElement>) => void;
  /** A test hook, or a state the stylesheet reads. */
  [data: `data-${string}`]: string | number | boolean | undefined;
};

type ButtonVariant = "default" | "primary" | "ghost" | "danger";
type ButtonSize = "md" | "sm";

/**
 * The `.btn` recipe, spelled once for every element drawn as a button.
 *
 * An icon-only control takes the square `icon` shape, so it is decided from
 * whether there is anything to read beside the glyph.
 */
function buttonClass({
  variant,
  size,
  iconOnly,
  block,
}: {
  variant: ButtonVariant;
  size: ButtonSize;
  iconOnly: boolean;
  block?: boolean;
}): string {
  return cx(
    "btn",
    variant !== "default" && variant,
    size === "sm" && "sm",
    iconOnly && "icon",
    block && "block",
  );
}

export function Button({
  children,
  icon: Glyph,
  variant = "default",
  size = "md",
  onClick,
  disabled,
  title,
  type = "button",
  block,
  active,
  ...forwarded
}: ButtonPassthrough & {
  children?: ReactNode;
  icon?: ComponentType<GlyphProps>;
  variant?: ButtonVariant;
  size?: ButtonSize;
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
      {...forwarded}
      type={type}
      className={buttonClass({ variant, size, iconOnly: !children, block })}
      onClick={onClick}
      disabled={disabled}
      title={title}
      // An icon button is named by its tooltip unless the caller names it
      // more precisely: "Move responsibility 2 of 3 up" where the tooltip
      // only says "Move up".
      aria-label={forwarded["aria-label"] ?? (!children ? title : undefined)}
      aria-pressed={active}
    >
      {Glyph && <Glyph size={size === "sm" ? "xs" : "sm"} />}
      {children}
    </button>
  );
}

/**
 * A link drawn as a button: it goes somewhere rather than doing something.
 *
 * An action that navigates has to be a real anchor, so it can be opened in a
 * tab, copied and read as a link by a screen reader. It used to be a
 * hand-written `<a className="btn ...">` at each such place, which is the
 * recipe spelled beside the primitive that owns it. `external` opens it in a
 * new tab and withholds the referrer and the opener, which a link to a
 * vendor's site must never be without.
 */
export function ButtonLink({
  href,
  children,
  icon: Glyph,
  variant = "default",
  size = "md",
  block,
  title,
  external,
}: {
  href: string;
  children?: ReactNode;
  icon?: ComponentType<GlyphProps>;
  variant?: ButtonVariant;
  size?: ButtonSize;
  block?: boolean;
  title?: string;
  external?: boolean;
}) {
  return (
    <a
      className={buttonClass({ variant, size, iconOnly: !children, block })}
      href={href}
      title={title}
      aria-label={!children ? title : undefined}
      target={external ? "_blank" : undefined}
      rel={external ? "noreferrer" : undefined}
    >
      {Glyph && <Glyph size={size === "sm" ? "xs" : "sm"} />}
      {children}
    </a>
  );
}

export function Badge({
  children,
  tone = "neutral",
  icon: Glyph,
  dot,
  outline,
  mono,
  title,
  onClick,
  pressed,
}: {
  children: ReactNode;
  tone?: Tone;
  icon?: ComponentType<GlyphProps>;
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
      {Glyph && <Glyph size="xs" />}
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
 * The id of the tab that labels a tab panel, so a panel and the tab row that
 * controls it agree on it without passing ids between them.
 */
export function tabId(panelId: string, value: string): string {
  return `${panelId}-tab-${value}`;
}

/**
 * Which button an arrow key moves to in a row of them, or null for a key that
 * is not a move. Tabs are a horizontal row and move on Left and Right only; a
 * radio group moves on all four arrows, as the platform's own does.
 */
function moveTo(key: string, at: number, count: number, vertical: boolean): number | null {
  if (count === 0) return null;
  switch (key) {
    case "ArrowRight":
      return (at + 1) % count;
    case "ArrowLeft":
      return (at - 1 + count) % count;
    case "ArrowDown":
      return vertical ? (at + 1) % count : null;
    case "ArrowUp":
      return vertical ? (at - 1 + count) % count : null;
    case "Home":
      return 0;
    case "End":
      return count - 1;
    default:
      return null;
  }
}

/**
 * Roving focus over a row of buttons: one tab stop for the row, arrows to move
 * within it. `select` is called on a move only when the row selects as focus
 * moves (a radio group); a tab row leaves selection to Enter and Space, which
 * a button already turns into its click.
 */
function useRoving(count: number, select: ((index: number) => void) | null) {
  const buttons = useRef<(HTMLButtonElement | null)[]>([]);
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>, vertical: boolean) => {
    const at = buttons.current.findIndex((b) => b === document.activeElement);
    if (at < 0) return;
    const next = moveTo(e.key, at, count, vertical);
    if (next === null) return;
    e.preventDefault();
    buttons.current[next]?.focus();
    select?.(next);
  };
  return { buttons, onKeyDown };
}

type SegmentedSemantics =
  /**
   * A row of SECTIONS: a lens or a tab, which pushes a history entry. Arrows
   * move focus and Enter or Space selects, so walking the row with the
   * keyboard does not leave an entry per keypress for Back to walk through.
   * `panelId` names the tab panel the row controls; render it with
   * [TabPanel].
   */
  | { semantics: "tabs"; panelId: string }
  /**
   * A SETTING or a filter (the theme, the density): announced as a radio
   * group with no panel, and arrows select immediately, because a choice that
   * replaces the history entry or is not in the URL at all costs nothing to
   * change on every keypress.
   */
  | { semantics: "radio"; panelId?: undefined };

export function Segmented<T extends string>({
  value,
  options,
  onChange,
  size,
  ariaLabel,
  semantics,
  panelId,
}: {
  value: T;
  options: { value: T; label: ReactNode; icon?: ComponentType<GlyphProps>; title?: string }[];
  onChange: (value: T) => void;
  size?: "sm";
  ariaLabel: string;
} & SegmentedSemantics) {
  const radio = semantics === "radio";
  const { buttons, onKeyDown } = useRoving(
    options.length,
    radio ? (i) => onChange(options[i]!.value) : null,
  );
  const selected = Math.max(
    0,
    options.findIndex((o) => o.value === value),
  );
  return (
    <div
      className={cx("segmented", size === "sm" && "sm")}
      role={radio ? "radiogroup" : "tablist"}
      aria-label={ariaLabel}
      onKeyDown={(e) => onKeyDown(e, radio)}
    >
      {options.map((o, i) => (
        <button
          key={o.value}
          ref={(el) => {
            buttons.current[i] = el;
          }}
          type="button"
          role={radio ? "radio" : "tab"}
          id={radio || !panelId ? undefined : tabId(panelId, o.value)}
          aria-checked={radio ? o.value === value : undefined}
          aria-selected={radio ? undefined : o.value === value}
          aria-controls={radio ? undefined : panelId}
          // An icon-only option is named by its title, which a tooltip alone
          // does not do for a screen reader.
          aria-label={o.title && !o.label ? o.title : undefined}
          tabIndex={i === selected ? 0 : -1}
          title={o.title}
          onClick={() => onChange(o.value)}
        >
          {o.icon && <o.icon size="xs" />}
          {o.label}
        </button>
      ))}
    </div>
  );
}

/**
 * The in-page sections a screen owns, as the ARIA tabs pattern with MANUAL
 * activation: arrows move focus along the row, Enter or Space selects. A tab
 * is a section and pushes a history entry, and a row that selected on every
 * arrow press pushed one per keypress. `panelId` names the [TabPanel] the row
 * controls.
 */
export function Tabs<T extends string>({
  value,
  options,
  onChange,
  ariaLabel,
  panelId,
}: {
  value: T;
  options: {
    value: T;
    label: ReactNode;
    icon?: ComponentType<GlyphProps>;
    count?: number | null;
  }[];
  onChange: (value: T) => void;
  ariaLabel: string;
  panelId: string;
}) {
  const { buttons, onKeyDown } = useRoving(options.length, null);
  const selected = Math.max(
    0,
    options.findIndex((o) => o.value === value),
  );
  return (
    <div
      className="tabs"
      role="tablist"
      aria-label={ariaLabel}
      onKeyDown={(e) => onKeyDown(e, false)}
    >
      {options.map((o, i) => (
        <button
          key={o.value}
          ref={(el) => {
            buttons.current[i] = el;
          }}
          type="button"
          role="tab"
          id={tabId(panelId, o.value)}
          aria-selected={o.value === value}
          aria-controls={panelId}
          tabIndex={i === selected ? 0 : -1}
          onClick={() => onChange(o.value)}
        >
          {o.icon && <o.icon size="sm" />}
          {o.label}
          {o.count != null && <span className="count-chip">{o.count}</span>}
        </button>
      ))}
    </div>
  );
}

/**
 * The region a tab row controls, labelled by whichever tab is selected. It
 * keeps the gap of the layout it sits in, so wrapping a screen's sections in
 * one changes nothing a reader can see.
 */
export function TabPanel({
  id,
  value,
  children,
}: {
  id: string;
  value: string;
  children: ReactNode;
}) {
  return (
    <div role="tabpanel" id={id} aria-labelledby={tabId(id, value)} className="tab-panel">
      {children}
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
      <SearchGlyph size="sm" />
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
      <KeyboardArrowDownGlyph size="xs" />
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
  icon: Glyph,
  tone,
}: {
  label: ReactNode;
  value: ReactNode;
  unit?: ReactNode;
  sub?: ReactNode;
  icon?: ComponentType<GlyphProps>;
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
        {Glyph && <Glyph size="xs" />}
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
type CodeProps = { children: ReactNode; plain?: boolean } & (
  | {
      /** Take ⌘A / Ctrl+A while focused, and take focus. */
      selectable: true;
      /**
       * What this block is. REQUIRED with `selectable`, not optional beside
       * it: a focusable `role="region"` with no accessible name is a tab stop
       * a screen reader announces as nothing, which is worse than the plain
       * block it replaced. The union is what stops the two drifting apart —
       * a typed prop cannot be forgotten.
       */
      label: string;
    }
  | { selectable?: false; label?: never }
);

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
          {open ? <KeyboardArrowDownGlyph size="xs" /> : <ChevronRightGlyph size="xs" />}
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
      tabIndex={selectable ? 0 : undefined}
      role={selectable ? "region" : undefined}
      aria-label={selectable ? label : undefined}
      onKeyDown={selectable ? onKeyDown : undefined}
    >
      {children}
    </pre>
  );
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
  /**
   * The text, or a THUNK that produces it.
   *
   * The thunk is not a convenience: on a live turn the thing worth copying is
   * assembled from every phase and every streamed frame, and `agents` is
   * pushed twice per tool round — so a string prop means a full
   * `JSON.stringify` of the whole turn on every push, for a button nobody has
   * clicked. Resolved on click, it costs nothing until it is asked for.
   */
  text: string | (() => string);
  label?: string;
  title?: string;
  variant?: "default" | "ghost";
  size?: "md" | "sm";
}) {
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  // The timeout outlives the component otherwise, and a screen left during
  // the two seconds after a copy sets state on something unmounted.
  useEffect(() => () => clearTimeout(timer.current), []);

  const onClick = useCallback(() => {
    void copyToClipboard(typeof text === "function" ? text() : text).then((ok) => {
      setState(ok ? "copied" : "failed");
      clearTimeout(timer.current);
      timer.current = setTimeout(() => setState("idle"), 2000);
    });
  }, [text]);

  const said =
    state === "copied"
      ? "copied to the clipboard"
      : state === "failed"
        ? "the browser refused the clipboard"
        : "";

  return (
    <span className="row gap-1">
      <Button
        size={size}
        variant={variant}
        icon={state === "copied" ? CheckGlyph : state === "failed" ? ErrorGlyph : ContentCopyGlyph}
        onClick={onClick}
        title={state === "failed" ? "the browser refused the clipboard" : title}
      >
        {state === "copied" ? "Copied" : state === "failed" ? "Copy failed" : label}
      </Button>
      {/* Announced, not just drawn: the icon swap is the only signal a
          sighted reader gets, and a screen reader gets none of it.

          A SIBLING of the button, never a child. A button's accessible name
          is computed from its contents, so inside it this named the control
          "Copied copied to the clipboard" — the status text becoming part of
          what the button claims to be. */}
      <span className="sr-only" role="status">
        {said}
      </span>
    </span>
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
