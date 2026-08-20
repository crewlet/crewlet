// The API bearer token the auth-gated views share.
//
// Configuration and the audit log both read `/config/*`, which sits behind
// `ApiAuthMiddleware`. They keep one token between them so setting it in
// either place unlocks both.

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

/** Prompt for a token; calls `onSet` when one is entered. */
export function promptForToken(onSet) {
  const v = window.prompt("Enter a Crewlet API token (from api.auth.tokens):");
  if (!v) return;
  try {
    localStorage.setItem(TOKEN_KEY, v.trim());
  } catch {
    // Storage refused the write (quota, privacy mode). Say so rather
    // than carrying on as if the token were saved — the next reload
    // would silently be unauthenticated again.
    window.alert("This browser refused to store the token.");
    return;
  }
  if (onSet) onSet();
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

/** Whether an API result is an auth rejection. */
export function isAuthError(result) {
  return !!result && result._error === 401;
}

/**
 * Show a one-time, page-level prompt when the API rejects our token.
 *
 * The whole API is guarded now, so a dashboard opened without a token
 * gets 401s from its very first fetch. Without this it would render an
 * empty shell and look broken rather than unauthenticated.
 */
let promptedOnce = false;
export function ensureTokenForApi(onSet) {
  if (promptedOnce) return;
  promptedOnce = true;
  promptForToken(() => {
    promptedOnce = false;
    if (onSet) onSet();
    else location.reload();
  });
}

/** The "that token was rejected" panel. */
export function tokenRejected() {
  return `
    <div class="banner err">${icon("alert", "sm")}<span>Invalid API token.</span></div>
    <button class="btn" data-action="set-token">${icon("key", "sm")} Re-enter token</button>`;
}
