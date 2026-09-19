/**
 * Eight facts about the company, on one line, above the Inbox.
 *
 * # Why the landing screen leads with this and not with the queue
 *
 * A queue-shaped home has one failure mode and it arrives on the first day: a
 * company where nothing is wrong renders as a blank page. The reader's first
 * contact with the product is then an empty box, which reads as a dashboard
 * that is broken rather than as a company that is fine — and the two are
 * indistinguishable from the outside, because an empty queue says nothing
 * about whether anything was ever asked.
 *
 * So the first fold is the COMPANY, and the queue is under it. Every figure
 * here is true of a healthy company and of a struggling one; none of them can
 * be zero because nothing is wrong. A reader who opens this on a quiet morning
 * sees four seats working, ninety-one open items and eleven overdue, and the
 * empty band below it then MEANS something: nothing is waiting on you, on a
 * company that is visibly running.
 *
 * # Every figure is a link, and every figure says where it came from
 *
 * A number with nowhere to go is a number a reader has to go and find again,
 * so each one navigates into the workspace that owns it. And each carries the
 * scope of its own claim in its title rather than in a footnote: `open` is
 * over the projects this answer returned, `tokens` is over the window the
 * engine pushed and NOT over "today" — a label this dashboard already got
 * wrong once, on the spend meter, by naming a window it was not given.
 *
 * # A figure that is not known yet is not a zero
 *
 * Every cell takes `number | null`, and null draws an em dash. A pulse strip
 * that renders 0 while its query is in flight tells a founder their company
 * has no open work, which is a false statement that corrects itself a second
 * later — and on a slow link, not for several.
 */

import type { ReactNode } from "react";
import { EmptyValue } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import {
  BlockGlyph,
  CalendarClockGlyph,
  GroupGlyph,
  ListGlyph,
  PauseGlyph,
  ScheduleGlyph,
  TokenGlyph,
  WarningGlyph,
} from "@crewlethq/icons/glyphs";

export interface PulseFact {
  key: string;
  icon: ReactNode;
  /** The figure. `null` is "not known yet" and draws an em dash, never a 0. */
  value: number | null;
  /** What the figure is: "open", "overdue". Lowercase — it is a noun, not a heading. */
  label: string;
  /** Where the figure came from and what it covers. Shown on hover. */
  title: string;
  path: string[];
  query?: Record<string, string>;
  /** Draws the figure in a status hue. Only ever for a count that IS a problem. */
  tone?: "caution" | "critical";
}

/**
 * The strip.
 *
 * ONE ROW THAT WRAPS rather than a grid: the eight facts are a sentence about
 * the company read left to right, and a grid would put `alarms` under `seats`
 * at one width and beside it at another, so the reading order would depend on
 * the viewport.
 */
export function Pulse({ facts }: { facts: PulseFact[] }) {
  return (
    <div className="pulse" role="group" aria-label="The company right now">
      {facts.map((f) => (
        <a key={f.key} className="pulse-fact" href={href(f.path, f.query)} title={f.title}>
          <span className="pulse-glyph">{f.icon}</span>
          {/* TABULAR NUMERALS AND A MINIMUM WIDTH, both in the stylesheet: this
              strip re-renders on every push, and a figure going from 9 to 10
              would otherwise shift the seven facts to its right by a pixel on
              each one. A number that twitches is a number a reader stops
              trusting. */}
          <strong className="pulse-value" data-tone={f.tone}>
            {f.value === null ? <EmptyValue label="Not counted" /> : f.value.toLocaleString()}
          </strong>
          <span className="pulse-label">{f.label}</span>
        </a>
      ))}
    </div>
  );
}

/** What the strip is made of, so the Inbox reads as a list of claims. */
export const PULSE_GLYPHS = {
  seats: <GroupGlyph size="xs" />,
  parked: <PauseGlyph size="xs" />,
  open: <ListGlyph size="xs" />,
  overdue: <ScheduleGlyph size="xs" />,
  blocked: <BlockGlyph size="xs" />,
  tokens: <TokenGlyph size="xs" />,
  alarms: <WarningGlyph size="xs" />,
} as const;
