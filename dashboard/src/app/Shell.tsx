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
 * palette, the watch on the viewer's own inbox, the strip that reports a
 * degraded connection, the page context a screen describes itself through
 * (and what stands over the page, told back to it: [usePageInView]), and the
 * peek column. Who is signed in is the sidebar's user block
 * (`sidebar/UserBlock.tsx`), and signing in is a screen outside the frame
 * (`App.tsx`): the frame holds no credential and asks for none, because the
 * browser's only credential is the session cookie the engine set.
 *
 * # The peek is a column of the sheet
 *
 * It is the kit's `peek` slot, beside `main` inside the sheet and under the
 * bar, and the kit lays it out as a column while one is open at a width that
 * can hold it. Below that width the peek is a drawer over the screen. It is
 * not a child of the screen, because a peek mounted per screen is a peek only
 * the screens that remembered to mount one have.
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
  useSyncExternalStore,
  type ReactNode,
} from "react";
import { AppShell, SectionFrame, useMediaMatch, useStoredWidth } from "@crewlethq/ui";
import {
  DESTINATIONS,
  WORKSPACES,
  landingPath,
  serviceShown,
  workspaceOf,
  workspaceRow,
} from "./nav.ts";
import { useServicesRun, useServicesAsk } from "~/lib/services.ts";
import { href, samePath, useNavigator, useRoute } from "./router.tsx";
import { crumbsFor, titleOf, type Labels } from "./crumbs.ts";
import { remember } from "~/lib/recents.ts";
import { SCREEN_SCROLL_ID } from "~/lib/scroller.ts";
import { AsideBoundary, LayerBoundary } from "./boundaries.tsx";
import { NewTaskOpener, type NewTaskPreset } from "./newTask.ts";
import { lazyScreen } from "./lazyScreen.ts";
import { Sidebar } from "./sidebar/Sidebar.tsx";
import { useSettingsSidebar } from "./sidebar/settingsFigures.tsx";
import { PageHeader, SectionColumn, type PageMenuEntry } from "./header/PageHeader.tsx";
import { ProjectSwitcherProvider } from "./header/ProjectSwitcher.tsx";
import { scopeAt } from "./scope.ts";
import { PeekHost, PeekNeighbours } from "./frame/PeekHost.tsx";
import { usePeek } from "./frame/peekAddress.ts";
import { peekable } from "./frame/peeks.tsx";
import { StateBar, degradationOf } from "./frame/StateBar.tsx";
import { useClient, useConnection, useEngineHealth } from "~/lib/store-hooks.ts";
import { LinkHostsProvider } from "~/lib/linkHosts.ts";
import { ProjectKeysProvider } from "~/lib/projectKeys.ts";
import { RoomNamesProvider } from "~/lib/roomNames.ts";
import { ViewerProvider, useHeldGrants, useViewer } from "~/lib/viewer.ts";
import { SessionReading, useFrameSession } from "~/lib/frameSession.ts";
import { InboxCountsProvider } from "~/lib/useInboxCounts.ts";
import { QueueCountProvider } from "~/lib/useQueueCount.ts";
import { useViewerPrefs } from "~/lib/prefs.ts";
import {
  ASSISTANT_MAX,
  ASSISTANT_MIN,
  ASSISTANT_WIDTH,
  PEEK_WIDTH,
  PHONE_BREAKPOINT,
  assistantColumnMin,
  assistantCounted,
  assistantMaxWidth,
  assistantOccupied,
  assistantTrack,
  columnWidth,
  densityScale,
  listReserve,
  peekColumnMin,
  sectionColumnMin,
  sheetFloor,
  useViewportWidth,
} from "./layout.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { ownName } from "~/lib/viewer.ts";
import { assistantModelFor, assistantUnavailableFor } from "./assistant/availability.ts";
import {
  ASSISTANT_COMPOSER_ID,
  AssistantControlContext,
  queueQuestion,
  type AssistantFocus,
} from "./assistant/control.ts";
import { framePageOf, setFramePage } from "./assistant/context.ts";
import { on as onAssistantBus } from "./assistant/bus.ts";
import { readTab, writeTab } from "./assistant/tab.ts";
import { resolves } from "./routes.ts";
import { useToastInset } from "./toastInset.tsx";
import { goSignIn } from "~/lib/session.ts";
import type { CoverageFacts } from "~/components/work.tsx";
import type { FleetAnswer } from "~/protocol/index.ts";
import { useKeymap } from "./keymap.ts";
import { useModalLayerOpen } from "~/lib/keys.ts";
import { runningTarget } from "~/lib/turns.ts";
import { focusSearchTarget } from "./searchTarget.ts";
import { KeyLegend } from "./KeyLegend.tsx";
import { FillRequest } from "./fill.tsx";
import { OwnHeaderRequest, OwnHeaderSlot } from "./frame/OwnHeader.tsx";
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
  setFleet: (fleet: PublishedFleet | null) => void;
  /** The screen's design keeps the star in its own menu, not the bar ([useOwnStar]). */
  setOwnStar: (own: boolean) => void;
}

/**
 * The `fleet` answer a screen is already holding, handed to the frame — or
 * null when no screen on the page reads it. `answer` is null while that
 * screen's first answer is in flight: the screen OWNS the reading from the
 * moment it mounts, so the column does not start a query of its own for the
 * second or so before it lands.
 */
export interface PublishedFleet {
  answer: FleetAnswer | null;
}

const noop: PageContext = {
  setLabels: () => {},
  setCoverage: () => {},
  setCounts: () => {},
  setShowWorking: () => {},
  setMenu: () => {},
  setFleet: () => {},
  setOwnStar: () => {},
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
 * Hand the frame the `fleet` answer this screen polls, for the Settings
 * column's Nodes figure.
 *
 * ONE READING, NOT TWO: the column says "n behind on config" as a mirror of
 * the Nodes screen's own "Behind on config" tile, and with a poll of its own
 * on a different clock the two disagreed for up to a poll interval while every
 * tick sent the operator's engine the same question twice. While a screen has
 * published here the column asks nothing and draws this answer. Cleared when
 * the screen goes, for the reason [usePageLabels] gives: the frame outlives it.
 */
export function usePublishFleet(answer: FleetAnswer | null): void {
  const { setFleet } = usePageContext();
  useEffect(() => {
    setFleet({ answer });
  }, [answer, setFleet]);
  useEffect(() => () => setFleet(null), [setFleet]);
}

/**
 * Draw "who is working" in the page bar — Home's, and nobody else's.
 *
 * ONE PAGE ASKS FOR IT, the way the approved Main artboard draws it: the
 * company's pulse belongs at the head of the page a reader opens to see how
 * the company is. Everywhere else the sidebar's Live row already carries the
 * count, and the chip drawn in every bar was a second copy of it — on a
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
 * Say that this screen's own menu carries the page's star, so the frame draws
 * none: one control per action on screen, and a screen whose design keeps the
 * star with its other page actions (Doclet's page) would otherwise show it
 * twice. Doclet's landing says the same, because its design keeps the star
 * in each page's own menu and none in its bar, where the frame's would offer
 * the workspace the sidebar already lists. Cleared when the screen goes, for
 * the reason [usePageLabels] gives.
 */
export function useOwnStar(): void {
  const { setOwnStar } = usePageContext();
  useEffect(() => {
    setOwnStar(true);
    return () => setOwnStar(false);
  }, [setOwnStar]);
}

/**
 * Whether something stands OVER the page, so the screen under it is not in
 * view: the assistant anywhere but its column, or a modal surface of the
 * kit's (a dialog, a sheet, the navigation drawer, the palette). The frame
 * publishes it ([PageCover]); outside a frame nothing covers the page.
 */
const PageCovered = createContext(false);

/**
 * Whether the screen is IN VIEW: the tab is in front, and nothing stands over
 * the page ([PageCovered]).
 *
 * FOR A SCREEN THAT WRITES DOWN WHAT ITS READER HAS SEEN, a chat room's read
 * cursor (`routes/chatlet/Room.tsx`). A row drawn in a tab in the background,
 * under the assistant or behind a dialog is drawn for nobody, and counted as
 * seen it clears a badge on every device over messages nobody read. A value
 * the screen re-renders on, so what it drew meanwhile can count the moment
 * the page is back in view.
 */
export function usePageInView(): boolean {
  const covered = useContext(PageCovered);
  const visible = useSyncExternalStore(onVisibilityChange, tabInFront, () => true);
  return visible && !covered;
}

/** Whether the tab is in front: a hidden one is drawing for nobody. */
function tabInFront(): boolean {
  return document.visibilityState !== "hidden";
}

function onVisibilityChange(notify: () => void): () => void {
  document.addEventListener("visibilitychange", notify);
  return () => document.removeEventListener("visibilitychange", notify);
}

/**
 * The page's [PageCovered], published around the screen: the assistant over
 * the page (`over`, the frame's own state) or any modal surface on the kit's
 * layer stack ([useModalLayerOpen]).
 *
 * ITS OWN COMPONENT, because the stack's feed re-renders whatever reads it on
 * every change, a menu opening included: here that is one small render, and
 * the screen under it re-renders only when the answer moves.
 */
function PageCover({ over, children }: { over: boolean; children: ReactNode }) {
  const modal = useModalLayerOpen();
  return <PageCovered.Provider value={over || modal}>{children}</PageCovered.Provider>;
}

/**
 * What the frame reads ONCE for every surface in it: who this browser is,
 * how much is waiting on them, how much open work is on them, and where the
 * company's apps live, which project keys are real and which rooms a `#name`
 * may be, which every rendered link, key and room reads (`lib/linkHosts.ts`,
 * `lib/projectKeys.ts`, `lib/roomNames.ts`).
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
        <QueueCountProvider>
          <LinkHostsProvider>
            <ProjectKeysProvider>
              <RoomNamesProvider>{children}</RoomNamesProvider>
            </ProjectKeysProvider>
          </LinkHostsProvider>
        </QueueCountProvider>
      </InboxCountsProvider>
    </ViewerProvider>
  );
}

/**
 * The frame, and the one thing only the frame does: DIAL THE SOCKET.
 *
 * The socket is made at boot and lives for the tab (`main.tsx`), and it is
 * dialled here because the frame is the one part of this dashboard that needs
 * a session. Dialled at boot, every signed-out page — the sign-in, an
 * invitation's, a reset link's — dialled `/ws/stream` once and read its
 * refusal and the degraded-mode snapshot: three `401`s in the console of a page
 * that needs nobody. A sign-in does not dial either: it lands here, and the
 * frame dials for the session it reads.
 *
 * AND ONLY FOR SOMEBODY. The frame asks `GET /auth/session` first
 * (`lib/frameSession.ts`): a `401` is nobody, which `rest.ts` turns into the
 * sign-in the app follows, and nothing is dialled; any session is dialled,
 * whatever access it holds, because the socket serves everybody the overview
 * and decides each push by the reader's access; any other failure says
 * nothing about a session, and the socket — which holds a dial for a tab that
 * has recorded no reader behind a session read of its own (`learnReader` in
 * `protocol/socket.ts`) — decides.
 */
export function Shell({ children }: { children: ReactNode }) {
  return (
    <SessionReading>
      <FrameReadings>
        {/* WORKLET'S SCOPE SWITCHER: the panel the trail's last crumb opens
            on Worklet's pages, and the same panel centred for G then P
            anywhere. */}
        <ProjectSwitcherProvider>
          <Frame>{children}</Frame>
        </ProjectSwitcherProvider>
      </FrameReadings>
    </SessionReading>
  );
}

function Frame({ children }: { children: ReactNode }) {
  const route = useRoute();
  const peek = usePeek();
  const nav = useNavigator();
  const { socket, store } = useClient();
  const { connected, authRejected, accessRefused } = useConnection();
  const viewer = useViewer();
  const engine = useEngineHealth();
  const session = useFrameSession();
  // WHAT THE READER HOLDS — the frame's one reading (`useHeldGrants`), which
  // Settings' column locks its sections by, as the sidebar locks its rows.
  // Read off the viewer alone, the column drew no lock at all until the
  // socket answered.
  const held = useHeldGrants();

  // DIAL FOR SOMEBODY — see [Shell] — when the session is first read. A read
  // that failed on anything but a `401` says nothing about the session, and
  // the socket reads it again itself before it dials.
  const serves = session.answer !== null;
  const undecided =
    session.answer === null && session.error !== null && session.error.status !== 401;
  useEffect(() => {
    if (serves || undecided) socket.start();
  }, [serves, undecided, socket]);
  // A REFUSAL THE SOCKET MET IS ASKED ABOUT AGAIN: a seat taken out of the
  // chart closes an open socket, and the session read before it still names
  // the seat — so the panel would say who the reader is from a reading the
  // refusal has just made stale.
  const { reload: readSessionAgain } = session;
  useEffect(() => {
    if (accessRefused !== null) void readSessionAgain();
  }, [accessRefused, readSessionAgain]);
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
  // THE NEW TASK SHEET, and what the door it was opened from knows. The frame
  // holds it for the reason it holds the palette: every door (a screen's page
  // bar, a board lane, a seat's Message) opens ONE sheet, and a route change
  // does not close it (the reader may have followed a link to check a name
  // while filing).
  const [newTask, setNewTask] = useState<NewTaskPreset | null>(null);
  const openNewTask = useCallback((preset: NewTaskPreset = {}) => setNewTask(preset), []);
  // THE ASSISTANT, OPEN OR NOT, and where focus goes as it opens. It follows
  // the New task sheet's rule rather than the palette's: a route change does
  // not close it, because a reader who followed a link to check something
  // keeps the conversation beside it. A reload brings back what this tab had
  // (`assistant/tab.ts`), and the hand-over that empties a tab's storage when
  // somebody else signs in takes that too.
  const [assistantOpen, setAssistantOpen] = useState(() => readTab().open);
  const [assistantFocus, setAssistantFocus] = useState<AssistantFocus>("heading");
  const showAssistant = useCallback((open: boolean, focus: AssistantFocus = "composer") => {
    setAssistantFocus(focus);
    setAssistantOpen(open);
    writeTab({ open });
  }, []);

  const [labels, setLabels] = useState<Labels>({});
  const [coverage, setCoverage] = useState<CoverageFacts | null>(null);
  const [counts, setCounts] = useState<Record<string, string>>({});
  const [showWorking, setShowWorking] = useState(false);
  const [menu, setMenu] = useState<PageMenuEntry[]>([]);
  const [publishedFleet, setFleet] = useState<PublishedFleet | null>(null);
  const [ownStar, setOwnStar] = useState(false);
  // A SCREEN THAT DRAWS ITS OWN HEAD (an open artifact): the frame draws the
  // slot it lands in instead of its bar. See `frame/OwnHeader.tsx`.
  const [ownHeader, setOwnHeader] = useState(false);
  const [headerSlot, setHeaderSlot] = useState<HTMLElement | null>(null);

  // WATCH THE VIEWER'S OWN RECORD, which is what asks the engine for its
  // `inbox_changed` frames. The shell owns it because the shell outlives every
  // screen: a watch taken by the Inbox screen would end the moment the reader
  // navigated away, and the sidebar's badge (`lib/useInboxCounts.ts`) is what
  // has to stay current. UNDER `owner` — the seat when bound, the login when
  // not — because that is the record the engine pushes about. The socket
  // re-sends it on every reconnect. ONLY WITH WORKLET: `inbox_changed` is
  // Worklet's push, and a watch from anybody else is a refusal on every
  // reconnect.
  // AND ONLY OF A COMPANY RUNNING WORKLET, whose push it is.
  const runs = useServicesRun();
  const ask = useServicesAsk();
  const watchesInbox = viewer.owner !== "" && viewer.worklet && ask.tracker;
  useEffect(() => {
    if (!watchesInbox) return;
    socket.watch(viewer.owner);
    return () => socket.watch("");
  }, [socket, viewer.owner, watchesInbox]);

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
    // A JUMP TO A PLACE THE COMPANY DOES NOT RUN is bound to nothing, as its
    // row is drawn nowhere (`nav.ts`'s `serviceShown`).
    ...Object.fromEntries(
      WORKSPACES.map((ws) => [
        `go.${ws.key}`,
        { run: () => nav.to(landingPath(ws, held, runs)), when: serviceShown(ws, runs) },
      ]),
    ),
    // READ AT THE PRESS, off the store, rather than through `useAgents`: the
    // agents push lands twice per tool-loop round, and a subscription here
    // would re-render the whole frame on every one of them for a key pressed
    // once in a while.
    running: () => {
      const target = runningTarget(store.state.agents);
      nav.to(target.path, target.query);
    },
    // ⌘. OPENS THE ASSISTANT AND PUTS FOCUS IN ITS COMPOSER; pressed with
    // focus inside the panel it closes it, and the panel hands focus back.
    "assistant.open": () => {
      const panel = document.querySelector(".crewlet-app-shell__aside");
      if (!assistantOpen) showAssistant(true, "composer");
      else if (panel?.contains(document.activeElement)) showAssistant(false);
      else document.getElementById(ASSISTANT_COMPOSER_ID)?.focus();
    },
  });

  // THE PALETTE CLOSES whenever the route moves: picking a row closes it on
  // the way out, and this covers every OTHER way — Back and Forward, a
  // phone's back gesture, a restored history entry — where a palette left
  // open would go on offering what it ranked for the screen just left. The
  // drawer is the kit's, and `navigationKey` closes it the same way.
  useEffect(() => {
    setPaletteOpen(false);
  }, [route.hash]);

  const workspace = workspaceOf(route.path);
  const row = workspaceRow(workspace);

  // WHICH SHAPE THE PEEK TAKES — see the file's doc. The frame is described
  // by what stands in front of the list, and the width asked is the one at
  // which this frame, at this density, still leaves the list its floor.
  // THE COLUMN STANDS BESIDE THE SECTION only where the window holds both
  // ([SECTION_COLUMN_MIN]); below that it folds to its one-row picker over
  // the section, and the peek arithmetic counts no column.
  //
  // THE ASSISTANT TAKES ITS TRACK FIRST, because the reader opened it to talk
  // about the page: a column where the window holds it beside the page's
  // sheet with the list at its floor, over the page's end edge below that,
  // and the whole window on a phone (`app/layout.ts`). Its track is then one
  // more term of every other width: Settings' column folds sooner, and a
  // peek beside it becomes a drawer first.
  const scale = densityScale(prefs.density);
  const phone = useMediaMatch(`(width < ${PHONE_BREAKPOINT}px)`);
  const assistantFits = useMediaMatch(`(width >= ${assistantColumnMin({ column: 0, scale })}px)`);
  const assistantShape = phone ? "sheet" : assistantFits ? "column" : "overlay";
  const assistantColumn = assistantOpen && assistantShape === "column";
  // AND WHETHER IT STANDS OVER THE SCREEN, for a screen that writes down what
  // its reader has seen ([usePageInView]): anywhere but its column, which only
  // narrows the page. As the whole window it hides the screen, and over the
  // page's end edge it leaves the page live, with no veil, yet stands over the
  // screen's end, where a chat room's transcript and its thread are.
  const assistantOver = assistantOpen && assistantShape !== "column";
  // THE READER'S WIDTH, kept per browser. Every other threshold counts it
  // (`assistantCounted`), not the resting width, so a panel dragged wider
  // folds Settings' column and turns a peek into a drawer at the width it
  // really leaves the page.
  const assistantWidth = useStoredWidth({
    key: STORAGE_KEYS.assistantWidth,
    min: ASSISTANT_MIN,
    max: ASSISTANT_MAX,
  });
  const track = assistantColumn ? assistantTrack(assistantCounted(assistantWidth.width), scale) : 0;
  const sectionColumn = columnWidth(row?.renderer);
  const sideBySide = useMediaMatch(`(width >= ${sectionColumnMin(track)}px)`);
  const column = sideBySide ? sectionColumn : 0;
  const frame = { column, scale, assistant: track };
  const roomy = useMediaMatch(`(width >= ${peekColumnMin(frame)}px)`);
  const peekShape = peekable(peek) ? (roomy ? "column" : "drawer") : undefined;
  // THE WIDTH DRAWN is the reader's, capped where the page's sheet keeps its
  // floor: a width dragged on a wide window never takes a narrower one's list
  // away.
  const viewport = useViewportWidth();
  const assistantMax = assistantMaxWidth(viewport, frame, peekShape === "column");
  const assistantDrawn = Math.min(assistantWidth.width ?? ASSISTANT_WIDTH, assistantMax);
  // THE TOAST STACK STANDS BESIDE THE PANEL, not over it, while the panel is
  // a column or over the page; on a phone the panel itself keeps its foot
  // clear (`toastInset.tsx`).
  useToastInset(
    "frame",
    assistantOpen && assistantShape !== "sheet"
      ? { inline: assistantOccupied(assistantShape, assistantDrawn, viewport, scale) }
      : null,
  );
  const crumbs = useMemo(() => crumbsFor(route.path, labels), [route.path, labels]);

  // WHAT THE ASSISTANT IS TOLD ABOUT THE PAGE: the screen, its parameters,
  // the address's query and the crumbs it is called by. A screen that knows
  // more registers it itself (`assistant/useAssistantContext.ts`).
  const routeQuery = route.query.toString();
  useEffect(() => {
    setFramePage(framePageOf(route.path, new URLSearchParams(routeQuery), crumbs));
  }, [route.path, routeQuery, crumbs]);

  // THE ASSISTANT MOVES THE PAGE ONLY THROUGH AN ADDRESS THE ENGINE BUILT,
  // checked again here before the router follows it. Over the page or as the
  // whole window, it closes so the reader sees where they went; as a column
  // it stays beside it.
  useEffect(
    () =>
      onAssistantBus("assistant.navigation.open", ({ path: to, query, mode }) => {
        if (!resolves(to)) return;
        if (mode === "new_tab") {
          window.open(href(to, query), "_blank", "noopener");
          return;
        }
        nav.to(to, query);
        if (assistantShape !== "column") showAssistant(false);
      }),
    [nav, assistantShape, showAssistant],
  );
  const org = useOrg();
  const assistantUnavailable = assistantUnavailableFor(viewer, org?.name ?? "");
  const assistantModel = assistantModelFor(viewer);
  const assistantControl = useMemo(
    () => ({
      open: assistantOpen,
      unavailable: assistantUnavailable,
      toggle: (focus: AssistantFocus = "composer") => showAssistant(!assistantOpen, focus),
      // --- assistant page actions (WP12) ---
      ask: (question: string) => {
        if (assistantUnavailable !== null) return;
        queueQuestion(question);
        if (!assistantOpen) showAssistant(true, "composer");
      },
      // --- end assistant page actions (WP12) ---
    }),
    [assistantOpen, assistantUnavailable, showAssistant],
  );

  // THE TAB SAYS WHERE YOU ARE, so a reader with four tabs open can tell
  // them apart: on Worklet's scope pages, the lens and then the scope.
  const where = titleOf(crumbs);
  const tab = titleOf(crumbs, scopeAt(route.path, route.query));
  useEffect(() => {
    document.title = tab === "Crewlet" ? "Crewlet" : `${tab} · Crewlet`;
  }, [tab]);

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
    () => ({ setLabels, setCoverage, setCounts, setShowWorking, setMenu, setFleet, setOwnStar }),
    [],
  );

  const degraded = degradationOf({
    authRejected,
    accessRefused,
    connected,
    configured: engine?.configured,
    onSignIn: goSignIn,
    onConfig: () => nav.to(["settings", "config"]),
  });

  // THE SETTINGS COLUMN'S FIGURES — the health push the frame already holds,
  // and three guarded answers asked only while the column is on screen and
  // only of a reader whose grants reach them. See `sidebar/settingsFigures.tsx`.
  const figures = useSettingsSidebar({
    here: row?.renderer === "column",
    grants: held ?? [],
    health: engine,
    published: publishedFleet,
  });

  const screen = (
    <FillRequest.Provider value={setFilling}>
      <PeekWidthRequest.Provider value={setRestingPeek}>
        <OwnHeaderRequest.Provider value={setOwnHeader}>
          <OwnHeaderSlot.Provider value={ownHeader ? headerSlot : null}>
            <PageContextValue.Provider value={page}>
              <PageCover over={assistantOver}>{children}</PageCover>
            </PageContextValue.Provider>
          </OwnHeaderSlot.Provider>
        </OwnHeaderRequest.Provider>
      </PeekWidthRequest.Provider>
    </FillRequest.Provider>
  );

  return (
    // THE STEPPER'S ORDER, published by whichever list the reader is on, so
    // `[` and `]` walk the rows they are actually looking at. See `PeekHost`.
    <NewTaskOpener.Provider value={openNewTask}>
      <PeekNeighbours>
        {/* AROUND THE PALETTE TOO (WP12): its "Ask the assistant" row asks
            through the same control the Ask button reads. */}
        <AssistantControlContext.Provider value={assistantControl}>
          <AppShell
            className="app"
            // THE SHEET GAINS ITS PEEK COLUMN ONLY WHILE A PEEK IS OPEN (an
            // empty column would take its width from every screen that never
            // opens one), and only where the frame has room for it. The kit
            // publishes the shape as `data-peek`; the reserve is what the
            // column leaves the list, so a dragged width cannot take it back.
            peek={
              peekShape && (
                <PeekRestingWidth.Provider value={restingPeek ?? PEEK_WIDTH}>
                  <PeekHost reserve={peekShape === "column" ? listReserve(frame) : undefined} />
                </PeekRestingWidth.Provider>
              )
            }
            peekShape={peekShape}
            // THE ASSISTANT, a third track of the frame mirroring the rail,
            // over the page's end edge below that, and the whole window on a
            // phone; see the arithmetic above. Only the whole window is a
            // modal (`asideModal`, below). Mounted only while open, and its
            // code is a chunk of its own that first paint never carries.
            aside={
              assistantOpen ? (
                <AsideBoundary onClose={() => showAssistant(false)}>
                  <AssistantDrawer
                    unavailable={assistantUnavailable}
                    model={assistantModel}
                    admin={viewer.admin}
                    onClose={() => showAssistant(false)}
                    initialFocus={assistantFocus}
                    shape={assistantShape}
                    name={ownName(session.answer)}
                  />
                </AsideBoundary>
              ) : null
            }
            asideShape={assistantShape}
            // AN OVERLAY LEAVES THE PAGE LIVE BESIDE IT: the assistant is
            // read beside the screen it talks about, so no veil and no Tab
            // trap, and Escape from inside still closes it. A column is never
            // modal and a sheet always is, so this decides the overlay alone.
            asideModal={false}
            asideWidth={assistantDrawn}
            asideReserve={sheetFloor(frame, peekShape === "column")}
            asideLabel="Ask Crewlet"
            onAsideClose={() => showAssistant(false)}
            asideResize={{
              min: ASSISTANT_MIN,
              max: assistantMax,
              defaultValue: ASSISTANT_WIDTH,
              onChange: assistantWidth.setWidth,
              onCommit: assistantWidth.commitWidth,
              label: "Resize the Ask Crewlet panel",
            }}
            mainId={SCREEN_SCROLL_ID}
            fill={filling}
            navigationKey={route.hash}
            toggleLabel="Navigation"
            sidebar={<Sidebar onSearch={() => setPaletteOpen(true)} />}
            topbar={
              ownHeader ? (
                <div className="page-head">
                  <div ref={setHeaderSlot} />
                  <StateBar degraded={degraded} coverage={coverage} />
                </div>
              ) : (
                <PageHeader
                  crumbs={crumbs}
                  row={row}
                  counts={counts}
                  title={where}
                  workspace={workspace}
                  working={showWorking}
                  menu={menu}
                  star={!ownStar}
                  runs={runs}
                  grants={held}
                >
                  <StateBar degraded={degraded} coverage={coverage} />
                </PageHeader>
              )
            }
          >
            {row && sectionColumn > 0 ? (
              // THE COLUMN'S WIDTH IS THE ONE THE PEEK ARITHMETIC COUNTED
              // ([columnWidth]), and the fold is the same answer the peek's
              // width reads ([SECTION_COLUMN_MIN]), both handed to the kit's
              // frame rather than written again in a stylesheet, so the column
              // drawn and the column counted cannot drift.
              <SectionFrame columnWidth={`${sectionColumn}px`} folded={!sideBySide}>
                <SectionColumn
                  row={row}
                  path={route.path}
                  query={route.query}
                  grants={held}
                  figures={figures}
                  runs={runs}
                />
                <SectionFrame.Body>{screen}</SectionFrame.Body>
              </SectionFrame>
            ) : (
              screen
            )}
          </AppShell>

          {/* THE PALETTE HAS A BOUNDARY OF ITS OWN, and it is mounted only while
          open, so a palette that threw is a dialog saying so — closed the
          way the palette closes — and the next ⌘K is a fresh palette. Its
          code is a chunk of its own, the first the idle prefetch fetches. */}
          {paletteOpen && (
            <LayerBoundary title="Search" onClose={() => setPaletteOpen(false)}>
              <CommandPalette
                onClose={() => setPaletteOpen(false)}
                onShowKeys={() => setLegendOpen(true)}
              />
            </LayerBoundary>
          )}
          {/* THE SHEET'S CODE IS THE WORKLET WORKSPACE'S CHUNK (it is a Worklet
            form, and the frame's own entry stays the frame), so it is mounted
            only while open, through the same loader every screen uses. */}
          {newTask !== null && (
            <LayerBoundary title="New task" onClose={() => setNewTask(null)}>
              <NewTaskSheet preset={newTask} onClose={() => setNewTask(null)} />
            </LayerBoundary>
          )}
          {legendOpen && <KeyLegend runs={runs} onClose={() => setLegendOpen(false)} />}
        </AssistantControlContext.Provider>
      </PeekNeighbours>
    </NewTaskOpener.Provider>
  );
}

/** The New task sheet, out of the Worklet chunk. See [LayerBoundary]. */
const NewTaskSheet = lazyScreen("worklet", (module) => module.NewTaskSheet);

/** The assistant's panel, out of a chunk of its own (`lazyScreen.ts`). See [AsideBoundary]. */
const AssistantDrawer = lazyScreen("assistant", (module) => module.Drawer);

/** The command palette, out of a chunk of its own (`lazyScreen.ts`). See [LayerBoundary]. */
const CommandPalette = lazyScreen("palette", (module) => module.CommandPalette);
