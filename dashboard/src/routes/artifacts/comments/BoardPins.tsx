/** Comment pins in a canvas's overlay, placed by the handle's `screenRect` on each view change. */

import { useEffect, useReducer, useRef, useState, type RefObject } from "react";
import type { CanvasHandle } from "@crewlethq/ui";
import {
  anchorLabel,
  boardAnchorAt,
  boardWorld,
  pinned,
  placeOf,
  type BoardFrame,
  type Box,
} from "~/lib/artifacts/comments.ts";
import { DraftPin, Outline, ThreadPin } from "./layer.tsx";
import { useComments } from "./state.tsx";

/** How much of the canvas around a pin Jump to brings into view, in canvas pixels. */
const REVEAL_ROOM = 160;

export function BoardPins({
  canvas,
  boards,
  moved,
}: {
  canvas: RefObject<CanvasHandle | null>;
  /** The canvas's boards, each by its page's path, with their titles. */
  boards: readonly (BoardFrame & { title: string })[];
  /** Called whenever the canvas's view moves: the layer is drawn again. */
  moved: Set<() => void>;
}) {
  const c = useComments();
  const [, redraw] = useReducer((n: number) => n + 1, 0);
  useEffect(() => {
    moved.add(redraw);
    return () => void moved.delete(redraw);
  }, [moved]);
  const [hover, setHover] = useState<(BoardFrame & { title: string }) | null>(null);
  const capture = useRef<(() => void) | null>(null);
  useEffect(() => () => capture.current?.(), []);
  useEffect(() => {
    if (!c?.placing) setHover(null);
  }, [c?.placing]);

  // JUMP TO: the canvas pans to the pin, to the middle when it is far off.
  const setRevealer = c?.setRevealer;
  useEffect(() => {
    if (!setRevealer) return undefined;
    setRevealer((t) => {
      const a = placeOf(t);
      const at = a ? boardWorld(boards, a) : null;
      if (!at) return;
      const half = REVEAL_ROOM / 2;
      canvas.current?.reveal(
        { x: at.x - half, y: at.y - half, width: REVEAL_ROOM, height: REVEAL_ROOM },
        "centre",
      );
    });
    return () => setRevealer(null);
  }, [setRevealer, boards, canvas]);

  const handle = canvas.current;
  if (!c || !handle) return null;
  const screen = (x: number, y: number) => handle.screenRect({ x, y, width: 0, height: 0 });
  const screenBox = (b: BoardFrame): Box => {
    const r = handle.screenRect({ x: b.x, y: b.y, width: b.w, height: b.h });
    return { x: r.x, y: r.y, w: r.width, h: r.height };
  };

  const boardUnder = (clientX: number, clientY: number) => {
    const anchor = boardAnchorAt(boards, handle.worldAt({ x: clientX, y: clientY }));
    return anchor ? { anchor, board: boards.find((b) => b.id === anchor.board)! } : null;
  };

  const place = (clientX: number, clientY: number) => {
    const hit = boardUnder(clientX, clientY);
    if (!hit) return;
    const { anchor, board } = hit;
    const seq = c.startDraft({ anchor, label: anchorLabel(anchor), crop: undefined });
    if (c.narrow) c.showPanel();
    else c.stepAside();
    capture.current?.();
    capture.current = null;
    // A BOARD THAT IS NOT LIVE (off screen, or drawn too small to run) has no
    // page to draw the picture: the draft goes without one.
    const host = c.pageHost(board.id);
    if (!host) {
      c.setDraftCrop(seq, null);
      return;
    }
    let id = 0;
    const stop = host.onComments((news) => {
      if (news.kind !== "crop" || news.crop.id !== id) return;
      c.setDraftCrop(seq, news.crop.image);
      stop();
      if (capture.current === stop) capture.current = null;
    });
    capture.current = stop;
    // A BOARD'S PIXELS ARE ITS PAGE'S VIEWPORT, which the page may scroll past the board.
    id = host.captureComment({ view: { x: anchor.x ?? 0, y: anchor.y ?? 0 } });
  };

  const pins = c.threads.filter(
    (t) => pinned(t, c.filter, c.selected) && placeOf(t)?.kind === "design",
  );
  const draftAt = c.draft ? boardWorld(boards, c.draft.anchor) : null;
  return (
    <>
      {c.placing && (
        <div
          className="artifact-comment-catch"
          aria-hidden="true"
          // THE CANVAS DOES NOT PAN OR SELECT under a press that places a
          // comment; its wheel still zooms.
          onPointerDown={(event) => event.stopPropagation()}
          onPointerMove={(event) => {
            const hit = boardUnder(event.clientX, event.clientY);
            setHover((was) => (was?.id === hit?.board.id ? was : (hit?.board ?? null)));
          }}
          onPointerLeave={() => setHover(null)}
          onClick={(event) => place(event.clientX, event.clientY)}
        />
      )}
      {hover && <Outline box={screenBox(hover)} label={hover.title} />}
      {pins.map((t) => {
        const at = boardWorld(boards, placeOf(t)!);
        if (!at) return null;
        const s = screen(at.x, at.y);
        return <ThreadPin key={t.id} t={t} x={s.x} y={s.y} />;
      })}
      {draftAt &&
        (() => {
          const s = screen(draftAt.x, draftAt.y);
          return <DraftPin x={s.x} y={s.y} />;
        })()}
    </>
  );
}
