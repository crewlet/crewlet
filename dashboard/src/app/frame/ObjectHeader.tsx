/**
 * The header of an object (on its page, in a peek and in a hover card), in
 * the engine's own vocabulary.
 *
 * THE DRAWING IS `@crewlethq/ui`'s: `ObjectHeader`, `FactLine` and
 * `SetByLine`, with their rules on the order a reader scans, the two-line
 * title, the fact line's two bands and the one wording of provenance. What
 * stays here is the TRANSLATION the package cannot do, because it is about
 * this product's data and routes:
 *
 *  - a glyph is named (`icon: GlyphName`, the prop every screen fills in), and
 *    the name to drawing lookup is the icons registry's `NamedGlyph`;
 *  - a fact's link is a route (`path` and `query`), turned into an href here;
 *  - who set a value names the turn that did it by id, and the turn's page is
 *    a route of ours.
 *
 * # A HEADER AND A RAIL STACKED IN ONE COLUMN ARE ONE READING
 *
 * `facts` is the PAGE's slot. In a peek the header and a properties rail sit
 * in one column, and there the same facts twice is the same answer given twice
 * before the description has been reached, so a peek whose body is a
 * properties rail passes NO facts. The caller decides: plenty of peeks have no
 * rail under them, and for those the fact line is the only place the object's
 * own values are stated. See `routes/worklet/WorkItem.tsx` for the worked case.
 */

import type { ReactElement, ReactNode } from "react";
import {
  FactLine as KitFactLine,
  ObjectHeader as KitObjectHeader,
  SetByLine as KitSetByLine,
  type ObjectFact,
  type SetBy as KitSetBy,
} from "@crewlethq/ui";
import type { GlyphName } from "@crewlethq/icons/glyphs";
import { href } from "../router.tsx";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";

/** Who last changed a value, as the engine's change log records it. */
export interface SetBy {
  actor: string;
  actorKind?: "agent" | "human" | "operator" | "system";
  turnId?: string;
  at?: string;
  /** Rendered as the relative time; the caller formats it. */
  ago?: string;
  /**
   * The change this names EMPTIED the field rather than filling it: "nobody
   * set a due date" and "ada took the due date off yesterday" are different
   * facts about the same blank cell, and only the second is one the change
   * log witnessed.
   */
  cleared?: boolean;
}

/** The provenance, with the turn that set it turned into its page. */
export function kitSetBy(setBy: SetBy): KitSetBy {
  return {
    actor: setBy.actor,
    ago: setBy.ago,
    cleared: setBy.cleared,
    turnHref: setBy.turnId ? href(["live", "turns", setBy.turnId]) : undefined,
  };
}

/** Who set a value, in the one wording the whole product uses. See uilet's `SetByLine`. */
export function SetByLine({
  setBy,
  fields,
  className,
}: {
  setBy: SetBy;
  /** The properties this one line covers, when it covers more than its own. */
  fields?: string[];
  className: string;
}) {
  return <KitSetByLine setBy={kitSetBy(setBy)} fields={fields} className={className} />;
}

export interface Fact {
  label: string;
  value: ReactNode;
  /** A link the value carries, as a route. */
  path?: string[];
  query?: Record<string, string>;
  setBy?: SetBy;
  /** Where this value came from, for a fact a reader can reasonably doubt. See uilet's `ObjectFact.note`. */
  note?: string;
  /** A stacked reading, drawn whole and never clamped. See uilet's `ObjectFact.whole`. */
  whole?: boolean;
  /** One token, kept on one line across two tracks. See uilet's `ObjectFact.token`. */
  token?: boolean;
  /** A set of chips, across two tracks and never clamped. See uilet's `ObjectFact.set`. */
  set?: boolean;
}

function kitFact(fact: Fact): ObjectFact {
  return {
    label: fact.label,
    value: fact.value,
    href: fact.path ? href(fact.path, fact.query) : undefined,
    setBy: fact.setBy ? kitSetBy(fact.setBy) : undefined,
    note: fact.note,
    whole: fact.whole,
    token: fact.token,
    set: fact.set,
  };
}

export function FactLine({ facts }: { facts: Fact[] }) {
  return <KitFactLine facts={facts.map(kitFact)} />;
}

export function ObjectHeader({
  kind,
  icon,
  mark,
  identifier,
  within,
  title,
  status,
  facts,
  description,
  actions,
  size = "page",
}: {
  /** The eyebrow: what kind of thing this is. */
  kind: string;
  /** The eyebrow's glyph: a name from the glyph set, or a drawn glyph (an artifact's own icon). */
  icon?: GlyphName | ReactElement;
  /** The object's own mark beside its title: a vendor's, a product's. See uilet's `ObjectHeader.mark`. */
  mark?: ReactNode;
  /** The key, handle or id, in the mono face. */
  identifier?: string;
  /** WHOSE the object is, where that is part of its identity and is a name rather than a key. */
  within?: string;
  title: ReactNode;
  /** The object's state: glyphs or pills, never identity. Several is the ordinary case. */
  status?: ReactNode;
  facts?: Fact[];
  /** One line under the title, as the object describes itself. */
  description?: ReactNode;
  actions?: ReactNode;
  size?: "page" | "peek";
}) {
  return (
    <KitObjectHeader
      kind={kind}
      icon={typeof icon === "string" ? <NamedGlyph name={icon} size="xs" /> : icon}
      mark={mark}
      description={description}
      identifier={identifier}
      within={within}
      title={title}
      status={status}
      facts={facts?.map(kitFact)}
      actions={actions}
      size={size}
    />
  );
}
