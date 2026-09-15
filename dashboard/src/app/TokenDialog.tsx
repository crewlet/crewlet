/**
 * Collecting the operator's API token.
 *
 * A real dialog, because a credential request has to be able to say who is
 * asking and why. This used to be a `window.prompt`, which arrives in a
 * chrome-drawn box that can say neither.
 *
 * The dialog is raised ONCE per refusal, deliberately: a 30-second reconnect
 * backoff must not reopen it forever. Dismissing it leaves the banner saying
 * what is wrong, with a button back to here.
 *
 * ON `ui/Dialog.tsx`, which was GENERALISED FROM THIS ONE and which this one
 * then went on not using. So the shell it hand-rolled had no Escape and no
 * focus trap — on the single modal in the product that collects a credential,
 * where tabbing out into the page behind and being unable to press escape are
 * the two things a reader tries first.
 */

import { useRef, useState } from "react";
import { Button } from "~/ui/primitives.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { Dialog } from "~/ui/Dialog.tsx";
import { apiToken, clearToken, storeToken } from "~/protocol/index.ts";

export function TokenDialog({
  onClose,
  onSaved,
}: {
  onClose: () => void;
  onSaved: (token: string) => void;
}) {
  const [value, setValue] = useState(() => apiToken());
  const [refused, setRefused] = useState(false);
  // THE SHELL FOCUSES THE FIRST CONTROL, which is this input — so there is no
  // focus effect here any more, and the ref is only what the field needs.
  const ref = useRef<HTMLInputElement>(null);

  // A credential you can set is one you must be able to drop — on a shared
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
    <Dialog
      title="API token"
      icon="key"
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
          <Button variant="ghost" onClick={onClose}>
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
        <label htmlFor="token">Token</label>
        <input
          id="token"
          ref={ref}
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
          <Icon name="alert" size="sm" />
          <span>
            This browser refused to store the token (private mode, or blocked site data). It will
            work until you reload.
          </span>
        </div>
      )}
    </Dialog>
  );
}
