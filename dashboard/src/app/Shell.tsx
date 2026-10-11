/**
 * The application frame: the sidebar of the shell the address is in, the page
 * header, the screen and the peek — composed on the kit's `AppShell`.
 *
 * # Two shells, one frame
 *
 * The address picks the shell (`nav.ts`'s `shellOf`): the person shell's
 * sidebar (`sidebar/PersonSidebar.tsx`) everywhere but `#/admin/…`, and the
 * console's (`console/ConsoleSidebar.tsx`, out of the console's own chunk)
 * there. Everything else in the frame — the header, the peek, the palette,
 * the keys — is one frame for both, because a second frame is a second place
 * each of them would be wired.
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
 * sections exist is `nav.ts`, and what each sidebar shows is `sidebar/` and
 * `console/`. This file wires them together and owns exactly the things
 * nothing else can: the palette, the token dialog, the page context a screen
 * describes itself through (`page.tsx`), and the peek column.
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
 * frame knows: whether the screen draws Knowledge's tree beside its list, and
 * the reader's density. `app/layout.ts` does the arithmetic and
 * says why a media query in the stylesheet cannot; the result is one
 * attribute, `data-peek="column" | "drawer"`, so the two shapes cannot both
 * hold at one width, or neither.
 */

import {
  useCallback,
  useEffect,
  useMemo,
  useState,
  type CSSProperties,
  type ReactNode,
} from "react";
import { AppShell } from "@crewlethq/ui";
import {
  DESTINATIONS,
  WORKSPACES,
  adminPathOf,
  personPathOf,
  shellOf,
  workspaceOf,
  workspaceRow,
} from "./nav.ts";
import { samePath, useNavigator, useRoute } from "./router.tsx";
import { crumbsFor, titleOf, type Labels } from "./crumbs.ts";
import { remember } from "~/lib/recents.ts";
import { SCREEN_SCROLL_ID } from "~/lib/scroller.ts";
import { CommandPalette } from "./palette/Palette.tsx";
import { ColumnBoundary, LayerBoundary } from "./boundaries.tsx";
import { NewTaskOpener, type NewTaskPreset } from "./newTask.ts";
import { lazyScreen } from "./lazyScreen.ts";
import { TokenDialog } from "./TokenDialog.tsx";
import { PersonSidebar } from "./sidebar/PersonSidebar.tsx";
import { PageHeader, type PageMenuEntry } from "./header/PageHeader.tsx";
import { PageContextValue, type PageContext } from "./page.tsx";
import { PeekHost, PeekNeighbours } from "./frame/PeekHost.tsx";
import { usePeek } from "./frame/DetailRail.tsx";
import { peekable } from "./frame/peeks.tsx";
import { StateBar, degradationOf } from "./frame/StateBar.tsx";
import { useClient, useConnection, useEngineHealth } from "~/lib/store-hooks.ts";
import { ViewerProvider, useViewer } from "~/lib/viewer.ts";
import { InboxCountsProvider } from "~/lib/useInboxCounts.ts";
import { useViewerPrefs } from "~/lib/prefs.ts";
import { useMediaQuery } from "~/lib/media.ts";
import { PEEK_WIDTH, columnWidth, densityScale, listReserve, peekColumnMin } from "./layout.ts";
import { onTokenRequested } from "~/protocol/index.ts";
import { routeProject } from "./palette/hits.ts";
import type { CoverageFacts } from "~/components/work.tsx";
import { useKeymap } from "./keymap.ts";
import { runningTarget } from "~/lib/turns.ts";
import { focusSearchTarget } from "./searchTarget.ts";
import { KeyLegend } from "./KeyLegend.tsx";
import { FillRequest } from "./fill.tsx";
import { PeekRestingWidth, PeekWidthRequest } from "./frame/peekWidth.ts";

/**
 * What the frame reads ONCE for every surface in it: who this browser is, and
 * how much is waiting on them.
 *
 * Both are standing reads, and both used to be made per caller — the viewer
 * by the frame, the sidebar and the Inbox count, the count by the sidebar,
 * the Overview and the Inbox — so one tab held up to six slots of the socket's four
 * on two facts, and the badge and the screen beside it polled on separate
 * minutes and could name two numbers. Mounted here, every caller reads one
 * answer. A suite that mounts a screen without the frame mounts this around
 * it, because a hook that fell back to its own read would bring the
 * per-caller reads back without a sound.
 */
export function FrameReadings({ children }: { children: ReactNode }) {
  return (
    <ViewerProvider>
      <InboxCountsProvider>{children}</InboxCountsProvider>
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
  const { socket, store } = useClient();
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
  // holds it for the reason it holds the palette: every door — the sidebar's
  // Hand off, its key, a screen's page bar, a board lane — opens ONE sheet,
  // and a route change does not close it (the reader may have followed a link
  // to check a name while filing).
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

  // THE FRAME'S KEYS, by row — `keymap.ts` says which key each one is. An
  // admin's keys are bound for an admin only (`KeyRow.admin`): a member
  // pressing a console chord would be taken to an address their key does not
  // reach.
  const [legendOpen, setLegendOpen] = useState(false);
  const shell = shellOf(route.path);
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
    ...Object.fromEntries(
      WORKSPACES.map((ws) => [
        `go.${ws.key}`,
        { run: () => nav.to(ws.path), when: ws.shell === "person" || viewer.admin },
      ]),
    ),
    // THE OTHER SHELL'S TWIN OF WHERE THE READER IS, both ways — the doors'
    // own rule (`adminPathOf`, `personPathOf`), so the key and the doors
    // cannot disagree about where a place's twin is.
    "shell.swap": {
      run: () => {
        const crossing =
          shell === "admin" ? personPathOf(route.path, route.query) : adminPathOf(route.path);
        nav.to(crossing.path, crossing.query);
      },
      when: viewer.admin,
    },
    // READ AT THE PRESS, off the store, rather than through `useAgents`: the
    // agents push lands twice per tool-loop round, and a subscription here
    // would re-render the whole frame on every one of them for a key pressed
    // once in a while.
    running: {
      run: () => {
        const target = runningTarget(store.state.agents);
        nav.to(target.path, target.query);
      },
      when: viewer.admin,
    },
    handoff: () => openNewTask({ project: routeProject(route.path) || undefined }),
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
  const peekShape = peekable(peek, shell) ? (roomy ? "column" : "drawer") : undefined;
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
    onConfig: () => nav.to(["admin", "engine", "config"]),
  });

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
          // WHICH SHELL THE FRAME IS DRAWING, for the stylesheet: the console
          // and the person shell are one frame, told apart by their ground.
          data-shell={shell}
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
          sidebar={
            shell === "admin" ? (
              <ColumnBoundary what="The admin console's navigation" resetKey="console">
                <ConsoleSidebar onSearch={() => setPaletteOpen(true)} onSetToken={openToken} />
              </ColumnBoundary>
            ) : (
              <PersonSidebar onSearch={() => setPaletteOpen(true)} onSetToken={openToken} />
            )
          }
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
              <ColumnBoundary what="The knowledge tree" resetKey={row.key}>
                <KnowledgeTree />
              </ColumnBoundary>
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
        {legendOpen && <KeyLegend admin={viewer.admin} onClose={() => setLegendOpen(false)} />}
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

/**
 * The console's sidebar, out of the console's own frame chunk: a member's
 * browser never downloads it (`lazyScreen.ts`).
 */
const ConsoleSidebar = lazyScreen("console", (module) => module.ConsoleSidebar);
