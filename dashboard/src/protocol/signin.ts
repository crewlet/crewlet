/**
 * What this browser's sign-in needs from the person, as the transports learn
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
 * Not `session.ts`, which is the READ FLOORS a tab's own writes raise: a
 * sign-in session and a read session are two different things, and one module
 * name for both was a reader opening the wrong file.
 *
 * # A need is a STATE, not an event
 *
 * The socket is started before React mounts, so its refusal probe can answer
 * before anything has subscribed to hear it. An event fired into nobody is a
 * browser that never learns it is signed out, so what is kept is the latest
 * need, read by whoever subscribes when they subscribe. It is cleared by the
 * one thing that answers it: a sign-in that ends holding a whole session.
 *
 * # And a step-up is a QUESTION, asked once however many requests need it
 *
 * A guarded write refused `403 step_up_required` is not a mistake the person
 * made: they are signed in, and the engine wants a fresher proof of who they
 * are before this particular gesture. The repair is to ask for the password
 * again and send the SAME request — so nothing typed into a form is lost —
 * and it is asked once however many requests were refused at the same
 * moment: a screen that saves three things together must not stack three
 * password dialogs. The transport asks here; the application answers.
 *
 * # And every request says who the tab is read by
 *
 * The tabs of one browser share ONE session cookie and each draws one person's
 * company, so a sign-in as somebody else in another tab moves the cookie under
 * every other tab. What hands those tabs over — the engine closing the
 * sockets of the session it ended, `4401` — rides a close the sign-in makes
 * best effort, and a window left open beside it may hear nothing: it would go
 * on drawing the last person's screen while every request it sent went out as
 * the new one, a retried change of theirs included. So every request a tab
 * makes FOR ITS READER names them ([readerHeaders], `READER_HEADER`), and the
 * engine refuses one whose credential resolves to anybody else `409
 * reader_changed` before it runs. The transport reads the reader here and
 * reports that refusal here ([readerMoved]); the application, which records
 * who the tab is read by (`lib/reader.ts`) and knows how a tab is handed over
 * (`lib/session.ts`), answers both. With nothing installed — the e2e replay,
 * a suite that installs none — a request names nobody and is served as the
 * cookie is.
 */

// RELATIVE, like every contract import in this directory: it is also built
// alone as `protocol.js`, where the `~` alias does not exist.
import { READER_HEADER, READER_PARAM } from "../contract/identity.ts";
import type { ACT_ERRORS } from "../contract/errors.ts";

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
 * The browser holds a session that may only enrol a second factor — a
 * sign-in or a redemption answered `second_factor_enrolment_required`.
 *
 * THE SIGN-IN'S OWN ANSWER REPLACES WHATEVER A TRANSPORT RECORDED BEFORE IT,
 * because it is the newest fact about this browser's session. The sign-in
 * screen's own `GET /auth/session` and the socket's refusal probe both record
 * `sign_in` for a browser holding nothing, which is the state every sign-in
 * starts from; left in place, it routed the enrolment straight back to the
 * sign-in form the moment the enrolment screen mounted.
 */
export function sessionNeedsEnrolment(): void {
  needSession("second_factor");
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

/**
 * Asks the person to confirm who they are, resolving true once the engine has
 * accepted the proof and false when they declined or could not.
 *
 * ONE WINDOW: the engine's step-up refusal names `step_up`, the one
 * `api.auth.session.step_up` window every guarded gesture is held to, so there
 * is nothing for the confirmer to choose between.
 */
export type StepUpConfirmer = () => Promise<boolean>;

let confirmer: StepUpConfirmer | null = null;
let confirming: Promise<boolean> | null = null;
const asking = new Set<() => void>();

/**
 * Whether a step-up is being asked of the person right now — what a dialog
 * whose write is held behind it says it is waiting for: the person, not the
 * engine.
 */
export function stepUpAsked(): boolean {
  return confirming !== null;
}

/** Subscribe to a step-up starting or ending. Returns the unsubscribe. */
export function onStepUpAsked(listener: () => void): () => void {
  asking.add(listener);
  return () => {
    asking.delete(listener);
  };
}

function toldAsking(): void {
  for (const listener of asking) listener();
}

/**
 * Install what confirms a step-up. Returns the uninstall, which leaves a
 * later installation in place.
 *
 * ONE AT A TIME, because there is one person at the keyboard: a second
 * installation replaces the first rather than queueing behind it.
 */
export function setStepUpConfirmer(fn: StepUpConfirmer): () => void {
  confirmer = fn;
  return () => {
    if (confirmer === fn) confirmer = null;
  };
}

/**
 * Ask for a step-up, or join the one already being asked.
 *
 * FALSE WITH NOBODY TO ASK, so a refused request is reported as the refusal it
 * was rather than waiting for a dialog that will never open; and false for a
 * confirmer that failed, which is not a proof.
 */
export function confirmStepUp(): Promise<boolean> {
  if (!confirmer) return Promise.resolve(false);
  if (!confirming) {
    confirming = confirmer()
      .catch(() => false)
      .finally(() => {
        confirming = null;
        toldAsking();
      });
    toldAsking();
  }
  return confirming;
}

/** Who the tab is read by, as the application knows it — see the module doc. */
export interface TabReader {
  /** The principal this tab is read by, or null where it has learned none. */
  current(): string | null;
  /**
   * The engine refused a request this tab made for `current()`: the browser's
   * session is somebody else's. Hand the tab to whoever it is now.
   */
  moved(): void;
}

let tabReader: TabReader | null = null;

/**
 * Install who the tab is read by. Returns the uninstall, which leaves a later
 * installation in place — the step-up confirmer's rule, for its reason: one
 * tab, one reader.
 */
export function setTabReader(reader: TabReader): () => void {
  tabReader = reader;
  return () => {
    if (tabReader === reader) tabReader = null;
  };
}

/**
 * The header naming this tab's reader, for a request made on their behalf —
 * or none, where nothing is installed or the tab has learned nobody: a
 * request that names nobody is served as the cookie is, which is all a tab
 * that does not know its reader can ask for.
 */
export function readerHeaders(): Record<string, string> {
  const reader = tabReader?.current() ?? null;
  return reader === null ? {} : { [READER_HEADER]: reader };
}

/**
 * The socket's handshake URL query naming this tab's reader (`READER_PARAM`),
 * or "" where nothing is installed or the tab has learned nobody.
 *
 * A browser sets no header on a WebSocket, so the handshake is the one request
 * [readerHeaders] cannot reach; the engine reads this parameter on an upgrade
 * instead, and refuses a dial made for anybody but the cookie's principal.
 */
export function readerQuery(): string {
  const reader = tabReader?.current() ?? null;
  return reader === null ? "" : `?${READER_PARAM}=${encodeURIComponent(reader)}`;
}

/**
 * The `error` the engine refuses a request made for somebody else with.
 *
 * ONE OF `ACT_ERRORS`' KEYS, checked by the compiler: the engine gate holds
 * that table to the act transport's codes, so a renamed code there is a red
 * build here rather than a refusal no tab is ever handed over on.
 */
export const READER_CHANGED = "reader_changed" satisfies keyof typeof ACT_ERRORS;

/** A request named this tab's reader and the engine said the session is somebody else's. */
export function readerMoved(): void {
  tabReader?.moved();
}
