/**
 * Home: how the company is, what waits on the person reading, and what the
 * company did, laid out as the reader's own board.
 *
 * # The first fold is the company, then the reader
 *
 * A queue-shaped home renders a healthy company as a blank page, and a reader
 * cannot tell that from a broken one. So Home opens, until its reader says
 * otherwise, with five figures that are true whatever the queue holds (who is
 * working, what waits on you, the work in progress, the work finished, the
 * tokens spent) and under them the decisions only this reader can make,
 * answered IN PLACE. An empty decisions card then means something: nothing
 * needs you, on a company that is visibly running.
 *
 * # The reader arranges it
 *
 * Edit Home turns the page into a board (`board/`): widgets are moved by
 * dragging, sized from their corner, grouped under coloured headings, and
 * added from a tray that offers Home's own cards, the reader's notes,
 * headings, dividers and links, and the widgets every section of the
 * dashboard offers (`board/registry.tsx`). The board is kept per reader in
 * this browser (`board/model.ts`), as stars are.
 *
 * A DRAFT, KEPT ON DONE. Arranging takes many moves and the page in between
 * is one nobody chose, so nothing is stored until Done. Every move is undoable
 * while editing (Undo, Redo, and the platform's keys), and Cancel with
 * changes asks before throwing them away.
 *
 * GROUPS FOLD OUTSIDE EDITING TOO, and that is kept at once: folding "Spend"
 * on a Monday is a choice about the page, not a layout change to confirm.
 *
 * # Every sentence is the engine's, and none prints a zero
 *
 * The status line is the health push and the engine's own seat vocabulary,
 * and the greeting is the reader's own, because it is their morning. See
 * `model.ts` for how each clause reads for none, one and many.
 */

import { useNarrowerThan } from "~/lib/useNarrowerThan.ts";
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  ArrangeBoard,
  ArrangeFrame,
  ArrangeGrid,
  ArrangeGroup,
  Button,
  ConfirmModal,
  EmptyState,
  IconButton,
  VisuallyHidden,
  useReorderLists,
  useToast,
} from "@crewlethq/ui";
import {
  CopyGlyph,
  FoldVerticalGlyph,
  LayoutDashboardGlyph,
  LayoutGridGlyph,
  PencilGlyph,
  UngroupGlyph,
} from "@crewlethq/icons/glyphs";
import { useLeaveGuard, useUnloadGuard } from "~/app/router.tsx";
import { useUndoKeys } from "~/lib/useUndoKeys.ts";
import { HomeDataProvider, useHomeData } from "./useHomeData.ts";
import { HomeHead } from "./sections.tsx";
import { shippedBoard } from "./board/catalog.tsx";
import {
  COLUMNS,
  PAGE,
  ROW,
  addWidget,
  boardStore,
  drawnWidths,
  duplicate,
  findWidget,
  fromLists,
  groupList,
  isOnce,
  listsOf,
  removeWidget,
  sameBoard,
  sizingOf,
  ungroup,
  updateWidget,
  type Board,
  type GroupWidget,
  type Widget,
} from "./board/model.ts";
import { LeafView } from "./board/widgets.tsx";
import { widgetShown } from "./board/kit.tsx";
import { widgetDef } from "./board/registry.tsx";
import { useServicesRun } from "~/lib/services.ts";
import { WidgetSettings, configurable, widgetName } from "./board/settings.tsx";
import { WidgetTray } from "./board/tray.tsx";
import { openLinksInNewTab } from "./board/newTab.ts";
import { homeBoardDraft } from "./board/assistantDraft.ts";
import { useAssistantDraft } from "~/app/assistant/draft.ts";
import { EditBar } from "./board/EditBar.tsx";

const store = boardStore(shippedBoard());

/**
 * Below this width the page is one column: Home's figures fold and every block
 * on the board spans the page, since the decisions card's buttons need the
 * width a column beside it would take. The same width `screens.css` folds the
 * figures at.
 */
const STACKS_BELOW = 944;

const HISTORY = 50;
/** Keystrokes in one settings field within this long are one step of Undo. */
const COALESCE_MS = 1000;

export function Home() {
  const data = useHomeData();
  const runs = useServicesRun();
  const saved = store.use();
  const toast = useToast();
  const [draft, setDraft] = useState<Board | null>(null);
  const [past, setPast] = useState<Board[]>([]);
  const [future, setFuture] = useState<Board[]>([]);
  const [settingsId, setSettingsId] = useState<string | null>(null);
  // A widget chosen in the tray that has a choice to make first (which page,
  // which saved view): held here, off the page, until Add to Home.
  const [pending, setPending] = useState<Widget | null>(null);
  // The board and the Undo depth when a widget's settings opened, so Cancel
  // can put both back.
  const opened = useRef<{ board: Board; steps: number } | null>(null);
  const [trayOpen, setTrayOpen] = useState(false);
  const [confirmCancel, setConfirmCancel] = useState(false);
  const [fresh, setFresh] = useState<string | null>(null);
  const [leaving, setLeaving] = useState<{ leave: () => void } | null>(null);
  const last = useRef<{ key: string; at: number } | null>(null);
  const editing = draft !== null;
  const root = useRef<HTMLDivElement>(null);
  const stacked = useNarrowerThan(root, STACKS_BELOW);
  const board = draft ?? saved;
  const changed = draft !== null && !sameBoard(draft, saved);

  // UNSAVED ARRANGING IS NOT LOST TO A CLICK ELSEWHERE: the sidebar, the
  // palette or a link in a card asks first, and so does closing the tab.
  useUnloadGuard(changed);
  useLeaveGuard(
    changed
      ? (_to, leave) => {
          setLeaving({ leave });
          return true;
        }
      : null,
  );

  // What is being edited, read by `apply` so a step needs no updater with
  // side effects (an updater may run twice).
  const current = useRef<Board | null>(null);
  current.current = draft;

  /** One step of editing, which Undo takes back; `key` folds a run of keystrokes into one. */
  const apply = useCallback((next: Board, key?: string) => {
    const was = current.current;
    if (was === null) return;
    const now = Date.now();
    const folds =
      key !== undefined && last.current?.key === key && now - last.current.at < COALESCE_MS;
    last.current = key === undefined ? null : { key, at: now };
    if (!folds) {
      setPast((p) => [...p.slice(-(HISTORY - 1)), was]);
      setFuture([]);
    }
    // Held at once, so a second step before the next render (two quick
    // presses in the tray) builds on this one rather than on the board both
    // presses saw.
    current.current = next;
    setDraft(next);
  }, []);
  const undo = useCallback(() => {
    if (!draft || past.length === 0) return;
    setFuture((f) => [draft, ...f]);
    setDraft(past[past.length - 1]!);
    setPast((p) => p.slice(0, -1));
    last.current = null;
  }, [draft, past]);
  const redo = useCallback(() => {
    if (!draft || future.length === 0) return;
    setPast((p) => [...p, draft]);
    setDraft(future[0]!);
    setFuture((f) => f.slice(1));
    last.current = null;
  }, [draft, future]);

  // ⌘Z and ⇧⌘Z (Ctrl on other platforms) while editing, read from the key
  // table, except in a field, where the keys belong to the text being typed.
  useUndoKeys({
    container: root,
    enabled: editing,
    onUndo: undo,
    onRedo: redo,
  });

  // The widget whose settings open is brought into view beside the sheet.
  useEffect(() => {
    if (!settingsId) return;
    const node = document.querySelector(`[data-widget="${settingsId}"]`);
    const still = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
    node?.scrollIntoView({ block: "nearest", behavior: still ? "auto" : "smooth" });
  }, [settingsId]);

  // A widget just added is brought into view and pulses once.
  useEffect(() => {
    if (!fresh) return;
    const node = document.querySelector(`[data-widget="${fresh}"]`);
    const still = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
    node?.scrollIntoView({ block: "nearest", behavior: still ? "auto" : "smooth" });
    const t = window.setTimeout(() => setFresh(null), 1300);
    return () => window.clearTimeout(t);
  }, [fresh]);

  const start = () => {
    setDraft(saved);
    setPast([]);
    setFuture([]);
  };
  const finish = () => {
    if (draft) store.write(draft);
    setDraft(null);
    setSettingsId(null);
    setPending(null);
    setTrayOpen(false);
    if (changed) toast.ok("Home saved");
  };
  const discard = () => {
    setDraft(null);
    setSettingsId(null);
    setPending(null);
    setTrayOpen(false);
    setConfirmCancel(false);
  };

  const lists = useMemo(() => listsOf(board), [board]);
  /** Whether a block draws nothing where the company runs what it does. */
  const hidden = useCallback(
    (id: string) => {
      const w = findWidget(board, id);
      if (w?.kind !== "widget") return false;
      const def = widgetDef(w.widget);
      return def !== undefined && !widgetShown(def, w.params, runs);
    },
    [board, runs],
  );
  // THE WIDTHS THE BOARD IS DRAWN AT, each list's rows closed up over what is
  // hidden ([drawnWidths]); the saved widths are what Edit Home resizes.
  const drawn = useMemo(() => {
    const widths = new Map<string, number>();
    for (const ids of Object.values(lists)) {
      const blocks = ids.flatMap((id) => {
        const w = findWidget(board, id);
        return w ? [{ id, w: w.w }] : [];
      });
      for (const [id, w] of drawnWidths(blocks, (id) => !hidden(id))) widths.set(id, w);
    }
    return widths;
  }, [lists, board, hidden]);
  const reorder = useReorderLists({
    lists,
    onReorder: (next) => apply(fromLists(board, next)),
    nameOf: (id) => {
      const w = findWidget(board, id);
      return w ? widgetName(w) : id;
    },
    accepts: (id, list) => list === PAGE || findWidget(board, id)?.kind !== "group",
    axis: "grid",
  });

  const fold = (group: GroupWidget, collapsed: boolean) => {
    const edit = (b: Board) => updateWidget(b, group.id, (w) => ({ ...w, collapsed }) as Widget);
    if (editing) apply(edit(board));
    else store.update(edit);
  };

  const settingsWidget = settingsId && editing ? findWidget(board, settingsId) : undefined;

  // WHAT IS OPEN, for the assistant, while the board is being arranged: what
  // it holds, read only (`board/assistantDraft.ts`).
  useAssistantDraft(
    editing
      ? homeBoardDraft({
          board,
          settings: settingsWidget ? widgetName(settingsWidget) : null,
          changed,
        })
      : null,
  );

  const openSettings = (id: string) => {
    opened.current = current.current ? { board: current.current, steps: past.length } : null;
    setSettingsId(id);
  };
  /** Every change made in the settings sheet, taken back, Undo history included. */
  const cancelSettings = () => {
    const was = opened.current;
    if (was) {
      current.current = was.board;
      setDraft(was.board);
      setPast((p) => p.slice(0, was.steps));
      setFuture([]);
      last.current = null;
    }
    opened.current = null;
    setSettingsId(null);
  };

  /** A widget's toolbar: its settings, and a menu of what else can be done to it. */
  // Every action a block has is a button of its own: there are few enough
  // that a menu would only hide them.
  const toolbarFor = (w: Widget) => {
    const name = widgetName(w);
    const tool = (label: string, icon: ReactNode, onClick: () => void) => (
      <IconButton size="sm" variant="ghost" label={label} icon={icon} onClick={onClick} />
    );
    return (
      <>
        {configurable(w) &&
          tool(`Edit ${name}`, <PencilGlyph size="sm" />, () => openSettings(w.id))}
        {!isOnce(w) &&
          tool(`Duplicate ${name}`, <CopyGlyph size="sm" />, () => {
            const made = duplicate(board, w.id);
            if (made) {
              apply(made.board);
              setFresh(made.id);
            }
          })}
        {w.kind === "group" &&
          tool(`Ungroup ${name}`, <UngroupGlyph size="sm" />, () => apply(ungroup(board, w.id)))}
        {w.h !== null &&
          tool(`Fit ${name} to its content`, <FoldVerticalGlyph size="sm" />, () =>
            apply(updateWidget(board, w.id, (x) => ({ ...x, h: null }) as Widget)),
          )}
      </>
    );
  };

  const renderLeaf = (id: string) => {
    const w = findWidget(board, id);
    if (!w || w.kind === "group") return null;
    // A WIDGET WITH NOTHING TO SHOW where the company does not run its
    // service draws no cell, and its row's survivors take its columns
    // ([drawn]). It stays on the saved board, and is drawn again once the
    // company runs it. While Home is edited it keeps its cell, saying why it
    // is hidden, so it can be removed rather than leaving a hole.
    if (!editing && hidden(id)) return null;
    const sizing = sizingOf(w);
    // WORDS WITH NO CARD OF THEIR OWN (a heading, a divider) are drawn bare,
    // centred in the cell they are given.
    const bare = w.kind === "heading" || w.kind === "divider";
    return (
      <ArrangeFrame
        key={id}
        {...(editing ? reorder.itemProps(id) : {})}
        data-widget={id}
        className={bare ? "home-board-cell home-board-cell--bare" : "home-board-cell"}
        name={widgetName(w)}
        editing={editing}
        bare={bare}
        selected={settingsId === id}
        columns={stacked ? COLUMNS : editing ? w.w : (drawn.get(id) ?? w.w)}
        rows={w.h}
        rowHeight={ROW}
        fresh={fresh === id}
        handle={reorder.handleProps(id)}
        resize={
          stacked
            ? undefined
            : {
                minColumns: sizing.minW,
                minRows: sizing.minH,
                vertical: sizing.vertical,
                onResize: ({ columns, rows }) =>
                  apply(updateWidget(board, id, (x) => ({ ...x, w: columns, h: rows }) as Widget)),
              }
        }
        actions={toolbarFor(w)}
        onRemove={() => {
          apply(removeWidget(board, id));
          setSettingsId((s) => (s === id ? null : s));
        }}
      >
        <LeafView widget={w} />
      </ArrangeFrame>
    );
  };

  const renderGroup = (g: GroupWidget) => {
    const children = reorder.lists[groupList(g.id)] ?? [];
    return (
      <ArrangeFrame
        key={g.id}
        {...(editing ? reorder.itemProps(g.id) : {})}
        data-widget={g.id}
        className="home-board-cell"
        name={widgetName(g)}
        editing={editing}
        container
        selected={settingsId === g.id}
        columns={stacked ? COLUMNS : g.w}
        rows={null}
        rowHeight={ROW}
        fresh={fresh === g.id}
        handle={reorder.handleProps(g.id)}
        resize={
          stacked
            ? undefined
            : {
                minColumns: sizingOf(g).minW,
                vertical: false,
                onResize: ({ columns }) =>
                  apply(updateWidget(board, g.id, (x) => ({ ...x, w: columns }) as Widget)),
              }
        }
        actions={toolbarFor(g)}
        onRemove={() => apply(removeWidget(board, g.id))}
      >
        <ArrangeGroup
          title={g.title}
          tone={g.tone}
          collapsed={g.collapsed}
          onCollapsedChange={(c) => fold(g, c)}
          count={children.length}
          columns={COLUMNS}
          onTitleChange={
            editing
              ? (title) =>
                  apply(
                    updateWidget(board, g.id, (x) => ({ ...x, title }) as Widget),
                    `title:${g.id}`,
                  )
              : undefined
          }
          bodyProps={editing ? reorder.listProps(groupList(g.id)) : undefined}
          empty={editing ? "Drag widgets here, or add them from the tray." : "This group is empty."}
        >
          {children.map((id) => renderLeaf(id))}
        </ArrangeGroup>
      </ArrangeFrame>
    );
  };

  const page = reorder.lists[PAGE] ?? [];
  // THE WHOLE PAGE IS THE PAGE'S LIST, not only the grid on the board: a
  // widget carried out of a group can be let go anywhere outside one (the
  // strip above the first block, the hatch, the space below the last) and
  // lands where the pointer falls among the page's blocks. The widgets on it
  // share the page's one HomeData.
  return (
    <HomeDataProvider value={data}>
      <div
        ref={root}
        className="home"
        data-stacked={stacked ? "" : undefined}
        {...(editing ? reorder.listProps(PAGE) : {})}
        // Links open in a new tab while arranging, the add sheet's preview
        // included (its portal's events reach this root), so following one
        // never leaves the draft.
        onClickCapture={editing ? openLinksInNewTab : undefined}
      >
        <HomeHead
          data={data}
          editing={editing}
          actions={
            editing ? (
              <EditBar
                changed={changed}
                steps={past.length}
                canRedo={future.length > 0}
                onUndo={undo}
                onRedo={redo}
                onAdd={() => setTrayOpen(true)}
                onCancel={() => (changed ? setConfirmCancel(true) : discard())}
                onDone={finish}
              />
            ) : (
              <Button
                variant="secondary"
                size="small"
                leadingIcon={<LayoutDashboardGlyph size="sm" />}
                onClick={start}
              >
                Edit Home
              </Button>
            )
          }
        />
        <VisuallyHidden id={reorder.hintId}>{reorder.hint}</VisuallyHidden>
        {page.length === 0 ? (
          <EmptyState
            icon={<LayoutGridGlyph size="md" />}
            title="Your Home is empty"
            description="Add widgets, or start from a template."
            action={
              <Button
                variant="primary"
                leadingIcon={<LayoutGridGlyph size="sm" />}
                onClick={() => {
                  if (!editing) start();
                  setTrayOpen(true);
                }}
              >
                Add widgets
              </Button>
            }
          />
        ) : (
          <ArrangeBoard editing={editing} columns={stacked ? 1 : COLUMNS}>
            <ArrangeGrid>
              {page.map((id) => {
                const w = findWidget(board, id);
                if (!w) return null;
                return w.kind === "group" ? renderGroup(w) : renderLeaf(id);
              })}
            </ArrangeGrid>
          </ArrangeBoard>
        )}
        {settingsWidget && (
          <WidgetSettings
            mode="edit"
            widget={settingsWidget}
            onChange={(next) =>
              apply(
                updateWidget(board, next.id, () => next),
                `settings:${next.id}`,
              )
            }
            onDone={() => {
              opened.current = null;
              setSettingsId(null);
            }}
            onCancel={cancelSettings}
          />
        )}
        {pending && editing && (
          <WidgetSettings
            mode="add"
            widget={pending}
            onChange={setPending}
            onAdd={() => {
              apply(addWidget(current.current ?? board, pending));
              setFresh(pending.id);
              setPending(null);
              setTrayOpen(true);
            }}
            onBack={() => {
              setPending(null);
              setTrayOpen(true);
            }}
            onClose={() => setPending(null)}
          />
        )}
        {trayOpen && editing && (
          <WidgetTray
            board={board}
            onAdd={(widget) => {
              // A widget with a choice to make (which page, which saved view)
              // is chosen in a sheet of its own first, with Back to widgets
              // beside Add to Home; it has nothing to show until then.
              if (widget.kind === "widget" && configurable(widget)) {
                setTrayOpen(false);
                setPending(widget);
                return;
              }
              apply(addWidget(current.current ?? board, widget));
              setFresh(widget.id);
            }}
            onTemplate={(next, name) => {
              apply(next);
              toast.show({
                variant: "info",
                title: `${name} applied`,
                message: "Undo restores your previous layout.",
              });
            }}
            onClose={() => setTrayOpen(false)}
          />
        )}
        <ConfirmModal
          open={leaving !== null}
          onClose={() => setLeaving(null)}
          onConfirm={() => {
            const go = leaving;
            discard();
            setLeaving(null);
            go?.leave();
          }}
          title="Leave without saving?"
          message="Your unsaved changes to Home will be lost."
          confirmLabel="Discard and leave"
          cancelLabel="Keep editing"
          destructive
        />
        <ConfirmModal
          open={confirmCancel}
          onClose={() => setConfirmCancel(false)}
          onConfirm={discard}
          title="Discard your changes?"
          message="Home returns to how it was before you started editing."
          confirmLabel="Discard changes"
          cancelLabel="Keep editing"
          destructive
        />
      </div>
    </HomeDataProvider>
  );
}
