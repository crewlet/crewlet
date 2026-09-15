/**
 * The application frame.
 *
 * Sidebar, topbar, and ONE scroll container, which is what lets the router
 * restore a scroll position per history entry. A page with three independent
 * scrollers has three positions and no way to name them.
 */

import {
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
  type RefObject,
} from "react";
import { NAV, activeNavKey, titleFor } from "./nav.ts";
import { href, useRoute } from "./router.tsx";
import {
  CommandPalette,
  SEARCH_ARIA_KEYSHORTCUTS,
  SEARCH_SHORTCUT,
  isSearchShortcut,
} from "./CommandPalette.tsx";
import { EnginePanel } from "./EnginePanel.tsx";
import { TokenDialog } from "./TokenDialog.tsx";
import { Kbd } from "~/ui/Kbd.tsx";
import {
  useAgents,
  useClient,
  useConnection,
  useOrg,
  useOrgBudget,
  useSandboxes,
} from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { attentionQueue } from "~/lib/attention.ts";
import { indexOrg } from "~/lib/seats.ts";
import {
  Button,
  ButtonLink,
  IconButton,
  SegmentedControl,
  cx,
  focusables,
  isComposing,
  isModalLayerOpen,
  type ThemePreference,
  useDensityPreference,
  useModalLayer,
  useNow,
  useThemePreference,
} from "@crewlethq/ui";
import { onTokenRequested } from "~/protocol/index.ts";
import {
  ComputerGlyph,
  DarkModeGlyph,
  KeyGlyph,
  LightModeGlyph,
  ManufacturingGlyph,
  MenuGlyph,
  RefreshGlyph,
  SearchGlyph,
} from "@crewlethq/icons/glyphs";

export function Shell({ children }: { children: ReactNode }) {
  const route = useRoute();
  const { socket } = useClient();
  const { connected, authRejected, health } = useConnection();
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const budget = useOrgBudget();
  const org = useOrg();
  const now = useNow();
  const [theme, setTheme] = useThemePreference({ storageKey: "crewlet_theme" });
  const [density, setDensity] = useDensityPreference({ storageKey: "crewlet_density" });

  const [paletteOpen, setPaletteOpen] = useState(false);
  const [enginePanel, setEnginePanel] = useState(false);
  const [tokenOpen, setTokenOpen] = useState(false);
  const [drawer, setDrawer] = useState(false);
  const sidebar = useRef<HTMLElement>(null);
  const drawerToggle = useRef<HTMLSpanElement>(null);

  const { data: engine } = useQuery("stream", undefined, { pollMs: 15_000 });

  // The socket asks ONCE per refusal: a reconnect backoff must not reopen a
  // dialog forever. Everything after that is the banner and the engine panel.
  useEffect(() => {
    socket.onAuthRejected(() => setTokenOpen(true));
  }, [socket]);

  // And from anywhere else that discovers it needs a credential: an
  // auth-gated answer on a screen the socket was never refused for.
  useEffect(() => onTokenRequested(() => setTokenOpen(true)), []);

  // The shortcuts that OPEN search, and nothing else. Closing a surface
  // belongs to the surface and the layer stack (`useModalLayer`): an Escape
  // handled here as well closed search and the engine panel along with
  // whatever sat above or beneath them, and a toggle here closed search from
  // beneath a dialog raised over it. A press a surface already handled (search
  // closing on its own chord) is left alone.
  //
  // AND ONLY FROM THE PAGE. While a modal is open the page behind it is inert,
  // and search opened over a dialog could navigate away from under it: the
  // screen unmounts, and the dialog goes with it, an unsaved editor or a
  // write whose outcome the operator has not seen yet included.
  useEffect(() => {
    function onKey(e: KeyboardEvent): void {
      if (e.defaultPrevented || isComposing(e) || isModalLayerOpen()) return;
      if (isSearchShortcut(e)) {
        e.preventDefault();
        setPaletteOpen(true);
        return;
      }
      // A bare "/" opens search the way every list-shaped tool does, but not
      // while somebody is typing into a field, and not as part of a chord
      // (Ctrl or Command with "/" is the browser's or the platform's).
      const el = document.activeElement;
      const typing =
        el instanceof HTMLElement &&
        (el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.isContentEditable);
      const chord = e.ctrlKey || e.metaKey || e.altKey;
      if (e.key === "/" && !typing && !chord) {
        e.preventDefault();
        setPaletteOpen(true);
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // Close the mobile drawer whenever the route changes: a drawer left open
  // over the screen you just navigated to is the classic mobile-nav bug.
  useEffect(() => setDrawer(false), [route.hash]);

  const index = useMemo(() => indexOrg(org), [org]);
  const attention = useMemo(
    () =>
      attentionQueue({
        agents,
        sandboxes,
        budget,
        engine: engine ?? null,
        seats: index.seats,
        connected,
        authRejected,
        now,
      }),
    [agents, sandboxes, budget, engine, index.seats, connected, authRejected, now],
  );

  const activeKey = activeNavKey(route.path);
  const title = titleFor(route.path);
  const working = agents.filter((a) => a.state === "working").length;

  return (
    <div className="app">
      {drawer && (
        <SectionsDrawer sidebar={sidebar} toggle={drawerToggle} onClose={() => setDrawer(false)} />
      )}
      <aside
        className="sidebar"
        ref={sidebar}
        data-open={drawer}
        // A DIALOG ONLY WHILE OPEN. Wide, the rail is the page's own
        // navigation beside the screen; narrow and open, it is a modal over
        // it, and a screen reader should hear that the page behind is inert.
        role={drawer ? "dialog" : undefined}
        aria-modal={drawer || undefined}
        aria-label={drawer ? "Sections" : undefined}
      >
        <a className="brand" href={href([])}>
          <img src="/static/crewlet-icon.svg" alt="" />
          <span className="col" style={{ gap: 0 }}>
            <span className="brand-name">Crewlet</span>
            {org?.name && <span className="brand-org truncate">{org.name}</span>}
          </span>
        </a>

        <nav className="nav" aria-label="Sections">
          {NAV.map((group) => (
            <div key={group.key}>
              {group.label && <div className="nav-group-label">{group.label}</div>}
              {group.items.map((item) => {
                // The one badge in the chrome allowed a status hue: the count
                // of things that need a person is the one thing that should
                // pull the eye out of whatever screen you are on.
                const badge =
                  item.key === "overview" && attention.length
                    ? { text: String(attention.length), attention: true }
                    : item.key === "people" && working
                      ? { text: `${working} live`, attention: false }
                      : null;
                return (
                  <a
                    key={item.key}
                    className={cx("nav-item", activeKey === item.key && "active")}
                    href={href(item.path)}
                    aria-current={activeKey === item.key ? "page" : undefined}
                  >
                    <item.icon size="sm" />
                    <span className="truncate">{item.label}</span>
                    {badge && (
                      <span className={cx("nav-badge", badge.attention && "attention")}>
                        {badge.text}
                      </span>
                    )}
                  </a>
                );
              })}
            </div>
          ))}
        </nav>

        <div className="sidebar-foot">
          <button className="engine-pill" onClick={() => setEnginePanel(true)}>
            <i
              className={cx(
                "dot",
                connected ? (engine?.configured === false ? "caution" : "positive") : "critical",
              )}
            />
            <span className="truncate">
              {connected
                ? engine?.configured === false
                  ? "no active config"
                  : "engine connected"
                : authRejected
                  ? "token refused"
                  : "engine unreachable"}
            </span>
            {(health.in_flight ?? 0) > 0 && (
              <span className="t-num" style={{ marginLeft: "auto" }}>
                {health.in_flight} ⟳
              </span>
            )}
          </button>
          <div className="row" style={{ gap: 4 }}>
            <SegmentedControl<ThemePreference>
              size="sm"
              semantics="radio"
              label="Theme"
              value={theme}
              onValueChange={setTheme}
              options={[
                { value: "light", label: "", icon: <LightModeGlyph />, title: "Light" },
                { value: "system", label: "", icon: <ComputerGlyph />, title: "Follow the system" },
                { value: "dark", label: "", icon: <DarkModeGlyph />, title: "Dark" },
              ]}
            />
            <span className="spacer" />
            <SegmentedControl
              size="sm"
              semantics="radio"
              label="Density"
              value={density}
              onValueChange={setDensity}
              options={[
                { value: "compact", label: "S", title: "Compact" },
                { value: "normal", label: "M", title: "Normal" },
                { value: "comfortable", label: "L", title: "Comfortable" },
              ]}
            />
          </div>
        </div>
      </aside>

      <main className="main">
        <header className="topbar">
          {/* The drawer EXISTS only under the layout breakpoint (above it
              the sidebar is always on screen), so the control that opens it
              is hidden by the same media query rather than by a second copy
              of the width rule in JavaScript. It used to show at every
              width, and clicking it wide put an unstyled veil into the
              shell's own grid, which took the sidebar's column and pushed
              the whole app into the next row. */}
          <span className="drawer-toggle" ref={drawerToggle}>
            <IconButton
              label="Sections"
              icon={<MenuGlyph />}
              size="sm"
              onClick={() => setDrawer((v) => !v)}
            />
          </span>
          <h1>{title}</h1>
          <span className="spacer" />
          {/* NAMED ON THE BUTTON, not by its text: the one breakpoint hides
              the label and the hint and leaves the icon, and content under
              `display: none` names nothing, so a narrow window had a search
              button a screen reader announced as "button". */}
          <button
            className="omni"
            aria-label="Search"
            aria-keyshortcuts={SEARCH_ARIA_KEYSHORTCUTS}
            onClick={() => setPaletteOpen(true)}
          >
            <SearchGlyph size="sm" />
            <span className="omni-label">Search</span>
            <Kbd keys={SEARCH_SHORTCUT} />
          </button>
        </header>

        {/* A banner reports; it does not nag. The ONE affordance is for the
            state that resolves for nobody: a refused token. Every other
            degraded state repairs itself when the engine comes back. */}
        {authRejected ? (
          <div className="degraded critical">
            <KeyGlyph size="sm" />
            <span>The engine refused this browser's API token.</span>
            <span className="spacer" />
            <Button variant="secondary" size="small" onClick={() => setTokenOpen(true)}>
              Set token
            </Button>
          </div>
        ) : !connected ? (
          <div className="degraded caution">
            <RefreshGlyph size="sm" />
            <span>
              Reconnecting to the engine. Showing the last state received, and polling meanwhile.
            </span>
          </div>
        ) : engine?.configured === false ? (
          <div className="degraded caution">
            <ManufacturingGlyph size="sm" />
            <span className="col" style={{ gap: 2 }}>
              <span>
                No company configuration is active: no seats are running and inbound webhooks are
                being dropped.
              </span>
              <span className="t-caption">
                Create the company here, or import a company file with{" "}
                <code className="inline">crewlet config import</code>.
              </span>
            </span>
            <span className="spacer" />
            {/* A LINK TO WHERE ONE CAN BE CREATED. It pointed at the
                Configuration screen, which reads and cannot write, so the
                banner reporting the problem sent the reader somewhere that
                could not fix it. */}
            <ButtonLink variant="secondary" size="small" href={href(["org"], { lens: "builder" })}>
              Create the company
            </ButtonLink>
          </div>
        ) : null}

        <div className="screen" id="screen-scroll">
          <div className="screen-inner">{children}</div>
        </div>
      </main>

      {paletteOpen && <CommandPalette onClose={() => setPaletteOpen(false)} />}
      {enginePanel && (
        <EnginePanel
          onClose={() => setEnginePanel(false)}
          onSetToken={() => {
            setEnginePanel(false);
            setTokenOpen(true);
          }}
        />
      )}
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
 * The narrow layout's sections drawer, on the layer stack.
 *
 * The rail itself is always mounted, because above the breakpoint it is the
 * page's navigation. What this adds while it is open is what every other
 * modal has: its veil, Escape, the Tab trap, focus moved in and handed back
 * to the toggle. It used to be a veil with a click handler and nothing else,
 * so Escape did nothing, Tab walked out behind the veil, and a dialog raised
 * over it (the engine panel opened from its own pill) shared no stack with it.
 */
function SectionsDrawer({
  sidebar,
  toggle,
  onClose,
}: {
  sidebar: RefObject<HTMLElement | null>;
  /** The toggle's wrapper, which the stylesheet shows only in the narrow layout. */
  toggle: RefObject<HTMLElement | null>;
  onClose: () => void;
}) {
  const modal = useModalLayer({
    onClose,
    // The row for the screen the reader is on, which is where the rail's
    // own highlight already points, else the first control.
    initialFocus: () => {
      const rail = sidebar.current;
      if (!rail) return null;
      return (
        rail.querySelector<HTMLElement>("[aria-current='page']") ?? focusables(rail)[0] ?? null
      );
    },
  });
  const { panelRef } = modal;
  // The panel is the rail the shell already renders, handed to the stack
  // before its focus effect reads it.
  useLayoutEffect(() => {
    panelRef(sidebar.current);
    return () => panelRef(null);
  }, [panelRef, sidebar]);
  // THE DRAWER ENDS WITH THE NARROW LAYOUT. A tablet turned to landscape
  // crosses the breakpoint with the drawer open, and the rail becomes the
  // page's column again under a veil that traps Tab in it. The toggle's own
  // display is the test, so the width stays a rule of the stylesheet alone.
  const close = useRef(onClose);
  close.current = onClose;
  useEffect(() => {
    function onResize(): void {
      const el = toggle.current;
      if (el && getComputedStyle(el).display === "none") close.current();
    }
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, [toggle]);
  return <div className="drawer-veil" ref={modal.veilRef} role="presentation" />;
}

/** The standard screen header: a title, a sentence saying what it answers. */
export function ScreenHead({
  title,
  sub,
  actions,
  badges,
}: {
  title: ReactNode;
  sub?: ReactNode;
  actions?: ReactNode;
  badges?: ReactNode;
}) {
  return (
    <header className="screen-head">
      <div className="col" style={{ gap: 2, flex: 1 }}>
        <div className="row">
          <span className="screen-title">{title}</span>
          {badges}
        </div>
        {sub && <span className="screen-sub">{sub}</span>}
      </div>
      {actions && <div className="row gap-1">{actions}</div>}
    </header>
  );
}
