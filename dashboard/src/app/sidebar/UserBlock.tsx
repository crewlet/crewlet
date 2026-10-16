/**
 * Who this browser is, at the very foot of the sidebar — what they can do
 * about their own session — and the reader's own preferences.
 *
 * # Three states, and the block says which
 *
 * The answer is the PRINCIPAL the engine resolved for this browser and the
 * seat the identity directory binds it to (`lib/viewer.ts`). BOUND: a person
 * the directory binds to a seat — their name, their login and their seat.
 * UNBOUND: somebody resolved with no seat — a Tier A token's session or a
 * service account, never a person, who holds a human seat for as long as they
 * are here (ADR-0032) — which is ORDINARY, so the block names the login and
 * says it is not bound rather than reporting a fault.
 * NOBODY: the block says so and offers the sign-in. Each is a sentence, never
 * a blank avatar, because "who does the engine think I am" is the question
 * every write this dashboard makes turns on. Their ACCESS is said in the
 * popover, under the login ("Admin", "Tracker · Knowledge", "Overview only"),
 * since it is what decides which of those writes and screens the reader
 * reaches.
 *
 * THE READER IS CALLED BY THEIR OWN NAME, never their seat's (`readerName` in
 * `lib/viewer.ts`): their directory row's name, else their login, with the
 * seat said beside it AS the seat — its name off the chart, or `@handle`
 * where the chart is unread. The block used the seat's name, which is the
 * chart's name for a place and often a role, so a person with no name of
 * their own was "Founder" here while their Account said they had no name. One
 * rule for every state, including a session the viewer has not answered for
 * (it never does for a socket the engine refused): that arm said "Signed in"
 * while the session it read named the reader's seat.
 *
 * AND A READ THAT FAILED IS NOT NOBODY. Where the frame's `GET /auth/session`
 * failed — a 503, the network — rather than being refused, the browser may
 * well hold a session, so the block says it could not tell who this is and
 * still offers the Account (whose page states its own read's failure, with a
 * retry) and the sign-out, which ends this browser's cookie whatever the node
 * can read. It said "Checking who you are" with no link until the tab came
 * back.
 *
 * # The name IS the way to the reader's own Account, and says so
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
 * THE LINK SAYS WHERE IT GOES, three ways, because nothing about a name says
 * "press me for your password": its accessible name ends ", your account";
 * its title is the Account row's own hint, the one description the palette
 * and the popover show; and at rest it draws the rail's pressable mark, a
 * chevron in quiet ink, where it drew nothing until hovered. Its title was the
 * access line, which answered a question the reader had not asked.
 *
 * AND IT IS THE ACCOUNT'S ROW, so it is marked current on `#/account` exactly
 * as a sidebar row is on its own page — raised, with the hairline — since the
 * Account has no row of its own to mark (`place: "menu"`).
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
 * seat_unavailable`), a stage that may not act — never has it answered: the
 * socket stops dialling on a refusal. So the block reads the frame's `GET
 * /auth/session` (`lib/frameSession.ts`) whatever the viewer says, and while
 * the viewer has
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
import { ChevronRightGlyph, Settings2Glyph } from "@crewlethq/icons/glyphs";
import { THEME_OPTIONS, useApplyTheme } from "~/app/appearance.tsx";
import { href, useRoute } from "~/app/router.tsx";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { previewPalette, useViewerPrefs } from "~/lib/prefs.ts";
import { refusalText } from "~/lib/refusal.ts";
import { useFrameSession } from "~/lib/frameSession.ts";
import { goSignIn, signOut } from "~/lib/session.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { SignOutEverywhereDialog } from "~/components/SignOutEverywhere.tsx";
import { readerName, type ViewerState } from "~/lib/viewer.ts";
import { accessLine } from "~/lib/access.ts";
import type { SessionAnswer } from "~/protocol/index.ts";
import { WORKSPACES, workspaceOf, workspaceRow } from "../nav.ts";
import { preload } from "../lazyScreen.ts";

/** The access a principal carries, as one line (`lib/access.ts`'s `accessLine`). */
export function grantsLine(grants: readonly string[]): string {
  return `Access: ${accessLine(grants)}`;
}

/**
 * What the block says about who this browser is. `session` is what `GET
 * /auth/session` answered — the reader's own name and, while the viewer has
 * not answered, who this browser is at all — and `failed` whether that read
 * failed rather than being refused.
 *
 * ONE RULE FOR THE NAME in every state ([readerName]): the reader's own name,
 * else their login, never the seat's. The SEAT is said beside it as the seat —
 * `seatName`, off the chart, or `@handle` where the chart is unread — and the
 * login beside a name that is not it. Nothing is said twice.
 *
 * THE SESSION STANDS IN FOR A VIEWER THAT NEVER ANSWERS. The viewer is a
 * socket question, and the socket stops dialling for a person the engine will
 * not serve (a seat taken out of the chart), so the foot said
 * "Checking who you are" for good while the menu beside it named them — and
 * then "Signed in" while the session it read named their seat.
 */
export function whoLine(
  viewer: ViewerState,
  seatName: string,
  session: Pick<SessionAnswer, "login" | "name" | "grants" | "seat"> | null = null,
  failed = false,
): { name: string; detail: string; grants: string } {
  if (viewer.loading) {
    if (session) {
      return {
        ...said(readerName(session), session.login, session.seat ? `@${session.seat}` : null),
        grants: grantsLine(session.grants ?? []),
      };
    }
    if (failed) {
      return {
        name: "Can’t check who you are",
        detail: "Try reloading",
        grants: "",
      };
    }
    return { name: "Checking who you are", detail: "Asking the engine", grants: "" };
  }
  if (viewer.anonymous) {
    return { name: "Not signed in", detail: "Sign in to continue", grants: "" };
  }
  const grants = grantsLine(viewer.grants);
  const name = readerName(session, viewer.login);
  // UNBOUND IS ORDINARY, so it is said as a fact: the login is who the engine
  // records them as, and their record is kept under it.
  if (viewer.unbound) return { ...said(name, viewer.login, null), grants };
  // THE SEAT AS THE SEAT: its name off the chart, or the engine's own word for
  // it, or its handle where neither has arrived. Its KIND is never captioned:
  // every binding the directory writes is to a human seat and the guard
  // refuses one that is not (`seat_unavailable`), so "human seat" would say
  // what the block already shows.
  const seat = seatName || viewer.name || (viewer.handle ? `@${viewer.handle}` : "");
  return { ...said(name, viewer.login, seat), grants };
}

/**
 * The name and the line under it: the login where it is not the name, then
 * the seat — or, where `seat` is null, that the reader holds none — leaving
 * out whatever would repeat what is already said.
 */
function said(name: string, login: string, seat: string | null): { name: string; detail: string } {
  const parts: string[] = [];
  if (login && login !== name) parts.push(login);
  if (seat === null) parts.push("No seat");
  else if (seat && seat !== name) parts.push(seat);
  return { name, detail: parts.join(" · ") };
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
  const route = useRoute();
  const [everywhere, setEverywhere] = useState(false);
  // THE FRAME'S ONE READ, asked again when the tab comes back: for the people
  // the socket refuses it is the ONLY way to the sign-outs, so a request lost
  // on the way must not leave them none until a reload.
  const frame = useFrameSession();
  const session = frame.answer;
  // FAILED, NOT REFUSED: a `401` is nobody, and `rest.ts` sends the tab to the
  // sign-in; anything else says nothing about whether this browser holds a
  // session (see the module's note).
  const failed = session === null && frame.error !== null && frame.error.status !== 401;
  const who = whoLine(viewer, seatName, session, failed);
  const answered = !viewer.loading && !viewer.anonymous;
  // WHO THIS IS IS SETTLED by the viewer, or by the session while the viewer
  // has not answered (see [whoLine]).
  const resolved = answered || (!viewer.anonymous && session !== null);
  const account = accountOf(viewer, session, failed);
  // THE ACCOUNT'S ROW IS THIS BLOCK, so it is current where the Account is.
  const here = workspaceOf(route.path) === ACCOUNT.key;

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
      {resolved || account?.kind === "unknown" ? (
        // THE READER'S OWN ACCOUNT, for anybody resolved — and for a browser
        // whose session read failed, which may hold one — see the file's doc:
        // it was the seat's page, and nobody found their second factor.
        <a
          className="user-block-who"
          href={href(ACCOUNT.path)}
          title={ACCOUNT.hint}
          aria-current={here ? "page" : undefined}
        >
          {identity}
          <span className="sr-only">, your account</span>
          <ChevronRightGlyph size="xs" className="user-block-go" aria-hidden="true" />
        </a>
      ) : (
        <span className="user-block-who">{identity}</span>
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

/**
 * What the popover may offer about the session behind this browser: nobody's
 * sign-in, a session under its login, or a browser whose session read FAILED
 * and which may hold one.
 */
export type Account = { kind: "nobody" } | { kind: "session"; login: string } | { kind: "unknown" };

/**
 * What the popover may offer, from the viewer and the session's own answer —
 * or null for nothing at all, while the first read is out.
 *
 * A SESSION THE VIEWER HAS NOT ANSWERED FOR (it may never, for a person the
 * socket refuses) is offered its two sign-outs and its Account under its own
 * login. A session read that FAILED (`failed`) is no answer either way, so it
 * is offered the Account, whose page says what its own read finds, and the
 * sign-out of this browser — never "everywhere", which needs to know whose.
 */
export function accountOf(
  viewer: ViewerState,
  session: SessionAnswer | null,
  failed = false,
): Account | null {
  if (viewer.anonymous) return { kind: "nobody" };
  if (viewer.loading) {
    if (session) return { kind: "session", login: session.login };
    return failed ? { kind: "unknown" } : null;
  }
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
      toast.failed(`${what} didn’t go through. ${refusalText(err)}`),
    );
  };
  return (
    <div className="preferences-account">
      {account.kind === "session" ? (
        <>
          <span className="t-label">Signed in as</span>
          <span className="mono truncate">{account.login}</span>
          {grants && <span className="t-caption">{grants}</span>}
        </>
      ) : (
        <span className="t-caption">We couldn’t check who’s signed in.</span>
      )}
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
      {account.kind === "session" && (
        <Button
          size="small"
          variant="ghost"
          title="Sign out on every device and stop your tokens"
          onClick={() => {
            onLeave?.();
            onSignOutEverywhere();
          }}
        >
          Sign out everywhere
        </Button>
      )}
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
