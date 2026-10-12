/**
 * Who this browser is, at the very foot of the sidebar — what they can do
 * about their own session — and the reader's own preferences.
 *
 * # Three states, and the block says which
 *
 * The answer is the PRINCIPAL the engine resolved for this browser and the
 * seat the identity directory binds it to (`lib/viewer.ts`). BOUND: a person
 * the directory binds to a seat — their name, their login and their seat.
 * UNBOUND: somebody resolved with no seat — a Tier A token's session, a
 * service account, or a person recorded before every person held a human
 * seat (ADR-0032) — which is ORDINARY, so the block names the login and says
 * it is not bound rather than reporting a fault.
 * NOBODY: the block says so and offers the sign-in. Each is a sentence, never
 * a blank avatar, because "who does the engine think I am" is the question
 * every write this dashboard makes turns on — and the GRANTS ride beside it,
 * since they are what decides which of those writes and screens the reader
 * reaches.
 *
 * # The name IS the way to the reader's own Account
 *
 * Somebody resolved — bound or not — is a link to `#/account`, the one
 * workspace with no sidebar row (`place: "menu"` in `nav.ts`), because this
 * block already IS the reader: their password, second factor, recovery codes,
 * sessions and personal tokens are there. It linked to the SEAT, and the seat
 * is the company's page about a place in the chart — a person clicking their
 * own name to find their account landed on a profile with no password and no
 * second factor on it, and the only way to either was a text link inside the
 * preferences popover, so a company's people reported that the dashboard had
 * no two-step verification at all. The seat is one click on from there,
 * Account's Profile naming it. Nobody is not a link: there is no account to
 * open, and the popover offers the sign-in.
 *
 * # The popover holds the session's own gestures
 *
 * The Account link again, beside the session's own gestures, because a
 * popover is where a reader looks for "my account" too. And signing out, here
 * or everywhere: the two ways a session ends
 * by its owner's hand, both of which end in a reload at the sign-in
 * (`lib/session.ts`), kept here because the Account page is a screen and this
 * menu is reached even by a person the socket refuses.
 *
 * SIGNING OUT DOES NOT WAIT FOR THE SOCKET. The viewer is a socket question,
 * and a person the socket refuses — a seat taken out of the chart (`403
 * seat_unavailable`), a session without `state:read` — never has it answered:
 * the socket stops dialling on a refusal, and is never dialled for a session
 * without the grant. So the block reads the frame's `GET /auth/session`
 * (`lib/frameSession.ts`) whatever the viewer says, and while the viewer has
 * not answered, a session it names is offered the two sign-outs under its own
 * login.
 *
 * # Preferences are per-browser, and they are here
 *
 * Mode, density and theme: none of them is the company's, and all of them are
 * about the person at this browser, so they sit behind the one control beside
 * that person's name. Settings › Appearance offers the same three with room
 * to show every theme. The mode also has a one-press flip beside the name,
 * the kit's `ThemeToggle`, because it is the preference a reader changes
 * most; "match the system" is a choice in the popover's `ThemeSwitcher`,
 * where there is room to name it.
 *
 * EVERY CONTROL IS CONTROLLED by `lib/prefs.ts`. The kit's controls can keep
 * their own copy under a storage key, and a second copy is a second opinion
 * about which mode is on; the command palette sets them too.
 *
 * THE THEME ROW PREVIEWS. Its list repaints the whole frame in the theme the
 * highlight rests on, and puts back the theme in effect when the list closes
 * or the pointer leaves it; Enter or a click applies (see `app/appearance.tsx`).
 */

import { useState } from "react";
import {
  Button,
  DensitySwitcher,
  IconButton,
  Popover,
  Select,
  ThemeSwitcher,
  ThemeToggle,
  useToast,
} from "@crewlethq/ui";
import { isThemeId } from "@crewlethq/tokens";
import { Settings2Glyph } from "@crewlethq/icons/glyphs";
import { THEME_OPTIONS, useApplyTheme } from "~/app/appearance.tsx";
import { href } from "~/app/router.tsx";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { previewPalette, useViewerPrefs } from "~/lib/prefs.ts";
import { refusalText } from "~/lib/refusal.ts";
import { useFrameSession } from "~/lib/frameSession.ts";
import { goSignIn, signOut } from "~/lib/session.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { SignOutEverywhereDialog } from "~/components/SignOutEverywhere.tsx";
import type { ViewerState } from "~/lib/viewer.ts";
import type { SessionAnswer } from "~/protocol/index.ts";
import { WORKSPACES, workspaceRow } from "../nav.ts";
import { preload } from "../lazyScreen.ts";

/** The grants a principal carries, as one line. */
export function grantsLine(grants: readonly string[]): string {
  return grants.length > 0 ? `Holds ${grants.join(", ")}` : "Holds no grants";
}

/**
 * What the block says about who this browser is. `session` is what `GET
 * /auth/session` answered: the person's own name, for one no seat names —
 * and, while the viewer has not answered, who this browser is at all.
 *
 * THE SESSION STANDS IN FOR A VIEWER THAT NEVER ANSWERS. The viewer is a
 * socket question, and the socket stops dialling for a person the engine will
 * not serve (no `state:read`, a seat taken out of the chart), so the foot said
 * "Checking who you are" for good while the menu beside it named them.
 */
export function whoLine(
  viewer: ViewerState,
  seatName: string,
  session: Pick<SessionAnswer, "login" | "name" | "grants"> | null = null,
): { name: string; detail: string; grants: string } {
  if (viewer.loading) {
    if (session) {
      return session.name
        ? { name: session.name, detail: session.login, grants: grantsLine(session.grants ?? []) }
        : { name: session.login, detail: "Signed in", grants: grantsLine(session.grants ?? []) };
    }
    return { name: "Checking who you are", detail: "Asking the engine", grants: "" };
  }
  const ownName = session?.name ?? "";
  if (viewer.anonymous) {
    return { name: "Not signed in", detail: "Sign in to read and act", grants: "" };
  }
  const grants = grantsLine(viewer.grants);
  // UNBOUND IS ORDINARY, so it is said as a fact: the login is who the engine
  // records them as, and their record is kept under it — beside their own
  // name, where their row holds one, since no seat names them.
  if (viewer.unbound) {
    return ownName
      ? { name: ownName, detail: `${viewer.login} · Not bound to a seat`, grants }
      : { name: viewer.login, detail: "Not bound to a seat", grants };
  }
  const name = viewer.name || viewer.handle;
  // THE LOGIN AND THE SEAT'S ROLE, never the handle with a bare `@`: the
  // login is who signed in, the role is the seat they act as, and the two
  // differ. The seat's KIND is never captioned: every binding the directory
  // writes is to a human seat and the guard refuses one that is not
  // (`seat_unavailable`), so "human seat" would say what the block already
  // shows and "agent seat" would describe a viewer no node answers.
  const role = seatName && seatName !== name ? seatName : "";
  return { name, detail: role ? `${viewer.login} · ${role}` : viewer.login, grants };
}

export function UserBlock({
  viewer,
  seatName,
}: {
  viewer: ViewerState;
  /** The name of the seat this principal is bound to, from the org chart. */
  seatName: string;
}) {
  const prefs = useViewerPrefs();
  const [everywhere, setEverywhere] = useState(false);
  // THE FRAME'S ONE READ, asked again when the tab comes back: for the people
  // the socket refuses it is the ONLY way to the sign-outs, so a request lost
  // on the way must not leave them none until a reload.
  const session = useFrameSession().answer;
  const who = whoLine(viewer, seatName, session);
  const answered = !viewer.loading && !viewer.anonymous;
  // WHO THIS IS IS SETTLED by the viewer, or by the session while the viewer
  // has not answered (see [whoLine]).
  const resolved = answered || (!viewer.anonymous && session !== null);
  const account = accountOf(viewer, session);

  const identity = (
    <>
      {/* RINGED IN THE ACCENT ONCE THE ENGINE HAS SAID WHO THIS IS: this one
          badge IS the reader, which is the single use `ring="brand"` exists
          for (`docs/reference/dashboard-design.md`). A badge standing in for
          nobody, or for an answer still out, is not the reader and is not
          ringed. */}
      <SeatAvatar
        name={resolved ? who.name || "?" : "?"}
        size="sm"
        kind="human"
        ring={resolved ? "brand" : undefined}
        decorative
      />
      <span className="user-block-text">
        <span className="user-block-name truncate">{who.name}</span>
        {who.detail ? <span className="user-block-detail truncate">{who.detail}</span> : null}
      </span>
    </>
  );

  return (
    <div className="user-block">
      {resolved ? (
        // THE READER'S OWN ACCOUNT, for anybody resolved — see the file's
        // doc: it was the seat's page, and nobody found their second factor.
        <a className="user-block-who" href={href(ACCOUNT.path)} title={who.grants}>
          {identity}
        </a>
      ) : (
        <span className="user-block-who" title={who.grants || undefined}>
          {identity}
        </span>
      )}
      <ThemeToggle size="sm" value={prefs.theme} onChange={prefs.setTheme} />
      {/* START-ALIGNED, OPENING INTO THE ROOM TO ITS RIGHT. The trigger sits at
          the sidebar's inner edge, a couple of hundred pixels from the left of
          the window, so an end-aligned panel would have to be slid back
          across the window edge to fit at all.

          And the kit cannot draw it end-aligned from here: @crewlethq/ui
          0.5.0's Popover (and Menu) asks whether the anchor left the layer
          using the END-ALIGNED PANEL'S left edge with the ANCHOR'S width, so
          any panel wider than the anchor's right edge plus its width reads as
          an anchor carried off screen and closes on the frame it opened. That
          is a kit defect, raised against uilet; the test beside this block
          lays the sidebar out at its real coordinates so a return to `end`
          goes red while the kit still has it. */}
      {everywhere && <SignOutEverywhereDialog onClose={() => setEverywhere(false)} />}
      <Popover
        label="Account and preferences"
        side="top"
        align="start"
        trigger={(open, toggle) => (
          <IconButton
            label="Account and preferences"
            icon={<Settings2Glyph size="sm" />}
            size="sm"
            variant="ghost"
            aria-expanded={open}
            onClick={toggle}
          />
        )}
      >
        {(close) => (
          <>
            {account && (
              <AccountActions
                account={account}
                grants={who.grants}
                onLeave={close}
                onSignOutEverywhere={() => setEverywhere(true)}
              />
            )}
            <Preferences />
          </>
        )}
      </Popover>
    </div>
  );
}

/** What the popover may offer about the session behind this browser. */
export type Account = { kind: "nobody" } | { kind: "session"; login: string };

/**
 * What the popover may offer, from the viewer and the session's own answer —
 * or null for nothing at all.
 *
 * A SESSION THE VIEWER HAS NOT ANSWERED FOR (it may never, for a person the
 * socket refuses) is offered its two sign-outs and its Account under its own
 * login.
 */
export function accountOf(viewer: ViewerState, session: SessionAnswer | null): Account | null {
  if (viewer.anonymous) return { kind: "nobody" };
  if (viewer.loading) return session ? { kind: "session", login: session.login } : null;
  return { kind: "session", login: viewer.login };
}

/** The menu's own rows: the workspaces no sidebar row draws (`place: "menu"`). */
const MENU_ROWS = WORKSPACES.filter((ws) => ws.place === "menu");

/** The reader's own Account, which the block's name opens. */
const ACCOUNT = workspaceRow("account")!;

/** The session's own gestures. Exported for its suite. */
export function AccountActions({
  account,
  grants,
  onLeave,
  onSignOutEverywhere,
}: {
  account: Account;
  /** The grants line, said under the login — empty where nothing settles it. */
  grants: string;
  /** Close the menu: a link out of it leads to a page the menu would cover. */
  onLeave?: () => void;
  /** Ask whether to sign out everywhere, outside the menu — see `SignOutEverywhereDialog`. */
  onSignOutEverywhere: () => void;
}) {
  const toast = useToast();
  const { socket } = useClient();
  if (account.kind === "nobody") {
    return (
      <div className="preferences-account">
        <Button size="small" variant="primary" onClick={goSignIn}>
          Sign in
        </Button>
      </div>
    );
  }
  // A FAILED SIGN-OUT IS SAID, and the page stays: see `signOut` for why a
  // sign-out that nothing answered must not look like one that worked.
  const run = (gesture: () => Promise<void>, what: string) => () => {
    gesture().catch((err: unknown) =>
      toast.failed(`${what} did not go through. ${refusalText(err)}`),
    );
  };
  return (
    <div className="preferences-account">
      <span className="t-label">Signed in as</span>
      <span className="mono truncate">{account.login}</span>
      {grants && <span className="t-caption">{grants}</span>}
      {/* A LINK, not a button: it is a page, so ⌘-click opens it in a tab. */}
      {MENU_ROWS.map((ws) => (
        <a
          key={ws.key}
          className="t-link"
          href={href(ws.path)}
          title={ws.hint}
          onPointerEnter={() => preload(ws.path)}
          onFocus={() => preload(ws.path)}
          onClick={onLeave}
        >
          {ws.label}
        </a>
      ))}
      <Button size="small" variant="secondary" onClick={run(() => signOut(socket), "Signing out")}>
        Sign out
      </Button>
      <Button
        size="small"
        variant="ghost"
        title="Ends every session you hold, on every device, and every personal access token"
        onClick={() => {
          onLeave?.();
          onSignOutEverywhere();
        }}
      >
        Sign out everywhere
      </Button>
    </div>
  );
}

/**
 * The per-browser preferences. Exported for its suite, which drives each
 * control and reads `lib/prefs.ts` back.
 */
export function Preferences() {
  const prefs = useViewerPrefs();
  const applyTheme = useApplyTheme();
  return (
    <div className="preferences">
      {/* EVERY CONTROL NAMES ITSELF: each switcher is a radio group carrying
          its own label and the select is named Theme, so a second label round
          one would be read twice. The two switchers FILL one box of one width,
          so a row of glyphs and a row of letters draw the same cells, and the
          theme's field is that width too: three controls, one column. */}
      <div className="preferences-row">
        <span className="t-label" aria-hidden="true">
          Mode
        </span>
        <div className="preferences-switch">
          <ThemeSwitcher
            label="Mode"
            value={prefs.theme}
            onChange={prefs.setTheme}
            size="sm"
            fill
          />
        </div>
      </div>
      <div className="preferences-row">
        <span className="t-label" aria-hidden="true">
          Density
        </span>
        <div className="preferences-switch">
          <DensitySwitcher
            label="Density"
            value={prefs.density}
            onChange={prefs.setDensity}
            size="sm"
            fill
          />
        </div>
      </div>
      <div className="preferences-row">
        <span className="t-label" aria-hidden="true">
          Theme
        </span>
        <div className="preferences-switch">
          <Select
            ariaLabel="Theme"
            size="sm"
            menuClassName="preferences-theme-menu"
            options={THEME_OPTIONS}
            value={prefs.palette}
            onChange={(next) => {
              if (isThemeId(next)) applyTheme(next);
            }}
            onHighlight={(option) =>
              previewPalette(option && isThemeId(option.value) ? option.value : null)
            }
          />
        </div>
      </div>
    </div>
  );
}
