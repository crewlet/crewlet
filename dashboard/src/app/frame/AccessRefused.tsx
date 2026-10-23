/**
 * What every screen but the reader's own says when the engine knows who this
 * browser is and will not serve it the dashboard — a person whose seat was
 * taken out of the chart, or one whose stage may not act.
 *
 * DRAWN BY THE FRAME, IN PLACE OF THE SCREEN, as `GrantRequired` is: the live
 * socket was refused, so nothing a screen asks over it is ever answered.
 * Mounted anyway, Home said "Not connected to the engine. What is shown is the
 * last state it sent" when nothing had ever been sent, and its figures were
 * skeletons that never resolved. So nothing is drawn but what is true: the
 * engine's own reason (left out where it gave none), and the three things
 * that change something from here: the Account page, which reads no socket
 * and is the reader's own, drawn in place of this (`nav.ts`'s `personalPath`);
 * dialling again once an admin has acted; and signing out, because the person
 * refused is the person who needs to leave.
 *
 * NEVER FOR WANT OF ACCESS. The socket admits every principal the engine
 * resolves whose stage may act, whatever they hold — a person with no app
 * reads the overview — so a reader missing an app or Admin is told so by the
 * one screen that needs it (`GrantRequired`), and the rest of the dashboard
 * stays theirs.
 */

import { Button, EmptyState, useToast } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { useNavigator } from "~/app/router.tsx";
import { asSentence } from "~/lib/format.ts";
import { refusalText } from "~/lib/refusal.ts";
import { signOut } from "~/lib/session.ts";
import { useClient } from "~/lib/store-hooks.ts";

export function AccessRefused({ reason }: { reason: string }) {
  const nav = useNavigator();
  const toast = useToast();
  const { socket } = useClient();
  // AN EMPTY REASON IS LEFT OUT, never drawn as a sentence with nothing in it.
  const why = asSentence(reason);
  return (
    <EmptyState
      icon={<KeyGlyph />}
      title="You can’t use Crewlet right now"
      description={why ? `${why} Your Account still works.` : "Your Account still works."}
      action={
        <span className="row gap-2">
          <Button variant="primary" onClick={() => nav.to(["account"])}>
            Open Account
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
                toast.failed(`Signing out didn’t go through. ${refusalText(err)}`),
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
