/**
 * The person sidebar's one word about the connection, drawn only while it is
 * in trouble.
 *
 * A PERSON'S SCREEN IS NOT A STATUS PAGE. The engine's health — its nodes, its
 * posture, its alarms — is the console's (`sidebar/HealthCard.tsx`, drawn in
 * the console's foot), and a card about it on every screen of a person's day
 * is a fact about somebody else's job. What a person does need is the two
 * states that change what they are looking at: the page is showing the last
 * thing it heard while it reconnects, or the engine stopped accepting this
 * browser's key and nothing new will arrive until they sign in again — which
 * is the one state a press here fixes.
 *
 * THE SAME PRECEDENCE THE HEALTH CARD READS (`healthReading`): a refused key
 * first, because while it stands the socket is closed as well and
 * "Reconnecting" would never end.
 */

import { StatusDot } from "@crewlethq/ui";
import { useConnection } from "~/lib/store-hooks.ts";

export function ConnectionLine({ onSetToken }: { onSetToken: () => void }) {
  const { connected, authRejected } = useConnection();
  if (authRejected) {
    return (
      <button type="button" className="side-connection" data-tone="danger" onClick={onSetToken}>
        Signed out — Sign in again
      </button>
    );
  }
  if (!connected) {
    return (
      <p className="side-connection" role="status">
        {/* THE KIT HAS NO SPINNER; its steady-state pulse, which holds still
            under reduced motion, is the one mark it draws for "ongoing". */}
        <StatusDot tone="warning" pulse />
        Reconnecting…
      </p>
    );
  }
  return null;
}
