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

// RELATIVE, not through `~`: protocol/ is also built alone as protocol.js,
// where the alias does not exist. `lib/storage.ts` imports nothing.
import { STORAGE_KEYS } from "../lib/storage.ts";

const TOKEN_KEY = STORAGE_KEYS.apiToken;

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
  } catch {
    return false;
  }
  announce();
  return true;
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

/**
 * The symmetric signal: the token CHANGED — in this tab or in any other.
 *
 * Every REST-backed surface used to learn nothing at all: it had fetched once
 * on mount, so setting a token left `/setup` and `/secrets` still showing the
 * refusal that prompted the reader to set one. The screen said "needs an
 * operator token", the reader supplied it, and nothing moved. The socket
 * learns here too (the shell's subscription), so the socket, every REST
 * request and every piece of state a screen holds for a person change person
 * together.
 *
 * Fired from `storeToken` and `clearToken` themselves rather than from the
 * dialog, so a future writer cannot forget to announce it — AND FROM ANOTHER
 * TAB'S WRITE. The token lives in `localStorage`, which every tab of the
 * origin shares, and a request reads it when it is sent (`rest.ts`), so a
 * token saved in one window is the credential every other window's next write
 * goes out under. Announced only for this tab's own writes, a window still
 * drawing the last person's chat went on flushing their reading — as the next
 * person, whose shared rooms it then marked read for good — while its socket
 * stayed the last person's. `storage` fires in every OTHER document of the
 * origin, so it is followed while anybody is listening, as `lib/recents.ts`
 * follows its own key; `e.key === null` is a `localStorage.clear()`, which
 * names no key and takes the token with everything else.
 */
const changed = new Set<Listener>();

/** Subscribe to token changes. Returns the unsubscribe. */
export function onTokenChanged(listener: Listener): () => void {
  if (changed.size === 0) followOtherTabs(true);
  changed.add(listener);
  return () => {
    changed.delete(listener);
    if (changed.size === 0) followOtherTabs(false);
  };
}

function announce(): void {
  for (const listener of [...changed]) listener();
}

/**
 * Hear another tab's write, while anybody is listening. NOT IN A DOCUMENT —
 * `protocol.js` is also loaded by plain `node` to replay a capture, where there
 * is no window and no other tab to hear — there is nothing to follow.
 */
function followOtherTabs(on: boolean): void {
  if (typeof window === "undefined") return;
  if (on) window.addEventListener("storage", fromAnotherTab);
  else window.removeEventListener("storage", fromAnotherTab);
}

function fromAnotherTab(e: StorageEvent): void {
  if (e.key !== null && e.key !== TOKEN_KEY) return;
  announce();
}

/** Forget the stored token. Returns false if the browser refused the write. */
export function clearToken(): boolean {
  try {
    localStorage.removeItem(TOKEN_KEY);
  } catch {
    return false;
  }
  announce();
  return true;
}
