/**
 * The application frame: the one sidebar, the page header, the screen and
 * the peek — composed on the kit's `AppShell`.
 *
 * THE KIT OWNS THE FRAME'S GEOMETRY AND ITS NARROW SHAPE. The sidebar stands
 * on the frame and the screen floats beside it on a sheet, inset by
 * `--size-shell-inset` and rounded; below the kit's shell breakpoint the sheet
 * is the window and the sidebar is a modal drawer that takes Escape, traps
 * Tab, hands focus back to its toggle and closes on a navigation. The skip
 * link precedes everything and lands on the one scroller. None of that is
 * written here, because a second copy of it is how the two drift.
 *
 * The shell composes; it does not decide. Where a route goes is
 * `routes.ts`, what the trail says is `crumbs.ts`, which workspaces and
 * sections exist is `nav.ts`, and what the sidebar shows is `sidebar/`. This
 * file wires them together and owns exactly the things nothing else can: the
 * palette, the token dialog, the page context a screen describes itself
 * through, and the peek column.
 *
 * # The peek is a column of the sheet
 *
 * It is the kit's `footer` slot, the one place inside the sheet and outside
 * `main`, and `frame.css` makes the sheet a grid while one is open at a width
 * that can hold it. Below that width the peek is a drawer over the screen. It
 * is not a child of the screen, because a peek mounted per screen is a peek
 * only the screens that remembered to mount one have.
 *
 * THIS FILE DECIDES WHICH, because the width depends on two things only the
 * frame knows: whether the screen draws Settings' section column beside its
 * list, and the reader's density. `app/layout.ts` does the arithmetic and
 * says why a media query in the stylesheet cannot; the result is one
 * attribute, `data-peek="column" | "drawer"`, so the two shapes cannot both
 * hold at one width, or neither.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type ReactNode,
} from "react";
import { AppShell } from "@crewlethq/ui";
import { DESTINATIONS, WORKSPACES, workspaceOf, workspaceRow } from "./nav.ts";
import { samePath, useNavigator, useRoute } from "./router.tsx";
import { crumbsFor, titleOf, type Labels } from "./crumbs.ts";
import { remember } from "~/lib/recents.ts";
import { SCREEN_SCROLL_ID } from "~/lib/scroller.ts";
import { CommandPalette } from "./palette/Palette.tsx";
import { ColumnBoundary, LayerBoundary } from "./boundaries.tsx";
import { NewTaskOpener, type NewTaskPreset } from "./newTask.ts";
import { lazyScreen } from "./lazyScreen.ts";
import { TokenDialog } from "./TokenDialog.tsx";
import { Sidebar } from "./sidebar/Sidebar.tsx";
import { PageHeader, SectionColumn, type PageMenuEntry } from "./header/PageHeader.tsx";
import { PeekHost, PeekNeighbours } from "./frame/PeekHost.tsx";
import { usePeek } from "./frame/DetailRail.tsx";
import { peekable } from "./frame/peeks.tsx";
import { StateBar, degradationOf } from "./frame/StateBar.tsx";
import { useClient, useConnection, useEngineHealth } from "~/lib/store-hooks.ts";
import { ViewerProvider, useViewer } from "~/lib/viewer.ts";
import { InboxCountsProvider } from "~/lib/useInboxCounts.ts";
import { QueueCountProvider } from "~/lib/useQueueCount.ts";
import { useViewerPrefs } from "~/lib/prefs.ts";
import { useMediaQuery } from "~/lib/media.ts";
import { PEEK_WIDTH, columnWidth, densityScale, listReserve, peekColumnMin } from "./layout.ts";
import { onTokenRequested } from "~/protocol/index.ts";
import type { CoverageFacts } from "~/components/work.tsx";
import { useKeymap } from "./keymap.ts";
import { focusSearchTarget } from "./searchTarget.ts";
import { KeyLegend } from "./KeyLegend.tsx";
import { FillRequest } from "./fill.tsx";
import { PeekRestingWidth, PeekWidthRequest } from "./frame/peekWidth.ts";

/**
 * What a screen tells the frame about itself.
 *
 * A SCREEN NEVER RENDERS CHROME. It publishes what the chrome needs — the
 * labels the route cannot supply, and the coverage of the answer it drew from
 * — and the frame draws both in the same place on every screen. The
 * alternative, which this replaces, is twenty screens each drawing their own
 * header, and five of them quietly dropping the coverage badge.
 *
 * A SCREEN'S CONTROLS DO NOT COME THROUGH HERE. They are a portal —
 * `frame/PageActions.tsx`, whose own doc argues the case — and this interface
 * briefly carried a `setActions` beside it that no screen ever called, so the
 * documented route rendered nothing while the working one sat next door.
 */
export interface PageContext {
  setLabels: (labels: Labels) => void;
  setCoverage: (coverage: CoverageFacts | null) => void;
  setCounts: (counts: Record<string, string>) => void;
  setShowWorking: (show: boolean) => void;
  setMenu: (entries: PageMenuEntry[]) => void;
}

const noop: PageContext = {
  setLabels: () => {},
  setCoverage: () => {},
  setCounts: () => {},
  setShowWorking: () => {},
  setMenu: () => {},
};

const PageContextValue = createContext<PageContext>(noop);

/** The frame's own hooks, for a screen to describe itself. */
export function usePageContext(): PageContext {
  return useContext(PageContextValue);
}

/**
 * Publish the labels the breadcrumb needs.
 *
 * A screen calls this with whatever it has resolved — a project's name for its
 * key, a seat's display name for its handle. Until it does, the trail renders
 * the raw segments, which are identifiers a reader can still act on.
 */
export function usePageLabels(labels: Labels): void {
  const { setLabels } = usePageContext();
  const key = JSON.stringify(labels);
  useEffect(() => {
    setLabels(labels);
    // AND THEY GO WHEN THE SCREEN DOES, for the reason `usePageCoverage` below
    // spells out: the Shell is mounted once for the life of the tab and only the
    // screen inside it remounts, so what a screen published is what the frame
    // keeps showing. A label map is keyed on the ROUTE SEGMENT, so a seat's name
    // left behind would title an identically spelled segment on the next screen
    // — a node id, a tool name — as that person. A cleanup here runs in the
    // right order (outgoing destroy before incoming create) and both writes land
    // in one batch, so nothing flickers through the raw segment.
    return () => setLabels({});
    // The labels object is rebuilt every render by most callers, so the
    // dependency is its CONTENT: an identity dependency here is an infinite
    // render loop, and one on `labels` alone would be exactly that.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, setLabels]);
}

/**
 * Publish the coverage of the answer this screen was drawn from.
 *
 * THE COVERAGE BELONGS TO THE SCREEN AND THE FRAME OUTLIVES IT, which is why
 * the effect returns a reset. The Shell never unmounts — only the screen
 * inside it does — so without one the inbox's freshness badge and its "this
 * answer is incomplete" banner stayed in the state bar over every screen the
 * reader visited afterwards, as claims about data those screens never read.
 * On Settings › Secrets, which somebody opens precisely to judge whether what
 * they are seeing is trustworthy, that is the worst possible stale value.
 *
 * A reset in the Shell keyed on the route would not do: React flushes a
 * child's effects before a parent's, so it would wipe the coverage the
 * incoming screen had just published. A cleanup here runs in the right order —
 * the outgoing screen's destroy before the incoming screen's create — and
 * both writes land in one batch, so nothing flickers through null.
 */
export function usePageCoverage(coverage: CoverageFacts | null | undefined): void {
  const { setCoverage } = usePageContext();
  const level = coverage?.read_level;
  const complete = coverage?.complete;
  const applied = coverage?.applied_through;
  const seq = coverage?.log_seq;
  useEffect(() => {
    setCoverage(coverage ?? null);
    return () => setCoverage(null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [level, complete, applied, seq, setCoverage]);
}

/**
 * Publish each section's figure, for the page header's section tabs.
 *
 * THE SCREEN OWNS THE NUMBER. Only the screen knows whether a count is a
 * total, a floor the engine stopped counting at, or not answered yet — so it
 * hands the header STRINGS it has already written ("12", "20+"), keyed on the
 * section, and omits a section it has no figure for. Cleared when the screen
 * goes, for the reason [usePageLabels] gives: the frame outlives it.
 */
export function useSectionCounts(counts: Record<string, string>): void {
  const { setCounts } = usePageContext();
  const key = JSON.stringify(counts);
  useEffect(() => {
    setCounts(counts);
    return () => setCounts({});
    // The dependency is the CONTENT, for the reason `usePageLabels` gives.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, setCounts]);
}

/**
 * Draw "who is working" in the page bar — Home's, and nobody else's.
 *
 * ONE PAGE ASKS FOR IT, the way the approved Main artboard draws it: the
 * company's pulse belongs at the head of the page a reader opens to see how
 * the company is. Everywhere else the sidebar's Agents badge already carries
 * the count, and the chip drawn in every bar was a second copy of it — on a
 * task page or a profile it sat between the trail and the page's own actions,
 * and at a phone's width it pushed them past the edge. Cleared when the screen
 * goes, for the reason [usePageLabels] gives: the frame outlives it.
 */
export function useWorkingNow(): void {
  const { setShowWorking } = usePageContext();
  useEffect(() => {
    setShowWorking(true);
    return () => setShowWorking(false);
  }, [setShowWorking]);
}

/**
 * Put a screen's SECONDARY actions in the page bar's "More" menu on a phone.
 *
 * ON A PHONE'S WIDTH THE BAR KEEPS ONE ACTION IN VIEW — the screen's primary
 * one — and folds the rest into one menu at its end: the frame's own star and
 * Copy link, and whatever a screen publishes here. A screen drawing a
 * secondary control inline (Edit project) marks it `page-action-folds` so the
 * bar hides it at that width, and publishes the same action here so it is
 * still one press away. Laid out whole, a project's bar stood three rows tall
 * on a 390px screen — the trail, the lenses, then four controls — about 105px
 * of chrome above the work.
 *
 * A HOOK, where the controls themselves are a portal: a menu's items are
 * values the kit's `Menu` lays out, not elements a screen could render into
 * it. Every press goes through the entry the screen published LAST, so a
 * callback that closes over newer state is the one that runs; what is laid
 * out is re-published only when a label, a description or a disabled state
 * moves. Cleared when the screen goes, for the reason [usePageLabels] gives.
 */
export function usePageMenu(entries: readonly PageMenuEntry[]): void {
  const { setMenu } = usePageContext();
  const latest = useRef(entries);
  useLayoutEffect(() => {
    latest.current = entries;
  });
  const key = JSON.stringify(
    entries.map((e) => [e.key, e.label, e.disabled ?? false, e.description ?? ""]),
  );
  useEffect(() => {
    setMenu(
      latest.current.map((entry) => ({
        ...entry,
        onSelect: () => latest.current.find((e) => e.key === entry.key)?.onSelect(),
      })),
    );
    return () => setMenu([]);
    // The dependency is the CONTENT, for the reason `usePageLabels` gives.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, setMenu]);
}

/**
 * What the frame reads ONCE for every surface in it: who this browser is,
 * how much is waiting on them, and how much open work is on them.
 *
 * All three are standing reads, and the first two used to be made per caller
 * — the viewer by the frame, the sidebar and the Inbox count, the count by the
 * sidebar, Home and the Inbox — so one tab held up to six slots of the
 * socket's four on two facts, and the badge and the screen beside it polled on
 * separate minutes and could name two numbers. The third is the same case: the
 * sidebar's My work figure and the Queue tab on the reader's own day are one
 * number (`lib/useQueueCount.ts`). Mounted here, every caller reads one
 * answer. A suite that mounts a screen without the frame mounts this around
 * it, because a hook that fell back to its own read would bring the
 * per-caller reads back without a sound.
 */
export function FrameReadings({ children }: { children: ReactNode }) {
  return (
    <ViewerProvider>
      <InboxCountsProvider>
        <QueueCountProvider>{children}</QueueCountProvider>
      </InboxCountsProvider>
    </ViewerProvider>
  );
}

export function Shell({ children }: { children: ReactNode }) {
  return (
    <FrameReadings>
      <Frame>{children}</Frame>
    </FrameReadings>
  );
}

function Frame({ children }: { children: ReactNode }) {
  const route = useRoute();
  const peek = usePeek();
  const nav = useNavigator();
  const { socket } = useClient();
  const { connected, authRejected } = useConnection();
  const viewer = useViewer();
  const engine = useEngineHealth();
  // A ZONE OR A DATE FORMAT CHANGED IN THE PREFERENCES REPAINTS EVERY
  // TIMESTAMP. The formatters read the preference when they are called, so
  // the frame subscribes and re-renders; the screen is a child of this render
  // and re-renders with it.
  const prefs = useViewerPrefs();

  // OPEN OR NOT: ⌘K opens it on All, and so does every other way in.
  const [paletteOpen, setPaletteOpen] = useState(false);
  // A SCREEN THAT ASKED FOR THE HEIGHT INSTEAD OF THE SCROLL — see
  // `app/fill.tsx` for why this is a request and not a selector. The kit's
  // `fill` is what honours it: the scroller stops scrolling and the content
  // column takes the height that is left.
  const [filling, setFilling] = useState(false);
  // AND ONE THAT ASKED FOR A NARROWER PEEK — a canvas, which the rail shrinks
  // rather than narrows. See `app/frame/peekWidth.ts`.
  const [restingPeek, setRestingPeek] = useState<number | null>(null);
  const [tokenOpen, setTokenOpen] = useState(false);
  // THE NEW TASK SHEET, and what the door it was opened from knows. The frame
  // holds it for the reason it holds the palette: four doors — the sidebar's
  // head, the Projects group, a screen's page bar, a board lane — open ONE
  // sheet, and a route change does not close it (the reader may have followed
  // a link to check a name while filing).
  const [newTask, setNewTask] = useState<NewTaskPreset | null>(null);
  const openNewTask = useCallback((preset: NewTaskPreset = {}) => setNewTask(preset), []);

  const [labels, setLabels] = useState<Labels>({});
  const [coverage, setCoverage] = useState<CoverageFacts | null>(null);
  const [counts, setCounts] = useState<Record<string, string>>({});
  const [showWorking, setShowWorking] = useState(false);
  const [menu, setMenu] = useState<PageMenuEntry[]>([]);

  // The socket asks ONCE per refusal — a reconnect backoff must not reopen a
  // dialog forever. Everything after that is the state bar.
  useEffect(() => {
    socket.onAuthRejected(() => setTokenOpen(true));
  }, [socket]);

  // And from anywhere else that discovers it needs a credential — an
  // auth-gated answer on a screen the socket was never refused for.
  useEffect(() => onTokenRequested(() => setTokenOpen(true)), []);

  // THE FRAME'S KEYS, by row — `keymap.ts` says which key each one is.
  const [legendOpen, setLegendOpen] = useState(false);
  useKeymap({
    // IT OPENS, and closing is the palette's own: while it is up it is a
    // modal layer, every page key stands aside, and it answers this row
    // itself (see `palette/Palette.tsx`).
    palette: () => setPaletteOpen(true),
    // `/` IS "SEARCH WHAT I AM LOOKING AT": the screen's own box where it
    // registered one, the palette where it did not. It opened the palette
    // everywhere, so on the eight screens with a search box of their own the
    // key a reader pressed to search the screen took them away from it.
    search: () => {
      if (!focusSearchTarget()) setPaletteOpen(true);
    },
    keys: () => setLegendOpen(true),
    ...Object.fromEntries(WORKSPACES.map((ws) => [`go.${ws.key}`, () => nav.to(ws.path)])),
  });

  // THE PALETTE CLOSES whenever the route moves: picking a row closes it on
  // the way out, and this covers every OTHER way — Back and Forward, a
  // phone's back gesture, a restored history entry — where a palette left
  // open would go on offering what it ranked for the screen just left. The
  // drawer is the kit's, and `navigationKey` closes it the same way. NOT THE
  // TOKEN DIALOG: a credential prompt is about the reader's access rather
  // than where they are, and dismissing it on a navigation would lose the one
  // thing the socket asked for.
  useEffect(() => {
    setPaletteOpen(false);
  }, [route.hash]);

  const workspace = workspaceOf(route.path);
  const row = workspaceRow(workspace);

  // WHICH SHAPE THE PEEK TAKES — see the file's doc. The frame is described
  // by what stands in front of the list, and the width asked is the one at
  // which this frame, at this density, still leaves the list its floor.
  const column = columnWidth(row?.renderer);
  const frame = { column, scale: densityScale(prefs.density) };
  const roomy = useMediaQuery(`(width >= ${peekColumnMin(frame)}px)`);
  const peekShape = peekable(peek) ? (roomy ? "column" : "drawer") : undefined;
  const crumbs = useMemo(() => crumbsFor(route.path, labels), [route.path, labels]);

  // THE TAB SAYS WHERE YOU ARE, so a reader with four tabs open can tell
  // them apart.
  const where = titleOf(crumbs);
  useEffect(() => {
    document.title = where === "Crewlet" ? "Crewlet" : `${where} · Crewlet`;
  }, [where]);

  // AND THE PALETTE REMEMBERS THE OBJECTS, so an empty palette has something
  // to offer before the reader types. OBJECTS ONLY — a fixed destination is in
  // the palette already, and a recents list repeating the navigation beside
  // it costs a reader a scan and tells them nothing. The label is the one the
  // SCREEN resolved, so this depends on the title rather than on the path;
  // `named` says whether a screen supplied it, asked of `labels` rather than
  // of a flag each crumb branch would have to set.
  const path = route.path;
  const named = useMemo(() => Object.values(labels).includes(where), [labels, where]);
  useEffect(() => {
    if (path.length < 2) return;
    if (DESTINATIONS.some((d) => samePath(d.path, path))) return;
    if (where === "Crewlet" || where === "Not found") return;
    if (!workspace) return;
    remember({ path, label: where, workspace }, named);
  }, [path, where, named, workspace]);

  const page: PageContext = useMemo(
    () => ({ setLabels, setCoverage, setCounts, setShowWorking, setMenu }),
    [],
  );

  const openToken = useCallback(() => setTokenOpen(true), []);
  const degraded = degradationOf({
    authRejected,
    connected,
    configured: engine?.configured,
    onSetToken: openToken,
    onConfig: () => nav.to(["settings", "config"]),
  });

  // THE SETTINGS COLUMN'S FIGURES come off the health push the frame already
  // holds — nothing here polls.
  const figures: Record<string, string> = {};
  if (engine?.nodes !== undefined) figures.nodes = String(engine.nodes);
  // "epoch 2", not "e2": a figure beside a row says what it counts, and a
  // letter and a number is a code the reader has to be told.
  if (engine?.applied_epoch !== undefined) figures.config = `epoch ${engine.applied_epoch}`;

  const screen = (
    <FillRequest.Provider value={setFilling}>
      <PeekWidthRequest.Provider value={setRestingPeek}>
        <PageContextValue.Provider value={page}>{children}</PageContextValue.Provider>
      </PeekWidthRequest.Provider>
    </FillRequest.Provider>
  );

  return (
    // THE STEPPER'S ORDER, published by whichever list the reader is on, so
    // `[` and `]` walk the rows they are actually looking at. See `PeekHost`.
    <NewTaskOpener.Provider value={openNewTask}>
      <PeekNeighbours>
        <AppShell
          className="app"
          // THE SHEET GAINS ITS PEEK COLUMN ONLY WHILE A PEEK IS OPEN — an
          // empty column would take its width from every screen that never
          // opens one — and only where the frame has room for it; see
          // `.app[data-peek]` in frame.css. The reserve is what the column's
          // track leaves the list, so a dragged width cannot take it back.
          data-peek={peekShape}
          style={
            peekShape === "column"
              ? ({ "--peek-reserve": `${listReserve(frame)}px` } as CSSProperties)
              : undefined
          }
          mainId={SCREEN_SCROLL_ID}
          fill={filling}
          navigationKey={route.hash}
          toggleLabel="Navigation"
          sidebar={<Sidebar onSearch={() => setPaletteOpen(true)} onSetToken={openToken} />}
          topbar={
            <PageHeader
              crumbs={crumbs}
              row={row}
              counts={counts}
              title={where}
              workspace={workspace}
              working={showWorking}
              menu={menu}
            >
              <StateBar degraded={degraded} coverage={coverage} />
            </PageHeader>
          }
          footer={
            <PeekRestingWidth.Provider value={restingPeek ?? PEEK_WIDTH}>
              <PeekHost />
            </PeekRestingWidth.Provider>
          }
        >
          {row && column > 0 ? (
            // THE COLUMN'S WIDTH IS THE ONE THE PEEK ARITHMETIC COUNTED
            // ([columnWidth]), handed to the grid rather than written again
            // in the stylesheet, so the two cannot drift.
            <div
              className="section-frame"
              style={{ "--section-column": `${column}px` } as CSSProperties}
            >
              {row.renderer === "tree" ? (
                <ColumnBoundary what="The knowledge tree" resetKey={row.key}>
                  <KnowledgeTree />
                </ColumnBoundary>
              ) : (
                <SectionColumn
                  row={row}
                  path={route.path}
                  operator={viewer.operator}
                  figures={figures}
                />
              )}
              <div className="section-body">{screen}</div>
            </div>
          ) : (
            screen
          )}
        </AppShell>

        {/* THE PALETTE HAS A BOUNDARY OF ITS OWN, and it is mounted only while
          open, so a palette that threw is a dialog saying so — closed the
          way the palette closes — and the next ⌘K is a fresh palette. */}
        {paletteOpen && (
          <LayerBoundary title="Search" onClose={() => setPaletteOpen(false)}>
            <CommandPalette
              onClose={() => setPaletteOpen(false)}
              onShowKeys={() => setLegendOpen(true)}
            />
          </LayerBoundary>
        )}
        {/* THE SHEET'S CODE IS THE WORK WORKSPACE'S CHUNK — it is a Work form,
          and the frame's own entry stays the frame — so it is mounted only
          while open, through the same loader every screen uses. */}
        {newTask !== null && (
          <LayerBoundary title="New task" onClose={() => setNewTask(null)}>
            <NewTaskSheet preset={newTask} onClose={() => setNewTask(null)} />
          </LayerBoundary>
        )}
        {legendOpen && <KeyLegend onClose={() => setLegendOpen(false)} />}
        {tokenOpen && (
          <TokenDialog
            onClose={() => setTokenOpen(false)}
            onSaved={(token) => {
              socket.setToken(token);
              socket.reconnect();
            }}
          />
        )}
      </PeekNeighbours>
    </NewTaskOpener.Provider>
  );
}

/**
 * Knowledge's tree, out of the Knowledge chunk: the one `tree` renderer, and
 * it is the workspace's own code — it reads spaces and pages — so the frame
 * loads it the way it loads a screen rather than carrying it in the entry.
 */
const KnowledgeTree = lazyScreen("knowledge", (module) => module.KnowledgeTree);

/** The New task sheet, out of the Work chunk. See [LayerBoundary]. */
const NewTaskSheet = lazyScreen("work", (module) => module.NewTaskSheet);
