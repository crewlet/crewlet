/**
 * What every screen but the reader's Account says when the engine knows who
 * this browser is and will not serve it the dashboard — a session without
 * `state:read`, or a person whose seat was taken out of the chart.
 *
 * DRAWN BY THE FRAME, IN PLACE OF THE SCREEN, as `GrantRequired` is: the live
 * socket was refused and stops dialling, so nothing a screen asks over it is
 * ever answered. Mounted anyway, Home said "Not connected to the engine. What
 * is shown is the last state it sent" when nothing had ever been sent, and its
 * figures were skeletons that never resolved. So nothing is drawn but what is
 * true: who the engine says you are (`GET /auth/session`, which `/auth/` keeps
 * serving to such a session), what you hold, the engine's own reason, and the
 * three things that change something from here — the Account page, which reads
 * no socket; trying again once an administrator has acted; and signing out,
 * because the person refused is the person who needs to leave.
 */

import { Button, EmptyState, InlineCode, useToast } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { refusalText } from "~/lib/refusal.ts";
import { signOut } from "~/lib/session.ts";
import { useClient } from "~/lib/store-hooks.ts";
import { useRest } from "~/lib/useRest.ts";
import { auth } from "~/protocol/index.ts";

export function AccessRefused({ reason }: { reason: string }) {
  const nav = useNavigator();
  const toast = useToast();
  const { socket } = useClient();
  const session = useRest("/auth/session", (signal) => auth.session(signal)).data;
  const grants = session?.grants ?? [];
  return (
    <EmptyState
      icon={<KeyGlyph />}
      title="The engine will not serve this screen to you"
      description={
        <>
          {session && (
            <>
              You are signed in as <InlineCode>{session.login}</InlineCode>, and you hold{" "}
              {grants.length > 0 ? grants.join(", ") : "no grants yet"}.{" "}
            </>
          )}
          The engine says: {reason}. Your Account still works — your password, second factor,
          sessions and tokens — and somebody holding <InlineCode>people:manage</InlineCode> can give
          you the access this needs.
        </>
      }
      action={
        <span className="row gap-2">
          <Button variant="primary" onClick={() => nav.to(["account"])}>
            Account
          </Button>
          <Button variant="secondary" onClick={() => socket.reconnect()}>
            Try again
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
