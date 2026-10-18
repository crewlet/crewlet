/**
 * The engine's own pieces for the product's grids, beside uilet's table cells.
 *
 * THE CELLS ARE UILET'S. A key, a figure, an instant, a seat, a state, tags
 * and a line of text are `TableKeyCell`, `TableNumberCell`, `TableDateCell`,
 * `TableEntityCell`, `TableStatusCell`, `TableTagsCell` and `TableTextCell`
 * from `@crewlethq/ui`, imported at each call site, so the same value looks
 * the same on every screen of every product the design system serves. What
 * stays here is what only the engine knows:
 *
 * - how a seat is linked and drawn ([linkedSeat], [plainSeat]): the seat
 *   route, the seat's own face (`SeatAvatar`, which carries an agent's
 *   character) and the handle as the hover text;
 * - a turn's summary with its work item ([TurnWhatCell]) and a turn still
 *   running ([UnsettledCell]), which mean nothing outside the engine.
 *
 * # A figure is spelled by lib/format.ts
 *
 * `TableNumberCell` takes the host's formatter, and every call site in this
 * tree hands it `fmtCount` or `fmtDuration`: uilet's default is plain locale
 * grouping, so a cell given no formatter would stop abbreviating the figures
 * the rest of the screen abbreviates. `cells.test.tsx` holds the tree to it.
 */

import type { AvatarStatus, TableEntityCellProps } from "@crewlethq/ui";
import { EmptyValue } from "@crewlethq/ui";
import { href } from "../router.tsx";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import type { AgentAvatar } from "~/lib/avatar.ts";
import { handleLabel, type SeatKind } from "~/lib/seats.ts";
import { workItemLabel } from "~/lib/turns.ts";
import type { WorkItemRef } from "~/protocol/index.ts";

/** A seat as a grid names it: its name, its kind and, for an agent, its face. */
export interface SeatFace {
  name?: string | undefined;
  /**
   * The seat's kind, which decides the badge's outline: a circle for a person
   * and a squircle for an agent. `undefined` is a handle the chart does not
   * hold, drawn with the kit's default, the agent's. A screen that holds its
   * writers' recorded kinds resolves them first (`kindWithAuthors` in
   * lib/seats.ts), which is what draws an operator as the person they are.
   */
  kind?: SeatKind | undefined;
  /** How an agent is drawn, where the chart holds the seat. */
  avatar?: AgentAvatar | null | undefined;
  /** What the seat is doing now (`statusOf` in lib/seats.ts), drawn on the badge. */
  status?: AvatarStatus | undefined;
}

/** The seat's face, as the badge of a `TableEntityCell`. Decorative: the name is printed beside it. */
function seatBadge(name: string, { kind, avatar, status }: SeatFace) {
  return (
    <SeatAvatar
      name={name}
      size="xs"
      kind={kind === "human" ? "human" : "agent"}
      avatar={avatar}
      status={status}
      decorative
    />
  );
}

/**
 * A seat that links to its page: `<TableEntityCell {...linkedSeat(seat)} />`.
 *
 * A missing handle is the "Nobody" absence, whatever name came with it: the
 * link is the seat's, so there is nothing to draw without one.
 */
export function linkedSeat({
  handle,
  title,
  ...face
}: SeatFace & {
  handle?: string | null | undefined;
  /**
   * The hover text, where the handle is not the whole story: an operator's
   * write names the person AND the token they wrote through.
   */
  title?: string | undefined;
}): TableEntityCellProps {
  if (!handle) return { name: null };
  const name = face.name || handle;
  return {
    name,
    kind: face.kind === "human" ? "human" : "agent",
    badge: seatBadge(name, face),
    href: href(["agents", "seats", handle]),
    title: title ?? handleLabel(handle),
  };
}

/**
 * A seat named inside a row that is ALREADY a link: its face and its name,
 * and no anchor of its own, since an anchor inside an anchor is markup no
 * browser agrees about. `<TableEntityCell {...plainSeat(seat)} />`.
 */
export function plainSeat(face: SeatFace & { name: string }): TableEntityCellProps {
  return {
    name: face.name,
    kind: face.kind === "human" ? "human" : "agent",
    badge: seatBadge(face.name, face),
  };
}

/**
 * What a turn did, and the work item it was on: the "What it did" cell of
 * every list of turns.
 *
 * THE SUMMARY IS WHAT KEEPS ITS WIDTH. The key beside it is one token and must
 * not wrap, but it is capped (`.turn-what-key`) rather than never shrinking:
 * an uncapped key took the whole cell whenever it was long, and on a phone's
 * stacked row the summary beside it was laid out one letter per line — a
 * single row 31,000px tall. Each half is cut on one line with its whole text
 * on its title.
 */
export function TurnWhatCell({
  summary,
  item,
  doing,
}: {
  summary?: string | undefined;
  item?: WorkItemRef | null | undefined;
  /** What the turn is doing now, for one still running (`runningNow`): said
   *  where a settled turn's summary goes, since a running turn has none yet. */
  doing?: string | undefined;
}) {
  const label = item ? workItemLabel(item) : null;
  return (
    <span className="turn-what">
      {/* A CLAMP, ONE LINE WIDE, rather than a nowrap cut: it is the same
          ellipsis on a table row, and it lets a phone's stacked card give the
          summary a second line (`frame.css`) without a second rule. */}
      <span className="turn-what-summary clamp" title={summary || doing || undefined}>
        {summary || (doing ? doing : <span className="muted">no summary recorded</span>)}
      </span>
      {label && (
        <span className="mono t-caption item-key turn-what-key" title={label.title}>
          {label.text}
        </span>
      )}
    </span>
  );
}

/**
 * A turn's figure that a turn still running does not have yet — its iteration
 * count, its tokens, how long it took. Each is written as the turn completes,
 * and a list that drew the row's zeros said a running turn had run no rounds
 * and spent nothing.
 */
export function UnsettledCell() {
  return <EmptyValue label="Not settled" />;
}
