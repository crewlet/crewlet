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
import { CommandPalette } from "./CommandPalette.tsx";
import { TokenDialog } from "./TokenDialog.tsx";
import { AppRail, useRailCollapsed, useWorkspaceChords, type RailBadge } from "./frame/AppRail.tsx";
import { WorkspaceSidebar, type SidebarSection } from "./frame/WorkspaceSidebar.tsx";
import { PageBar, CopyLink } from "./frame/PageBar.tsx";
import { StateBar, degradationOf } from "./frame/StateBar.tsx";
import { crumbsFor, titleOf, type Labels } from "./workspaces/crumbs.ts";
import {
  useActivitySidebar,
  useAdminSidebar,
  useCompanySidebar,
  useCostSidebar,
  useKnowledgeSidebar,
  useWorkSidebar,
} from "./workspaces/sidebars.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { Badge, Segmented, cx } from "~/ui/primitives.tsx";
import { useAgents, useClient, useConnection } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useViewer } from "~/lib/viewer.ts";
import { useDensity, useTheme, type Density, type ThemeChoice } from "~/lib/theme.ts";
import { onTokenRequested } from "~/protocol/index.ts";
import type { CoverageFacts } from "~/components/work.tsx";

/**
 * What a screen tells the frame about itself.
 *
 * A SCREEN NEVER RENDERS CHROME. It publishes what the chrome needs — the
 * labels the route cannot supply, the coverage of the answer it drew from, the
 * actions that belong to this object — and the frame draws all of it in the
 * same place on every screen. The alternative, which this replaces, is twenty
 * screens each drawing their own header, and five of them quietly dropping the
 * coverage badge.
 */
export interface PageContext {
  setLabels: (labels: Labels) => void;
  setCoverage: (coverage: CoverageFacts | null) => void;
  setActions: (actions: ReactNode) => void;
}

const noop: PageContext = {
  setLabels: () => {},
  setCoverage: () => {},
  setActions: () => {},
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
    // The labels object is rebuilt every render by most callers, so the
    // dependency is its CONTENT: an identity dependency here is an infinite
    // render loop, and one on `labels` alone would be exactly that.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, setLabels]);
}

/** Publish the coverage of the answer this screen was drawn from. */
export function usePageCoverage(coverage: CoverageFacts | null | undefined): void {
  const { setCoverage } = usePageContext();
  const level = coverage?.read_level;
  const complete = coverage?.complete;
  const applied = coverage?.applied_through;
  const seq = coverage?.log_seq;
  useEffect(() => {
    setCoverage(coverage ?? null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [level, complete, applied, seq, setCoverage]);
}

/**
 * Publish this page's own actions and badges into the page bar.
 *
 * They belong to the OBJECT rather than to the chrome, and they used to be
 * drawn by each screen's own header — which is how five screens came to have
 * their controls in five different places on the page.
 */
export function usePageActions(actions: ReactNode): void {
  const { setActions } = usePageContext();
  useEffect(() => {
    setActions(actions);
    // CLEARED ON THE WAY OUT, or the last screen's controls sit in the bar of
    // the next one — pointing at an object the reader has left.
    return () => setActions(null);
  }, [actions, setActions]);
}

/** Which sidebar a workspace has, or none for the two full-bleed screens. */
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
  const admin = useAdminSidebar();
  switch (workspace) {
    case "work":
      return work;
    case "company":
      return company;
    case "knowledge":
      return knowledge;
    case "activity":
      return activity;
    case "cost":
      return cost;
    case "admin":
      return admin;
    default:
      // The Inbox and My work are two-pane screens whose scope lives in the
      // page itself — a sidebar of filters would be the grammar's first
      // casualty.
      return null;
  }
}

export function Shell({ children }: { children: ReactNode }) {
  const route = useRoute();
  const nav = useNavigator();
  const { socket } = useClient();
  const { connected, authRejected, health } = useConnection();
  const agents = useAgents();
  const viewer = useViewer();
  const [theme, setTheme] = useTheme();
  const [density, setDensity] = useDensity();
  const [collapsed, toggleRail] = useRailCollapsed();

  const [paletteOpen, setPaletteOpen] = useState(false);
  const [tokenOpen, setTokenOpen] = useState(false);
  const [drawer, setDrawer] = useState(false);

  const [labels, setLabels] = useState<Labels>({});
  const [coverage, setCoverage] = useState<CoverageFacts | null>(null);
  const [actions, setActions] = useState<ReactNode>(null);

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

  useEffect(() => {
    function onKey(e: KeyboardEvent): void {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPaletteOpen((v) => !v);
      }
      if (e.key === "Escape") setPaletteOpen(false);
      const el = document.activeElement;
      const typing =
        el instanceof HTMLElement &&
        (el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.isContentEditable);
      // A bare "/" opens search the way every list-shaped tool does — but not
      // while somebody is typing into a field.
      if (e.key === "/" && !typing) {
        e.preventDefault();
        setPaletteOpen(true);
      }
      // `[` collapses the rail, which is the one piece of chrome a reader
      // trades for width on a narrow laptop.
      if (e.key === "[" && !typing && !e.metaKey && !e.ctrlKey) {
        const peekOpen = route.query.has("peek");
        // `[` steps the peek when one is open — the rail is not what the
        // reader means by it there.
        if (!peekOpen) {
          e.preventDefault();
          toggleRail();
        }
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [toggleRail, route.query]);

  const goTo = useCallback((path: string[]) => nav.to(path), [nav]);
  useWorkspaceChords(goTo);

  // Close the drawer whenever the route changes — a drawer left open over the
  // screen you just navigated to is the classic mobile-nav bug.
  useEffect(() => setDrawer(false), [route.hash]);

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

  const page: PageContext = useMemo(
    () => ({ setLabels, setCoverage, setActions }),
    [setLabels, setCoverage, setActions],
  );

  const working = agents.filter((a) => a.state === "working").length;
  const badges: Partial<Record<Workspace, RailBadge | null>> = {
    // UNREAD OVER THE FIRST PAGE, and the title says so: the reader returns at
    // most 50 notices and counts unread over them, so a badge claiming a total
    // would be inventing a number the engine never computed.
    inbox: inbox.data?.unread
      ? {
          text: inbox.data.unread >= 50 ? "50+" : String(inbox.data.unread),
          attention: true,
          title: "unread notices on the first page — the engine counts no total",
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
    <div
      className="app"
      data-rail-collapsed={collapsed || undefined}
      data-no-sidebar={sections === null || undefined}
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
              {actions}
              <CopyLink />
            </>
          }
          viewer={<ViewerChip />}
          onSearch={() => setPaletteOpen(true)}
          onToggleSidebar={sections ? () => setDrawer((v) => !v) : undefined}
        />
        <StateBar degraded={degraded} coverage={coverage} />
        <div className="screen" id="screen-scroll">
          <div className="screen-inner">
            <PageContextValue.Provider value={page}>{children}</PageContextValue.Provider>
          </div>
        </div>
      </main>

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
      <Badge outline title="No API token is presented; the guarded screens are locked">
        anonymous
      </Badge>
    );
  }
  if (viewer.unbound) {
    return (
      <Badge
        outline
        title={`Token ${viewer.operatorID} is not bound to a seat — give a human seat contact.crewlet_operator_id: ${viewer.operatorID}`}
      >
        {viewer.operatorID}
      </Badge>
    );
  }
  return (
    <a
      className="viewer-chip"
      href={`#/company/people/${viewer.handle}`}
      title={`@${viewer.handle}`}
    >
      <span className="seat-mark human" aria-hidden="true">
        <Icon name="user" size="xs" />
      </span>
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
  const tone = connected ? (configured === false ? "caution" : "positive") : "critical";
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
        <i className={cx("dot", tone)} />
        {!collapsed && <span className="truncate">{word}</span>}
        {inFlight > 0 && <span className="t-num">{inFlight}</span>}
      </a>
      {!collapsed && (
        <>
          <Segmented<ThemeChoice>
            size="sm"
            ariaLabel="Theme"
            activate="automatic"
            value={theme}
            onChange={setTheme}
            options={[
              { value: "light", label: "", icon: "sun", title: "Light" },
              { value: "system", label: "", icon: "monitor", title: "Follow the system" },
              { value: "dark", label: "", icon: "moon", title: "Dark" },
            ]}
          />
          <Segmented<Density>
            size="sm"
            ariaLabel="Density"
            activate="automatic"
            value={density}
            onChange={setDensity}
            options={[
              { value: "compact", label: "S", title: "Compact" },
              { value: "normal", label: "M", title: "Normal" },
              { value: "comfortable", label: "L", title: "Comfortable" },
            ]}
          />
        </>
      )}
    </>
  );
}
