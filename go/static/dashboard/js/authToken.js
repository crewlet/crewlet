// The API bearer token the auth-gated views share.
//
// Configuration reads `/config/*`, which sits behind
// `ApiAuthMiddleware`, and the socket carries the same credential on its
// handshake and on every query frame. One token between them, so setting
// it anywhere unlocks everything.
//
// Asking for it is the shell's job (`askForToken` in app.js, over a real
// dialog). This module only knows how to read and write it, and what to
// render in place of a view that cannot be shown without one — the
// prompting used to live here as a `window.prompt`, which meant a
// request for a credential arrived in a chrome-drawn box that could not
// say who was asking or why.

import { icon } from "./icons.js";

const TOKEN_KEY = "crewlet_api_token";

export function apiToken() {
  // Reading storage can THROW, not merely return null: a browser in a
  // privacy mode, a sandboxed iframe, a blocked third-party context. It
  // is read on every fetch, including the degraded-mode snapshot poll —
  // the one read that exists for when everything else is already down —
  // so a throw here would blank the page precisely when the last state
  // it holds is the only state it has.
  try {
    return localStorage.getItem(TOKEN_KEY) || "";
  } catch {
    return "";
  }
}

/**
 * Persist a token. Returns false if the browser refused the write.
 *
 * The caller has to know: a silently-unsaved token works until the next
 * reload and is then unauthenticated again, with nothing on screen to
 * explain why.
 */
export function storeToken(token) {
  try {
    localStorage.setItem(TOKEN_KEY, String(token || "").trim());
    return true;
  } catch {
    return false;
  }
}

/** The "you need a token" panel, shown in place of an auth-gated view. */
export function tokenGate(what) {
  return `
    <div class="banner warn">${icon("key", "sm")}
      <span>An API token is required to view ${what}.</span>
    </div>
    <div class="empty">
      ${icon("database", "lg")}
      <div>This view is auth-gated</div>
      <div class="empty-sub">Set a bearer token matching one of your <code>api.auth.tokens</code> entries.</div>
      <button class="btn primary" data-action="set-token" style="margin-top:8px">${icon("key", "sm")} Set API token</button>
    </div>`;
}

/** The "that token was rejected" panel. */
export function tokenRejected() {
  return `
    <div class="banner err">${icon("alert", "sm")}<span>Invalid API token.</span></div>
    <button class="btn" data-action="set-token">${icon("key", "sm")} Re-enter token</button>`;
}
