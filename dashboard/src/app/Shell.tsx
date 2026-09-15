/**
 * The application frame.
 *
 * It is the design system's shell: a rail, a top bar, a banner slot and ONE
 * scroll container, which is what lets the router restore a scroll position
 * per history entry. A page with three independent scrollers has three
 * positions and no way to name them. What this file adds is the engine's own
 * half: which sections exist, what the rail's foot says about the engine, and
 * which surfaces the chrome can raise.
 */

import { useEffect, useMemo, useState, type ReactNode } from "react";
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
import {
  useAgents,
  useClient,
  useConnection,
  useEngineHealth,
  useOrg,
  useOrgBudget,
  useSandboxes,
} from "~/lib/store-hooks.ts";
import { attentionQueue } from "~/lib/attention.ts";
import { plural } from "~/lib/format.ts";
import { indexOrg } from "~/lib/seats.ts";
import {
  AppShell,
  BrandLockup,
  Button,
  ButtonLink,
  Callout,
  Count,
  DensitySwitcher,
  InlineCode,
  Kbd,
  Menu,
  NavGroup,
  NavItem,
  SearchTrigger,
  SidebarNav,
  StatusDot,
  Tag,
  ThemeSwitcher,
  isComposing,
  isModalLayerOpen,
  useNow,
} from "@crewlethq/ui";
import { onTokenRequested } from "~/protocol/index.ts";
import { CrewletIcon } from "@crewlethq/icons";
import {
  KeyGlyph,
  ManufacturingGlyph,
  PowerSettingsNewGlyph,
  RefreshGlyph,
  SettingsGlyph,
} from "@crewlethq/icons/glyphs";

/**
 * Where the reader is typing, for the shortcuts the page owns.
 *
 * A LISTBOX COUNTS AS TYPING. A bare slash is search on this page and the
 * first letter of a type-ahead inside a list of choices, and every dropdown in
 * this product is the design system's listbox rather than the platform's own,
 * so the element holding the keys is a div with a role rather than an input.
 * Asked only about `INPUT` and `TEXTAREA`, the shell swallowed the press and a
 * reader trying to reach the seats beginning with a slash opened search.
 */
function isTypingTarget(el: Element | null): boolean {
  if (!(el instanceof HTMLElement)) return false;
  if (el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.isContentEditable) return true;
  const role = el.getAttribute("role");
  return role === "listbox" || role === "combobox" || role === "option" || role === "menu";
}

export function Shell({ children }: { children: ReactNode }) {
  const route = useRoute();
  const { socket } = useClient();
  const { connected, authRejected, health } = useConnection();
  const agents = useAgents();
  const sandboxes = useSandboxes();
  const budget = useOrgBudget();
  const org = useOrg();
  const now = useNow();

  const [paletteOpen, setPaletteOpen] = useState(false);
  const [enginePanel, setEnginePanel] = useState(false);
  const [tokenOpen, setTokenOpen] = useState(false);

  // ONE READ OF THE ENGINE'S HEALTH, shared with the panel. The rail polled at
  // 15 seconds and the panel at 5, so for as long as ten seconds after a
  // revision applied the rail said one thing and the panel open in front of it
  // said another.
  const { data: engine } = useEngineHealth();

  // The socket asks ONCE per refusal: a reconnect backoff must not reopen a
  // dialog forever. Everything after that is the banner and the engine panel.
  useEffect(() => {
    socket.onAuthRejected(() => setTokenOpen(true));
  }, [socket]);

  // And from anywhere else that discovers it needs a credential: an
  // auth-gated answer on a screen the socket was never refused for.
  useEffect(() => onTokenRequested(() => setTokenOpen(true)), []);

  // The shortcuts that OPEN search, and nothing else. Closing a surface
  // belongs to the surface and the layer stack: an Escape handled here as well
  // closed search and the engine panel along with whatever sat above or
  // beneath them, and a toggle here closed search from beneath a dialog raised
  // over it. A press a surface already handled (search closing on its own
  // chord) is left alone.
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
      // while somebody is typing, and not as part of a chord (Ctrl or Command
      // with "/" is the browser's or the platform's).
      const chord = e.ctrlKey || e.metaKey || e.altKey;
      if (e.key === "/" && !chord && !isTypingTarget(document.activeElement)) {
        e.preventDefault();
        setPaletteOpen(true);
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

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
  const working = agents.filter((a) => a.state === "working").length;

  /*
   * A banner REPORTS; it does not nag. The one affordance is for the state
   * that resolves for nobody: a refused token. Every other degraded state
   * repairs itself when the engine comes back, so it is drawn and nothing
   * more.
   */
  const banner = authRejected ? (
    <Callout
      variant="danger"
      layout="banner"
      icon={<KeyGlyph />}
      action={
        <Button variant="secondary" size="small" onClick={() => setTokenOpen(true)}>
          Set token
        </Button>
      }
    >
      The engine refused this browser&apos;s API token.
    </Callout>
  ) : !connected ? (
    <Callout variant="warning" layout="banner" icon={<RefreshGlyph />}>
      Reconnecting to the engine. Showing the last state received, and polling meanwhile.
    </Callout>
  ) : engine?.configured === false ? (
    <Callout
      variant="warning"
      layout="banner"
      icon={<ManufacturingGlyph />}
      title="No company configuration is active"
      action={
        /* A LINK TO WHERE ONE CAN BE CREATED. It pointed at the Configuration
           screen, which reads and cannot write, so the banner reporting the
           problem sent the reader somewhere that could not fix it. */
        <ButtonLink variant="secondary" size="small" href={href(["org"], { lens: "builder" })}>
          Create the company
        </ButtonLink>
      }
    >
      No seats are running and inbound webhooks are being dropped. Create the company here, or
      import a company file with <InlineCode>crewlet config import</InlineCode>.
    </Callout>
  ) : null;

  return (
    <>
      <AppShell
        mainId="screen-scroll"
        navigationKey={route.hash}
        toggleLabel="Sections"
        sidebar={
          <AppShell.Rail
            header={
              <BrandLockup
                name="Crewlet"
                mark={<CrewletIcon aria-hidden />}
                context={org?.name}
                href={href([])}
              />
            }
            footer={
              <>
                <EnginePill
                  connected={connected}
                  authRejected={authRejected}
                  configured={engine?.configured !== false}
                  inFlight={health.in_flight ?? 0}
                  onOpen={() => setEnginePanel(true)}
                />
                <div className="row gap-1">
                  {/* THE SETTINGS MENU IS WHERE THE TOKEN LIVES. The token
                      dialog used to be reachable only when the engine had
                      refused it, so an operator on a shared machine could not
                      sign out while their token still worked. */}
                  <Menu
                    label="Settings"
                    icon={<SettingsGlyph size="sm" />}
                    items={[
                      {
                        key: "token",
                        label: "API token",
                        icon: <KeyGlyph />,
                        description: "Set the token this browser sends, or sign out",
                        onSelect: () => setTokenOpen(true),
                      },
                      {
                        key: "engine",
                        label: "Engine",
                        icon: <PowerSettingsNewGlyph />,
                        description: "What this node is running, and since when",
                        onSelect: () => setEnginePanel(true),
                      },
                    ]}
                  />
                  <span className="spacer" />
                  <ThemeAndDensity />
                </div>
              </>
            }
          >
            <SidebarNav label="Sections">
              {NAV.map((group) => (
                <NavGroup key={group.key} label={group.label}>
                  {group.items.map((item) => (
                    <NavItem
                      key={item.key}
                      label={item.label}
                      icon={<item.icon size="sm" />}
                      href={href(item.path)}
                      current={activeKey === item.key}
                      badge={navBadge(item.key, attention.length, working)}
                    />
                  ))}
                </NavGroup>
              ))}
            </SidebarNav>
          </AppShell.Rail>
        }
        topbar={
          <AppShell.Topbar
            title={titleFor(route.path)}
            actions={
              <SearchTrigger
                label="Search"
                shortcut={<Kbd keys={SEARCH_SHORTCUT} />}
                keyshortcuts={SEARCH_ARIA_KEYSHORTCUTS}
                onClick={() => setPaletteOpen(true)}
              />
            }
          />
        }
        banner={banner}
      >
        {children}
      </AppShell>

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
    </>
  );
}

/**
 * The one badge in the chrome allowed a status hue.
 *
 * The count of things that need a person is the one thing that should pull the
 * eye out of whatever screen you are on. It SAYS WHAT IT COUNTS: a bare number
 * beside a row was announced as "Overview 3", which a reader could take for a
 * third overview.
 */
function navBadge(key: string, attention: number, working: number): ReactNode {
  if (key === "overview" && attention > 0) {
    return (
      <Count
        value={attention}
        label={attention === 1 ? "thing waiting on somebody" : "things waiting on somebody"}
      />
    );
  }
  if (key === "people" && working > 0) {
    // No label beside this one: "3 live" is already a sentence a reader is
    // told, where a bare number after a row's name is read as "People 3" and
    // can be taken for a third People.
    return <Count value={`${working} live`} />;
  }
  return null;
}

/** The theme and density choices, which sit together at the foot of the rail. */
function ThemeAndDensity() {
  return (
    <>
      <ThemeSwitcher size="sm" storageKey="crewlet_theme" label="Theme" />
      <DensitySwitcher
        size="sm"
        storageKey="crewlet_density"
        label="Density"
        compactLabel="Compact"
        normalLabel="Normal"
        comfortableLabel="Comfortable"
      />
    </>
  );
}

/**
 * What the engine is doing, at the foot of the rail.
 *
 * The state is a word and a mark, never the mark alone, and the count of turns
 * in flight is a labelled number: it was an unlabelled glyph beside a digit, so
 * a screen reader read a seat's busiest moment as "3" and nothing else.
 */
function EnginePill({
  connected,
  authRejected,
  configured,
  inFlight,
  onOpen,
}: {
  connected: boolean;
  authRejected: boolean;
  configured: boolean;
  inFlight: number;
  onOpen: () => void;
}) {
  const tone = connected ? (configured ? "success" : "warning") : "danger";
  const said = connected
    ? configured
      ? "engine connected"
      : "no active config"
    : authRejected
      ? "token refused"
      : "engine unreachable";
  return (
    <Button
      variant="tertiary"
      size="small"
      onClick={onOpen}
      leadingIcon={<StatusDot tone={tone} />}
      trailingIcon={
        inFlight > 0 ? (
          <Tag variant="info" size="sm">
            {plural(inFlight, "turn")} in flight
          </Tag>
        ) : undefined
      }
    >
      {said}
    </Button>
  );
}
