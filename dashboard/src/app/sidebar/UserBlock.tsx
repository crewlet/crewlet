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
 * header of the reader's menu ("Admin", "Worklet · Doclet", "Overview
 * only"), since it is what decides which of those writes and screens the
 * reader reaches.
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
 * A CREDENTIAL THAT IS NOT A PERSON BUT ACTS AS A SEAT is called by the SEAT,
 * because it has no name of its own and the seat is who it acts as: a Tier A
 * token bound to Miles's seat is "Miles", "via API token" under it.
 * It was "token:review" over "Miles", the credential's spelling as the name
 * and the place it acts for as the aside. The row says only the KIND of
 * credential (`viaKind`): the token's name beside it ("via API token review")
 * cut the line short in the rail's width and said what the reader looks up
 * rather than what they glance at. The whole line (`viaLine`) is where the
 * reader looks for detail: the row's title, which a screen reader hears as
 * its description, and the menu's header, under the access. Its badge is the seat's own face,
 * the initials, wearing a key on its corner (the kit's `Avatar` `mark`), so
 * the face says who and the corner says how. Which credential it is comes
 * from the login's grammar (`lib/login.ts`), never the session's `kind`: a
 * bound token's session is answered as a person's. A credential bound to no
 * seat has no face to keep, so it stays its login with a key for a face.
 *
 * AND A READ THAT FAILED IS NOT NOBODY. Where the frame's `GET /auth/session`
 * failed — a 503, the network — rather than being refused, the browser may
 * well hold a session, so the block says it could not tell who this is and
 * still offers the Account (whose page states its own read's failure, with a
 * retry) and the sign-out, which ends this browser's cookie whatever the node
 * can read. It said "Checking who you are" with no link until the tab came
 * back.
 *
 * # The name OPENS the reader's own menu
 *
 * Pressing who you are is where a reader looks for their account and for
 * signing out, so for anybody resolved (and for a browser whose session read
 * failed, which may hold a session) the row is the MENU BUTTON (the kit's
 * `AccountRow` `menu`), ending in the up-and-down chevron a menu's trigger
 * draws. It linked straight to `#/account`, and the session's own gestures sat
 * in a second popover beside it, mixed with the preferences: the two ways to
 * the same account, and a popover that said "Signed in as" over the very name
 * the row had just said.
 *
 * THE MENU SAYS ONLY WHAT THE ROW DOES NOT. Its header is the reader's ACCESS
 * ("Admin", "Worklet · Doclet", "Overview only"), which decides which
 * writes and screens they reach and which the row has no room for; the name,
 * the login and the seat are the row's. Under it:
 *
 * - **Account**, the workspace with no sidebar row (`place: "menu"` in
 *   `nav.ts`): their password, second factor, recovery codes, sessions and
 *   personal tokens. It linked to the SEAT once, and the seat is the
 *   company's page about a place in the chart, so a company's people reported
 *   that the dashboard had no two-step verification at all. The seat is one
 *   step on from there, Account's Profile naming it.
 * - **Sign out** and **Sign out everywhere**, the two ways a session ends by
 *   its owner's hand, both of which end in a reload at the sign-in
 *   (`lib/session.ts`), kept here because the Account page is a screen and
 *   this menu is reached even by a person the socket refuses. Everywhere is
 *   drawn in the danger ink and says what it ends.
 *
 * NOBODY is offered the sign-in, and the row says so; a failed read is
 * offered the Account (whose page states its own read's failure, with a
 * retry) and the sign-out of this browser, never "everywhere", which needs to
 * know whose.
 *
 * AND IT IS THE ACCOUNT'S ROW, so it is marked current on `#/account` exactly
 * as a sidebar row is on its own page, raised, with the hairline, since the
 * Account has no row of its own to mark (`place: "menu"`).
 *
 * SIGNING OUT DOES NOT WAIT FOR THE SOCKET. The viewer is a socket question,
 * and a person the socket refuses (a seat taken out of the chart, `403
 * seat_unavailable`, a stage that may not act) never has it answered: the
 * socket stops dialling on a refusal. So the block reads the frame's `GET
 * /auth/session` (`lib/frameSession.ts`) whatever the viewer says, and while
 * the viewer has not answered, a session it names is offered the two
 * sign-outs.
 *
 * # Preferences are per-browser, and they are here
 *
 * Mode, density and theme: none of them is the company's, and all of them are
 * about the person at this browser, so they sit behind the one control beside
 * that person's name, **Preferences**, drawn with a palette and holding
 * nothing else: the session's gestures are the row's own menu. This is the one
 * place they are set on a page, and the command palette sets them too.
 *
 * ONE CONTROL ON THE ROW. The mode had a one-press flip beside the name as
 * well, and the foot then carried the link's chevron and two glyph buttons
 * beside a name already cut short by the rail's width. The mode is the
 * popover's first row, one press away.
 *
 * EVERY CONTROL IS CONTROLLED by `lib/prefs.ts`. The kit's controls can keep
 * their own copy under a storage key, and a second copy is a second opinion
 * about which mode is on; the command palette sets them too.
 *
 * THE THEME ROW PREVIEWS. Its list repaints the whole frame in the theme the
 * highlight rests on, and puts back the theme in effect when the list closes
 * or the pointer leaves it; Enter or a click applies (see `app/appearance.tsx`).
 *
 * # Folded, the foot is the places and the reader's face
 *
 * In the compact sidebar (`railShape.ts`) the foot has no room for a name, so
 * the places stand as tiles of the kit's `CompactNav` above the reader's face,
 * and the face alone is the kit's compact `AccountRow`: the same menu button,
 * named by the same words, which it says in its tooltip. Preferences has no
 * glyph of its own there, so the reader's menu offers it, and it opens as a
 * dialog holding the same three controls: a choice the full sidebar keeps one
 * press away is never lost by folding it.
 */

import { useState, type ReactNode } from "react";
import {
  AccountRow,
  CompactNav,
  CompactNavItem,
  DensitySwitcher,
  DescriptionList,
  IconButton,
  IconButtonLink,
  Modal,
  Popover,
  Select,
  Tag,
  ThemeSwitcher,
  Text,
  useToast,
  type AccountRowMenu,
  type MenuEntry,
} from "@crewlethq/ui";
import { isThemeId } from "@crewlethq/tokens";
import { KeyGlyph, LogInGlyph, LogOutGlyph, PaletteGlyph } from "@crewlethq/icons/glyphs";
import { THEME_OPTIONS, useApplyTheme } from "~/app/appearance.tsx";
import { useNavigator, useRoute } from "~/app/router.tsx";
import { NamedGlyph } from "@crewlethq/icons/glyphs/registry";
import { SeatAvatar } from "~/ui/SeatAvatar.tsx";
import { previewPalette, useViewerPrefs } from "~/lib/prefs.ts";
import { refusalText } from "~/lib/refusal.ts";
import { useFrameSession } from "~/lib/frameSession.ts";
import { goSignIn, signOut } from "~/lib/session.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { SignOutEverywhereDialog } from "~/components/SignOutEverywhere.tsx";
import { readerName, type ViewerState } from "~/lib/viewer.ts";
import { accessLine } from "~/lib/access.ts";
import { loginKind, tierATokenOf } from "~/lib/login.ts";
import type { SessionAnswer } from "~/protocol/index.ts";
import { WORKSPACES, workspaceOf, workspaceRow } from "../nav.ts";
import { preload } from "../lazyScreen.ts";

/** The access a principal carries, as one line (`lib/access.ts`'s `accessLine`): "Admin". */
export function grantsLine(grants: readonly string[]): string {
  return accessLine(grants);
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
      const grants = grantsLine(session.grants ?? []);
      // A CREDENTIAL ACTING AS A SEAT is the seat: the chart is unread
      // under a viewer that has not answered, so the handle stands in.
      const via = viaKind(session.login);
      if (via && session.seat) return { name: `@${session.seat}`, detail: via, grants };
      return {
        ...said(readerName(session), session.login, session.seat ? `@${session.seat}` : null),
        grants,
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
  // A CREDENTIAL ACTING AS A SEAT is called by the seat, and the line under
  // it says what kind of credential it is signed in with (see the file's doc).
  const via = viaKind(viewer.login);
  if (via && seat) return { name: seat, detail: via, grants };
  return { ...said(name, viewer.login, seat), grants };
}

/**
 * The credential a login that is not a person's names: its kind, as the row
 * says it, and the name that tells it from every other of that kind. Null for
 * a person's login, which is never said this way. Decided by the login's
 * grammar (`lib/login.ts`), because a seat-bound token's session is answered
 * with a person's `kind`.
 */
function credentialOf(login: string): { kind: string; name: string } | null {
  const token = tierATokenOf(login);
  if (token) return { kind: "via API token", name: token };
  return loginKind(login) === "machine" ? { kind: "via service account", name: login } : null;
}

/**
 * How a credential that is not a person is signed in, as the line under the
 * seat it acts as in the row: "via API token", "via service account". Empty
 * for a person's login.
 */
export function viaKind(login: string): string {
  return credentialOf(login)?.kind ?? "";
}

/**
 * The whole of how a credential is signed in, with the name that tells it
 * apart: "via API token review", "via service account ci:deploy". Said where
 * the reader looks for detail (the row's title and the menu's header), never
 * in the row itself. Empty for a person's login.
 */
export function viaLine(login: string): string {
  const credential = credentialOf(login);
  return credential ? `${credential.kind} ${credential.name}` : "";
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

/**
 * A place the foot reaches by its glyph, beside the reader's own row: Settings.
 * The sidebar says where it goes and whether the reader is there; the row
 * draws it as the kit's icon link, before Preferences.
 */
export interface FootPlace {
  key: string;
  label: string;
  icon: ReactNode;
  href: string;
  /** Fetches where it leads, as a nav row does on the pointer or the focus. */
  onPrefetch: () => void;
  current: boolean;
  /** The hint, and a lock's reason, after the name in the tooltip. */
  title?: string | undefined;
  /** Why the reader cannot open it, as a nav row names its lock; absent when they can. */
  locked?: string | undefined;
}

export function UserBlock({
  viewer,
  seatName,
  places = [],
  compact = false,
}: {
  viewer: ViewerState;
  /** The name of the seat this principal is bound to, from the org chart. */
  seatName: string;
  /** The places drawn as icons beside the row, before Preferences. */
  places?: readonly FootPlace[];
  /** Whether the sidebar is folded to its compact rail (see the file's doc). */
  compact?: boolean;
}) {
  const route = useRoute();
  const [everywhere, setEverywhere] = useState(false);
  const [preferences, setPreferences] = useState(false);
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

  // A PRINCIPAL THAT IS NOT A PERSON (an API token's session, a service
  // account) is told by its login, never by the session's `kind`, which is a
  // person's for a token bound to a seat. ACTING AS A SEAT it keeps the seat's
  // face, the initials, and wears a key on its corner; BOUND TO NONE it has no
  // face to keep, so the key IS its face: the initials of `token:reader` drew
  // "TO", as if it were somebody.
  const login = answered ? viewer.login : (session?.login ?? "");
  const bound = answered ? !viewer.unbound : Boolean(session?.seat);
  // THE WHOLE LINE, with the credential's name, for the title and the menu.
  const via = resolved ? viaLine(login) : "";
  const credential = via !== "";

  const menu = useAccountMenu(
    account,
    who.grants,
    () => setEverywhere(true),
    via,
    // FOLDED, THE MENU IS THE WAY TO PREFERENCES (see the file's doc).
    compact ? () => setPreferences(true) : undefined,
  );

  return (
    <>
      {everywhere && <SignOutEverywhereDialog onClose={() => setEverywhere(false)} />}
      {/* OPEN UNTIL CLOSED, whatever shape the sidebar takes meanwhile: a
          window resized into the drawer's width keeps the dialog it opened. */}
      {preferences && (
        <Modal open size="sm" title="Preferences" onClose={() => setPreferences(false)}>
          <Preferences bare />
        </Modal>
      )}
      {compact && places.length > 0 && (
        // THE PLACES AS TILES OVER THE FACE, each the same link its glyph is
        // in the full foot, naming its lock the way a nav row does.
        <CompactNav label="Settings">
          {places.map((place) => (
            <CompactNavItem
              key={place.key}
              label={place.label}
              icon={place.icon}
              href={place.href}
              current={place.current}
              locked={place.locked}
              onPrefetch={place.onPrefetch}
            />
          ))}
        </CompactNav>
      )}
      <AccountRow
        compact={compact}
        avatar={
          // RINGED IN THE ACCENT ONCE THE ENGINE HAS SAID WHO THIS IS: this
          // one badge IS the reader, which is the single use `ring="brand"`
          // exists for (`docs/reference/dashboard-design.md`). A badge
          // standing in for nobody, or for an answer still out, is not the
          // reader and is not ringed. A badge wearing a mark draws no ring
          // either: the kit cuts the mark's notch from the outline a ring
          // would follow. AT THE MEDIUM STEP in the full row, which is what
          // the kit's account row sizes its badge column around: the small
          // badge sat in that column with room to spare and read as a
          // placeholder. FOLDED, AT THE SMALL STEP, the height of the compact
          // tiles above it, on whose line the kit centres the face.
          <SeatAvatar
            name={resolved ? who.name || "?" : "?"}
            size={compact ? "sm" : "md"}
            kind="human"
            ring={resolved ? "brand" : undefined}
            icon={credential && !bound ? <KeyGlyph size="sm" /> : undefined}
            mark={credential && bound ? <KeyGlyph size="xs" /> : undefined}
            decorative
          />
        }
        name={who.name}
        detail={who.detail || undefined}
        // THE READER'S OWN MENU, once anything can be offered (see the file's doc).
        menu={menu}
        current={here}
        // THE TITLE NAMES THE CREDENTIAL the row only says the kind of, so the
        // pointer and a screen reader (which hears it as the description)
        // reach the whole line without opening the menu.
        title={menu ? (via ? `Signed in ${via}. ${MENU_TITLE}` : MENU_TITLE) : undefined}
        linkSuffix=", your menu"
        // FOLDED, THE FACE STANDS ALONE: the places are tiles above it and
        // Preferences is in its menu.
        actions={
          compact ? undefined : (
            <>
              {places.map((place) => (
                <IconButtonLink
                  key={place.key}
                  label={place.label}
                  title={place.title}
                  href={place.href}
                  icon={place.icon}
                  size="sm"
                  variant="ghost"
                  current={place.current}
                  onPointerEnter={place.onPrefetch}
                  onFocus={place.onPrefetch}
                />
              ))}
              {/* START-ALIGNED, OPENING INTO THE ROOM TO ITS RIGHT. The trigger
                sits at the sidebar's inner edge, a couple of hundred pixels
                from the left of the window, so an end-aligned panel would have
                to be slid back across the window edge to fit at all; the test
                beside this block lays the sidebar out at its real
                coordinates. */}
              <Popover
                label="Preferences"
                side="top"
                align="start"
                trigger={(open, toggle) => (
                  <IconButton
                    label="Preferences"
                    icon={<PaletteGlyph size="sm" />}
                    size="sm"
                    variant="ghost"
                    aria-expanded={open}
                    onClick={toggle}
                  />
                )}
              >
                <Preferences />
              </Popover>
            </>
          )
        }
      />
    </>
  );
}

/**
 * What the reader's menu may offer about the session behind this browser:
 * nobody's sign-in, a session under its login, or a browser whose session read
 * FAILED and which may hold one.
 */
export type Account = { kind: "nobody" } | { kind: "session"; login: string } | { kind: "unknown" };

/**
 * What the menu may offer, from the viewer and the session's own answer, or
 * null for nothing at all while the first read is out.
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

/** The reader's own Account, which the menu opens. */
const ACCOUNT = workspaceRow("account")!;

/** What the row's menu holds, as its title. */
const MENU_TITLE = "Your account and signing out";

/**
 * The reader's menu: their access above, then the Account and the session's
 * own gestures, or null while nothing can be offered. Exported for its suite.
 */
export function useAccountMenu(
  account: Account | null,
  /** The grants line, said in the header; empty where nothing settles it. */
  grants: string,
  /** Ask whether to sign out everywhere, outside the menu (`SignOutEverywhereDialog`). */
  onSignOutEverywhere: () => void,
  /**
   * How a credential that is not a person is signed in, whole (`viaLine`):
   * said under the access, since the row says only its kind. Empty for a person.
   */
  via = "",
  /**
   * Open the reader's preferences, where the menu is the way to them: the
   * folded sidebar's, which has no room for their glyph beside the face.
   */
  onPreferences?: () => void,
): AccountRowMenu | undefined {
  const toast = useToast();
  const { socket } = useClient();
  const nav = useNavigator();
  if (account === null) return undefined;
  // PREFERENCES ARE THE BROWSER'S, so they are offered whoever this is.
  const preferences: MenuEntry[] = onPreferences
    ? [
        {
          key: "preferences",
          label: "Preferences",
          description: "Mode, density and theme",
          icon: <PaletteGlyph />,
          onSelect: onPreferences,
        },
      ]
    : [];
  if (account.kind === "nobody") {
    return {
      items: [
        ...preferences,
        { key: "sign-in", label: "Sign in", icon: <LogInGlyph />, onSelect: goSignIn },
      ],
    };
  }
  // A FAILED SIGN-OUT IS SAID, and the page stays: see `signOut` for why a
  // sign-out that nothing answered must not look like one that worked.
  const run = (gesture: () => Promise<void>, what: string) => () => {
    gesture().catch((err: unknown) =>
      toast.failed(`${what} didn’t go through. ${refusalText(err)}`),
    );
  };
  const items: MenuEntry[] = [
    // THE ACCOUNT, from the navigation's own row, so its words and its mark
    // are the palette's.
    ...MENU_ROWS.map((ws): MenuEntry => ({
      key: ws.key,
      label: ws.label,
      description:
        ws.key === ACCOUNT.key ? "Password, two-step verification and sessions" : ws.hint,
      icon: <NamedGlyph name={ws.icon} />,
      onSelect: () => {
        preload(ws.path);
        nav.to(ws.path);
      },
    })),
    ...preferences,
    { kind: "separator", key: "session" },
    {
      key: "sign-out",
      label: "Sign out",
      icon: <LogOutGlyph />,
      onSelect: run(() => signOut(socket), "Signing out"),
    },
  ];
  if (account.kind === "session") {
    items.push({
      key: "sign-out-everywhere",
      label: "Sign out everywhere",
      description: "Every device, and stops your tokens",
      icon: <LogOutGlyph />,
      danger: true,
      onSelect: onSignOutEverywhere,
    });
  }
  return {
    header:
      account.kind === "session" ? (
        grants && via ? (
          // TWO FACTS, ONE A LINE: a pair each, so the credential's whole line
          // stands under the access rather than wrapping beside it.
          <DescriptionList fit dense>
            <DescriptionList.Item term="Access">
              <Tag size="xs" variant="neutral">
                {grants}
              </Tag>
            </DescriptionList.Item>
            <DescriptionList.Item term="Signed in">{via}</DescriptionList.Item>
          </DescriptionList>
        ) : grants ? (
          <>
            <Text variant="label">Access</Text>
            <span>
              <Tag size="xs" variant="neutral">
                {grants}
              </Tag>
            </span>
          </>
        ) : undefined
      ) : (
        <Text variant="caption">We couldn’t check who’s signed in.</Text>
      ),
    items,
  };
}

/**
 * The per-browser preferences. Exported for its suite, which drives each
 * control and reads `lib/prefs.ts` back. `bare` drops the inset the popover's
 * body stands on, for a dialog whose body brings its own.
 */
export function Preferences({ bare = false }: { bare?: boolean }) {
  const prefs = useViewerPrefs();
  const applyTheme = useApplyTheme();
  return (
    <div className={bare ? "preferences preferences--bare" : "preferences"}>
      {/* EVERY CONTROL NAMES ITSELF: each switcher is a radio group carrying
          its own label and the select is named Theme, so a second label round
          one would be read twice. The two switchers FILL one box of one width,
          so a row of glyphs and a row of letters draw the same cells, and the
          theme's field is that width too: three controls, one column. */}
      <div className="preferences-row">
        <Text variant="label" aria-hidden="true">
          Mode
        </Text>
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
        <Text variant="label" aria-hidden="true">
          Density
        </Text>
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
        <Text variant="label" aria-hidden="true">
          Theme
        </Text>
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
