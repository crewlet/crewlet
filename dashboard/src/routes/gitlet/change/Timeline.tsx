/**
 * The pieces a change's conversation is drawn with, as a code host draws one:
 * a rail down the side, every record on it in the order the records landed,
 * what somebody WROTE a card — who, what they did and when across its head,
 * what they said under it — and what somebody DID (a revision pushed) a line
 * of its own on the rail, with its glyph.
 *
 * NOTHING HERE IS COLOURED: a review's verdict is a judgement, never a state
 * of the change (`ChangeList.tsx`'s reviews say why), and a revision pushed
 * is a fact. A face is the seat's own (`ui.tsx`).
 */

import type { ReactNode } from "react";
import { Card } from "@crewlethq/ui";
import "~/styles/gitlet.css";

/** The rail: an ordered list, each record one item on it. */
export function TimelineList({ label, children }: { label: string; children: ReactNode }) {
  return (
    <ol className="code-timeline" aria-label={label}>
      {children}
    </ol>
  );
}

/**
 * What somebody wrote, as a card on the rail: its head says who did what and
 * when (a `Byline`), with anything the record carries beside it (its verdict,
 * a resolved mark); its body is what they said; its foot what a reader does
 * with it.
 */
export function TimelineCard({
  head,
  marks,
  label,
  children,
  foot,
}: {
  /** Who did what and when: the record's `Byline`. */
  head: ReactNode;
  /** What the record carries beside its head, at its end. */
  marks?: ReactNode;
  /** What a screen reader calls the card. */
  label: string;
  children?: ReactNode;
  foot?: ReactNode;
}) {
  return (
    <li className="code-timeline__item">
      <Card as="article" padding="none" className="code-tl-card" aria-label={label}>
        <div className="code-tl-card__head">
          {head}
          {marks && <span className="code-tl-card__marks">{marks}</span>}
        </div>
        {children && <div className="code-tl-card__body">{children}</div>}
        {foot && <div className="code-tl-card__foot">{foot}</div>}
      </Card>
    </li>
  );
}

/**
 * What somebody did, as a line on the rail: its glyph on the rail itself, then
 * the sentence — who did it, when, and what it touched.
 */
export function TimelineEvent({ glyph, children }: { glyph: ReactNode; children: ReactNode }) {
  return (
    <li className="code-timeline__item code-tl-event">
      <span className="code-tl-event__glyph" aria-hidden="true">
        {glyph}
      </span>
      <span className="code-tl-event__said">{children}</span>
    </li>
  );
}
