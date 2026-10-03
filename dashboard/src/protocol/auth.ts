/**
 * The sign-in surface, `/auth`, as the functions the screens call.
 *
 * ONE MODULE over `rest.ts`, for the reason `rest.ts` gives about itself: a
 * screen that composed its own path, header and body for each of these would
 * be a second place for the invitation's secret to end up in a URL, and that
 * is the one mistake this surface exists to make impossible.
 *
 * WHAT NONE OF THESE DOES is keep a credential. Every answer that signs a
 * person in sets a cookie the browser holds and no script can read, and the
 * body carries no bearer at all; the one credential a function here is
 * handed — a Tier A token being exchanged — is sent once, in the header, and
 * dropped. There is nothing for this page to store, so it stores nothing.
 */

import { rest } from "./rest.ts";
import type {
  AuthConfig,
  InvitationView,
  RecoveryCodes,
  SecondFactorEnrolled,
  SecondFactorSeed,
  SessionAnswer,
  SignedIn,
} from "./types.ts";

/**
 * The header an invitation's secret travels in to the view.
 *
 * A HEADER, because the view is a GET and a GET has no body — and never the
 * query string, which every access log between the browser and the engine
 * records. The link carries the secret in its FRAGMENT precisely so that no
 * server sees it on the way to this page.
 */
const INVITE_SECRET_HEADER = "X-Crewlet-Invite-Secret";

export const auth = {
  /** What a sign-in page may know before anybody has signed in. */
  config: async (): Promise<AuthConfig> => (await rest.get("/auth/config")) as AuthConfig,

  /**
   * Sign in with a login or an address and a password — and, once the engine
   * has answered `second_factor_required`, the code.
   */
  login: async (body: { login: string; password: string; code?: string }): Promise<SignedIn> =>
    (await rest.post("/auth/login", body)) as SignedIn,

  /**
   * Exchange a Tier A token for a one-hour session.
   *
   * THE TOKEN IS THE BEARER OF THIS ONE REQUEST and nothing else: it goes in
   * the `Authorization` header, which is how the engine takes it, and the
   * session that comes back is a cookie. Nothing here holds it afterwards.
   */
  exchangeToken: async (token: string): Promise<SignedIn> =>
    (await rest.post("/auth/token", {}, { Authorization: `Bearer ${token}` })) as SignedIn,

  /** Render an invitation without spending it. */
  viewInvite: async (id: string, secret: string): Promise<InvitationView> =>
    (
      await rest.request("GET", `/auth/invite/${encodeURIComponent(id)}`, {
        headers: { [INVITE_SECRET_HEADER]: secret },
      })
    ).body as InvitationView,

  /** Redeem an invitation, which creates the person and signs them in. */
  redeemInvite: async (
    id: string,
    body: { secret: string; login: string; name: string; password: string },
  ): Promise<SignedIn> =>
    (await rest.post(`/auth/invite/${encodeURIComponent(id)}`, body)) as SignedIn,

  /**
   * Enrolment's first leg: a seed, and nothing stored. A person who never
   * completes the second leg has enrolled nothing.
   */
  secondFactorSeed: async (): Promise<SecondFactorSeed> =>
    (await rest.post("/auth/totp", {})) as SecondFactorSeed,

  /**
   * Enrolment's second leg: the seed back, with a code derived from it — the
   * only evidence the authenticator on the other side works.
   */
  enrolSecondFactor: async (secret: string, code: string): Promise<SecondFactorEnrolled> =>
    (await rest.post("/auth/totp", { secret, code })) as SecondFactorEnrolled,

  /** Ten fresh single-use codes, retiring the old set, shown this once. */
  recoveryCodes: async (): Promise<RecoveryCodes> =>
    (await rest.post("/auth/totp/recovery", {})) as RecoveryCodes,

  /**
   * Confirm who you are on a session that is already valid: the password,
   * and the code where a second factor is held. The engine answers a fresh
   * session cookie and ends the one it replaces.
   */
  stepUp: async (body: { password: string; code?: string }): Promise<SignedIn> =>
    (await rest.post("/auth/step-up", body)) as SignedIn,

  /**
   * Who this browser is signed in as — ended by `signal` where the caller
   * passes one, for a read a newer one has superseded.
   */
  session: async (signal?: AbortSignal): Promise<SessionAnswer> =>
    (await rest.get("/auth/session", signal)) as SessionAnswer,

  /**
   * End this browser's session. The engine clears the cookie whatever its
   * own write did, so an answer at all means this browser holds no session.
   */
  logout: async (): Promise<void> => {
    await rest.post("/auth/logout", {});
  },

  /**
   * End every session the caller holds, on every device, by moving their
   * revocation epoch — this browser's included.
   */
  logoutEverywhere: async (): Promise<void> => {
    await rest.post("/auth/logout/all", {});
  },
};
