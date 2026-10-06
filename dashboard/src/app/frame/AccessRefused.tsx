/**
 * What every screen but the reader's Account says when the engine knows who
 * this browser is and will not serve it the dashboard — a session without
 * `state:read`, or a person whose seat was taken out of the chart.
 *
 * DRAWN BY THE FRAME, IN PLACE OF THE SCREEN, as `GrantRequired` is: the live
 * socket was refused, or never dialled, so nothing a screen asks over it is
 * ever answered. Mounted anyway, Home said "Not connected to the engine. What
 * is shown is the last state it sent" when nothing had ever been sent, and its
 * figures were skeletons that never resolved. So nothing is drawn but what is
 * true: who the engine says you are (`GET /auth/session`, which `/auth/` keeps
 * serving to such a session, read once by the frame), what you hold, why, and
 * the three things that change something from here — the Account page, which
 * reads no socket; asking again once an administrator has acted; and signing
 * out, because the person refused is the person who needs to leave.
 *
 * # Two states, and they read differently
 *
 * NO ACCESS YET is a session holding no `state:read` — a person invited with
 * no grants, or one whose grant was taken away. Nothing is wrong with them,
 * and the frame asks again on its own (`lib/frameSession.ts`), so the panel
 * says what would open the dashboard and that it opens by itself once that is
 * given. REFUSED is the engine's own answer about somebody it does serve the
 * grant to — their seat is gone — in the engine's words. The panel decides
 * from the SESSION, never from the socket's reason: a socket refused for want
 * of `state:read` while the session read before it still held the grant said
 * "you hold state:read" beside "the live socket needs state:read".
 */

import { Button, EmptyState, InlineCode, useToast } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { LIVE_VIEW_GRANT, useFrameSession } from "~/lib/frameSession.ts";
import { refusalText } from "~/lib/refusal.ts";
import { signOut } from "~/lib/session.ts";
import { useClient } from "~/lib/store-hooks.ts";

export function AccessRefused({ reason }: { reason: string | null }) {
  const nav = useNavigator();
  const toast = useToast();
  const { socket } = useClient();
  const { answer, noAccess, reload } = useFrameSession();
  const grants = answer?.grants ?? [];
  const who = answer && (
    <>
      You are signed in as <InlineCode>{answer.login}</InlineCode>, and you hold{" "}
      {grants.length > 0 ? grants.join(", ") : "no grants yet"}.{" "}
    </>
  );
  return (
    <EmptyState
      icon={<KeyGlyph />}
      title={
        noAccess
          ? "You have no access to the company yet"
          : "The engine will not serve this screen to you"
      }
      description={
        noAccess ? (
          <>
            {who}
            Every screen but your Account reads the company&rsquo;s live state, which takes{" "}
            <InlineCode>{LIVE_VIEW_GRANT}</InlineCode>. Somebody holding{" "}
            <InlineCode>people:manage</InlineCode> can give it to you, and this page opens by itself
            once they have. Your Account works meanwhile — your password, second factor, sessions
            and tokens.
          </>
        ) : (
          <>
            {who}
            The engine says: {reason}. Your Account still works — your password, second factor,
            sessions and tokens — and somebody holding <InlineCode>people:manage</InlineCode> can
            give you the access this needs.
          </>
        )
      }
      action={
        <span className="row gap-2">
          <Button variant="primary" onClick={() => nav.to(["account"])}>
            Account
          </Button>
          <Button
            variant="secondary"
            // NO ACCESS asks who this is again, and the frame dials once the
            // answer holds the grant; a REFUSAL dials again, and the
            // handshake answers.
            onClick={() => (noAccess ? void reload() : socket.reconnect())}
          >
            {noAccess ? "Check again" : "Try again"}
          </Button>
          <Button
            variant="ghost"
            onClick={() => {
              signOut(socket).catch((err: unknown) =>
                toast.failed(`Signing out did not go through. ${refusalText(err)}`),
              );
            }}
          >
            Sign out
          </Button>
        </span>
      }
    />
  );
}
