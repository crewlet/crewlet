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
 * in progress by ada, eleven minutes ago, in turn ↗" — where the arrow is
 * the design system's own `arrow_outward`, the one drawing every "this leaves
 * the page" in the product is made of.
 */

import type { ReactNode } from "react";
import { ArrowOutwardGlyph } from "@crewlethq/icons/glyphs";
import { href } from "../router.tsx";
import { cx } from "@crewlethq/ui";
// AN OBJECT'S EYEBROW MARK IS NAME-KEYED — `icon: IconName` is the prop every
// screen and every peek fills in — so the name→drawing lookup stays in
// `~/ui/Icon.tsx` and moves all of them onto uilet's glyphs in one place.
import { Mark, type MarkName } from "~/ui/glyph.tsx";

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
  /**
   * WHERE THIS VALUE CAME FROM, for a fact a reader can reasonably doubt.
   *
   * Distinct from [SetBy], which names WHO changed a recorded field. A note
   * answers the other question: whether the number was MEASURED by the engine
   * or DERIVED by this page, and what it covers. A turn's duration is the
   * worked case — the engine's own milliseconds where a turn record carries
   * them, the span of the events this frame holds where it does not, and the
   * two differ by however much of the turn fell outside the read.
   *
   * Only for facts where the answer is not obvious. A fact with a note on
   * every value is a fact line nobody reads.
   */
  note?: string;
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
          {/* A NOTE AND A `setBy` ARE NOT EXCLUSIVE, and the note comes
              first: it qualifies the value directly above it, where "set by"
              is about a person and reads as a footnote to both. */}
          {fact.note && <span className="fact-note">{fact.note}</span>}
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
                    turn <ArrowOutwardGlyph size="xs" />
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
  icon?: MarkName;
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
        {icon && <Mark name={icon} size="xs" />}
        <span>{kind}</span>
        {/* NOT `InlineCode`: this is a key sitting inside an eyebrow, on the
            eyebrow's own type step and ink, and their inline code is a chip —
            a tinted box with a boundary — which puts a second surface inside a
            line that is already the quietest thing on the screen. The mono
            face is the whole of what this needs, and `.object-id` is it. */}
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
