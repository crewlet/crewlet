/**
 * Storing a credential, and rotating one.
 *
 * ONE DIALOG FOR BOTH, because they are one write: `PUT /secrets/{name}` does
 * not care whether a row was there, and a second dialog would be a second
 * place for the keyring refusal, the size cap and the name rule to be handled
 * differently. What changes between them is whether the name is still the
 * operator's to choose.
 *
 * THE VALUE NEVER ARRIVES WITH THE FORM. A rotation opens on an empty secret
 * field, and that is not an oversight: the engine's one route that returns a
 * value is break-glass and logs the access, so this page has never held one
 * and must not start. [Field] refuses a default value for a secret kind for
 * the same reason.
 *
 * THE NAME RULE IS COPY, NOT A SECOND REGEX. A secret is keyed by
 * environment-variable name and the engine refuses anything else by name,
 * carrying the rule in its own refusal. A pattern written here too would be a
 * fourth copy of the ${VAR} grammar, and the two the engine already retired
 * had both drifted: one displayed a literal secret unmasked, the other would
 * mint a credential into a variable nothing reads. So the help line says the
 * rule and the engine remains the judge.
 */

import { useState } from "react";
import { Dialog } from "~/ui/Dialog.tsx";
import { Button } from "~/ui/primitives.tsx";
import { Field } from "~/ui/Field.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { rest, RestError } from "~/protocol/index.ts";

export function SecretDialog({
  /** The name being rotated, or "" to store a new one. */
  rotating,
  /**
   * The config fields that read this name, or null when the reference index
   * could not be read. Shown on a rotation because the value a running seat
   * holds was captured when its provider was built, so an operator needs to
   * know how many of them are about to go stale.
   */
  paths,
  onClose,
  onDone,
}: {
  rotating: string;
  paths: string[] | null;
  onClose: () => void;
  onDone: (name: string) => void;
}) {
  const [name, setName] = useState(rotating);
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const readers = paths ?? [];

  async function submit() {
    if (busy || !name.trim() || value === "") return;
    setBusy(true);
    setError(null);
    try {
      // THE PATH SEGMENT IS ENCODED, and the value goes as the body. A name
      // is the operator's text until the engine has judged it, so it cannot
      // be pasted into a URL raw.
      await rest.putText("/secrets/" + encodeURIComponent(name.trim()), value);
      onDone(name.trim());
      onClose();
    } catch (err) {
      // `message` rather than `detail || code`: a refusal that carried
      // neither leaves both empty, and an empty string is falsy, so the
      // banner would never render and the button would look inert.
      setError(err instanceof RestError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog
      title={rotating ? `Rotate ${rotating}` : "Store a secret"}
      icon="key"
      onClose={onClose}
      dismissable={!busy}
      width={520}
      onSubmit={() => void submit()}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={busy || !name.trim() || value === ""}>
            {busy ? "Saving" : rotating ? "Rotate" : "Store"}
          </Button>
        </>
      }
    >
      {rotating ? (
        <p className="t-body secondary" style={{ margin: 0 }}>
          The value replaces what the fleet holds under <code className="inline">{rotating}</code>.
          Every node reads the same row, so nothing has to be copied anywhere.
        </p>
      ) : (
        <Field
          label="Name"
          value={name}
          onChange={setName}
          autoFocus
          placeholder="GITLAB_TOKEN_SWE"
          help="The name a ${VAR} in the company configuration points at. Letters, digits and underscores, starting with a letter or an underscore."
        />
      )}

      <Field
        label={rotating ? "New value" : "Value"}
        kind="secret"
        value={value}
        onChange={setValue}
        autoFocus={Boolean(rotating)}
        help="Sealed with this node's keyring before it leaves the browser's request. This page never reads a value back."
      />

      {rotating && readers.length > 0 && (
        <div className="banner neutral">
          <Icon name="info" size="sm" />
          <span className="col" style={{ gap: 4 }}>
            <span>
              {readers.length === 1
                ? "One config field reads"
                : `${readers.length} config fields read`}{" "}
              this name. The pointer keeps pointing at it, so nothing breaks.
            </span>
            {/* WHEN it takes effect, because a rotation that appears to
                succeed and quietly does not is the failure the store exists
                to remove. A provider holds the value it was built with. */}
            <span className="t-caption faint">
              A running seat keeps the value it was built with until the configuration is
              re-activated or the node restarts.
            </span>
          </span>
        </div>
      )}

      {error && (
        <div className="banner critical" role="alert">
          <Icon name="alert" size="sm" />
          <span>{error}</span>
        </div>
      )}
    </Dialog>
  );
}
