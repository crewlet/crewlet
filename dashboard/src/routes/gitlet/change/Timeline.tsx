/**
 * The pieces a change's conversation is drawn with, as a code host draws one:
 * a rail down the side, every record on it in the order the records landed;
 * what somebody WROTE is a card beside their face on the rail, and what
 * somebody DID (a revision pushed, a review, an ask) a row with its disc on it.
 *
 * A review's disc wears its verdict's tone, the verdict said in words beside
 * it; the merge box's discs wear where each condition stands, as the readiness
 * meter's segments do. A face is the seat's own (`ui.tsx`).
 */

import type { ReactNode } from "react";
import { Card } from "@crewlethq/ui";
import { ago } from "~/lib/ago.ts";
import { fmtDateTime } from "~/lib/format.ts";
import { Face, useWhoName, type Who } from "../ui.tsx";
import "~/styles/gitlet.css";

/** The rail: an ordered list, each record one item on it; `tail` the merge box and composer after the break. */
export function TimelineList({
  label,
  tail = false,
  children,
}: {
  label: string;
  tail?: boolean;
  children: ReactNode;
}) {
  return (
    <ol className={tail ? "code-timeline code-timeline--tail" : "code-timeline"} aria-label={label}>
      {children}
    </ol>
  );
}

/** A face on the rail beside a card: the writer's, 32 px. */
function RailFace({ who }: { who: Who }) {
  return (
    <span className="code-tl-face" aria-hidden="true">
      <Face who={who} size="md" />
    </span>
  );
}

/** Who did what and when, without a face: a card's head whose face is on the rail. */
export function Said({
  who,
  verb,
  at,
  now,
  children,
}: {
  who: Who;
  verb?: ReactNode;
  at?: string | undefined;
  now: number;
  children?: ReactNode;
}) {
  const name = useWhoName()(who);
  return (
    <span className="code-byline">
      <span className="code-byline__who">{name}</span>
      {verb && <span className="code-byline__verb">{verb}</span>}
      {at && (
        <time className="code-byline__when" dateTime={at} title={fmtDateTime(at)}>
          {ago(at, now, { clock: true })}
        </time>
      )}
      {children}
    </span>
  );
}

/**
 * What somebody wrote, as a card on the rail: its head says who did what and
 * when, with anything the record carries beside it (a resolved mark); its
 * body is what they said; its foot what a reader does with it. `face` puts
 * the writer's face on the rail.
 */
export function TimelineCard({
  head,
  marks,
  label,
  face,
  children,
  foot,
}: {
  /** Who did what and when: the record's `Byline`, or `Said` beside a `face`. */
  head: ReactNode;
  /** What the record carries beside its head, at its end. */
  marks?: ReactNode;
  /** What a screen reader calls the card. */
  label: string;
  /** The writer, drawn on the rail. */
  face?: Who | undefined;
  children?: ReactNode;
  foot?: ReactNode;
}) {
  return (
    <li className="code-timeline__item">
      {face !== undefined && <RailFace who={face} />}
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

/** A card the caller draws itself beside a disc on the rail: the merge box. */
export function TimelineMark({ disc, children }: { disc: ReactNode; children: ReactNode }) {
  return (
    <li className="code-timeline__item">
      <span className="code-tl-face">{disc}</span>
      {children}
    </li>
  );
}

/** A card the caller draws itself (a thread, the description), beside its writer's face. */
export function TimelineItem({ face, children }: { face: Who; children: ReactNode }) {
  return (
    <li className="code-timeline__item">
      <RailFace who={face} />
      {children}
    </li>
  );
}

/** A disc's tone: a review's verdict, a merge condition's standing, or neutral. */
export type DiscTone = "neutral" | "success" | "info" | "warning" | "danger";

/** A mark on the timeline's rail: its glyph on a disc in a tone, `small` for what somebody did in passing. */
export function Disc({
  glyph,
  tone = "neutral",
  size = "regular",
  label,
}: {
  glyph: ReactNode;
  tone?: DiscTone;
  size?: "small" | "regular" | "large";
  /** Named only where no sentence beside it says what it marks. */
  label?: string;
}) {
  return (
    <span
      className="code-disc"
      data-tone={tone}
      data-size={size}
      {...(label ? { role: "img", "aria-label": label } : { "aria-hidden": true })}
    >
      {glyph}
    </span>
  );
}

/**
 * What somebody did, as a row on the rail: its disc on the rail, the
 * sentence, what sits at the row's end, and what hangs under it (a review's
 * summary and findings).
 */
export function TimelineEvent({
  disc,
  children,
  end,
  under,
}: {
  disc: ReactNode;
  children: ReactNode;
  end?: ReactNode;
  under?: ReactNode;
}) {
  return (
    <li className="code-timeline__item code-tl-event">
      <div className="code-tl-event__row">
        <span className="code-tl-event__disc">{disc}</span>
        <span className="code-tl-event__said">{children}</span>
        {end && <span className="code-tl-event__end">{end}</span>}
      </div>
      {under && <div className="code-tl-event__under">{under}</div>}
    </li>
  );
}
