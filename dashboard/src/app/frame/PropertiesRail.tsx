/**
 * An object's own facts, as labelled rows beside it.
 *
 * TWO COMPONENTS DID THIS. `KeyValue` was a flat `<dl>` with no sections, used
 * by the event, the run, the turn and the seat; the work item had its own
 * `work-props` with sections, used by nothing else. They had different label
 * widths, different gaps and different rules about an absent value, so the
 * same kind of panel read differently depending on which screen it was on —
 * and neither could say WHO set a fact.
 *
 * THAT LAST PART IS THE ONE THIS PRODUCT ACTUALLY NEEDS. The objects here are
 * mostly written by agents, and "in progress" is a different fact from "moved
 * to in progress by ada, eleven minutes ago, in turn ↗" — the arrow being
 * the same `arrow_outward` the peek rail's own Open draws. `ObjectHeader` has
 * carried [SetBy] since it was written, because a header shows the same six
 * facts; a rail shows thirty, and it is the thirty that a reader asks "who
 * did that" about.
 *
 * AN ABSENT VALUE IS A DECISION, not a default. A property nothing set can be
 * three different things and they are not interchangeable:
 *
 *   - the row is DROPPED — this object has no such property at all (a task
 *     with no collaborators has no collaborators row);
 *   - the row is a DASH — the property exists and holds nothing (a task with
 *     no due date has a due date, unset);
 *   - the row says SOMETHING — the empty state means something a reader
 *     should know ("none — reflection uses the default").
 *
 * So the caller says which, and there is no path by which forgetting produces
 * a plausible-looking wrong one: an absent `value` renders the dash, and a row
 * that should vanish is not in the list.
 */

import { Fragment, type ReactNode } from "react";
import { EmptyValue } from "@crewlethq/ui";
import { ArrowOutwardGlyph } from "@crewlethq/icons/glyphs";
import { href } from "../router.tsx";
import type { SetBy } from "./ObjectHeader.tsx";

export interface Property {
  label: string;
  /** Absent renders as a dash — the property exists and holds nothing. */
  value?: ReactNode;
  /** A link the value carries, in place of the value being one. */
  path?: string[];
  query?: Record<string, string>;
  /** Who last changed it, when, and in which turn. */
  setBy?: SetBy;
  /** Said on hover, where a label needs a sentence a rail has no room for. */
  title?: string;
  /**
   * The label is an IDENTIFIER rather than a word — an environment variable's
   * name, a tag's key — and wears the mono face.
   *
   * A flag rather than a `ReactNode` label, because the label is also this
   * row's React key and its accessible name: a node can be neither.
   */
  code?: boolean;
}

export interface PropertyGroup {
  /** The section heading. Empty runs the rows on without one. */
  name?: string;
  properties: Property[];
}

export function PropertiesRail({ groups }: { groups: PropertyGroup[] }) {
  return (
    <dl className="props">
      {groups.map((group, i) => (
        // A KEYED FRAGMENT, so every row of every group is a DIRECT CHILD of
        // the one grid — which is what keeps the labels in a single column
        // down the whole rail instead of restarting it per section.
        //
        // These were `display: contents` wrappers, which looks like the same
        // thing and is not: it removes the BOX, never the node, so the rail's
        // own rules stopped matching. `.props > dd` is what cancels the user
        // agent's 40px indent on a definition, and it was not applying to a
        // single value in the product; `.props-section:first-child` is what
        // drops the separator above the rail's FIRST heading, and under a
        // wrapper per group it was true of every heading, so no group ever
        // drew its hairline. A fragment has no node to get in the way.
        <Fragment key={group.name ?? i}>
          {group.name && <div className="props-section">{group.name}</div>}
          {group.properties.map((p) => (
            <Fragment key={p.label}>
              <dt title={p.title} className={p.code ? "mono" : undefined}>
                {p.label}
              </dt>
              <dd>
                <Value property={p} />
                {/* NEVER "set by —". A fact nothing recorded a change for
                    renders no line at all: an em dash there would claim the
                    engine keeps a record it does not. */}
                {p.setBy && (
                  <span className="props-setby">
                    {p.setBy.actor}
                    {p.setBy.ago ? ` · ${p.setBy.ago}` : ""}
                    {p.setBy.turnId && (
                      <>
                        {" · "}
                        <a className="t-link" href={href(["activity", "turns", p.setBy.turnId])}>
                          turn <ArrowOutwardGlyph size="xs" />
                        </a>
                      </>
                    )}
                  </span>
                )}
              </dd>
            </Fragment>
          ))}
        </Fragment>
      ))}
    </dl>
  );
}

function Value({ property }: { property: Property }) {
  const { value, path, query } = property;
  // NULL, UNDEFINED AND THE EMPTY STRING, and nothing else: `0` and `false`
  // are values a property can legitimately hold, and a truthiness test here
  // would render both as "nothing set".
  if (value === null || value === undefined || value === "") {
    return <EmptyValue label="Not set" />;
  }
  if (path) {
    return (
      <a className="t-link" href={href(path, query)}>
        {value}
      </a>
    );
  }
  return <>{value}</>;
}
