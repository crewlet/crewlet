/**
 * What this browser's session needs from the person, as the transports learn
 * it.
 *
 * # Why this is a module and not a prop
 *
 * The code that DISCOVERS a session problem is the transport: a REST answer
 * of `401`, a socket handshake the refusal probe reads as one, a `403` that
 * says the session may only enrol a second factor. The code that REPAIRS it is
 * the application: a route to the sign-in screen, or to the screen that
 * enrols the factor. Neither may reach into the other — a transport that draws a
 * dialog cannot be tested without a browser, and a screen that inspects every
 * status itself is a screen that forgets one — so the fact travels here,
 * beside neither.
 *
 * # A need is a STATE, not an event
 *
 * The socket is started before React mounts, so its refusal probe can answer
 * before anything has subscribed to hear it. An event fired into nobody is a
 * browser that never learns it is signed out, so what is kept is the latest
 * need, read by whoever subscribes when they subscribe. It is cleared by the
 * one thing that answers it: a sign-in that ends holding a whole session.
 */

/**
 * What the session lacks: a credential the engine accepts at all, or a second
 * factor the deployment requires before this session may do anything else.
 */
export type SessionNeed = "sign_in" | "second_factor";

let need: SessionNeed | null = null;
const listeners = new Set<() => void>();

function announce(): void {
  for (const listener of listeners) listener();
}

/** The session's outstanding need, or null for one that needs nothing. */
export function currentSessionNeed(): SessionNeed | null {
  return need;
}

/**
 * Record what the session lacks. Called by the transports, never by a screen:
 * a screen that wants a person to sign in navigates there itself.
 */
export function needSession(what: SessionNeed): void {
  if (need === what) return;
  need = what;
  announce();
}

/**
 * The browser holds a whole session again — a sign-in, a redemption or an
 * enrolment finished — so whatever a transport recorded before it no longer
 * describes this browser.
 */
export function sessionRestored(): void {
  if (need === null) return;
  need = null;
  announce();
}

/**
 * Subscribe to a change of need. Returns the unsubscribe. The shape
 * `useSyncExternalStore` takes, which is what reads it.
 */
export function onSessionNeed(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}
