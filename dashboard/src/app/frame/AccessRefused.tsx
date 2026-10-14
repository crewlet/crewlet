/**
 * What every screen but the reader's own says when the engine knows who this
 * browser is and will not serve it the dashboard — a person whose seat was
 * taken out of the chart, or one whose stage may not act.
 *
 * DRAWN BY THE FRAME, IN PLACE OF THE SCREEN, as `GrantRequired` is: the live
 * socket was refused, so nothing a screen asks over it is ever answered.
 * Mounted anyway, Home said "Not connected to the engine. What is shown is the
 * last state it sent" when nothing had ever been sent, and its figures were
 * skeletons that never resolved. So nothing is drawn but what is true: who the
 * engine says you are (`GET /auth/session`, which `/auth/` keeps serving to
 * such a session, read once by the frame), the engine's own reason, and the
 * three things that change something from here — the Account page, which
 * reads no socket (nor does Settings › Appearance, the other screen that is
 * the reader's own and drawn in place of this, `nav.ts`'s `personalPath`);
 * dialling again once an admin has acted; and signing out, because the person
 * refused is the person who needs to leave.
 *
 * NEVER FOR WANT OF ACCESS. The socket admits every principal the engine
 * resolves whose stage may act, whatever they hold — a person with no app
 * reads the overview — so a reader missing an app or Admin is told so by the
 * one screen that needs it (`GrantRequired`), and the rest of the dashboard
 * stays theirs.
 */

import { Button, EmptyState, InlineCode, useToast } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { accessLine } from "~/lib/access.ts";
import { useFrameSession } from "~/lib/frameSession.ts";
import { refusalText } from "~/lib/refusal.ts";
import { signOut } from "~/lib/session.ts";
import { useClient } from "~/lib/store-hooks.ts";

export function AccessRefused({ reason }: { reason: string }) {
  const nav = useNavigator();
  const toast = useToast();
  const { socket } = useClient();
  const { answer } = useFrameSession();
  return (
    <EmptyState
      icon={<KeyGlyph />}
      title="The engine will not serve this screen to you"
      description={
        <>
          {answer && (
            <>
              You are signed in as <InlineCode>{answer.login}</InlineCode> (your access:{" "}
              {accessLine(answer.grants ?? [])}).{" "}
            </>
          )}
          The engine says: {reason}. Your Account still works — your password, two-step
          verification, sessions and tokens — and an admin can put this right.
        </>
      }
      action={
        <span className="row gap-2">
          <Button variant="primary" onClick={() => nav.to(["account"])}>
            Account
          </Button>
          <Button
            variant="secondary"
            // A REFUSAL DIALS AGAIN, and the handshake answers.
            onClick={() => socket.reconnect()}
          >
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
