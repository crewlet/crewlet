/**
 * The two application close codes the engine ends a socket with, which the
 * dashboard's socket branches on and no frame carries.
 *
 * COPIES of `stream.CloseUnauthenticated` and `stream.CloseUnauthorized`, held
 * to them in both directions by `internal/api/stream`'s
 * `TestTheDashboardClosesOnTheEnginesCloseCodes`: each constant here equals
 * the engine's, and every 4000-range code the engine closes with has one here.
 * An engine that renumbered one would otherwise have a dashboard reconnecting
 * for ever against a withdrawn grant, or stopping dead over a cookie that
 * merely expired. What a close MEANS to a tab is `protocol/socket.ts`'s.
 */

/**
 * The credential the socket was opened with names nobody any more — the
 * session ended, expired or was revoked. The engine re-checks an open socket
 * and closes it with this. The ordinary reconnect is the repair, because the
 * browser may hold a newer cookie than the one the socket was opened with.
 */
export const CLOSE_UNAUTHENTICATED = 4401;

/**
 * The credential still names somebody who may not have this surface: their
 * seat is gone from the chart, or the grant the socket needs was withdrawn.
 * Reconnecting reaches the same person with the same access, so the socket
 * stops and the page says why.
 */
export const CLOSE_FORBIDDEN = 4403;
