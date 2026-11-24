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
import { Button, Callout, ResourceName, type ConsequenceItem } from "@crewlethq/ui";
import { ConfirmModal, Modal } from "~/components/Modal.tsx";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
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

  async function submit() {
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

  if (unapplied) {
    return (
      <Modal
        open
        title={`Remove ${name}`}
        icon={<KeyGlyph size="md" />}
        onClose={onClose}
        size="md"
        stackBody
        footer={
          <Button variant="primary" onClick={onClose}>
            Close
          </Button>
        }
      >
        <Callout variant="warning" role="alert">
          <span className="col" style={{ gap: 4 }}>
            <span>Removed, but the company is still using the old value.</span>
            <span className="t-caption">{unapplied}</span>
            <span className="t-caption">It stops at the next configuration change.</span>
          </span>
        </Callout>
      </Modal>
    );
  }

  const acknowledgement = (label: string, description?: string): ConsequenceItem => ({
    kind: "choice",
    key: "anyway",
    label,
    description,
    tone: "danger",
    required: true,
    checked: acknowledged,
    disabled: busy,
    onCheckedChange: setAcknowledged,
  });
  const consequences: ConsequenceItem[] = referenced
    ? [
        ...paths.map((path): ConsequenceItem => ({
          kind: "fact",
          key: path,
          tone: "warning",
          text: (
            <>
              <ResourceName>{path}</ResourceName> stops working
            </>
          ),
        })),
        acknowledgement(
          "Remove it anyway",
          paths.length === 1
            ? "Repoint it first to keep it working."
            : "Repoint them first to keep them working.",
        ),
      ]
    : unchecked
      ? [
          {
            kind: "fact",
            key: "unchecked",
            tone: "warning",
            text: "Couldn’t check whether anything uses it.",
          },
          ...(unknown ? [{ kind: "fact" as const, key: "why", text: unknown }] : []),
          acknowledgement("Remove it without knowing what points at it"),
        ]
      : [{ kind: "fact", key: "unused", tone: "info", text: "Nothing uses it." }];

  return (
    <ConfirmModal
      open
      destructive
      shape="framed"
      size="md"
      title={`Remove ${name}`}
      icon={<KeyGlyph size="md" />}
      onClose={onClose}
      onConfirm={submit}
      // A REMOVAL IN FLIGHT CANNOT BE ABANDONED: the row may or may not be gone.
      submitting={busy}
      busyReason="Waiting for the store to answer."
      message="The value is deleted and can’t be recovered."
      consequencesHeading={
        referenced
          ? paths.length === 1
            ? "One field uses it"
            : `${paths.length} fields use it`
          : "When you remove it"
      }
      consequences={consequences}
      confirmLabel="Remove"
      confirmDisabledReason={
        referenced ? "Tick Remove it anyway to remove it." : "Tick the box to remove it."
      }
    >
      {error && (
        <Callout variant="danger" role="alert">
          <span>{error}</span>
        </Callout>
      )}
    </ConfirmModal>
  );
}
