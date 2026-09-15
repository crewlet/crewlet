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
