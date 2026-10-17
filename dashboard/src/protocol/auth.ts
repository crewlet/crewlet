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

import type { EngineHealth } from "../contract/health.ts";
import type { ResetView } from "../contract/identity.ts";
import { rest } from "./rest.ts";
import type {
  AuthConfig,
  InvitationView,
  PasswordChanged,
  PasswordSet,
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

/** The header a password reset link's secret travels in to its view, for
 *  [INVITE_SECRET_HEADER]'s reason. */
const RESET_SECRET_HEADER = "X-Crewlet-Reset-Secret";

export const auth = {
  /**
   * Whether the company has its first person — `/health`'s `identity`:
   * `ready`, `unclaimed`, `unknown`, or absent. `/health` is unguarded and is
   * the one answer an install with nobody in it can reach, which is why the
   * sign-in page asks it rather than `/iam`.
   */
  firstPerson: async (): Promise<EngineHealth["identity"]> =>
    ((await rest.get("/health")) as EngineHealth | null)?.identity,

  /** The password floor and the second-factor posture — unguarded, like `/health`. */
  config: async (signal?: AbortSignal): Promise<AuthConfig> =>
    (await rest.get("/auth/config", signal)) as AuthConfig,

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
   *
   * MADE FOR NOBODY ([RequestOptions.forReader]): it signs somebody in, who
   * may be anybody but the person this tab was read by, and the screen that
   * made it hands the tab to them on the answer (`lib/session.ts`).
   */
  exchangeToken: async (token: string): Promise<SignedIn> =>
    (
      await rest.request("POST", "/auth/token", {
        body: {},
        headers: { Authorization: `Bearer ${token}` },
        forReader: false,
      })
    ).body as SignedIn,

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

  /** Say whose password a reset link sets, without spending it. */
  viewReset: async (id: string, secret: string): Promise<ResetView> =>
    (
      await rest.request("GET", `/auth/reset/${encodeURIComponent(id)}`, {
        headers: { [RESET_SECRET_HEADER]: secret },
      })
    ).body as ResetView,

  /**
   * Set a new password from a reset link, once. It ends every session the
   * person held and signs nobody in: the person signs in next.
   */
  spendReset: async (
    id: string,
    body: { secret: string; password: string },
  ): Promise<PasswordSet> =>
    (await rest.post(`/auth/reset/${encodeURIComponent(id)}`, body)) as PasswordSet,

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
   * Change your own password. The current one — and, from somebody who holds
   * a second factor, a code, exactly as a step-up asks it — is the proof, so
   * no step-up is asked first; the change ends every other session and
   * personal token you hold, and answers a fresh session for this browser.
   */
  changePassword: async (body: {
    current_password: string;
    new_password: string;
    code?: string;
  }): Promise<PasswordChanged> => (await rest.post("/auth/password", body)) as PasswordChanged,

  /**
   * Who this browser is signed in as — ended by `signal` where the caller
   * passes one, for a read a newer one has superseded.
   *
   * MADE FOR NOBODY ([RequestOptions.forReader]), because it is how a tab
   * LEARNS who the session is: the frame's read and the hand-over a
   * `reader_changed` asks for both take its answer (`lib/session.ts`), and a
   * read refused for naming the last reader would leave the tab nothing to be
   * handed over with.
   */
  session: async (signal?: AbortSignal): Promise<SessionAnswer> =>
    (await rest.request("GET", "/auth/session", { signal, forReader: false }))
      .body as SessionAnswer,

  /**
   * The same read, made FOR THE TAB'S READER — what the keepalive asks
   * (`keepalive.ts`). It re-issues the cookie exactly as [session] does, and
   * it is how a tab nobody is using learns that the browser changed hands
   * under it: refused `reader_changed`, it is handed to whoever the session
   * is now, within two keepalive intervals of the sign-in that moved it.
   */
  keepAlive: async (): Promise<SessionAnswer> => (await rest.get("/auth/session")) as SessionAnswer,

  /**
   * End this browser's session. The engine clears the cookie whatever its
   * own write did, so an answer at all means this browser holds no session.
   */
  logout: async (): Promise<void> => {
    await rest.post("/auth/logout", {});
  },

  /**
   * End ONE named session of yours — a laptop left signed in somewhere —
   * without ending this browser's.
   */
  logoutOne: async (lineage: string): Promise<void> => {
    await rest.post(`/auth/logout/${encodeURIComponent(lineage)}`, {});
  },

  /**
   * End every session the caller holds, on every device, by moving their
   * revocation epoch — this browser's included.
   */
  logoutEverywhere: async (): Promise<void> => {
    await rest.post("/auth/logout/all", {});
  },
};
