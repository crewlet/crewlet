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
      });
  }
  return confirming;
}
