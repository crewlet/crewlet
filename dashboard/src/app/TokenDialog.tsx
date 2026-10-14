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

/**
 * WHAT A SAVE OR A SIGN-OUT MOVES IS NOT THIS DIALOG'S TO SAY: both announce
 * the change through `protocol/authToken.ts`, where the shell's socket and
 * every screen holding a person's state hear it — as they hear a token another
 * tab saved, which no dialog here was open for.
 */
export function TokenDialog({ onClose }: { onClose: () => void }) {
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
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" type="submit">
            Save and reconnect
          </Button>
        </>
      }
    >
      <Text as="p" variant="body" tone="secondary">
        Paste a token from <InlineCode>api.auth.tokens</InlineCode> in your{" "}
        <InlineCode>crewlet.yaml</InlineCode>.
      </Text>
      {/* ON uilet's [FormField], which is what the hand-pairing here was. The
          label, the help line and the `aria-describedby` tying them to the
          control belong to the component now, so this screen writes the
          sentence and nothing else: the id the label points at and the id the
          help line is read from are minted together and cannot disagree. */}
      <FormField label="Token" helper="Stored in this browser only.">
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
          This browser can’t save the token. It works until you reload.
        </Callout>
      )}
    </Modal>
  );
}
