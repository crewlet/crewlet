/**
 * The application frame.
 *
 * Sidebar, topbar, and ONE scroll container — which is what lets the router
 * restore a scroll position per history entry. A page with three independent
 * scrollers has three positions and no way to name them.
 */

import { useEffect, useMemo, useState, type ReactNode } from "react";
import { NAV, activeNavKey, titleFor } from "./nav.ts";
import { href, useNavigator, useRoute } from "./router.tsx";
import { CommandPalette } from "./CommandPalette.tsx";
import { EnginePanel } from "./EnginePanel.tsx";
import { TokenDialog } from "./TokenDialog.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { Badge, Button, Segmented, cx } from "~/ui/primitives.tsx";
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
import { useNow } from "~/lib/clock.ts";
import { useDensity, useTheme, type ThemeChoice } from "~/lib/theme.ts";
import { onTokenRequested } from "~/protocol/index.ts";

export function Shell({ children }: { children: ReactNode }) {
  const route = useRoute();
  const nav = useNavigator();
  const { socket } = useClient();
  const { connected, authRejected, health } = useConnection();
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const budget = useOrgBudget();
  const org = useOrg();
  const now = useNow();
  const [theme, setTheme] = useTheme();
  const [density, setDensity] = useDensity();

  const [paletteOpen, setPaletteOpen] = useState(false);
  const [enginePanel, setEnginePanel] = useState(false);
  const [tokenOpen, setTokenOpen] = useState(false);
  const [drawer, setDrawer] = useState(false);

  const { data: engine } = useQuery("stream", undefined, { pollMs: 15_000 });

  // The socket asks ONCE per refusal — a reconnect backoff must not reopen a
  // dialog forever. Everything after that is the banner and the engine panel.
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
      if (e.key === "Escape") {
        setPaletteOpen(false);
        setEnginePanel(false);
      }
      // A bare "/" opens search the way every list-shaped tool does — but not
      // while somebody is typing into a field.
      const el = document.activeElement;
      const typing =
        el instanceof HTMLElement &&
        (el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.isContentEditable);
      if (e.key === "/" && !typing) {
        e.preventDefault();
        setPaletteOpen(true);
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // Close the mobile drawer whenever the route changes — a drawer left open
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
      {drawer && <div className="drawer-veil" onClick={() => setDrawer(false)} />}
      <aside className="sidebar" data-open={drawer}>
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
                    <Icon name={item.icon} size="sm" />
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
            <Segmented<ThemeChoice>
              size="sm"
              ariaLabel="Theme"
              value={theme}
              onChange={setTheme}
              options={[
                { value: "light", label: "", icon: "sun", title: "Light" },
                { value: "system", label: "", icon: "monitor", title: "Follow the system" },
                { value: "dark", label: "", icon: "moon", title: "Dark" },
              ]}
            />
            <span className="spacer" />
            <Segmented
              size="sm"
              ariaLabel="Density"
              value={density}
              onChange={setDensity}
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
          {/* The drawer EXISTS only under the layout breakpoint — above it
              the sidebar is always on screen — so the control that opens it
              is hidden by the same media query rather than by a second copy
              of the width rule in JavaScript. It used to show at every
              width, and clicking it wide put an unstyled veil into the
              shell's own grid, which took the sidebar's column and pushed
              the whole app into the next row. */}
          <span className="drawer-toggle">
            <Button
              icon="menu"
              variant="ghost"
              size="sm"
              title="Sections"
              onClick={() => setDrawer((v) => !v)}
            />
          </span>
          <h1>{title}</h1>
          <span className="spacer" />
          <button className="omni" onClick={() => setPaletteOpen(true)}>
            <Icon name="search" size="sm" />
            <span className="omni-label">Search</span>
            <kbd>⌘K</kbd>
          </button>
        </header>

        {/* A banner reports; it does not nag. The ONE affordance is for the
            state that resolves for nobody — a refused token. Every other
            degraded state repairs itself when the engine comes back. */}
        {authRejected ? (
          <div className="degraded critical">
            <Icon name="key" size="sm" />
            <span>The engine refused this browser's API token.</span>
            <span className="spacer" />
            <Button size="sm" onClick={() => setTokenOpen(true)}>
              Set token
            </Button>
          </div>
        ) : !connected ? (
          <div className="degraded caution">
            <Icon name="refresh" size="sm" />
            <span>
              Reconnecting to the engine — showing the last state received, polling meanwhile.
            </span>
          </div>
        ) : engine?.configured === false ? (
          <div className="degraded caution">
            <Icon name="sliders" size="sm" />
            <span>
              No company configuration is active: no seats are running and inbound webhooks are
              being dropped.
            </span>
            <span className="spacer" />
            <Button size="sm" onClick={() => nav.to(["config"])}>
              Configuration
            </Button>
          </div>
        ) : null}

        <div className="screen" id="screen-scroll">
          <div className="screen-inner">{children}</div>
        </div>
      </main>

      {paletteOpen && <CommandPalette onClose={() => setPaletteOpen(false)} />}
      {enginePanel && (
        <div className="veil" onMouseDown={() => setEnginePanel(false)} role="presentation">
          <div onMouseDown={(e) => e.stopPropagation()}>
            <EnginePanel
              onClose={() => setEnginePanel(false)}
              onSetToken={() => {
                setEnginePanel(false);
                setTokenOpen(true);
              }}
            />
          </div>
        </div>
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

export { Badge };
