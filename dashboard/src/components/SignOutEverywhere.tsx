/**
 * Signing out everywhere, asked first.
 *
 * ONE CLICK DID IT, from the user menu and the Account page alike, and it ends
 * more than the sessions its name says: every personal access token the person
 * minted stops working with them, so a script or an assistant using one breaks
 * at once. An administrator's "End all sessions" asks before it acts; ending
 * your own, tokens included, asks too.
 *
 * Opened by whoever holds the button, and never from inside the user menu's
 * popover: a popover opens nothing of its own, so the menu closes and the block
 * that holds the menu opens this.
 */

import { useState } from "react";
import { Button, Callout, Modal, Text } from "@crewlethq/ui";
import { refusalText } from "~/lib/refusal.ts";
import { signOutEverywhere } from "~/lib/session.ts";
import { useClient } from "~/lib/store-hooks.ts";

export function SignOutEverywhereDialog({ onClose }: { onClose: () => void }) {
  const { socket } = useClient();
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);

  async function confirm() {
    if (busy) return;
    setBusy(true);
    setRefusal(null);
    try {
      // A SIGN-OUT THAT LANDED LEAVES THE PAGE, so nothing here settles it.
      await signOutEverywhere(socket);
    } catch (err) {
      setRefusal(`Signing out everywhere did not go through. ${refusalText(err)}`);
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      size="sm"
      title="Sign out everywhere?"
      onClose={onClose}
      dismissable={!busy}
      closeDisabledReason="Waiting for the engine to answer."
      stackBody
      onSubmit={() => void confirm()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button type="submit" variant="danger" disabled={busy}>
            {busy ? "Signing out" : "Sign out everywhere"}
          </Button>
        </>
      }
    >
      <Text as="p" variant="body">
        This ends every session you hold, this one included, and every personal access token you
        minted: a script or an assistant using one stops working until you mint it a new one.
      </Text>
      {refusal && (
        <Callout variant="danger" role="alert">
          {refusal}
        </Callout>
      )}
    </Modal>
  );
}
