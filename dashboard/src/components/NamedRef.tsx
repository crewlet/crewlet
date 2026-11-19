/**
 * One object a line names, drawn: the kit's `InlineRef` with its kind's mark,
 * its kind said to a screen reader, its title on hover, and a link to its page
 * ONLY where the reader may open it (`lib/namedRefs.ts`). Every surface that
 * draws what an engine line names draws it here, through the one parser
 * (`lib/inlineTokens.ts`'s `named` token) or, for a field that is one
 * reference and no sentence, directly.
 *
 * THREE STATES, never a dead link:
 *  - ASKED (the engine has not said yet): its mark and its words, no link.
 *  - OPEN: a link to its page, its title on hover.
 *  - CLOSED (the reader may not open it, or it is gone, never told apart):
 *    its mark and its words, as written, no link.
 *
 * A GUESS (rule 6a, a change the prose fallback read) is drawn only once the
 * engine says it is one the reader may open, and as its words otherwise: a
 * word shaped like a reference is not one until the engine says so. A KEY the
 * fallback read (`optimistic`) is one only where its project is the company's
 * (`lib/projectKeys.ts`), so it links while it is asked, and is drawn closed
 * once the engine says the reader may not open it.
 *
 * A KIND THIS BUILD DOES NOT KNOW (a newer engine's) is its words.
 */

import { createContext, useContext, type ReactNode } from "react";
import { InlineRef } from "@crewlethq/ui";
import {
  ActivityGlyph,
  CalendarClockGlyph,
  FileTextGlyph,
  GitCommitVerticalGlyph,
  GitPullRequestGlyph,
  LayersGlyph,
  MessageSquareGlyph,
  MessagesSquareGlyph,
  SquareCodeGlyph,
  SquareKanbanGlyph,
  UserGlyph,
} from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { artifactPath } from "~/app/routes.ts";
import { KINDS } from "~/app/frame/objects.ts";
import { NAMED_REF_KINDS } from "~/contract/named.ts";
import type { LinkMode } from "~/lib/inlineTokens.ts";
import { commitPath, repoPath } from "~/lib/code.ts";
import { useReference } from "~/lib/namedRefs.ts";
import { itemPath } from "~/lib/work.ts";
import type { NamedRef as Ref } from "~/protocol/index.ts";

/**
 * Whether the references drawn below sit in a line cut to one row (a feed row,
 * a grid cell, a running turn's line): there each FLOWS as the row's own words
 * (the kit's `InlineRef` `flow`), so the row's ellipsis cuts its words after
 * its mark rather than clipping it whole. A line that wraps (a clamp) keeps
 * the kit's box, which moves to the next line whole.
 */
export const CutLine = createContext(false);

/** A kind a line names (`contract/named.ts`). */
type NamedKind = (typeof NAMED_REF_KINDS)[number];

/** How one kind is drawn: its mark, what the mark says, and its page. */
interface KindDrawing {
  mark: ReactNode;
  /** What the mark says to a screen reader, before the words. */
  said: string;
  /** An identifier (a key, a reference, a commit) is set in mono. */
  code: boolean;
  /** The kind's page, by the id the engine names it with; null for none. */
  path: (id: string) => string[] | null;
}

/** Every kind a line names (`contract/named.ts`), drawn. */
export const NAMED_DRAWINGS: Record<NamedKind, KindDrawing> = {
  work_item: {
    mark: <SquareKanbanGlyph />,
    said: "Task",
    code: true,
    path: (id) => itemPath({ id, key: id }),
  },
  code_change: {
    mark: <GitPullRequestGlyph />,
    said: "Change",
    code: true,
    path: KINDS.change.pathOf,
  },
  commit: {
    mark: <GitCommitVerticalGlyph />,
    said: "Commit",
    code: true,
    path: (id) => {
      const cut = id.indexOf("@");
      return cut > 0 ? commitPath(id.slice(0, cut), id.slice(cut + 1)) : null;
    },
  },
  repository: { mark: <SquareCodeGlyph />, said: "Repository", code: true, path: repoPath },
  page: { mark: <FileTextGlyph />, said: "Page", code: false, path: KINDS.page.pathOf },
  artifact: { mark: <LayersGlyph />, said: "Artifact", code: false, path: artifactPath },
  turn: { mark: <ActivityGlyph />, said: "Turn", code: true, path: KINDS.turn.pathOf },
  schedule: {
    mark: <CalendarClockGlyph />,
    said: "Schedule",
    code: false,
    path: KINDS.schedule.pathOf,
  },
  a2a_channel: {
    mark: <MessagesSquareGlyph />,
    said: "Channel",
    code: true,
    path: KINDS.channel.pathOf,
  },
  seat: { mark: <UserGlyph />, said: "Seat", code: false, path: KINDS.seat.pathOf },
  chat_room: {
    mark: <MessageSquareGlyph />,
    said: "Room",
    code: false,
    path: (id) => ["chatlet", id],
  },
};

/** The drawing of a kind this build knows, or undefined for a newer engine's. */
export function drawingOf(kind: string): KindDrawing | undefined {
  return (NAMED_REF_KINDS as readonly string[]).includes(kind)
    ? NAMED_DRAWINGS[kind as NamedKind]
    : undefined;
}

/**
 * One reference, drawn by its kind. `links` is the run's link mode: under
 * `text` (a row that is itself a link) it is never a link. `guessed` is a
 * reference the prose fallback read rather than one the line declared, and
 * `optimistic` one that links while it is asked.
 */
export function NamedRef({
  named,
  links = "anchor",
  guessed = false,
  optimistic = false,
  open = "",
  close = "",
}: {
  named: Ref;
  links?: LinkMode | undefined;
  guessed?: boolean | undefined;
  optimistic?: boolean | undefined;
  open?: string | undefined;
  close?: string | undefined;
}) {
  const drawing = drawingOf(named.kind);
  const reading = useReference(drawing ? named : null);
  const cut = useContext(CutLine);
  const words = `${open}${named.text}${close}`;
  if (drawing === undefined) return words;
  // A GUESS IS WORDS until the engine opens it.
  if (guessed && !reading) return words;
  const linkable = reading ? true : reading === undefined && optimistic;
  const path = linkable ? drawing.path(named.id) : null;
  const target = path !== null && links !== "text" ? href(path) : undefined;
  return (
    <InlineRef
      mark={drawing.mark}
      kind={drawing.said}
      href={target}
      title={reading ? reading.title || `${drawing.said} ${named.text}` : undefined}
      code={drawing.code}
      flow={cut}
      open={open}
      close={close}
    >
      {named.text}
    </InlineRef>
  );
}
