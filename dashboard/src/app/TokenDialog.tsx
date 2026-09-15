/**
 * Collecting the operator's API token.
 *
 * A real dialog, because a credential request has to be able to say who is
 * asking and why. This used to be a `window.prompt`, which arrives in a
 * chrome-drawn box that can say neither.
 *
 * The dialog is raised ONCE per refusal, deliberately: a 30-second reconnect
 * backoff must not reopen it forever. Dismissing it leaves the banner and the
 * engine panel saying what is wrong, both with a button back to here.
 *
 * It is the shared `Dialog`, on the layer stack like every other modal. A
 * refused request can raise it while a drawer is open, and a hand-rolled veil
 * beside the stack is how one Escape came to close both, or none: the stack
 * knows it opened last, so its Escape closes it alone and Tab stays inside it.
 */

import { useId, useState } from "react";
import { apiToken, clearToken, storeToken } from "~/protocol/index.ts";
import { ErrorGlyph, KeyGlyph } from "@crewlethq/icons/glyphs";
import { Button, Modal } from "@crewlethq/ui";

export function TokenDialog({
  onClose,
  onSaved,
}: {
  onClose: () => void;
  onSaved: (token: string) => void;
}) {
  const [value, setValue] = useState(() => apiToken());
  const [refused, setRefused] = useState(false);
  // A MINTED ID, not a literal. The dialog opens over whatever screen raised
  // it, and a label pointing at `id="token"` names the first element in the
  // document with that id: a screen's own field would take the label, and
  // the credential box would be announced with no name at all.
  const fieldID = useId();

  // A credential you can set is one you must be able to drop, on a shared
  // machine especially, where the token outlives the person who typed it.
  function signOut(): void {
    if (!clearToken()) {
      setRefused(true);
      return;
    }
    setValue("");
    onSaved("");
    onClose();
  }

  function save(): void {
    const token = value.trim();
    // The caller has to know when the browser refused the write: a silently
    // unsaved token works until the next reload and is then unauthenticated
    // again, with nothing on screen to explain why.
    if (!storeToken(token)) {
      setRefused(true);
      return;
    }
    onSaved(token);
    onClose();
  }

  return (
    <Modal
      open
      stackBody
      title="API token"
      icon={<KeyGlyph />}
      onClose={onClose}
      onSubmit={save}
      footer={
        <>
          {/* Only when there is something to drop. An always-present sign-out
              on a surface nobody has signed into is a control that does
              nothing, next to the one that does the work. */}
          {apiToken() !== "" && (
            <Button variant="danger" onClick={signOut}>
              Sign out
            </Button>
          )}
          <span className="spacer" />
          <Button variant="tertiary" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" type="submit">
            Save and reconnect
          </Button>
        </>
      }
    >
      <p className="t-body secondary" style={{ margin: 0 }}>
        The engine is guarding this surface. Paste a bearer token matching one of the{" "}
        <code className="inline">api.auth.tokens</code> entries in your{" "}
        <code className="inline">crewlet.yaml</code>.
      </p>
      <div className="field">
        <label htmlFor={fieldID}>Token</label>
        {/* The first control, so the layer stack opens the dialog on it. */}
        <input
          id={fieldID}
          className="input"
          type="password"
          value={value}
          autoComplete="off"
          spellCheck={false}
          onChange={(e) => setValue(e.target.value)}
        />
        <span className="hint">
          Stored in this browser only, and sent on the socket handshake and every guarded query.
        </span>
      </div>
      {refused && (
        <div className="banner critical">
          <ErrorGlyph size="sm" />
          <span>
            This browser refused to store the token (private mode, or blocked site data). It will
            work until you reload.
          </span>
        </div>
      )}
    </Modal>
  );
}
