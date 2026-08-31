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

import type { CSSProperties, ReactNode } from "react";
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
  onClick?: () => void;
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
}: {
  children: ReactNode;
  tone?: Tone;
  icon?: IconName;
  dot?: boolean;
  outline?: boolean;
  mono?: boolean;
  title?: string;
}) {
  return (
    <span
      className={cx("badge", tone !== "neutral" && tone, outline && "outline", mono && "mono")}
      title={title}
    >
      {dot && <i className={cx("dot", tone !== "neutral" && tone)} />}
      {icon && <Icon name={icon} size="xs" />}
      {children}
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

export function Segmented<T extends string>({
  value,
  options,
  onChange,
  size,
  ariaLabel,
}: {
  value: T;
  options: { value: T; label: ReactNode; icon?: IconName; title?: string }[];
  onChange: (value: T) => void;
  size?: "sm";
  ariaLabel: string;
}) {
  return (
    <div className={cx("segmented", size === "sm" && "sm")} role="tablist" aria-label={ariaLabel}>
      {options.map((o) => (
        <button
          key={o.value}
          role="tab"
          aria-selected={o.value === value}
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

export function Tabs<T extends string>({
  value,
  options,
  onChange,
  ariaLabel,
}: {
  value: T;
  options: { value: T; label: ReactNode; icon?: IconName; count?: number | null }[];
  onChange: (value: T) => void;
  ariaLabel: string;
}) {
  return (
    <div className="tabs" role="tablist" aria-label={ariaLabel}>
      {options.map((o) => (
        <button
          key={o.value}
          role="tab"
          aria-selected={o.value === value}
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

export function Meter({
  used,
  max,
  label,
  right,
  tone,
}: {
  used: number;
  max: number;
  label?: ReactNode;
  right?: ReactNode;
  tone?: "accent" | "positive" | "caution" | "critical" | "neutral";
}) {
  const pct = max > 0 ? Math.min(100, (used / max) * 100) : 0;
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
        role="meter"
        aria-valuenow={used}
        aria-valuemin={0}
        aria-valuemax={max}
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
}: {
  label: ReactNode;
  value: ReactNode;
  unit?: ReactNode;
  sub?: ReactNode;
  icon?: IconName;
}) {
  return (
    <div className="stat">
      <div className="stat-label">
        {icon && <Icon name={icon} size="xs" />}
        {label}
      </div>
      <div className="stat-value truncate">
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

export function Code({ children, plain }: { children: ReactNode; plain?: boolean }) {
  return <pre className={cx("code", plain && "plain")}>{children}</pre>;
}
