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
// Controls
// ---------------------------------------------------------------------------

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
