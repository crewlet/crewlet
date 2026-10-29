/**
 * Taking a credential away, and saying first what was reading it.
 *
 * THIS IS THE SHARP EDGE OF THE WHOLE SCREEN. The company configuration keeps
 * `${VAR}` POINTERS rather than credentials, so removing a row does not fail
 * anywhere: every pointer at it simply resolves to the empty string at the
 * next activation, and the webhook route or transport holding one starts
 * refusing deliveries with nothing in the refusal naming the row that went
 * away. An operator tidying up a list of names cannot see that from the list.
 *
 * So the confirmation shows the config paths that name it, read from
 * `GET /config/references`, and the answer is THREE-VALUED. "These fields
 * read it", "no field reads it" and "the configuration could not be read" are
 * three different facts, and the third must never be shown as the second: a
 * confirmation that said "nothing points at this" because a read failed is
 * worse than one that never checked, because it was believed.
 *
 * The acknowledgement is required exactly when the removal is not known to be
 * safe — something reads it, or nothing could be established. A row nothing
 * names is an ordinary delete and does not need a second gesture to prove the
 * operator meant it.
 */

import { useState } from "react";
import { Button, Callout, Checkbox, InlineCode, Modal } from "@crewlethq/ui";
import { CheckGlyph, KeyGlyph } from "@crewlethq/icons/glyphs";
import { pathSegment, rest, RestError } from "~/protocol/index.ts";
import { reloadFailure } from "./secretReload.ts";

export function RemoveSecretDialog({
  name,
  /** The config fields that name it, or null when that could not be read. */
  paths,
  /** Why the reference index is unknown, when it is. */
  unknown,
  onClose,
  onDone,
}: {
  name: string;
  paths: string[] | null;
  unknown: string | null;
  onClose: () => void;
  onDone: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [acknowledged, setAcknowledged] = useState(false);
  // THE ROW IS GONE AND THE COMPANY STILL READS IT: the removal answered
  // 200, and its re-activation did not land.
  const [unapplied, setUnapplied] = useState<string | null>(null);

  const referenced = paths !== null && paths.length > 0;
  const unchecked = paths === null;
  const needsAcknowledgement = referenced || unchecked;

  async function submit() {
    if (busy || (needsAcknowledgement && !acknowledged)) return;
    setBusy(true);
    setError(null);
    try {
      const answer = await rest.del("/secrets/" + pathSegment(name));
      onDone();
      const failure = reloadFailure(answer);
      if (failure) {
        setUnapplied(failure);
        return;
      }
      onClose();
    } catch (err) {
      setError(err instanceof RestError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      title={`Remove ${name}`}
      icon={<KeyGlyph size="md" />}
      onClose={onClose}
      // A REMOVAL IN FLIGHT CANNOT BE ABANDONED, and the close control says
      // so rather than going quiet: the row may or may not be gone, and a
      // dialog dismissed mid-request is how nobody finds out which.
      dismissable={!busy}
      closeDisabledReason="Waiting for the store to answer."
      size="md"
      stackBody
      footer={
        unapplied ? (
          <Button variant="primary" onClick={onClose}>
            Close
          </Button>
        ) : (
          <>
            <Button variant="ghost" onClick={onClose} disabled={busy}>
              Cancel
            </Button>
            <Button
              variant="danger"
              onClick={() => void submit()}
              disabled={busy || (needsAcknowledgement && !acknowledged)}
            >
              {busy ? "Removing" : "Remove"}
            </Button>
          </>
        )
      }
    >
      <p className="t-body secondary" style={{ margin: 0 }}>
        The value is deleted and can’t be recovered.
      </p>

      {referenced && (
        <Callout variant="danger" role="alert">
          <span className="col" style={{ gap: 6 }}>
            <span>
              {paths.length === 1
                ? "One field uses it and will stop working:"
                : `${paths.length} fields use it and will stop working:`}
            </span>
            <span className="col" style={{ gap: 2 }}>
              {paths.map((path) => (
                <InlineCode key={path}>{path}</InlineCode>
              ))}
            </span>
            <span className="t-caption">
              {paths.length === 1 ? "Repoint it first." : "Repoint them first."}
            </span>
          </span>
        </Callout>
      )}

      {unchecked && (
        <Callout variant="warning">
          <span className="col" style={{ gap: 4 }}>
            <span>Couldn’t check whether anything uses it.</span>
            {unknown && <span className="t-caption">{unknown}</span>}
          </span>
        </Callout>
      )}

      {/* NEUTRAL, not success: "nothing points at this" is a fact about the
          configuration rather than a good outcome, and the tone rule is that
          colour carries state. The tick is the mark our own banner drew, so it
          is passed rather than taking Callout's neutral info glyph. */}
      {!referenced && !unchecked && (
        <Callout variant="neutral" icon={<CheckGlyph size="md" />}>
          <span>Nothing uses it.</span>
        </Callout>
      )}

      {needsAcknowledgement && !unapplied && (
        <Checkbox
          framed
          tone="danger"
          checked={acknowledged}
          disabled={busy}
          onCheckedChange={setAcknowledged}
          label={referenced ? "Remove it anyway" : "Remove it without knowing what points at it"}
        />
      )}

      {unapplied && (
        <Callout variant="warning" role="alert">
          <span className="col" style={{ gap: 4 }}>
            <span>Removed, but the company is still using the old value.</span>
            <span className="t-caption">{unapplied}</span>
            <span className="t-caption">It stops at the next configuration change.</span>
          </span>
        </Callout>
      )}

      {error && (
        <Callout variant="danger" role="alert">
          <span>{error}</span>
        </Callout>
      )}
    </Modal>
  );
}
