/**
 * A design: a canvas of artboards, each an HTML page framed on the artifact
 * origin under a ticket of its own, with titles and stickies beside them.
 *
 * THE CANVAS IS DRAWN HERE from `canvas.json` (the version's entry, read as its
 * source), on the design system's `CanvasBoard`: pan, zoom, fit, and the
 * visibility budget, so only boards on screen and wide enough to read are
 * framed, at most twelve at once. A board opens at its own size in a dialog,
 * still framed and still ticketed.
 *
 * THE LIST OF BOARDS BESIDE IT IS THE CANVAS'S OTHER HALF, as a design tool's
 * layers are: the selection is held here and handed to both, so the board
 * picked on the canvas is the one marked in the list, and a board picked in
 * the list is selected on the canvas and brought into view.
 *
 * THE CANVAS WORKS AS A DESIGN TOOL'S DOES, and all of it is the design
 * system's: a floating palette of tools (Move, Hand, and while editing Text
 * and Sticky), each with its key, the pointer the tool means, hairline board
 * edges, hover and selection outlines, and resize corners on a selected item.
 *
 * PEOPLE REARRANGE IT (D69). An editor at the head moves and resizes boards and
 * places notes with the Text and Sticky tools, then edits and deletes them;
 * Save publishes a version that changes
 * `canvas.json` alone against the version the draft started from. A save
 * refused because somebody published since is merged onto their version item
 * by item (lib/artifacts/canvas.ts) and saved again by the person.
 *
 * PRINT IS PER BOARD, and the host's (`crewlet.print()`, or Print on an opened
 * board): a print view covers the window with that board alone, framed afresh
 * at its own size at the window's corner, and once its page has said hello
 * the browser's print is handed a page of the board's size holding nothing
 * else, which it saves as a PDF. The view is on screen, not only in print,
 * because the browser prints a cross-origin frame as it is drawn on screen.
 */

import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { createPortal } from "react-dom";
import {
  Button,
  Callout,
  CanvasBoard,
  LayersList,
  Modal,
  SegmentedControl,
  Skeleton,
  SlideStage,
  useToast,
  type CanvasBoardItem,
  type CanvasBoardSelection,
  type CanvasHandle,
} from "@crewlethq/ui";
import type { ArtifactDetail, ArtifactSource } from "~/protocol/index.ts";
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
import { refuse, useAssistantDraft, type DraftFieldSpec } from "~/app/assistant/draft.ts";
import { FrameView } from "./FrameView.tsx";
import type { PresenceReport } from "./presence.ts";

/** The region a reader on one board is at, as the room hears it. */
export const boardRegion = (path: string): string => `board:${path}`;

interface DesignViewProps {
  detail: ArtifactDetail;
  version: number;
  room: ArtifactRoom;
  presence: PresenceReport;
  bridgeStores: Set<ArtifactDataStore>;
  /** Absent for a secondary drawing (a side of a compare): read only, and silent to the room. */
  onPublished?: ((version: number) => void) | undefined;
}

/**
 * The fields of a canvas's edit: its boards, read as their titles and sizes,
 * and each note's text by its place, set as typing on the canvas sets it.
 */
export function canvasFields(canvas: DesignCanvas): DraftFieldSpec<DesignCanvas>[] {
  return [
    {
      path: "boards",
      label: "Boards",
      read: (c) => boardItems(c).map((b) => `${b.title} (${b.w} by ${b.h})`),
    },
    ...(canvas.notes ?? []).map((note, i): DraftFieldSpec<DesignCanvas> => ({
      path: `notes.${i + 1}`,
      label: note.kind === "title" ? `Title ${i + 1}` : `Sticky ${i + 1}`,
      read: (c) => c.notes?.[i]?.text ?? "",
      write: (c, text) => {
        const now = c.notes?.[i];
        if (!now || now.id !== note.id) return refuse("That note is no longer on the canvas.");
        if (typeof text !== "string") return refuse("A note's text is text.");
        return changeNote(c, { ...now, text });
      },
    })),
  ];
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
  // HOW AN OPENED BOARD IS SHOWN: fitted to the dialog, letterboxed as
  // Present is, or at its own size with scroll. Each board opens fitted.
  const [openedSize, setOpenedSize] = useState<"fit" | "actual">("fit");
  const openBoard = useCallback((path: string) => {
    setOpenedSize("fit");
    setOpened(path);
  }, []);
  const [printing, setPrinting] = useState<string | null>(null);

  const shown = draft ?? base.canvas;
  const dirty = draft !== null && !sameCanvas(draft, base.canvas);
  // ONLY THE HEAD IS EDITED; a draft already open stays open when somebody
  // else publishes, and its save is merged onto their version.
  const editable = primary && publish.can && (version === detail.head_number || draft !== null);
  const boards = useMemo(() => boardItems(shown), [shown]);
  const boardLayers = useMemo(() => boards.map((b) => ({ id: b.id, label: b.title })), [boards]);
  const notes = useMemo(() => noteItems(shown), [shown]);

  const { at } = presence;
  const [active, setActive] = useState<string | null>(null);
  const [selected, setSelected] = useState<CanvasBoardSelection | null>(null);
  const selectedBoard = selected?.kind === "board" ? selected.id : null;
  useEffect(() => {
    if (primary) at(active ? { region: boardRegion(active) } : null, editing && dirty);
  }, [at, primary, active, editing, dirty]);

  const change = useCallback(
    (fn: (c: DesignCanvas) => DesignCanvas) => setDraft((d) => fn(d ?? base.canvas)),
    [base.canvas],
  );

  // WHAT IS OPEN, for the assistant, while the canvas is edited: its boards,
  // read, and the text of its titles and stickies, set through the canvas's
  // own note change; moving boards and Save stay the editor's.
  useAssistantDraft<DesignCanvas>(
    editing
      ? {
          kind: "client",
          id: detail.id,
          revision: base.number,
          step: "canvas",
          help:
            "You can set the text of the canvas's titles and stickies. Boards are read here: " +
            "moving, resizing and drawing them are the person's and the agents', and you make " +
            "no images. Saving makes a new version as them.",
          scope: `Your edit of “${detail.title}” (not saved)`,
          value: shown,
          fields: canvasFields(shown),
          apply: setDraft,
          blocked: () => (saving ? "The design is being saved." : null),
        }
      : null,
  );

  const print = useCallback((path: string) => {
    setPrinting(path);
    return Promise.resolve(true);
  }, []);

  /*
   * A NOTE PLACED WITH THE TEXT OR STICKY TOOL, where the person clicked. Its
   * id goes back to the canvas, which opens it for typing over its starting
   * words. At the format's note bound nothing is added and no id is returned.
   */
  const placeNote = (kind: "title" | "sticky", at: { x: number; y: number }) => {
    const current = draft ?? base.canvas;
    const next = addNote(current, kind, at, kind === "title" ? "Title" : "Note");
    if (next === current) return undefined;
    setDraft(next);
    return next.notes?.at(-1)?.id;
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

  const pick = (b: CanvasBoardItem) => {
    setSelected({ kind: "board", id: b.id });
    canvasRef.current?.focusRegion({ x: b.x, y: b.y, width: b.w, height: b.h });
  };

  const frameOf = (b: CanvasBoardItem, className: string, onReady?: () => void): ReactNode => (
    // Keyed by board, so another board opened is a frame of its own.
    <FrameView
      detail={detail}
      version={version}
      room={room}
      presence={presence}
      bridgeStores={bridgeStores}
      key={b.id}
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
        <LayersList
          className="design-boards"
          label="Boards"
          items={boardLayers}
          selected={selectedBoard}
          onSelect={(id) => {
            const b = boards.find((x) => x.id === id);
            if (b) pick(b);
          }}
          onOpen={openBoard}
        />
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
            onNoteCreate={placeNote}
            onOpen={openBoard}
            onActivate={setActive}
            selected={selected}
            onSelectedChange={setSelected}
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
        headerActions={
          openedBoard ? (
            <>
              <SegmentedControl
                label="Board size"
                semantics="radio"
                size="sm"
                options={[
                  { value: "fit", label: "Fit" },
                  { value: "actual", label: "Actual size" },
                ]}
                value={openedSize}
                onValueChange={(v: string) => setOpenedSize(v === "actual" ? "actual" : "fit")}
              />
              <Button size="small" variant="secondary" onClick={() => void print(openedBoard.id)}>
                Print
              </Button>
            </>
          ) : undefined
        }
      >
        {openedBoard &&
          (openedSize === "fit" ? (
            // FITTED: the page lays itself out at its board's own size and
            // is scaled as a whole into the dialog, so it reads as the board
            // drawn on the canvas does, only larger. The stage takes the
            // board's own proportions, so it is letterboxed only where the
            // window is too short for it, never on a narrow one.
            <div
              className="design-opened design-opened--fit"
              style={{ aspectRatio: `${openedBoard.w} / ${openedBoard.h}` }}
            >
              <SlideStage
                label={`${openedBoard.title}, ${openedBoard.w} by ${openedBoard.h}`}
                roleDescription="artboard"
                fit="contain"
                width={openedBoard.w}
                height={openedBoard.h}
              >
                {frameOf(openedBoard, "design-opened-frame")}
              </SlideStage>
            </div>
          ) : (
            // AT ITS OWN SIZE, scrolled both ways where it is larger than
            // the dialog.
            <div
              className="design-opened design-opened--actual"
              role="region"
              aria-label={`${openedBoard.title} at actual size`}
              // The box scrolls, so a keyboard reader can reach it to.
              // eslint-disable-next-line jsx-a11y/no-noninteractive-tabindex
              tabIndex={0}
            >
              <div style={{ width: openedBoard.w, height: openedBoard.h }}>
                {frameOf(openedBoard, "design-opened-frame")}
              </div>
            </div>
          ))}
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

/** How long the print view stays after the browser says the print is over. */
export const PRINT_TEARDOWN_MS = 1500;

/**
 * One board printed: a view over the whole window holding the board alone,
 * framed afresh under its own ticket at the window's corner and at its own
 * size, handed to the browser's print once its page has said hello and drawn.
 * In print nothing else of the dashboard shows, on a page of the board's own
 * size. The page size is a constructed stylesheet, since the dashboard's
 * policy admits no inline style element; it and the view go shortly after the
 * browser says the print is over, in one commit: the sheet is a layout
 * effect's, so it never outlives the view by the task a passive effect's
 * cleanup waits for, and a dashboard printed in that gap would come out on
 * the board's page size.
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
  useLayoutEffect(() => {
    const sheet = new CSSStyleSheet();
    sheet.replaceSync(`@media print {
  html, body { height: ${board.h}px !important; overflow: hidden !important; }
}
@page { size: ${board.w}px ${board.h}px; margin: 0; }`);
    document.adoptedStyleSheets = [...document.adoptedStyleSheets, sheet];
    // THE VIEW OUTLIVES `afterprint` BY A MOMENT: the event fires once the
    // dashboard's own page is printed, while the browser is still taking the
    // board's cross-origin frame into the print, and a frame removed then
    // prints as a blank page.
    let later: ReturnType<typeof setTimeout> | undefined;
    const over = () => {
      later = setTimeout(() => done.current(), PRINT_TEARDOWN_MS);
    };
    window.addEventListener("afterprint", over);
    return () => {
      clearTimeout(later);
      window.removeEventListener("afterprint", over);
      document.adoptedStyleSheets = document.adoptedStyleSheets.filter((s) => s !== sheet);
    };
  }, [board.w, board.h]);
  const printed = useRef(false);
  const ready = () => {
    if (printed.current) return;
    printed.current = true;
    // TWO FRAMES LATER the page has drawn, so what prints is what is shown.
    requestAnimationFrame(() => requestAnimationFrame(() => window.print()));
  };
  return createPortal(
    <div className="design-print-view" data-board={board.id}>
      <div style={{ width: board.w, height: board.h }}>{frame(ready)}</div>
    </div>,
    document.body,
  );
}
