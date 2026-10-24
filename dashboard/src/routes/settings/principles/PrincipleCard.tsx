/**
 * One part of the company's charter as a card: its glyph and its name in the
 * header, what the company wrote (or that it wrote nothing) in the body, and,
 * while the part is being written, the editor in the body's place.
 *
 * A REGION NAMED BY ITS TITLE (`as="section"`), so a screen reader listing the
 * page's landmarks finds Mission, Vision and Standing policies by name, and the
 * editor that replaces a body stays inside the region the reader was in.
 *
 * # What a card is handed
 *
 * The card draws; it decides nothing about writing. Whoever writes the charter
 * hands each card a [PrincipleCardEdit]: the words to show, the head's own
 * control, the empty state's Add, the editor while the part is being written,
 * and a note under the body. Declared here, by the view that places them, so
 * the editing module and the page meet on one set of props.
 */

import type { ReactNode } from "react";
import { Card, EmptyState, List, ListItem, Text } from "@crewlethq/ui";

/** How one card is written, as whoever writes the charter hands it to the card. */
export interface PrincipleCardEdit<T> {
  /** The words the card shows: the company's, or a save not yet applied here. */
  shown: T;
  /** The head's own control (Edit), or nothing. */
  actions: ReactNode;
  /** The empty state's way to write the part (Add a mission), or nothing. */
  add: ReactNode;
  /** The part is being written: the body is this editor. Nothing otherwise. */
  editor: ReactNode;
  /** A caption under the body, such as a save this node has not applied yet, or nothing. */
  note: ReactNode;
}

/** How the whole charter is written, card by card, and one notice over the cards. */
export interface PrinciplesCardsEdit {
  mission: PrincipleCardEdit<string>;
  vision: PrincipleCardEdit<string>;
  policies: PrincipleCardEdit<readonly string[]>;
  /** Over the cards, or anywhere a portal takes it: nothing when there is nothing to say. */
  notice: ReactNode;
}

/** A part of the charter, in the order an agent reads it. */
export type CharterPart = Exclude<keyof PrinciplesCardsEdit, "notice">;

export interface PrincipleCardProps {
  part: CharterPart;
  /** The part's name, the card's title: "Mission". */
  title: string;
  /** The part's glyph, at the small step. Decoration: the title carries the meaning. */
  icon: ReactNode;
  /** How many the part holds, for a part that is a list; drawn only while it is read. */
  count?: number | undefined;
  /** The head's own control, at the end of its row. */
  actions: ReactNode;
  /** The part is being written: the body is the editor, and no count is drawn. */
  editing: boolean;
  /** A caption under the body. */
  note: ReactNode;
  /** The body reaches the card's edges, for a divided list whose rows carry their own inset. */
  flush?: boolean | undefined;
  children: ReactNode;
}

export function PrincipleCard({
  part,
  title,
  icon,
  count,
  actions,
  editing,
  note,
  flush = false,
  children,
}: PrincipleCardProps) {
  return (
    <Card
      as="section"
      className="principles-card"
      data-part={part}
      data-editing={editing ? "" : undefined}
    >
      <Card.Header icon={icon} count={editing ? undefined : count} actions={actions}>
        <Card.Title>{title}</Card.Title>
      </Card.Header>
      <Card.Body padding={flush ? "none" : undefined}>{children}</Card.Body>
      {note ? (
        <Card.Footer variant="meta">
          <Text as="p" variant="caption">
            {note}
          </Text>
        </Card.Footer>
      ) : null}
    </Card>
  );
}

/**
 * A statement the company wrote, as it wrote it: the kit's passage, which
 * keeps the author's line breaks. The engine allows them in the vision alone
 * (its paragraphs), and holds the mission to one line, which the passage
 * wraps like any other.
 */
export function StatementText({ children }: { children: string }) {
  return (
    <Text as="p" variant="passage">
      {children}
    </Text>
  );
}

/**
 * The standing policies, numbered (a reader cites "policy 2") and divided in
 * the kit's compact rows. A policy is one line by the engine's own rule, so
 * it wraps as prose and keeps no breaks of its own, held to the kit's wide
 * reading measure: across a card as wide as the page a rule otherwise runs
 * to some 150 characters a line, while the rows and their rules still span
 * the card.
 */
export function PolicyList({ policies, label }: { policies: readonly string[]; label: string }) {
  return (
    <List variant="numbered-divided" density="compact" measure="wide" aria-label={label}>
      {policies.map((policy, i) => (
        <ListItem key={i}>{policy}</ListItem>
      ))}
    </List>
  );
}

/**
 * An unwritten part inside its card: what it is for, and the way to write it
 * for a reader who may. The card's title already names the part, so the empty
 * state draws no heading of its own.
 */
export function UnwrittenPart({
  title,
  description,
  action,
}: {
  title: string;
  description: string;
  action: ReactNode;
}) {
  return (
    <EmptyState
      size="inline"
      headingLevel="none"
      title={title}
      description={description}
      action={action ?? undefined}
    />
  );
}
