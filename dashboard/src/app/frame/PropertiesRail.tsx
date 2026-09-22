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
 * four different things and they are not interchangeable:
 *
 *   - the row is DROPPED — this object has no such property at all (a task
 *     with no collaborators has no collaborators row);
 *   - the row is a DASH — the property exists and holds nothing (a task with
 *     no due date has a due date, unset);
 *   - the row says SOMETHING — the empty state means something a reader
 *     should know ("none — reflection uses the default");
 *   - THE WHOLE GROUP says something, in one line, because every property in
 *     it is absent — a heading, a hairline and four dashes cost a reader four
 *     lines to learn that nothing about this task is scheduled, and
 *     `whenAllAbsent` says it in one.
 *
 * So the caller says which, on all four, and there is no path by which
 * forgetting produces a plausible-looking wrong one: an absent `value` renders
 * the dash, a row that should vanish is not in the list, and a group with no
 * `whenAllAbsent` keeps every dash it has. The fourth case is deliberately NOT
 * a rule the rail applies by itself — a rail of annotation rows would collapse
 * to one sentence the day somebody cleared them, with nobody having decided
 * that.
 *
 * ONE ATTRIBUTION PER CHANGE, NOT PER ROW. A create sets six fields at once,
 * so six consecutive rows carried six identical copies of "agent-ceo · 21h ago
 * · turn ↗" — the same sentence written out six times under six values. A RUN
 * of consecutive properties sharing one (actor, instant, turn) draws ONE line,
 * naming what it covers, under the last of them. It is the rule the feed
 * already keeps for its rows: what is the same on every row under one heading
 * is said once.
 *
 * AND NEVER UNDER AN ABSENCE. Provenance under a dash claims the engine
 * recorded somebody setting nothing. The one exception is a change that
 * EMPTIED the field, which the log did witness — [SetBy.cleared] — and it
 * reads "cleared by" rather than "set by", because they are different events.
 */

import { Fragment, type ReactNode } from "react";
import { EmptyValue } from "@crewlethq/ui";
import { href } from "../router.tsx";
import { SetByLine, type SetBy } from "./ObjectHeader.tsx";

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
  /**
   * Said in place of the rows when every one of them is absent.
   *
   * THE FOURTH ABSENCE, and the caller's decision like the other three: a
   * group nobody has filled in anywhere is one fact, not N. Set it only where
   * the whole group genuinely means one thing when empty — "Nothing
   * scheduled: no dates, no estimate, no size" — never as a way of making a
   * rail shorter, which is what the per-row dash is already the honest answer
   * to.
   */
  whenAllAbsent?: string;
}

/**
 * NULL, UNDEFINED AND THE EMPTY STRING, and nothing else: `0` and `false` are
 * values a property can legitimately hold, and a truthiness test would render
 * both as "nothing set".
 */
function absent(value: ReactNode): boolean {
  return value === null || value === undefined || value === "";
}

/** One by-line and the properties it speaks for. */
interface Attribution {
  setBy: SetBy;
  labels: string[];
}

/**
 * Where each group's by-lines go: one entry per property, filled only at the
 * LAST row of each run that shares an attribution.
 *
 * THE LAST rather than the first, because the line is a footnote to the values
 * above it — under the first row it would interrupt the run it describes.
 *
 * A ROW THAT DRAWS NO LINE BREAKS THE RUN rather than being passed over. A
 * line naming "Status and Type" drawn under Type, with a Priority between them
 * that it does not cover, is a claim a reader has to check; two shorter lines
 * are not.
 */
function attributions(properties: Property[]): (Attribution | undefined)[] {
  const out: (Attribution | undefined)[] = properties.map(() => undefined);
  let start = -1;
  let key = "";
  const close = (end: number) => {
    if (start < 0) return;
    out[end] = {
      setBy: properties[start]!.setBy!,
      labels: properties.slice(start, end + 1).map((p) => p.label),
    };
    start = -1;
    key = "";
  };
  properties.forEach((property, i) => {
    const line = drawnSetBy(property);
    if (!line) {
      close(i - 1);
      return;
    }
    // THE INSTANT, and the rendering beside it. `ago` is derived from `at`
    // wherever both are given, so it adds nothing there — but a caller may
    // hand over only the rendered form, and two rows reading "2h ago" and
    // "yesterday" are visibly two changes whatever the line sorted them by.
    // One line is drawn from the FIRST row of its run, so a key that let them
    // merge would put one row's time under both.
    const next = [line.actor, line.actorKind, line.at, line.ago, line.turnId, line.cleared].join(
      "\u0000",
    );
    if (start >= 0 && next === key) return;
    close(i - 1);
    start = i;
    key = next;
  });
  close(properties.length - 1);
  return out;
}

/** The attribution this row actually draws, which an absence suppresses. */
function drawnSetBy(property: Property): SetBy | undefined {
  if (!property.setBy) return undefined;
  if (absent(property.value) && !property.setBy.cleared) return undefined;
  return property.setBy;
}

export function PropertiesRail({ groups }: { groups: PropertyGroup[] }) {
  return (
    <dl className="props">
      {groups.map((group, i) => {
        const collapsed =
          group.whenAllAbsent !== undefined && group.properties.every((p) => absent(p.value));
        const lines = collapsed ? [] : attributions(group.properties);
        return (
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
            {/* THE HEADING STAYS. The group still exists and is still empty,
                and a section that vanished entirely would say this object has
                no plan rather than an unplanned one — which is the DROP case,
                and a different claim. A div rather than a `dd`, because a
                definition with no term is not a row of this list. */}
            {collapsed ? (
              <div className="props-empty">{group.whenAllAbsent}</div>
            ) : (
              group.properties.map((p, row) => (
                <Fragment key={p.label}>
                  <dt title={p.title} className={p.code ? "mono" : undefined}>
                    {p.label}
                  </dt>
                  <dd>
                    <Value property={p} />
                    {/* NEVER "set by —". A property nothing recorded a change
                        for renders no line at all: an em dash there would
                        claim the engine keeps a record it does not. And this
                        row draws the line only if it ENDS a run — see
                        [attributions]. */}
                    {lines[row] && (
                      <SetByLine
                        setBy={lines[row]!.setBy}
                        fields={lines[row]!.labels}
                        className="props-setby"
                      />
                    )}
                  </dd>
                </Fragment>
              ))
            )}
          </Fragment>
        );
      })}
    </dl>
  );
}

function Value({ property }: { property: Property }) {
  const { value, path, query } = property;
  if (absent(value)) {
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
