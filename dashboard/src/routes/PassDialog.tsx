/**
 * Confirming a provisioning pass, and collecting the credential it runs as.
 *
 * A pass is not a form submission: it goes and does things at the vendor that
 * outlive the request. GitLab's creates a service account per agent, mints a
 * token on each and adds them to a group. So the button that starts one says
 * what it will do before it does it, rather than after.
 *
 * # The administrator credential is transient, and this says so
 *
 * A pass that needs one asks on every run. It is never stored, never written
 * into the company configuration and never sealed: a group Owner token held
 * permanently is a standing power to create accounts, where the same token
 * asked for once is a grant with an end. The engine drops it the moment the
 * pass returns, and this field is cleared with the dialog.
 */

import { useState } from "react";
import { Button } from "~/ui/primitives.tsx";
import { Dialog } from "~/ui/Dialog.tsx";
import { Field } from "~/ui/Field.tsx";
import { Icon } from "~/ui/Icon.tsx";
import type { SetupToolState } from "~/protocol/index.ts";

export function PassDialog({
  tool,
  title,
  onClose,
  onRun,
}: {
  tool: SetupToolState;
  /** The tool's own name, for the sentence describing what happens. */
  title: string;
  onClose: () => void;
  /** Start the pass. The credential is empty when the pass needs none. */
  onRun: (operatorCredential: string) => void;
}) {
  const needs = tool.needs_operator;
  const [credential, setCredential] = useState("");
  const ready = !needs || credential.trim() !== "";

  return (
    <Dialog
      title={`Set up ${title}`}
      icon="zap"
      onClose={onClose}
      width={520}
      onSubmit={() => ready && onRun(credential)}
      footer={
        <>
          <span className="spacer" />
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" type="submit" disabled={!ready}>
            Run
          </Button>
        </>
      }
    >
      <span className="t-body secondary">
        This runs {title}&apos;s own setup against the vendor: it registers what the engine needs to
        receive events, and creates whatever this vendor requires an agent to act as. It is safe to
        run again.
      </span>

      {needs && (
        <>
          <Field
            label={needs.label}
            kind="secret"
            value={credential}
            onChange={setCredential}
            required
            autoFocus
            help={
              <>
                {needs.help}
                {needs.where && <> {needs.where}</>}
                {needs.vendor_url && (
                  <>
                    {" "}
                    <a href={needs.vendor_url} target="_blank" rel="noreferrer">
                      Open at the vendor
                    </a>
                  </>
                )}
              </>
            }
          />
          <div className="banner neutral">
            <Icon name="shield" size="sm" />
            <span>
              Used for this run only. It is not stored, not sealed and not written into the company
              configuration.
            </span>
          </div>
        </>
      )}
    </Dialog>
  );
}
