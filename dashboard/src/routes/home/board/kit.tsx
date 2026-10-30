/**
 * What a section of the dashboard needs to offer widgets for Home: the
 * contribution API, and the card every contributed widget is drawn in.
 *
 * A SECTION OFFERS ITS OWN WIDGETS. Each one lives beside the screen it
 * summarises, in a `*.widgets.tsx` file exporting `widgets = defineWidgets([...])`,
 * and Home finds every such file at build time (`registry.tsx`). A section
 * added next year offers widgets by adding that one file: nothing in Home, the
 * tray or the board's storage changes, and a reader sees the new widgets in
 * the tray under the section's own name.
 *
 * A WIDGET NAMES THE PAGE IT COMES FROM (`from`, a key of the navigation's
 * `DESTINATIONS`), and takes everything it does not need to say from there:
 * the tray category it is listed under, the access that opens it, and where
 * its card's Open link goes. So a widget can never be readable by someone its
 * page refuses, and the access rule lives in one place, the navigation.
 *
 * A WIDGET READS, IT DOES NOT ADMINISTER. It answers "is anything wrong or
 * waiting?" with the same queries its page asks, and links to the page for
 * anything that writes; a form or a step-up never moves onto Home.
 */

import type { ComponentType, ReactNode } from "react";
import type { GlyphProps } from "@crewlethq/icons/glyphs";
import { Card, type NodeHue } from "@crewlethq/ui";
import {
  DESTINATIONS,
  WORKSPACES,
  closedBy,
  serviceShown,
  type Destination,
  type Service,
  type ServicesRun,
} from "~/app/nav.ts";
import type { Grant } from "~/lib/access.ts";
import { href, useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { missingFrom } from "~/components/CoverageNote.tsx";
import { INCOMPLETE_NOTE } from "~/components/work.tsx";
import type { QueryErrorCode } from "~/contract/errors.ts";
import type { Coverage } from "~/contract/coverage.ts";
import type { LogRefusal, QueryRefusal } from "~/protocol/index.ts";
import { DEFAULT_RANGE, rangeOf } from "~/routes/home/model.ts";

/** One choice a widget's setting offers: a saved view, a project, a seat. */
export interface WidgetOption {
  value: string;
  label: string;
}

export interface WidgetChoices {
  options: readonly WidgetOption[];
  loading: boolean;
  /** Said when there is nothing to choose from: "No saved views yet." */
  empty?: string;
  /**
   * Said beside the choices when they are not every one there is: the
   * question that lists them stopped at its page ("Only the first 200
   * projects are offered."). A reader looking for one that is not offered is
   * told why rather than left to conclude it does not exist.
   */
  partial?: string;
}

/**
 * Something the reader chooses for one copy of a widget: which saved view it
 * lists, which project it follows. Its choices are read live, by a hook, so a
 * picker asks the engine the same question the section's own screen asks.
 */
export interface WidgetParam {
  key: string;
  label: string;
  helper?: string;
  useChoices: () => WidgetChoices;
}

export interface WidgetProps {
  /** The reader's choices for this copy, by `WidgetParam.key`; checked against the choices before use. */
  params: Readonly<Record<string, string>>;
}

export interface WidgetSize {
  /** Columns of twelve it is added at. */
  w: number;
  /** The narrowest it reads at. */
  minW: number;
  /** The shortest it reads at, in rows of 16px, when its height is set. */
  minH: number;
  /** Whether a reader may set its height (a list may scroll; a figure may not). */
  vertical: boolean;
}

export interface WidgetDef {
  /**
   * `<workspace>.<name>`: `settings.backups`. Every board that holds the widget
   * stores it, so a key a release has shipped is never renamed.
   */
  key: string;
  /** The page it summarises, a key of `DESTINATIONS`: `settings-backups`. */
  from: string;
  label: string;
  /** One short line on what it shows, for the tray. */
  description: string;
  /**
   * The line where what it shows depends on the services the company runs:
   * Figures draws no task figures without Worklet, and a line that
   * promised them described a widget the reader will not get
   * ([widgetDescription]). Absent, [description] holds everywhere.
   */
  describe?: (runs: ServicesRun) => string;
  icon: ComponentType<GlyphProps>;
  /** Its tint in the tray. Decorative, per widget. */
  hue: NodeHue;
  size: WidgetSize;
  /** At most one on a board: a card drawn twice is one fact drawn twice. */
  once?: boolean;
  /**
   * The service built into Crewlet this widget reads where the page it
   * summarises reads none (Home's task chart reads Worklet's flow, the
   * Inbox's updates its notices), for its settings where they decide it (a
   * figure, a page shortcut). Where the company does not run it the widget is
   * left off the board and out of the tray ([widgetShown]), and drawn again
   * once it does. A widget whose page is hidden goes with the page.
   */
  service?: (params: Readonly<Record<string, string>>) => Service | undefined;
  /**
   * What the widget READS beyond its page, where that is narrower than the
   * page: Integrations is every reader's, and its widget counts the connected
   * apps, which is an Admin's question. A reader holding none of these is not
   * offered it ([canUse]). Absent, the page's own access decides.
   */
  grants?: readonly Grant[];
  params?: readonly WidgetParam[];
  /** What this copy is called, when its params name it: "Waiting on your decision". */
  nameOf?: (params: Readonly<Record<string, string>>) => string | undefined;
  Component: ComponentType<WidgetProps>;
}

const KEY = /^[a-z]+(\.[a-z0-9-]+)+$/;
const PARAM = /^[a-z][a-z0-9-]{0,30}$/;

/** The page a widget names. */
export function destinationOf(from: string): Destination | undefined {
  return DESTINATIONS.find((d) => d.key === from);
}

/**
 * The service a widget cannot draw without that the company does not run,
 * if any: its page's ([serviceShown]), then the one it reads itself.
 */
export function widgetMissing(
  def: Pick<WidgetDef, "from" | "service">,
  params: Readonly<Record<string, string>>,
  runs: ServicesRun,
): Service | undefined {
  const dest = destinationOf(def.from);
  if (dest !== undefined && !serviceShown(dest, runs)) return dest.service;
  const service = def.service?.(params);
  return service !== undefined && !runs[service] ? service : undefined;
}

/** What the tray says a widget shows, where the company runs `runs`. */
export function widgetDescription(
  def: Pick<WidgetDef, "description" | "describe">,
  runs: ServicesRun,
): string {
  return def.describe?.(runs) ?? def.description;
}

/** Whether a widget has anything to show where the company runs `runs` ([widgetMissing]). */
export function widgetShown(
  def: Pick<WidgetDef, "from" | "service">,
  params: Readonly<Record<string, string>>,
  runs: ServicesRun,
): boolean {
  return widgetMissing(def, params, runs) === undefined;
}

/**
 * Whether a reader holding `grants` may open a page: nothing closes its
 * ADDRESS, by the one rule the frame's refusal, the sidebar's locks and the
 * palette read (`closedBy` in `nav.ts`) — the workspace's permissions first,
 * then the section's. A destination carries no grants of its own to read, and
 * a second rule here would offer a widget whose Open meets the frame's refusal.
 */
export function pageOpens(dest: Destination, grants: readonly string[]): boolean {
  return closedBy(dest.path, grants) === null;
}

/** A page's name with its workspace's, for a list of every page: "Settings › Backups & retention". */
export function pageName(dest: Destination): string {
  const row = WORKSPACES.find((w) => w.key === dest.workspace);
  return row && row.label !== dest.label ? `${row.label} › ${dest.label}` : dest.label;
}

/**
 * Every contribution refused so far, in words. A suite asserts it is empty,
 * so a widget that names a page the navigation does not have fails the build;
 * at run time the refused widget is left out and the rest of Home draws.
 */
export const WIDGET_PROBLEMS: string[] = [];

/** What is wrong with one contribution, or nothing. */
function problemsOf(d: WidgetDef): string[] {
  const problems: string[] = [];
  const dest = destinationOf(d.from);
  if (!KEY.test(d.key)) problems.push(`${d.key}: a key is <workspace>.<name>, lowercase.`);
  if (!dest) problems.push(`${d.key}: "${d.from}" is not a page in the navigation.`);
  else if (!d.key.startsWith(`${dest.workspace}.`))
    problems.push(`${d.key}: its key starts with its page's workspace, "${dest.workspace}.".`);
  const { w, minW, minH } = d.size;
  if (!(minW >= 2 && minW <= w && w <= 12)) problems.push(`${d.key}: 2 <= minW <= w <= 12.`);
  if (!(minH >= 2 && minH <= 40)) problems.push(`${d.key}: minH is between 2 and 40 rows.`);
  const keys = new Set<string>();
  for (const p of d.params ?? []) {
    if (!PARAM.test(p.key) || keys.has(p.key))
      problems.push(`${d.key}: param "${p.key}" is a unique lowercase key.`);
    keys.add(p.key);
  }
  return problems;
}

/**
 * A section's widgets, checked as the module loads. ONE BAD CONTRIBUTION
 * COSTS ONLY ITSELF: a widget that fails its check is reported and left out,
 * rather than a throw that would take every widget, and Home, down with it.
 */
export function defineWidgets(defs: WidgetDef[]): readonly WidgetDef[] {
  return defs.filter((d) => {
    const problems = problemsOf(d);
    if (problems.length === 0) return true;
    WIDGET_PROBLEMS.push(...problems);
    console.error(`Home widget left out:\n${problems.join("\n")}`);
    return false;
  });
}

/**
 * The card a contributed widget is drawn in: the shipped Home card, with the
 * widget's name and a link to the page it summarises.
 *
 * A widget that IS a card already (a shipped `SeatCard`, a `StatGroup` of
 * tiles) draws itself without this one: a card inside a card is a double
 * border.
 */
export function WidgetCard({
  title,
  from,
  query,
  actions,
  children,
}: {
  title: ReactNode;
  /** The page it summarises; its Open link goes there. */
  from: string;
  /** What the Open link narrows that page to, as the widget does: `{ blocked: "1" }`. */
  query?: Record<string, string>;
  /** Controls for the header, in place of the Open link. */
  actions?: ReactNode;
  children: ReactNode;
}) {
  const dest = destinationOf(from);
  return (
    <Card padding="none" className="home-card home-widget">
      <Card.Header
        actions={
          actions ??
          (dest ? (
            // "Open" alone, named in full for a screen reader: a page's label
            // after a verb reads oddly ("Open Asked of me").
            <a
              className="t-link home-widget-open"
              href={href(dest.path, query)}
              aria-label={`Open ${pageName(dest)}`}
            >
              Open
            </a>
          ) : null)
        }
      >
        <Card.Title as="h3">{title}</Card.Title>
      </Card.Header>
      <div className="home-widget-body">{children}</div>
    </Card>
  );
}

/**
 * Home's range (Today, 7 days, 30 days), for a widget with a time axis: the
 * page's one range control moves every such widget at once.
 */
export function useHomeRange() {
  const [value] = useParam("range", DEFAULT_RANGE);
  return rangeOf(value);
}

/** A widget's note in place of its content. */
export function WidgetNote({ children }: { children: ReactNode }) {
  return <p className="home-card-note">{children}</p>;
}

/**
 * The note a widget draws when its choice is missing or no longer exists (a
 * saved view deleted, a seat taken off the chart): one sentence for every
 * widget, saying where the choice is made.
 */
export function ChoiceNote({ thing }: { thing: string }) {
  return <WidgetNote>{`Choose a ${thing} in Edit Home.`}</WidgetNote>;
}

// ---------------------------------------------------------------------------
// What an answer could not account for
// ---------------------------------------------------------------------------

/**
 * The flags an answer carries about what it could not account for — the two
 * every widget's questions share.
 */
export interface AnswerGaps {
  /**
   * False when the read met records this build cannot decode (the tracker's,
   * the pages' and chat's answers), so rows may be missing and a count taken
   * over them may be short. Only an explicit false is a gap: an answer that
   * does not say is not one that said "incomplete".
   */
  complete?: boolean;
  /** Which nodes a fleet answer was assembled from (ADR-0021). */
  coverage?: Coverage | null;
}

/** What a widget with no rows says over an answer that could not account for everything. */
export const NOTHING_TO_LIST = "Nothing to list";

/** A figure's second line over an answer that could not account for everything. */
export const PARTIAL_FIGURE = "May be incomplete";

/**
 * What the answers a widget draws from could not account for, one sentence
 * per kind of gap, each said once however many answers share it.
 *
 * THE ONE PLACE A WIDGET LEARNS IT MAY BE WRONG. Every widget reads the
 * answers its page reads, and those answers say when they are short: a
 * record this build cannot decode met the read (`complete: false`), or a node
 * did not answer a fleet read (`coverage`). Each widget reading those flags on
 * its own was a chance per widget to miss one, and every one of them did:
 * "You're caught up" was drawn over an inbox that said it could not account
 * for everything, and "No turn failed" over a fleet read a node never
 * answered. A widget's own reasons (a muted room, a list cut at its bound) go
 * beside these, through [WidgetQuery]'s `gaps`.
 */
export function gapsOf(answers: readonly (AnswerGaps | null | undefined)[]): string[] {
  const lines: string[] = [];
  if (answers.some((a) => a?.complete === false)) lines.push(INCOMPLETE_NOTE);
  const { nodes, incomplete } = missingFrom(answers.map((a) => a?.coverage));
  if (nodes.length === 1) lines.push(`Node ${nodes[0]!.id} didn’t answer, so this may be short.`);
  else if (nodes.length > 1)
    lines.push(`${nodes.length} nodes didn’t answer, so this may be short.`);
  else if (incomplete) lines.push("The list of nodes couldn’t be read, so this may be short.");
  return lines;
}

/** What a widget claims when it has nothing to list: "You're caught up". */
export interface EmptyClaim {
  title: ReactNode;
  hint: ReactNode;
  /**
   * `"none"` where the widget's card already names it with a title of its
   * own, so the claim is not a second heading beside that title.
   */
  headingLevel?: "none";
}

/**
 * Every widget's answer, drawn: the refusal or the wait its page would draw,
 * its rows, and what the answers could not account for.
 *
 * A WIDGET NEVER CLAIMS "NOTHING" OVER AN ANSWER THAT COULD NOT ACCOUNT FOR
 * EVERYTHING. Its `empty` is the claim it makes when there is nothing to list
 * ("You're caught up", "No turn failed in the last 24 hours"), and that claim
 * is drawn only when `gaps` is empty: with a gap and no rows the card says
 * there is nothing to list and why it may be wrong, and with a gap and rows it
 * draws the rows and says the gap under them. So the rule is decided here,
 * once, for every widget, rather than remembered in each.
 *
 * `gaps` IS REQUIRED, from [gapsOf] over every answer the widget draws from
 * and the widget's own lines: a widget whose answers carry no flag passes an
 * empty list, which is a decision written down rather than a flag forgotten.
 * Every widget module draws its answers through this and never through
 * `QueryState` or `EmptyState` directly (`kit.test.tsx` holds them to it).
 */
export function WidgetQuery({
  error,
  refusal,
  detail,
  loading,
  empty,
  gaps,
  children,
}: {
  error: QueryErrorCode | string | null;
  refusal?: QueryRefusal | LogRefusal | null;
  detail?: string;
  loading: boolean;
  /**
   * What the widget says when there is nothing to list, given only when that
   * is so: the claim it makes.
   */
  empty?: EmptyClaim;
  /** What the answers could not account for: [gapsOf], and the widget's own lines. */
  gaps: readonly string[];
  children?: ReactNode;
}) {
  const state = { error, refusal, detail, loading };
  if (empty !== undefined) {
    if (gaps.length > 0) {
      return (
        <QueryState
          {...state}
          empty={{
            title: NOTHING_TO_LIST,
            hint: gaps.join(" "),
            headingLevel: empty.headingLevel,
          }}
        />
      );
    }
    return <QueryState {...state} empty={empty} />;
  }
  return (
    <QueryState {...state}>
      {children}
      {gaps.map((line) => (
        <p key={line} className="home-card-note">
          {line}
        </p>
      ))}
    </QueryState>
  );
}
