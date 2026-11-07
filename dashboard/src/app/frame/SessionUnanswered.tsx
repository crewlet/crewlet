/**
 * What every screen says when the frame asked the engine who this browser is
 * and heard nothing back: the read ran out its deadline
 * (`SESSION_READ_TIMEOUT_MS`) or never reached the engine, and the socket's
 * viewer has not answered either.
 *
 * DRAWN BY THE FRAME, IN PLACE OF THE SCREEN, as `AccessRefused` is. Without
 * a session the socket is not dialled, so every screen drew placeholders that
 * never resolved and nothing said why. This says what is known, that the
 * engine has not answered, and offers the one thing that can change it from
 * here: asking again.
 *
 * ONLY BEFORE ANY ANSWER. A tab that already knows who it is read by keeps
 * its screen when a later re-read fails; the state bar says the socket is
 * reconnecting, and that is the truth for it.
 */

import { useState } from "react";
import { Button, EmptyState } from "@crewlethq/ui";
import { ServerGlyph } from "@crewlethq/icons/glyphs";
import { useFrameSession } from "~/lib/frameSession.ts";

export function SessionUnanswered() {
  const session = useFrameSession();
  const [asking, setAsking] = useState(false);
  const askAgain = async () => {
    setAsking(true);
    await session.reload();
    setAsking(false);
  };
  return (
    <EmptyState
      icon={<ServerGlyph />}
      title="The engine hasn’t answered"
      description="Crewlet couldn’t reach the engine to check who is signed in here. Check that the engine is running and reachable from this browser, then try again."
      action={
        <Button variant="primary" loading={asking} onClick={() => void askAgain()}>
          Try again
        </Button>
      }
    />
  );
}
