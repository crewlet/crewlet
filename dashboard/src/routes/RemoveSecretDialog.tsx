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
import { rest, RestError } from "~/protocol/index.ts";
import { CheckGlyph, ErrorGlyph, KeyGlyph, WarningGlyph } from "@crewlethq/icons/glyphs";
import { Button, Callout, Checkbox, InlineCode, Modal } from "@crewlethq/ui";

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

  const referenced = paths !== null && paths.length > 0;
  const unchecked = paths === null;
  const needsAcknowledgement = referenced || unchecked;

  async function submit() {
    if (busy || (needsAcknowledgement && !acknowledged)) return;
    setBusy(true);
    setError(null);
    try {
      await rest.del("/secrets/" + encodeURIComponent(name));
      onDone();
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
      stackBody
      title={`Remove ${name}`}
      icon={<KeyGlyph />}
      onClose={onClose}
      dismissable={!busy}
      size="md"
      footer={
        <>
          <Button variant="tertiary" onClick={onClose} disabled={busy}>
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
      }
    >
      <p className="t-body secondary" style={{ margin: 0 }}>
        The sealed value is deleted from the fleet&apos;s store. Nothing can read it back
        afterwards, and rotating it later means supplying the credential again.
      </p>

      {referenced && (
        <Callout variant="danger" role="alert" icon={<ErrorGlyph size="sm" />}>
          <span className="col" style={{ gap: 6 }}>
            <span>
              {paths.length === 1
                ? "One field in the active configuration points at this name."
                : `${paths.length} fields in the active configuration point at this name.`}{" "}
              Removing it leaves each of them resolving to nothing, and the surfaces holding one
              start refusing deliveries.
            </span>
            <span className="col" style={{ gap: 2 }}>
              {paths.map((path) => (
                <InlineCode key={path}>{path}</InlineCode>
              ))}
            </span>
            <span className="t-caption">
              Point those fields somewhere else, or remove them, before removing this.
            </span>
          </span>
        </Callout>
      )}

      {unchecked && (
        <Callout variant="warning" icon={<WarningGlyph size="sm" />}>
          <span className="col" style={{ gap: 4 }}>
            <span>
              The active configuration could not be read, so it is not known whether anything points
              at this name.
            </span>
            {unknown && <span className="t-caption">{unknown}</span>}
          </span>
        </Callout>
      )}

      {!referenced && !unchecked && (
        <Callout variant="neutral" icon={<CheckGlyph size="sm" />}>
          <span>
            No field in the active configuration names this. Removing it changes nothing the company
            is running.
          </span>
        </Callout>
      )}

      {needsAcknowledgement && (
        <Checkbox
          framed
          tone="danger"
          checked={acknowledged}
          disabled={busy}
          onCheckedChange={setAcknowledged}
          label={
            referenced
              ? "Remove it anyway, and leave those fields pointing at nothing"
              : "Remove it without knowing what points at it"
          }
        />
      )}

      {error && (
        <Callout variant="danger" role="alert" icon={<ErrorGlyph size="sm" />}>
          <span>{error}</span>
        </Callout>
      )}
    </Modal>
  );
}
