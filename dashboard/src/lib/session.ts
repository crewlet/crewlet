/**
 * Where a browser goes on its way into a session, and what it does once it
 * has one.
 *
 * # `next` is an address on THIS page, or it is nothing
 *
 * Every sign-in screen takes `?next=`, the route the reader was on when the
 * engine said they were not signed in, and returns them there. That makes it
 * the one parameter an outsider can choose for somebody else — a link to
 * `#/login?next=…` sent in a message — and a sign-in that forwards wherever
 * `next` says is the ordinary way a sign-in page becomes somebody else's
 * redirect. So `next` is honoured only as a HASH ROUTE of this dashboard,
 * rebuilt through the router's own parser so what is followed is what the
 * router would have produced, and anything else falls back to the landing
 * screen: an absolute URL, a scheme, a protocol-relative `//host`, a
 * backslash a browser reads as a slash, a control character a browser strips
 * before it parses, and a route back into a sign-in screen, which would loop.
 *
 * # A sign-in ends in one of two places
 *
 * A session that may do everything goes to `next`, REPLACING the sign-in
 * screen's history entry — Back from the page the reader asked for must not
 * land them on a form for a session they already hold — and the socket
 * re-dials, because the one it has was opened as nobody. A session that may
 * only enrol a second factor goes to the enrolment screen instead, carrying
 * `next` along, and the socket waits: it would only be refused again.
 *
 * # And a sign-out leaves nothing behind
 *
 * Signing out, here or everywhere, ends in a RELOAD at the sign-in — see
 * [page] — because the tab is about to be somebody else's.
 */

import { useCallback } from "react";
import { buildHash, parseHash, useNavigator } from "~/app/router.tsx";
import { framelessOf } from "~/app/nav.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { auth, sessionRestored, type SessionStatus } from "~/protocol/index.ts";

/** Where a sign-in lands when `next` names nowhere it may go. */
export const LANDING = "#/";

/**
 * The longest `next` honoured. An address in this product is a path of a few
 * segments and a handful of filters; four kilobytes is far past any of them
 * and short of anything a browser would truncate.
 */
const MAX_NEXT = 4096;

/**
 * The same-origin hash route `raw` names, rebuilt canonically, or
 * [LANDING] for anything else. See the note above for what is refused.
 */
export function safeNext(raw: string | null | undefined): string {
  if (!raw || raw.length > MAX_NEXT) return LANDING;
  // A HASH ROUTE AND NOTHING ELSE: `#/` then a path. `#//host` is refused as
  // well, not because a hash can leave the page but because it is the shape
  // of the protocol-relative address it most resembles, and nothing in this
  // product writes one.
  if (!raw.startsWith("#/") || raw.startsWith("#//")) return LANDING;
  // A BACKSLASH OR A CONTROL CHARACTER, tab and newline included: the two
  // things a browser rewrites before it parses an address, which is how a
  // value that reads as a path to one parser becomes a host to another.
  if (/[\\\u0000-\u001f\u007f]/.test(raw)) return LANDING;
  const route = parseHash(raw);
  // NEVER BACK INTO A SIGN-IN SCREEN, which would answer a completed sign-in
  // with another one.
  if (framelessOf(route.path) !== null) return LANDING;
  return buildHash(route.path, route.query);
}

/**
 * The address of the sign-in screen for a reader who was at `from`.
 *
 * A reader already on a sign-in screen carries THAT screen's own `next`
 * rather than the screen itself, so an enrolment interrupted by a lost session
 * still ends where it was going.
 */
export function signInHash(from: string): string {
  const route = parseHash(from);
  const next = framelessOf(route.path) !== null ? route.query.get("next") : route.hash;
  return buildHash(["login"], { next: safeNext(next) });
}

/**
 * Send the reader to sign in, from wherever they are — a control that offers
 * "sign in as somebody else" on a refusal, drawn by components that do not
 * know which screen holds them.
 *
 * A MOVE LIKE A LINK: it goes through the browser's own hash change, so a
 * surface holding unsaved work is asked first, exactly as a link would ask
 * it.
 */
export function goSignIn(): void {
  location.hash = signInHash(location.hash);
}

/**
 * The one move that leaves NOTHING of a session in the tab.
 *
 * A RELOAD, not a navigation. A sign-out on a shared machine is a person
 * handing the tab to somebody else, and a route change would leave the whole
 * company as the last person saw it in memory — the store, every answer a
 * screen cached, the socket's last snapshot — for whoever sits down next. A
 * reload is the only thing that drops all of it at once and cannot miss a
 * cache added later. An object rather than a bare function, so a suite can
 * stand in for a reload jsdom cannot perform.
 */
export const page = {
  reloadInto(hash: string): void {
    history.replaceState(null, "", hash);
    location.reload();
  },
};

/**
 * Sign this browser out, and start again at the sign-in with nothing held.
 *
 * ONLY ON AN ANSWER. The engine clears the cookie whatever its own write did,
 * so any answer means this browser holds no session; a request it never
 * answered — or refused before it ran, as a cross-site post — cleared
 * nothing, and reloading would put the person straight back where they were
 * while telling them they had left. That throws, for the caller to say so.
 */
export async function signOut(): Promise<void> {
  await auth.logout();
  page.reloadInto("#/login");
}

/**
 * End every session this person holds, on every device, and this one with
 * them.
 *
 * A REVOCATION NOBODY CAN CONFIRM is a `503` carrying its operation id, and
 * it throws rather than reloading: saying "signed out everywhere" about a
 * laptop left open somewhere, when nothing can say it was, is the one claim
 * this gesture exists to make true.
 */
export async function signOutEverywhere(): Promise<void> {
  await auth.logoutEverywhere();
  page.reloadInto("#/login");
}

/**
 * What a screen calls once the browser holds a session — with the status the
 * engine answered for it, and the `next` the screen was given.
 */
export function useSignedIn(): (status: SessionStatus, next: string | null) => void {
  const nav = useNavigator();
  const { socket } = useClient();
  return useCallback(
    (status, next) => {
      const target = safeNext(next);
      if (status === "second_factor_enrolment_required") {
        nav.replace(["enrol"], { next: target });
        return;
      }
      // IN THIS ORDER. The need clears first, so the screen `next` names
      // does not mount under a need it would be routed away for; the socket
      // re-dials second, before the move, so that screen's first questions
      // wait for the new socket rather than the one opened as nobody.
      sessionRestored();
      socket.reconnect();
      const route = parseHash(target);
      nav.replace(route.path, route.query);
    },
    [nav, socket],
  );
}
