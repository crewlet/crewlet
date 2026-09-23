/**
 * The header of an object — on its page, in a peek, and in a hover card.
 *
 * ONE COMPONENT, because the reader scans the same facts in the same order
 * wherever the object appears. A page that put status first and a peek that
 * put the assignee first would make the reader re-learn the object every time
 * it changed frame.
 *
 * A fact is `{label, value}` and may carry `setBy` — who last changed it, when
 * and in which turn. That line exists because this product's objects are
 * mostly written by agents: "in progress" is a different fact from "moved to
 * in progress by ada, eleven minutes ago, in turn ↗" — where the arrow is
 * the design system's own `arrow_outward`, the one drawing every "this leaves
 * the page" in the product is made of. [SetByLine] draws it, and the
 * properties rail draws the same component: one wording, one markup, and one
 * place to fix the glyph that used to fall off the end of it.
 *
 * # A HEADER AND A RAIL STACKED IN ONE COLUMN ARE ONE READING
 *
 * `facts` is the PAGE's slot. On a page the properties rail sits BESIDE the
 * header, in a side column, so the header's line and the rail's rows are two
 * readings a reader chooses between — the line to scan in a second, the rows
 * to study. In a peek both are in ONE 420px column, the rail a hundred pixels
 * under the header, and there the same facts twice is not a second reading:
 * it is the same answer given twice before the description has been reached.
 * So a peek whose body is a properties rail passes NO facts, and the rail
 * states every property once. See `routes/work/WorkItem.tsx` for the worked
 * case.
 *
 * THE CALLER DECIDES, and this component does not drop `facts` on `size`.
 * Plenty of peeks here have no rail under them at all — a unit, a revision, a
 * schedule — and for those the fact line is the only place the object's own
 * values are ever stated. A component that swallowed them by size would take
 * a rule about one frame's BODY and apply it to every frame's header.
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
  /**
   * The change this names EMPTIED the field rather than filling it.
   *
   * The one case in which provenance belongs under a value that is not there.
   * "Nobody has set a due date" and "ada took the due date off yesterday" are
   * different facts about the same blank cell, and only the second one is
   * something the change log actually witnessed — see [SetByLine], which is
   * what turns it into "cleared by" instead of "set by".
   */
  cleared?: boolean;
}

/**
 * Who set a value, in the one wording the whole product uses.
 *
 * TWO COPIES OF THIS EXISTED — the fact line's said "set by ada · 2h ago ·
 * turn ↗" and the properties rail's said "ada · 2h ago · turn ↗", the same
 * provenance phrased two ways on ONE screen, where a bare handle under a
 * value reads as who wrote the ROW rather than as who set the field. Written
 * twice a wording drifts with nothing to catch it, which is the shape
 * `textcut` and `api/httpjson` were each written to end.
 *
 * `fields` is how a RUN of properties shares one line: the rail coalesces
 * consecutive rows set by one actor in one turn at one instant and names them
 * here ("Status, Priority and Type · set by …"), because a create sets six
 * fields at once and six identical by-lines under six rows is the same
 * sentence copied out six times. It is the rule
 * [A row is not a row](../../../../docs/reference/dashboard-design.md) already
 * states for a feed: rows under one heading do not each repeat the actor.
 */
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
  return (
    <span className={className}>
      {fields && fields.length > 1 && <>{listOf(fields)} · </>}
      {setBy.cleared ? "cleared by " : "set by "}
      {setBy.actor}
      {setBy.ago ? ` · ${setBy.ago}` : ""}
      {setBy.turnId && (
        <>
          {" · "}
          {/* THE LINK IS ONE TOKEN, and `.setby-turn` is what keeps it one:
              the glyph is a `display: block` svg by the base reset, and a
              block box inside an inline anchor splits the anchor around it,
              so the arrow landed on a line of its own at EVERY width. A
              `white-space` cannot suppress that — `.t-link` already carries
              one — and only a formatting context of the anchor's own can. */}
          <a className="t-link setby-turn" href={href(["activity", "turns", setBy.turnId])}>
            turn <ArrowOutwardGlyph size="xs" />
          </a>
        </>
      )}
    </span>
  );
}

/**
 * "Status", "Status and Priority", "Status, Priority and Type".
 *
 * BY HAND RATHER THAN `Intl.ListFormat`, which is locale-keyed: `en-US`
 * writes the Oxford comma and `en-GB` does not, so the string this product
 * renders would depend on the reader's browser and no test could name it.
 */
function listOf(names: string[]): string {
  if (names.length <= 1) return names[0] ?? "";
  return `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
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
              record it does not. A fact with no VALUE never reaches here —
              the filter above drops it — so `cleared` has no worked case on
              this line and one on the rail's, which keeps its row. */}
          {fact.setBy && <SetByLine setBy={fact.setBy} className="fact-setby" />}
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
  /**
   * The object's state — glyphs or pills, never identity.
   *
   * MORE THAN ONE IS THE ORDINARY CASE, which is what the row below is built
   * for now: a turn is running, or a retry, or carrying failures, or read to
   * the store's cap, and any two of those can be true at once. They used to
   * be a screen's `PageActions`, in a slot whose subject is what the reader
   * can DO — see `routes/activity/Turn.tsx`'s `turnStatus`.
   */
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
      {/* AND THE HEAD ROW WRAPS. It holds a heading, the object's state marks
          and the frame's actions, and the first of those is the only one that
          can be shortened without saying something else — so on a narrow
          header the marks take a line under the title rather than every pill
          on the row giving up a few characters each. */}
      <div className="row wrap">
        {/* TWO LINES, AND THAT IS THE CEILING. Every other object in the
            product heads itself with a NAME — a seat, a work key, a node id, a
            page title — and a turn has none, so `routes/activity/Turn.tsx`
            heads it with the lead sentence of the reviewer's own prose.
            `lead` bounds the ordinary case; this bounds the one it
            deliberately does not, a summary written as a single unpunctuated
            clause, which no sentence rule can shorten and which set three
            lines of `--fs-xl` semibold above the facts, pushing them off a
            laptop's first screen.

            `.clamp` rather than `.truncate`: one line cuts most real titles,
            which run a little over, and two hold them and catch only the
            paragraph. Two is the clamp's own default, so nothing here sets a
            count. */}
        <h1 className="object-title clamp">{title}</h1>
        {/* ONE GROUP, so the marks travel together when the row above wraps
            and never split a running turn's pill from its problem count. */}
        {status && <div className="row gap-1 wrap object-status">{status}</div>}
        <span className="spacer" />
        {actions && <div className="row gap-1 wrap">{actions}</div>}
      </div>
      {facts && <FactLine facts={facts} />}
    </header>
  );
}
