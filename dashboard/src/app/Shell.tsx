/**
 * The application frame: rail, workspace sidebar, page, detail rail.
 *
 * Four columns and ONE scroll container — which is what lets the router
 * restore a scroll position per history entry. The sidebar and the peek scroll
 * internally; the page is the one the router owns, and a page with three
 * independent scrollers has three positions and no way to name them.
 *
 * The shell composes; it does not decide. Which workspace a route belongs to
 * is `nav.ts`, what the trail says is `workspaces/crumbs.ts`, what a
 * workspace's tree holds is `workspaces/sidebars.tsx`. This file wires them
 * together and owns exactly three things nothing else can: the palette, the
 * token dialog, and the one strip that reports a degraded connection.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { DESTINATIONS, RAIL, workspaceOf, type Workspace } from "./nav.ts";
import { samePath, useNavigator, useRoute } from "./router.tsx";
import { remember } from "~/lib/recents.ts";
import { SCREEN_SCROLL_ID } from "~/lib/scroller.ts";
import { CommandPalette } from "./CommandPalette.tsx";
import { TokenDialog } from "./TokenDialog.tsx";
import { AppRail, useRailCollapsed, useWorkspaceChords, type RailBadge } from "./frame/AppRail.tsx";
import { WorkspaceSidebar, type SidebarSection } from "./frame/WorkspaceSidebar.tsx";
import { PageBar, CopyLink, StarPage } from "./frame/PageBar.tsx";
import { PeekHost, PeekNeighbours } from "./frame/PeekHost.tsx";
import { usePeek } from "./frame/DetailRail.tsx";
import { peekable } from "./frame/peeks.tsx";
import { StateBar, degradationOf } from "./frame/StateBar.tsx";
import { crumbsFor, titleOf, type Labels } from "./workspaces/crumbs.ts";
import {
  useActivitySidebar,
  useAdminSidebar,
  useCompanySidebar,
  useCostSidebar,
  useKnowledgeSidebar,
  useKeptSections,
  useWorkSidebar,
} from "./workspaces/sidebars.tsx";
import { Avatar, SegmentedControl, StatusDot, Tag } from "@crewlethq/ui";
import { ComputerGlyph, DarkModeGlyph, LightModeGlyph } from "@crewlethq/icons/glyphs";
import { useAgents, useClient, useConnection } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useDensity, useTheme, type Density, type ThemeChoice } from "~/lib/prefs.ts";
import { onTokenRequested } from "~/protocol/index.ts";
import type { CoverageFacts } from "~/components/work.tsx";
import { useKeyChords } from "~/lib/keys.ts";
import { FillRequest } from "./fill.tsx";

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
}

const noop: PageContext = {
  setLabels: () => {},
  setCoverage: () => {},
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
 * On Admin > Credentials, which somebody opens precisely to judge whether what
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

/** Which sidebar a workspace has, or none for the three full-bleed screens. */
function useSidebar(workspace: Workspace | ""): SidebarSection[] | null {
  // EVERY HOOK RUNS, whatever the workspace. React's rules are not a style
  // preference here: calling six hooks conditionally would change the hook
  // order on every navigation between workspaces, which is the one thing that
  // corrupts a component's state rather than merely re-rendering it.
  const work = useWorkSidebar();
  const company = useCompanySidebar();
  const knowledge = useKnowledgeSidebar();
  const activity = useActivitySidebar();
  const cost = useCostSidebar();
  const admin = useAdminSidebar(workspace === "admin");
  // WHAT THIS READER KEPT AND OPENED, appended to whichever tree is shown, so
  // every workspace has them and none of the six implements them.
  const kept = useKeptSections(workspace);
  switch (workspace) {
    case "work":
      return [...work, ...kept];
    case "company":
      return [...company, ...kept];
    case "knowledge":
      return [...knowledge, ...kept];
    case "activity":
      return [...activity, ...kept];
    case "cost":
      return [...cost, ...kept];
    case "admin":
      return [...admin, ...kept];
    default:
      // The Inbox and My work are two-pane screens whose scope lives in the
      // page itself — a sidebar of filters would be the grammar's first
      // casualty. Chat is the third: its rooms ARE its tree, and they carry
      // unread counts, mute state and a preview that no sidebar row can hold.
      return null;
  }
}

export function Shell({ children }: { children: ReactNode }) {
  const route = useRoute();
  const peek = usePeek();
  const nav = useNavigator();
  const { socket } = useClient();
  const { connected, authRejected, health } = useConnection();
  const agents = useAgents();
  const viewer = useViewer();
  const [theme, setTheme] = useTheme();
  const [density, setDensity] = useDensity();
  const [collapsed, toggleRail] = useRailCollapsed();

  const [paletteOpen, setPaletteOpen] = useState(false);
  // A SCREEN THAT ASKED FOR THE HEIGHT INSTEAD OF THE SCROLL — see
  // `app/fill.tsx` for why this is a request and not a selector. One screen
  // makes it (the org builder's canvas lens), and while it holds, the
  // scroller stops being one and hands the column what is left of the window.
  const [filling, setFilling] = useState(false);
  const [tokenOpen, setTokenOpen] = useState(false);
  const [drawer, setDrawer] = useState(false);

  const [labels, setLabels] = useState<Labels>({});
  const [coverage, setCoverage] = useState<CoverageFacts | null>(null);

  const { data: engine } = useQuery("stream", undefined, { pollMs: 15_000 });
  const inbox = useQuery(
    "work_inbox",
    viewer.handle ? { handle: viewer.handle, limit: 50 } : undefined,
    {
      enabled: viewer.handle !== "",
      pollMs: 60_000,
    },
  );

  // The socket asks ONCE per refusal — a reconnect backoff must not reopen a
  // dialog forever. Everything after that is the state bar.
  useEffect(() => {
    socket.onAuthRejected(() => setTokenOpen(true));
  }, [socket]);

  // And from anywhere else that discovers it needs a credential — an
  // auth-gated answer on a screen the socket was never refused for.
  useEffect(() => onTokenRequested(() => setTokenOpen(true)), []);

  useKeyChords([
    { key: "k", meta: true, run: () => setPaletteOpen((v) => !v), whileTyping: true },
    // ESCAPE REACHES THE PALETTE FROM ITS OWN INPUT, which is the only field
    // the reader can be in when they want it closed.
    { key: "escape", run: () => setPaletteOpen(false), whileTyping: true },
    // A bare "/" opens search the way every list-shaped tool does.
    { key: "/", run: () => setPaletteOpen(true) },
    // `[` collapses the rail, which is the one piece of chrome a reader
    // trades for width on a narrow laptop — EXCEPT while a peek is open,
    // where `[` steps it and the rail is not what the reader means.
    { key: "[", run: toggleRail, when: !route.query.has("peek") },
  ]);

  const goTo = useCallback((path: string[]) => nav.to(path), [nav]);
  useWorkspaceChords(goTo);

  // Close the drawer AND THE PALETTE whenever the route changes — either one
  // left open over the screen you just navigated to is the classic mobile-nav
  // bug, and the palette had it too.
  //
  // Picking a palette row closes it on the way out, so the case this covers is
  // every OTHER way the route moves while it is open: the browser's Back and
  // Forward buttons, a phone's back gesture, a restored history entry. The
  // palette that survived one of those was still offering the objects it had
  // ranked for the screen the reader had just left.
  //
  // NOT THE TOKEN DIALOG, which is deliberately not in here: a credential
  // prompt is about the reader's access rather than about where they are, and
  // dismissing it on a navigation would lose the one thing the socket asked
  // for.
  useEffect(() => {
    setDrawer(false);
    setPaletteOpen(false);
  }, [route.hash]);

  const workspace = workspaceOf(route.path);
  const sections = useSidebar(workspace);
  const crumbs = useMemo(() => crumbsFor(route.path, labels), [route.path, labels]);

  // THE TAB SAYS WHERE YOU ARE. It said "Crewlet" on every screen, so a reader
  // with four tabs open had four identical ones.
  const where = titleOf(crumbs);
  useEffect(() => {
    document.title = where === "Crewlet" ? "Crewlet" : `${where} · Crewlet`;
  }, [where]);

  // AND THE PALETTE REMEMBERS THE OBJECTS, so an empty palette has something
  // to offer before the reader types.
  //
  // OBJECTS ONLY — anything the rail or a sidebar already lists is left out,
  // because a recents list repeating the navigation beside it costs a reader
  // a scan and tells them nothing. The label is the one the SCREEN resolved:
  // it lands a render after the route, which is why this depends on the title
  // rather than on the path.
  const path = route.path;
  useEffect(() => {
    if (path.length < 2) return;
    if (DESTINATIONS.some((d) => samePath(d.path, path))) return;
    if (where === "Crewlet") return;
    remember({ path, label: where, workspace: workspaceOf(path) || "" });
  }, [path, where]);

  const page: PageContext = useMemo(() => ({ setLabels, setCoverage }), [setLabels, setCoverage]);

  const working = agents.filter((a) => a.state === "working").length;

  // WHAT THE READER CAN ACTUALLY DRIVE DOWN, which is the whole of why this
  // badge is not `answer.unread`.
  //
  // `unread` counts every notice on the page, and most of a busy company's
  // notices are things it merely told you: a task you watch moved, a goal
  // you own was updated. Nobody answers those, so a badge built on them never
  // reaches zero however diligent the reader is — and a number that cannot go
  // down is read, correctly, as a broken counter. The PRIMARY half is the set
  // a person is on the hook for (a mention, a question, work assigned to
  // them), it is small by construction, and it goes down by answering. The
  // engine states which reasons were applied as primary — defaulted from the
  // person's own record — so this is the company's own split rather than one
  // the client invented.
  //
  // OVER THE FIRST PAGE, and the title says so: the reader returns at most 50
  // notices, so a badge claiming a total would be inventing a number the
  // engine never computed. `answer.unread` and `answer.primary` are each one
  // half of this question and neither is it, so it is counted here rather than
  // read off a field that does not mean what it looks like.
  const waiting = useMemo(() => {
    const data = inbox.data;
    if (!data) return 0;
    const primary = new Set(data.primary_reasons);
    return data.notices.filter((n) => !n.read && primary.has(n.reason)).length;
  }, [inbox.data]);

  const badges: Partial<Record<Workspace, RailBadge | null>> = {
    inbox: waiting
      ? {
          text: waiting >= 50 ? "50+" : String(waiting),
          attention: true,
          title:
            "unread notices under a reason your record counts as primary, on the first page — the engine counts no total",
        }
      : null,
    activity: working ? { text: String(working), title: `${working} seats working` } : null,
  };

  const degraded = degradationOf({
    authRejected,
    connected,
    configured: engine?.configured,
    onSetToken: () => setTokenOpen(true),
    onConfig: () => nav.to(["admin", "config"]),
  });

  return (
    // THE STEPPER'S ORDER, published by whichever list the reader is on, so
    // `[` and `]` walk the rows they are actually looking at. See `PeekHost`.
    <PeekNeighbours>
      <div
        className="app"
        data-rail-collapsed={collapsed || undefined}
        data-no-sidebar={sections === null || undefined}
        // THE GRID GAINS ITS FOURTH TRACK ONLY WHILE THE RAIL IS OPEN — an
        // empty column would take its width from every screen that never opens
        // one. See `.app[data-peek]` in frame.css.
        data-peek={peekable(peek) || undefined}
      >
        <AppRail
          active={workspace}
          badges={badges}
          collapsed={collapsed}
          onToggle={toggleRail}
          locked={!viewer.operator}
          footer={
            <EngineFooter
              connected={connected}
              authRejected={authRejected}
              configured={engine?.configured}
              inFlight={health.in_flight ?? 0}
              theme={theme}
              setTheme={setTheme}
              density={density}
              setDensity={setDensity}
              collapsed={collapsed}
            />
          }
        />

        {sections && (
          <WorkspaceSidebar
            title={RAIL.find((r) => r.key === workspace)?.label ?? ""}
            sections={sections}
            open={drawer}
            onClose={() => setDrawer(false)}
          />
        )}

        <main className="page">
          <PageBar
            crumbs={crumbs}
            actions={
              <>
                <StarPage path={path} label={where} workspace={workspaceOf(path) || ""} />
                <CopyLink />
              </>
            }
            viewer={<ViewerChip />}
            onSearch={() => setPaletteOpen(true)}
            onToggleSidebar={sections ? () => setDrawer((v) => !v) : undefined}
          />
          <StateBar degraded={degraded} coverage={coverage} />
          {/* THE ATTRIBUTE IS THE SHELL'S OWN, set from the request the screen
              made. The chain it drives is in the stylesheet beside it, and it
              names nothing inside any screen: a screen says THAT it wants the
              height, never how the shell is built. */}
          <div className="screen" id={SCREEN_SCROLL_ID} data-fill={filling || undefined}>
            <div className="screen-inner">
              <FillRequest.Provider value={setFilling}>
                <PageContextValue.Provider value={page}>{children}</PageContextValue.Provider>
              </FillRequest.Provider>
            </div>
          </div>
        </main>

        {/* THE ONE PEEK IN THE PRODUCT. Mounted here rather than by a screen,
          which is what makes it available to every list instead of to the one
          that remembered to render a rail — see `frame/PeekHost.tsx`. */}
        <PeekHost />

        {paletteOpen && <CommandPalette onClose={() => setPaletteOpen(false)} />}
        {tokenOpen && (
          <TokenDialog
            onClose={() => setTokenOpen(false)}
            onSaved={(token) => {
              socket.setToken(token);
              socket.reconnect();
            }}
          />
        )}
      </div>
    </PeekNeighbours>
  );
}

/**
 * Who the frame thinks you are, in the page bar.
 *
 * Three states and three different things to say — see `lib/viewer.ts`. The
 * unbound one is the interesting case: it is ORDINARY, so the chip names the
 * token rather than reporting a fault, and its title says what to bind.
 */
function ViewerChip() {
  const viewer = useViewer();
  if (viewer.loading) return null;
  if (viewer.anonymous) {
    return (
      // NEUTRAL, AND OUTLINED. "anonymous" and an operator id are both
      // IDENTITY — who the frame thinks you are — and uilet's tone doc draws
      // the same line this dashboard does: a tone says what a thing IS, never
      // who it is. The boundary is what separates the chip from the page bar
      // behind it; a tint would read as a state nobody is in.
      <Tag appearance="outline" title="No API token is presented; the guarded screens are locked">
        anonymous
      </Tag>
    );
  }
  if (viewer.unbound) {
    return (
      <Tag
        appearance="outline"
        // A TOKEN ID IS A MACHINE VALUE, so it is set in the mono face: the
        // operator compares it character by character against the one in their
        // `crewlet.yaml`, which proportional digits make harder than it needs
        // to be. Ours had `mono` for the same reason and this chip never asked
        // for it.
        monospace
        title={`Token ${viewer.operatorID} is not bound to a seat — give a human seat contact.crewlet_operator_id: ${viewer.operatorID}`}
      >
        {viewer.operatorID}
      </Tag>
    );
  }
  return (
    <a
      className="viewer-chip"
      href={`#/company/people/${viewer.handle}`}
      title={`@${viewer.handle}`}
    >
      {/* THE SAME BADGE THE REST OF THE PRODUCT DRAWS. This held the last
          hand-rolled `.seat-mark` in the tree, kept on the reasoning that
          "uilet's Avatar is a picture of a person rather than a kind-of-seat
          mark" — which was never true of the badge and is not true of the
          product either: `Avatar` draws INITIALS, and its `dashed` variant is
          documented as "a HUMAN seat: the engine does not run it", the exact
          fact the dashed ring carried. Meanwhile the claim beside it, that the
          mark is "drawn identically in the people list, the peek and the org
          chart", had stopped holding — all three draw `Avatar` — so the one
          place a reader sees THEMSELVES was the one place they did not look
          like themselves. `brand`, because this badge IS the reader: it is the
          single use the tone exists for. */}
      <Avatar
        name={viewer.name || viewer.handle}
        size="xs"
        tone="brand"
        variant="dashed"
        decorative
      />
      <span className="truncate">{viewer.name || viewer.handle}</span>
    </a>
  );
}

/** The rail's foot: the engine's own state, the theme and the density. */
function EngineFooter({
  connected,
  authRejected,
  configured,
  inFlight,
  theme,
  setTheme,
  density,
  setDensity,
  collapsed,
}: {
  connected: boolean;
  authRejected: boolean;
  configured: boolean | undefined;
  inFlight: number;
  theme: ThemeChoice;
  setTheme: (t: ThemeChoice) => void;
  density: Density;
  setDensity: (d: Density) => void;
  collapsed: boolean;
}) {
  // uilet's tone vocabulary, which is ours renamed: positive → success,
  // caution → warning, critical → danger.
  const tone = connected ? (configured === false ? "warning" : "success") : "danger";
  const word = connected
    ? configured === false
      ? "no config"
      : "connected"
    : authRejected
      ? "refused"
      : "unreachable";
  return (
    <>
      {/* A LINK TO THIS NODE'S PAGE, not a modal. The engine's own state is an
          object with a page like every other, and a panel that could only be
          reached from here was the one surface with no address. */}
      <a className="rail-engine" href="#/admin/fleet" title={`Engine ${word}`}>
        {/* The dot is the shape half and the word beside it is the state —
            which is exactly StatusDot's contract, so it is `aria-hidden` and
            a screen reader reads the word once rather than twice. */}
        <span className="rail-engine-marks">
          <StatusDot tone={tone} />
          {inFlight > 0 && <span className="t-num">{inFlight}</span>}
        </span>
        {!collapsed && <span className="truncate">{word}</span>}
      </a>
      {!collapsed && (
        <>
          {/* BOTH ROWS ARE SETTINGS, which is what `semantics="radio"` says:
              announced as a radio group, and the arrows select as they move.
              That is our `activate="automatic"` under uilet's name, and it is
              right here for the reason uilet gives — a choice that is not in
              the URL and pushes no history entry costs nothing to change on
              every keypress.

              NOT uilet's own ThemeSwitcher / DensitySwitcher, which draw
              exactly these two rows: they own the value themselves behind a
              `storageKey` and expose no controlled `value` / `onChange`. This
              dashboard's theme is also set from the command palette's `>`
              scope and read by `lib/prefs.ts`, so a control holding a second
              copy of it would sit unmoved while the page around it changed. */}
          <SegmentedControl<ThemeChoice>
            semantics="radio"
            size="sm"
            label="Theme"
            value={theme}
            onValueChange={setTheme}
            options={[
              { value: "light", icon: <LightModeGlyph size="xs" />, title: "Light" },
              {
                value: "system",
                icon: <ComputerGlyph size="xs" />,
                title: "Follow the system",
              },
              { value: "dark", icon: <DarkModeGlyph size="xs" />, title: "Dark" },
            ]}
          />
          <SegmentedControl<Density>
            semantics="radio"
            size="sm"
            label="Density"
            value={density}
            onValueChange={setDensity}
            options={[
              // THE LETTER IS A PICTURE AND THE WORD IS THE NAME. S, M and L
              // name nothing out loud, so each option carries the word too —
              // `srLabel`, which ours had no place for at all.
              { value: "compact", label: "S", srLabel: "Compact", title: "Compact" },
              { value: "normal", label: "M", srLabel: "Normal", title: "Normal" },
              {
                value: "comfortable",
                label: "L",
                srLabel: "Comfortable",
                title: "Comfortable",
              },
            ]}
          />
        </>
      )}
    </>
  );
}
