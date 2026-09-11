/**
 * Taking an integration away, and deciding how far that goes.
 *
 * TWO DIFFERENT ACTS BEHIND ONE BUTTON, which is why this asks rather than
 * just confirming. Removing what the engine registered for ITSELF — the
 * webhooks — is never in question: nothing else uses them, and one left
 * behind delivers a company's events to an engine with no block to route
 * them. Removing the ACCOUNTS it created is a different thing: each is a
 * colleague at that third-party app with commits, comments and history
 * attached, and deleting one because somebody pressed Disconnect is not a
 * decision a button gets to make. So the checkbox is off until it is ticked.
 *
 * The disconnect is ASKED FOR, not done here. The engine keeps the block
 * until the third-party app teardown succeeds, because that block carries the
 * credential the teardown authenticates with, so the card goes to
 * Disconnecting and clears when the loop finishes.
 */

import { useState } from "react";
import { Dialog } from "~/ui/Dialog.tsx";
import { Avatar, Button } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { marked } from "~/ui/Problems.tsx";
import { rest, RestError } from "~/protocol/index.ts";

export function DisconnectDialog({
  name,
  kinds,
  stuck,
  apps,
  appPath,
  onClose,
  onDone,
}: {
  /** The tool's name, as the catalogue writes it. */
  name: string;
  /** The wire key of the surface being disconnected. */
  /**
   * Every surface this card covers, in the order they are taken away.
   *
   * A CARD IS THE UNIT, not a surface: Atlassian is an organization and two
   * products, and taking only the first left the other two connected under a
   * card still reading Connected.
   */
  kinds: string[];
  /**
   * Why a disconnect already asked for has not finished, when one has not.
   *
   * THE REASON THIS IS A PROP. Forcing used to appear only after the REQUEST
   * failed — but a teardown fails in the LOOP, minutes later, so the request
   * had already answered 202 and the only way out of a permanently refusing
   * app was a shell. The card knows the surface is stuck; the dialog needs
   * telling so it can offer the way out.
   */
  stuck?: string;
  /**
   * The per-agent apps this disconnect cannot delete on its own, with the
   * page a person deletes each one from.
   *
   * GitHub is the case: uninstalling an app revokes its access, and that the
   * engine does, but there is no endpoint at any permission for deleting the
   * app registration. Offering the links beside the checkbox is the honest
   * shape: the engine says what it will do, and hands over what it cannot.
   */
  apps?: { handle: string; name: string; url: string }[];
  /**
   * What a person clicks at the third-party app to finish one of them off,
   * once the link has opened it. The app's own words, stated once because it
   * is the same for every agent.
   */
  appPath?: string;
  onClose: () => void;
  onDone: () => void;
}) {
  const [removeSeats, setRemoveSeats] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(stuck ?? null);
  /**
   * The credentials the engine has left in the store, once it has answered.
   *
   * NAMED, NOT DELETED, deliberately: one an operator may be sharing with
   * another deployment is not something a Disconnect button decides about.
   * The API has always said so and has always returned the list — and this
   * dialog threw every response away and closed, so nobody ever saw it.
   */
  const [orphans, setOrphans] = useState<string[] | null>(null);

  async function submit(force: boolean) {
    setBusy(true);
    setError(null);
    try {
      // ONE AT A TIME, in order, and the first refusal stops the rest.
      //
      // The organization goes LAST on Atlassian because the two products are
      // reached with their own credentials and the organization's is what
      // removes the accounts: taking it first would strand whatever the
      // products still hold.
      const left = new Set<string>();
      for (const kind of kinds) {
        const answer = (await rest.del(`/setup/integrations/${kind}`, {
          remove_seats: removeSeats,
          force,
        })) as { orphaned_secrets?: string[] } | undefined;
        for (const name of answer?.orphaned_secrets ?? []) left.add(name);
      }
      onDone();
      if (left.size === 0) {
        onClose();
        return;
      }
      // HELD OPEN, because closing is what lost the list. The disconnect has
      // already been asked for — onDone has run — so what is left on screen
      // is the part the operator still has to do.
      setOrphans([...left].sort());
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

  // WHAT IS LEFT TO DO, once the disconnect has been asked for. The engine
  // does not delete a company's credentials and never has; this is the half
  // of that promise nobody could see.
  if (orphans) {
    return (
      <Dialog
        title={`${name} disconnected`}
        icon="plug"
        onClose={onClose}
        width={520}
        footer={
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        }
      >
        <div className="col gap-3">
          <p className="t-body secondary" style={{ margin: 0 }}>
            These credentials are still in your secret store. They are named rather than deleted:
            one you share with another deployment is not something this button decides about.
          </p>
          <ul className="col gap-1" style={{ margin: 0, paddingLeft: "1.1rem" }}>
            {orphans.map((name) => (
              <li key={name}>
                <code className="inline">{name}</code>
              </li>
            ))}
          </ul>
          <p className="t-body secondary" style={{ margin: 0 }}>
            Revoke each one at the app, then remove it with{" "}
            <code className="inline">crewlet secrets unset &lt;name&gt;</code>.
          </p>
        </div>
      </Dialog>
    );
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

        {/* WHO IS LEFT, and where. The engine uninstalls each agent's app,
            which stops it acting at once, and cannot delete the app itself:
            neither vendor offers that at any permission this engine could
            hold. So the agents are named, as agents rather than as handles in
            a sentence, each with the page that finishes it off.

            A ROW PER AGENT, not a bulleted list of links: this is the same
            roster the card shows, and the question it answers is "which of my
            colleagues do I still have to go and remove", which is a list of
            people rather than a list of URLs. */}
        {removeSeats && apps && apps.length > 0 && (
          <div className="col gap-2">
            <span className="t-caption faint">
              Each agent&apos;s app is uninstalled, which stops it acting immediately. Deleting the
              app itself is yours to do{appPath ? <>: {marked(appPath)}</> : null}.
            </span>
            <ul className="int-rows">
              {apps.map((app) => (
                <li key={app.handle} className="int-row int-seat-row">
                  <Avatar name={app.name || app.handle} size="sm" />
                  <div className="int-row-identity">
                    <span className="int-row-name">{app.name || app.handle}</span>
                  </div>
                  <a
                    className="t-caption int-row-link"
                    href={app.url}
                    target="_blank"
                    rel="noreferrer"
                  >
                    <Icon name="external" size="xs" />
                    Link to delete
                  </a>
                </li>
              ))}
            </ul>
          </div>
        )}

        {error && (
          <div className="banner critical">
            <Icon name="alert" size="sm" />
            <span className="col" style={{ gap: 4 }}>
              {stuck && (
                <span>
                  This disconnect was already asked for and {name} has not let the engine finish it.
                </span>
              )}
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
