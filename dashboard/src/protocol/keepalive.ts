/**
 * Keeping a tab's session alive while it only listens.
 *
 * A session ends after twelve hours with nothing happening on it
 * (`session.Idle`), and what counts as something happening is a REQUEST: the
 * engine re-issues the cookie with a fresh idle deadline on any answer to a
 * request carrying one that is five minutes old (`session.ReissueAfter`). The
 * live socket neither enforces nor extends that deadline — it is authenticated
 * once, at its handshake — so a tab somebody leaves open on a dashboard that
 * only reads the socket would idle out with the person looking at it, and the
 * next reconnect would land them on the sign-in form.
 *
 * So while the socket is open, a REST read is made at least once per
 * re-issue interval — `GET /auth/session`, the cheapest guarded read there is —
 * unless some other request already heard from the engine since the last
 * tick, which re-issued the cookie just as well. Hidden or not: an open live
 * view is somebody using the dashboard, and the session's absolute deadline
 * still bounds how long that can last. A 401 is read where every other is
 * (`rest.ts`): a session that ended needs a sign-in.
 */

import { auth } from "./auth.ts";
import { lastAnsweredAt } from "./rest.ts";

/**
 * How often an open tab makes sure the engine has heard from it, in ms:
 * the engine's own re-issue interval (`session.ReissueAfter`, five minutes).
 * Any sooner re-issues nothing — a cookie younger than that is not
 * re-issued — and any later lets a whole interval go unrenewed.
 */
export const SESSION_KEEPALIVE_MS = 300_000;

/** What the keepalive needs of the socket: whether it is open now. */
export interface Connected {
  readonly connected: boolean;
}

export class SessionKeepAlive {
  private timer: ReturnType<typeof setInterval> | 0 = 0;

  constructor(private readonly socket: Connected) {}

  start(): void {
    if (this.timer) return;
    this.timer = setInterval(() => this.tick(), SESSION_KEEPALIVE_MS);
  }

  stop(): void {
    clearInterval(this.timer);
    this.timer = 0;
  }

  /** One interval: a read, unless the socket is down or a request already answered. */
  tick(now: number = Date.now()): void {
    if (!this.socket.connected) return;
    if (now - lastAnsweredAt() < SESSION_KEEPALIVE_MS) return;
    // A refusal is noted by `rest.ts` (a 401 needs a sign-in); a failure that
    // never reached the engine changes nothing, and the next tick asks again.
    auth.session().catch(() => undefined);
  }
}
