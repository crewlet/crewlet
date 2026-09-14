/**
 * The header of an object — on its page, in a peek, and in a hover card.
 *
 * ONE COMPONENT, because the reader scans the same six facts in the same order
 * wherever the object appears. A page that put status first and a peek that
 * put the assignee first would make the reader re-learn the object every time
 * it changed frame.
 *
 * A fact is `{label, value}` and may carry `setBy` — who last changed it, when
 * and in which turn. That line exists because this product's objects are
 * mostly written by agents: "in progress" is a different fact from "moved to
 * in progress by ada, eleven minutes ago, in turn ↗".
 */

import type { ReactNode } from "react";
import { href } from "../router.tsx";
import { Icon, type IconName } from "~/ui/Icon.tsx";
import { cx } from "~/ui/primitives.tsx";

export interface SetBy {
  actor: string;
  actorKind?: "agent" | "human" | "operator" | "system";
  turnId?: string;
  at?: string;
  /** Rendered as the relative time; the caller formats it. */
  ago?: string;
}

export interface Fact {
  label: string;
  value: ReactNode;
  /** A link the value carries. */
  path?: string[];
  query?: Record<string, string>;
  setBy?: SetBy;
}

export function FactLine({ facts }: { facts: Fact[] }) {
  const shown = facts.filter((f) => f.value !== null && f.value !== undefined && f.value !== "");
  if (shown.length === 0) return null;
  return (
    <div className="fact-line">
      {shown.map((fact) => (
        <span key={fact.label} className="fact">
          <span className="fact-label">{fact.label}</span>
          <span className="fact-value truncate">
            {fact.path ? (
              <a className="t-link" href={href(fact.path, fact.query)}>
                {fact.value}
              </a>
            ) : (
              fact.value
            )}
          </span>
          {/* NEVER "set by —". A fact nothing recorded a change for renders
              no line at all: an em dash there would claim the engine keeps a
              record it does not. */}
          {fact.setBy && (
            <span className="fact-setby">
              set by {fact.setBy.actor}
              {fact.setBy.ago ? ` · ${fact.setBy.ago}` : ""}
              {fact.setBy.turnId && (
                <>
                  {" · "}
                  <a className="t-link" href={href(["activity", "turns", fact.setBy.turnId])}>
                    turn ↗
                  </a>
                </>
              )}
            </span>
          )}
        </span>
      ))}
    </div>
  );
}

export function ObjectHeader({
  kind,
  icon,
  identifier,
  title,
  status,
  facts,
  actions,
  size = "page",
}: {
  /** The eyebrow: what kind of thing this is. */
  kind: string;
  icon?: IconName;
  /** The key, handle or id, in the mono face. */
  identifier?: string;
  title: ReactNode;
  /** A status glyph or pill — state, never identity. */
  status?: ReactNode;
  facts?: Fact[];
  actions?: ReactNode;
  size?: "page" | "peek";
}) {
  return (
    <header className={cx("object-head", size === "peek" && "peek")}>
      <div className="object-eyebrow">
        {icon && <Icon name={icon} size="xs" />}
        <span>{kind}</span>
        {identifier && <span className="mono object-id">{identifier}</span>}
      </div>
      <div className="row">
        <h1 className="object-title">{title}</h1>
        {status}
        <span className="spacer" />
        {actions && <div className="row gap-1 wrap">{actions}</div>}
      </div>
      {facts && <FactLine facts={facts} />}
    </header>
  );
}
