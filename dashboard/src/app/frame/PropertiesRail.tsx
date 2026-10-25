/**
 * An object's own facts, as labelled rows beside it, in the engine's own
 * vocabulary.
 *
 * THE DRAWING IS `@crewlethq/ui`'s `PropertyList`: one term column down every
 * section, the four kinds of absence (a row left out, a dash, a sentence of
 * the caller's, a whole section said in one line), one by-line per change
 * under the last row it set, and never a by-line under a value that is not
 * there. Its suite holds those rules. What stays here is the TRANSLATION the
 * package cannot do, because it is about this product's data and routes, as
 * `ObjectHeader.tsx` does for the header beside it:
 *
 *  - a value's link is a route (`path` and `query`), turned into an href;
 *  - who set a value names the turn that did it by id, and the turn's page is
 *    a route of ours;
 *  - which CHANGE set it is the change log's (the actor's kind and the
 *    instant), so two edits by one seat that both read "2h ago" keep a
 *    by-line each.
 */

import type { ReactNode } from "react";
import { PropertyList, type PropertyListHeadings, type PropertyListItem } from "@crewlethq/ui";
import { href } from "../router.tsx";
import { kitSetBy, type SetBy } from "./ObjectHeader.tsx";

export interface Property {
  label: string;
  /** Absent renders as a dash: the property exists and holds nothing. */
  value?: ReactNode;
  /** A link the value carries, in place of the value being one. */
  path?: string[];
  query?: Record<string, string>;
  /** Who last changed it, when, and in which turn. */
  setBy?: SetBy;
  /** Said on hover, where a label needs a sentence a rail has no room for. */
  title?: string;
  /**
   * The LABEL is an IDENTIFIER rather than a word (an environment variable's
   * name, a tag's key) and wears the mono face. A value that is an identifier
   * is the caller's to set: `<span className="mono">`.
   */
  identifierLabel?: boolean;
}

export interface PropertyGroup {
  /** The section heading. Empty runs the rows on without one. */
  name?: string;
  properties: Property[];
  /**
   * Said in place of the rows when every one of them is absent. The caller's
   * decision, set only where the whole group genuinely means one thing when
   * empty. See the kit's `PropertyListGroup.whenAllAbsent`.
   */
  whenAllAbsent?: string;
}

function kitProperty(property: Property): PropertyListItem {
  const { setBy } = property;
  return {
    label: property.label,
    value: property.value,
    href: property.path ? href(property.path, property.query) : undefined,
    setBy: setBy ? kitSetBy(setBy) : undefined,
    change: setBy ? [setBy.actorKind ?? "", setBy.at ?? ""].join(" ") : undefined,
    title: property.title,
    identifierLabel: property.identifierLabel,
  };
}

export function PropertiesRail({
  groups,
  headings,
}: {
  groups: PropertyGroup[];
  /** `sentence` for a column whose own sections are set in sentence case. */
  headings?: PropertyListHeadings;
}) {
  return (
    <PropertyList
      headings={headings}
      groups={groups.map((group) => ({
        name: group.name,
        whenAllAbsent: group.whenAllAbsent,
        items: group.properties.map(kitProperty),
      }))}
    />
  );
}
