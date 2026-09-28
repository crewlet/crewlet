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
 * The two windows a step-up refusal names, spelled as the `api.auth.session`
 * keys that size them: `step_up` for an ordinary guarded gesture, and
 * `step_up_sensitive` for one that changes how somebody proves who they are
 * or reveals a credential. A value this build does not know is still a
 * window, and is asked for the same way.
 */
export type StepUpWindow = "step_up" | "step_up_sensitive" | (string & {});

/**
 * Asks the person to confirm who they are, resolving true once the engine has
 * accepted the proof and false when they declined or could not.
 */
export type StepUpConfirmer = (window: StepUpWindow) => Promise<boolean>;

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
 *
 * ANY CONFIRMATION COVERS EVERY WINDOW: the password the dialog asks for
 * proves inside both, so a request refused for the sensitive window joins one
 * opened for the ordinary one rather than asking twice.
 */
export function confirmStepUp(window: StepUpWindow): Promise<boolean> {
  if (!confirmer) return Promise.resolve(false);
  if (!confirming) {
    confirming = confirmer(window)
      .catch(() => false)
      .finally(() => {
        confirming = null;
      });
  }
  return confirming;
}
