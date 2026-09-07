/**
 * Taking an integration away, and deciding how far that goes.
 *
 * TWO DIFFERENT ACTS BEHIND ONE BUTTON, which is why this asks rather than
 * just confirming. Removing what the engine registered for ITSELF — the
 * webhooks — is never in question: nothing else uses them, and one left
 * behind delivers a company's events to an engine with no block to route
 * them. Removing the ACCOUNTS it created is a different thing: each is a
 * colleague at that third-party app with commits, comments and history attached, and
 * deleting one because somebody pressed Disconnect is not a decision a button
 * gets to make. So the checkbox is off until it is ticked.
 *
 * The disconnect is ASKED FOR, not done here. The engine keeps the block
 * until the third-party app teardown succeeds, because that block carries the
 * credential the teardown authenticates with, so the card goes to
 * Disconnecting and clears when the loop finishes.
 */

import { useState } from "react";
import { Dialog } from "~/ui/Dialog.tsx";
import { Button } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { rest, RestError } from "~/protocol/index.ts";

export function DisconnectDialog({
  name,
  kind,
  onClose,
  onDone,
}: {
  /** The tool's name, as the catalogue writes it. */
  name: string;
  /** The wire key of the surface being disconnected. */
  kind: string;
  onClose: () => void;
  onDone: () => void;
}) {
  const [removeSeats, setRemoveSeats] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function submit(force: boolean) {
    setBusy(true);
    setError(null);
    try {
      await rest.del(`/setup/integrations/${kind}`, { remove_seats: removeSeats, force });
      onDone();
      onClose();
    } catch (err) {
      // `message` rather than `detail || code`: those two are both empty on
      // a refusal that carried neither, and an empty string is falsy, so the
      // banner never rendered and the button looked like it did nothing.
      // RestError.message already falls back to the status itself.
      setError(err instanceof RestError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog
      title={`Disconnect ${name}`}
      icon="plug"
      onClose={onClose}
      dismissable={!busy}
      width={520}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button variant="danger" onClick={() => void submit(false)} disabled={busy}>
            {busy ? "Disconnecting" : "Disconnect"}
          </Button>
        </>
      }
    >
      <div className="col gap-3">
        <p className="t-body secondary" style={{ margin: 0 }}>
          Agents stop working in {name}. The engine removes the webhooks it registered, then drops
          the integration from your company configuration.
        </p>

        <label className="int-choice">
          <input
            type="checkbox"
            checked={removeSeats}
            disabled={busy}
            onChange={(e) => setRemoveSeats(e.target.checked)}
          />
          <span className="col" style={{ gap: 2 }}>
            <span className="t-body">Also remove the accounts Crewlet created</span>
            <span className="t-caption faint">
              Each agent&apos;s account at the vendor is deleted. What those accounts wrote stays,
              but they can do nothing more. Leave this off to keep them.
            </span>
          </span>
        </label>

        {error && (
          <div className="banner critical">
            <Icon name="alert" size="sm" />
            <span className="col" style={{ gap: 4 }}>
              <span>{error}</span>
              {/* THE WAY OUT of a teardown that can never succeed: a
                  revoked credential, an instance that is gone. Offered
                  only after one has actually failed, because it leaves
                  the vendor holding things nobody will remove. */}
              <span className="t-caption faint">
                Forcing drops the integration without waiting for {name}. Whatever it still holds
                becomes yours to remove there.
              </span>
              <span>
                <Button size="sm" variant="ghost" onClick={() => void submit(true)} disabled={busy}>
                  Disconnect anyway
                </Button>
              </span>
            </span>
          </div>
        )}
      </div>
    </Dialog>
  );
}
