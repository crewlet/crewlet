/**
 * A design: a canvas of artboards, each an HTML page framed on the artifact
 * origin under a ticket of its own, with titles and stickies beside them.
 *
 * THE CANVAS IS DRAWN HERE from `canvas.json` (the version's entry, read as its
 * source), on the design system's `CanvasBoard`: pan, zoom, fit, and the
 * visibility budget, so only boards on screen and wide enough to read are
 * framed, at most twelve at once. A board opens full size in a sheet, still
 * framed and still ticketed.
 *
 * PEOPLE REARRANGE IT (D69). An editor at the head moves and resizes boards and
 * adds, edits and deletes notes; Save publishes a version that changes
 * `canvas.json` alone against the version the draft started from. A save
 * refused because somebody published since is merged onto their version item
 * by item (lib/artifacts/canvas.ts) and saved again by the person.
 *
 * PRINT IS PER BOARD, and the host's (`crewlet.print()`, or Print on an opened
 * board): that board alone is framed again on a page of its own size and
 * handed to the browser's print, which saves it as a PDF.
 */

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import {
  Button,
  Callout,
  CanvasBoard,
  Modal,
  Skeleton,
  useToast,
  type CanvasBoardItem,
  type CanvasHandle,
} from "@crewlethq/ui";
import type { ArtifactDetail, ArtifactSource, ArtifactVersionSummary } from "~/protocol/index.ts";
import { QueryState } from "~/components/common.tsx";
import { useRest } from "~/lib/useRest.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import { publishVersion, readSource } from "~/lib/artifacts/api.ts";
import type { ArtifactRoom } from "~/lib/artifacts/room.ts";
import type { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import {
  CANVAS_MAX_SIDE,
  CANVAS_MIN_SIDE,
  addNote,
  boardItems,
  changeNote,
  mergeCanvas,
  moveBoard,
  noteItems,
  parseCanvas,
  publishOf,
  removeNote,
  sameCanvas,
  type CanvasMerge,
  type DesignCanvas,
} from "~/lib/artifacts/canvas.ts";
import { FrameView } from "./FrameView.tsx";
import type { PresenceReport } from "./presence.ts";

/** The region a reader on one board is at, as the room hears it. */
export const boardRegion = (path: string): string => `board:${path}`;

interface DesignViewProps {
  detail: ArtifactDetail;
  version: number;
  versions: ArtifactVersionSummary[];
  room: ArtifactRoom;
  presence: PresenceReport;
  bridgeStores: Set<ArtifactDataStore>;
  /** Absent for a secondary drawing (a side of a compare): read only, and silent to the room. */
  onPublished?: ((version: number) => void) | undefined;
}

export function DesignView(props: DesignViewProps) {
  const { detail, version } = props;
  const source = useRest<ArtifactSource>(
    version > 0 ? `canvas:${detail.id}:${version}` : null,
    (signal) => readSource(detail.id, version, signal),
  );
  const canvas = useMemo(() => {
    if (!source.data) return null;
    try {
      return { canvas: parseCanvas(source.data.text), text: source.data.text };
    } catch (err) {
      return { error: err instanceof Error ? err.message : "The canvas could not be read." };
    }
  }, [source.data]);
  if (!canvas) {
    return source.loading ? (
      <Skeleton variant="box" label={`Loading ${detail.title}`} className="design-skeleton" />
    ) : (
      <QueryState error={source.code} refusal={source.refusal} loading={false} />
    );
  }
  if ("error" in canvas) {
    return (
      <Callout variant="danger" title="This design could not be drawn">
        <p>{canvas.error}</p>
      </Callout>
    );
  }
  return (
    <Design key={`${detail.id}:${version}`} {...props} canvas={canvas.canvas} text={canvas.text} />
  );
}

function Design({
  detail,
  version,
  versions,
  room,
  presence,
  bridgeStores,
  onPublished,
  canvas,
  text,
}: DesignViewProps & { canvas: DesignCanvas; text: string }) {
  const primary = onPublished !== undefined;
  const toast = useToast();
  const save = useKeyedGesture();
  const publish = useArtifactAccess({ app: true, role: "editor", held: detail.role });
  const canvasRef = useRef<CanvasHandle>(null);

  // THE DRAFT AND THE TEXT IT STARTED FROM: a save replaces exactly that text,
  // which after a merge is the newer version's.
  const [draft, setDraft] = useState<DesignCanvas | null>(null);
  const [base, setBase] = useState<{ canvas: DesignCanvas; text: string; number: number }>({
    canvas,
    text,
    number: version,
  });
  const [merge, setMerge] = useState<CanvasMerge | null>(null);
  const [editing, setEditing] = useState(false);
  const [saving, setSaving] = useState(false);
  const [opened, setOpened] = useState<string | null>(null);
  const [printing, setPrinting] = useState<string | null>(null);

  const shown = draft ?? base.canvas;
  const dirty = draft !== null && !sameCanvas(draft, base.canvas);
  // ONLY THE HEAD IS EDITED; a draft already open stays open when somebody
  // else publishes, and its save is merged onto their version.
  const editable = primary && publish.can && (version === detail.head_number || draft !== null);
  const boards = useMemo(() => boardItems(shown), [shown]);
  const notes = useMemo(() => noteItems(shown), [shown]);

  const { at } = presence;
  const [active, setActive] = useState<string | null>(null);
  useEffect(() => {
    if (primary) at(active ? { region: boardRegion(active) } : null, editing && dirty);
  }, [at, primary, active, editing, dirty]);

  const change = useCallback(
    (fn: (c: DesignCanvas) => DesignCanvas) => setDraft((d) => fn(d ?? base.canvas)),
    [base.canvas],
  );

  const print = useCallback((path: string) => {
    setPrinting(path);
    return Promise.resolve(true);
  }, []);

  const addAt = (kind: "title" | "sticky") => {
    // A new note goes beside everything already on the canvas, where it
    // covers nothing; the person then moves it where it belongs.
    const right = Math.max(0, ...boards.map((b) => b.x + b.w), ...notes.map((n) => n.x + n.w));
    const top = Math.min(0, ...boards.map((b) => b.y), ...notes.map((n) => n.y));
    change((c) =>
      addNote(c, kind, { x: right + 100, y: top }, kind === "title" ? "Title" : "Note"),
    );
  };

  const saveNow = async () => {
    if (!draft) return;
    setSaving(true);
    try {
      const answer = await save.run(
        publishVersion(detail.id, base.number, publishOf(base.text, draft, "")),
      );
      if (answer?.kind === "done") {
        const n = Number(answer.body.number ?? base.number + 1);
        toast.ok(`Saved as version ${n}`);
        setDraft(null);
        setMerge(null);
        setEditing(false);
        onPublished?.(n);
        return;
      }
      if (answer?.kind === "refused" && answer.error?.code === "version_moved") {
        const live = answer.error.body.live as { number?: unknown } | undefined;
        const head = Number(live?.number);
        if (!Number.isInteger(head) || head < 1) {
          toast.show({
            variant: "danger",
            title: "Could not save the design",
            message: answer.text,
          });
          return;
        }
        const newer = await readSource(detail.id, head, new AbortController().signal);
        const liveCanvas = parseCanvas(newer.text);
        const merged = mergeCanvas(base.canvas, draft, liveCanvas);
        setBase({ canvas: liveCanvas, text: newer.text, number: head });
        setDraft(merged.draft);
        setMerge(merged);
        return;
      }
      if (answer)
        toast.show({ variant: "danger", title: "Could not save the design", message: answer.text });
    } catch (err) {
      toast.show({
        variant: "danger",
        title: "Could not save the design",
        message: err instanceof Error ? err.message : "The engine did not answer.",
      });
    } finally {
      setSaving(false);
    }
  };

  const reveal = (b: CanvasBoardItem) =>
    canvasRef.current?.focusRegion({ x: b.x, y: b.y, width: b.w, height: b.h });

  const frameOf = (b: CanvasBoardItem, className: string, onReady?: () => void): ReactNode => (
    <FrameView
      detail={detail}
      version={version}
      versions={versions}
      room={room}
      presence={presence}
      bridgeStores={bridgeStores}
      document={b.id}
      title={`${detail.title}, ${b.title}`}
      print={() => print(b.id)}
      onReady={onReady}
      className={className}
    />
  );

  const openedBoard = boards.find((b) => b.id === opened);
  const printedBoard = boards.find((b) => b.id === printing);

  return (
    <div className="design-view">
      <div className="design-bar">
        <span className="design-bar-count">
          {boards.length} {boards.length === 1 ? "board" : "boards"}
        </span>
        {editable && !editing && (
          <Button size="small" variant="secondary" onClick={() => setEditing(true)}>
            Edit layout
          </Button>
        )}
        {editing && (
          <>
            <Button size="small" variant="ghost" onClick={() => addAt("title")}>
              Add title
            </Button>
            <Button size="small" variant="ghost" onClick={() => addAt("sticky")}>
              Add sticky
            </Button>
            <Button
              size="small"
              variant="ghost"
              onClick={() => {
                setDraft(null);
                setMerge(null);
                setBase({ canvas, text, number: version });
                setEditing(false);
              }}
            >
              Discard
            </Button>
            <Button
              size="small"
              variant="primary"
              disabled={!dirty || saving}
              onClick={() => void saveNow()}
            >
              {saving ? "Saving…" : "Save version"}
            </Button>
          </>
        )}
      </div>
      {merge && (
        <Callout
          className="design-merged"
          variant={merge.overlaps.length > 0 ? "warning" : "info"}
          title={`Moved onto version ${base.number}`}
          action={
            <Button size="small" variant="ghost" onClick={() => setMerge(null)}>
              Dismiss
            </Button>
          }
        >
          {merge.overlaps.length > 0
            ? `Somebody published while you were editing, and changed ${merge.overlaps.join(", ")} too. Your changes to those are kept. Save again to publish both.`
            : "Somebody published while you were editing. Their changes are kept beside yours. Save again to publish both."}
        </Callout>
      )}
      <div className="design-body">
        <nav className="design-boards" aria-label="Boards">
          <ol>
            {boards.map((b) => (
              <li key={b.id}>
                <button type="button" className="design-board-link" onClick={() => reveal(b)}>
                  {b.title}
                </button>
              </li>
            ))}
          </ol>
        </nav>
        <div className="design-canvas">
          <CanvasBoard
            label={shown.title || detail.title}
            boards={boards}
            notes={notes}
            renderBoard={(b) => frameOf(b, "design-board-frame")}
            editable={editing}
            minSide={CANVAS_MIN_SIDE}
            maxSide={CANVAS_MAX_SIDE}
            onBoardChange={(id, rect) => change((c) => moveBoard(c, id, rect))}
            onNoteChange={(note) => change((c) => changeNote(c, note))}
            onNoteRemove={(id) => change((c) => removeNote(c, id))}
            onOpen={setOpened}
            onActivate={setActive}
            canvasRef={canvasRef}
          />
        </div>
      </div>
      <Modal
        open={openedBoard !== undefined}
        onClose={() => setOpened(null)}
        title={openedBoard?.title}
        subtitle={openedBoard ? `${openedBoard.w} by ${openedBoard.h}` : undefined}
        size="xl"
        variant="sheet"
        headerActions={
          openedBoard ? (
            <Button size="small" variant="secondary" onClick={() => setPrinting(openedBoard.id)}>
              Print
            </Button>
          ) : undefined
        }
      >
        {openedBoard && frameOf(openedBoard, "design-opened-frame")}
      </Modal>
      {printedBoard && (
        <BoardPrint
          board={printedBoard}
          frame={(onReady) => frameOf(printedBoard, "design-print-frame", onReady)}
          onDone={() => setPrinting(null)}
        />
      )}
    </div>
  );
}

/**
 * One board on a page of its own size, shown only in print: framed afresh
 * under its own ticket, and handed to the browser's print once its page has
 * said hello. The page size is a constructed stylesheet, since the
 * dashboard's policy admits no inline style element; it is removed, with the
 * frame, when the browser says the print is over.
 */
function BoardPrint({
  board,
  frame,
  onDone,
}: {
  board: CanvasBoardItem;
  frame: (onReady: () => void) => ReactNode;
  onDone: () => void;
}): ReactNode {
  const done = useRef(onDone);
  done.current = onDone;
  useEffect(() => {
    const sheet = new CSSStyleSheet();
    sheet.replaceSync(`@page design-board { size: ${board.w}px ${board.h}px; margin: 0; }`);
    document.adoptedStyleSheets = [...document.adoptedStyleSheets, sheet];
    const over = () => done.current();
    window.addEventListener("afterprint", over);
    return () => {
      window.removeEventListener("afterprint", over);
      document.adoptedStyleSheets = document.adoptedStyleSheets.filter((s) => s !== sheet);
    };
  }, [board.w, board.h]);
  const printed = useRef(false);
  const ready = () => {
    if (printed.current) return;
    printed.current = true;
    window.print();
  };
  return createPortal(
    <div className="design-print" data-board={board.id} style={{ width: board.w, height: board.h }}>
      {frame(ready)}
    </div>,
    document.body,
  );
}
