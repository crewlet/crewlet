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
 * ON uilet's [Modal], which is what `ui/Dialog.tsx` was — a veil, a frame, a
 * head/body/foot and the behaviour around them — with the two things ours had
 * to be told: the surface is named to a screen reader by an id the component
 * mints rather than by a label a caller can forget, and the layer stack owns
 * Escape, so a prompt raised over this one closes itself rather than this one.
 * `footerStart` is the `<span className="spacer" />` ours pushed Sign out over
 * with, as a real slot.
 */

import { useState } from "react";
import { Button, Callout, FormField, InlineCode, Input, Modal, Text } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
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
    <Modal
      open
      title="API token"
      icon={<KeyGlyph size="md" />}
      onClose={onClose}
      onSubmit={save}
      stackBody
      footerStart={
        // Only when there is something to drop. An always-present sign-out on
        // a surface nobody has signed into is a control that does nothing,
        // next to the one that does the work.
        apiToken() !== "" ? (
          <Button variant="danger" onClick={signOut}>
            Sign out
          </Button>
        ) : undefined
      }
      footer={
        <>
          <Button variant="tertiary" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" type="submit">
            Save and reconnect
          </Button>
        </>
      }
    >
      <Text as="p" variant="body" tone="secondary">
        The engine is guarding this surface. Paste a bearer token matching one of the{" "}
        <InlineCode>api.auth.tokens</InlineCode> entries in your{" "}
        <InlineCode>crewlet.yaml</InlineCode>.
      </Text>
      {/* ON uilet's [FormField], which is what the hand-pairing here was. The
          label, the help line and the `aria-describedby` tying them to the
          control belong to the component now, so this screen writes the
          sentence and nothing else: the id the label points at and the id the
          help line is read from are minted together and cannot disagree. */}
      <FormField
        label="Token"
        helper="Stored in this browser only, and sent on the socket handshake and every guarded query."
      >
        {(field) => (
          <Input
            id={field.id}
            type="password"
            width="full"
            value={value}
            autoComplete="off"
            spellCheck={false}
            aria-describedby={field.describedBy}
            onChange={(e) => setValue(e.target.value)}
          />
        )}
      </FormField>
      {refused && (
        // LIVE, because it appears in response to a press the reader just
        // made. Not `role="alert"`: the dialog is already what they are
        // looking at, so nothing has to interrupt to get them there.
        <Callout variant="danger" live="polite">
          This browser refused to store the token (private mode, or blocked site data). It will work
          until you reload.
        </Callout>
      )}
    </Modal>
  );
}
