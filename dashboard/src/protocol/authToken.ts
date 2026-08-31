/**
 * The API bearer token the auth-gated screens share.
 *
 * Configuration and Secrets sit behind the auth middleware, and the socket
 * carries the same credential on its handshake and on every query frame. One
 * token between them, so setting it anywhere unlocks everything.
 *
 * Asking for it is the shell's job, over a real dialog. This module only knows
 * how to read and write it: the prompting used to live here as a
 * `window.prompt`, which meant a request for a credential arrived in a
 * chrome-drawn box that could not say who was asking or why.
 */

const TOKEN_KEY = "crewlet_api_token";

/** The stored token, or "". Never throws. */
export function apiToken(): string {
  // Reading storage can THROW, not merely return null: a browser in a privacy
  // mode, a sandboxed iframe, a blocked third-party context. It is read on
  // every fetch, including the degraded-mode snapshot poll — the one read that
  // exists for when everything else is already down — so a throw here would
  // blank the page precisely when the last state it holds is the only state
  // it has.
  try {
    return localStorage.getItem(TOKEN_KEY) ?? "";
  } catch {
    return "";
  }
}

/**
 * Persist a token. Returns false if the browser refused the write.
 *
 * The caller has to know: a silently-unsaved token works until the next reload
 * and is then unauthenticated again, with nothing on screen to explain why.
 */
export function storeToken(token: string): boolean {
  try {
    localStorage.setItem(TOKEN_KEY, String(token ?? "").trim());
    return true;
  } catch {
    return false;
  }
}

/**
 * Asking the shell to raise the token dialog, from anywhere.
 *
 * The dialog belongs to the shell — it is chrome, and only the shell can
 * render over the whole app — but the screens that DISCOVER a missing
 * credential are anywhere. Threading a callback down to every one of them
 * would put a prop about authentication on components that have nothing else
 * to do with it, so the signal travels here instead, beside the token itself.
 *
 * This is the gap it closes: the dialog used to be reachable only from a
 * REFUSAL. With anonymous reads allowed the socket is never refused — it
 * connects, and only the operator-gated answers come back `unauthorized` — so
 * the banner told a reader to set a token and offered nothing that could.
 */
type Listener = () => void;
const listeners = new Set<Listener>();

/** Ask for the token dialog. A no-op when no shell is mounted. */
export function requestToken(): void {
  for (const listener of listeners) listener();
}

/** Subscribe the shell. Returns the unsubscribe. */
export function onTokenRequested(listener: Listener): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** Forget the stored token. Returns false if the browser refused the write. */
export function clearToken(): boolean {
  try {
    localStorage.removeItem(TOKEN_KEY);
    return true;
  } catch {
    return false;
  }
}
